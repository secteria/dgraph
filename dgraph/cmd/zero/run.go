/*
 * Copyright 2017-2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package zero

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.opencensus.io/plugin/ocgrpc"
	otrace "go.opencensus.io/trace"
	"go.opencensus.io/zpages"
	"golang.org/x/net/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/dgraph-io/badger/v2"
	bopt "github.com/dgraph-io/badger/v2/options"
	"github.com/dgraph-io/dgraph/conn"
	"github.com/dgraph-io/dgraph/ee/enc"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/raftwal"
	"github.com/dgraph-io/dgraph/x"
	"github.com/dgraph-io/ristretto/z"
	"github.com/golang/glog"
	"github.com/spf13/cobra"
)

type options struct {
	bindall           bool
	myAddr            string
	portOffset        int
	nodeId            uint64
	numReplicas       int
	peer              string
	w                 string
	rebalanceInterval time.Duration
	LudicrousMode     bool

	totalCache        int64
	cachePercentage   string
	tlsDir            string
	tlsDisabledRoutes []string
	tlsClientConfig   *tls.Config
}

var opts options

// Zero is the sub-command used to start Zero servers.
var Zero x.SubCommand

func init() {
	Zero.Cmd = &cobra.Command{
		Use:   "zero",
		Short: "Run Dgraph Zero",
		Long: `
A Dgraph Zero instance manages the Dgraph cluster.  Typically, a single Zero
instance is sufficient for the cluster; however, one can run multiple Zero
instances to achieve high-availability.
`,
		Run: func(cmd *cobra.Command, args []string) {
			defer x.StartProfile(Zero.Conf).Stop()
			run()
		},
	}
	Zero.EnvPrefix = "DGRAPH_ZERO"

	flag := Zero.Cmd.Flags()
	flag.String("my", "",
		"addr:port of this server, so other Dgraph alphas can talk to this.")
	flag.IntP("port_offset", "o", 0,
		"Value added to all listening port numbers. [Grpc=5080, HTTP=6080]")
	flag.Uint64("idx", 1, "Unique node index for this server. idx cannot be 0.")
	flag.Int("replicas", 1, "How many replicas to run per data shard."+
		" The count includes the original shard.")
	flag.String("peer", "", "Address of another dgraphzero server.")
	flag.StringP("wal", "w", "zw", "Directory storing WAL.")
	flag.Duration("rebalance_interval", 8*time.Minute, "Interval for trying a predicate move.")
	flag.Bool("telemetry", true, "Send anonymous telemetry data to Dgraph devs.")
	flag.Bool("enable_sentry", true, "Turn on/off sending events to Sentry. (default on)")

	// OpenCensus flags.
	flag.Float64("trace", 0.01, "The ratio of queries to trace.")
	flag.String("jaeger.collector", "", "Send opencensus traces to Jaeger.")
	// See https://github.com/DataDog/opencensus-go-exporter-datadog/issues/34
	// about the status of supporting annotation logs through the datadog exporter
	flag.String("datadog.collector", "", "Send opencensus traces to Datadog. As of now, the trace"+
		" exporter does not support annotation logs and would discard them.")
	flag.Bool("ludicrous_mode", false, "Run zero in ludicrous mode")
	flag.String("enterprise_license", "", "Path to the enterprise license file.")
	// TLS configurations
	flag.String("tls_dir", "", "Path to directory that has TLS certificates and keys.")
	flag.Bool("tls_use_system_ca", true, "Include System CA into CA Certs.")
	flag.String("tls_client_auth", "VERIFYIFGIVEN", "Enable TLS client authentication")
	flag.Bool("tls_internal_port_enabled", false, "(optional) enable inter node TLS encryption between cluster nodes.")
	flag.String("tls_cert", "", "(optional) The Cert file name in tls_dir which is needed to "+
		"connect as a client with the other nodes in the cluster.")
	flag.String("tls_key", "", "(optional) The private key file name "+
		"in tls_dir which is needed to connect as a client with the other nodes in the cluster.")

	// Cache flags
	flag.Int64("cache_mb", 0, "Total size of cache (in MB) to be used in zero.")
	flag.String("cache_percentage", "100,0",
		"Cache percentages summing up to 100 for various caches (FORMAT: blockCache,indexCache).")

	// Badger flags
	flag.String("badger.tables", "mmap",
		"[ram, mmap, disk] Specifies how Badger LSM tree is stored for write-ahead log directory "+
			"write-ahead directory. Option sequence consume most to least RAM while providing "+
			"best to worst read performance respectively")
	flag.String("badger.vlog", "mmap",
		"[mmap, disk] Specifies how Badger Value log is stored for the write-ahead log directory "+
			"log directory. mmap consumes more RAM, but provides better performance.")
	flag.Int("badger.compression_level", 3,
		"The compression level for Badger. A higher value uses more resources.")
}

func setupListener(addr string, port int, kind string) (listener net.Listener, err error) {
	laddr := fmt.Sprintf("%s:%d", addr, port)
	glog.Infof("Setting up %s listener at: %v\n", kind, laddr)
	return net.Listen("tcp", laddr)
}

type state struct {
	node *node
	rs   *conn.RaftServer
	zero *Server
}

func (st *state) serveGRPC(l net.Listener, store *raftwal.DiskStorage) {
	x.RegisterExporters(Zero.Conf, "dgraph.zero")
	grpcOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(x.GrpcMaxSize),
		grpc.MaxSendMsgSize(x.GrpcMaxSize),
		grpc.MaxConcurrentStreams(1000),
		grpc.StatsHandler(&ocgrpc.ServerHandler{}),
	}

	tlsConf, err := x.LoadServerTLSConfigForInternalPort(Zero.Conf.GetBool("tls_internal_port_enabled"), Zero.Conf.GetString("tls_dir"))
	x.Check(err)
	if tlsConf != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(tlsConf)))
	}
	s := grpc.NewServer(grpcOpts...)

	rc := pb.RaftContext{Id: opts.nodeId, Addr: opts.myAddr, Group: 0}
	m := conn.NewNode(&rc, store, opts.tlsClientConfig)

	// Zero followers should not be forwarding proposals to the leader, to avoid txn commits which
	// were calculated in a previous Zero leader.
	m.Cfg.DisableProposalForwarding = true
	st.rs = conn.NewRaftServer(m)

	st.node = &node{Node: m, ctx: context.Background(), closer: z.NewCloser(1)}
	st.zero = &Server{NumReplicas: opts.numReplicas, Node: st.node, tlsClientConfig: opts.tlsClientConfig}
	st.zero.Init()
	st.node.server = st.zero

	pb.RegisterZeroServer(s, st.zero)
	pb.RegisterRaftServer(s, st.rs)

	go func() {
		defer st.zero.closer.Done()
		err := s.Serve(l)
		glog.Infof("gRPC server stopped : %v", err)

		// Attempt graceful stop (waits for pending RPCs), but force a stop if
		// it doesn't happen in a reasonable amount of time.
		done := make(chan struct{})
		const timeout = 5 * time.Second
		go func() {
			s.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(timeout):
			glog.Infof("Stopping grpc gracefully is taking longer than %v."+
				" Force stopping now. Pending RPCs will be abandoned.", timeout)
			s.Stop()
		}
	}()
}

func run() {
	if Zero.Conf.GetBool("enable_sentry") {
		x.InitSentry(enc.EeBuild)
		defer x.FlushSentry()
		x.ConfigureSentryScope("zero")
		x.WrapPanics()
		x.SentryOptOutNote()
	}

	x.PrintVersion()
	var tlsDisRoutes []string
	if Zero.Conf.GetString("tls_disabled_route") != "" {
		tlsDisRoutes = strings.Split(Zero.Conf.GetString("tls_disabled_route"), ",")
	}

	tlsConf, err := x.LoadClientTLSConfigForInternalPort(Zero.Conf)
	x.Check(err)
	opts = options{
		bindall:           Zero.Conf.GetBool("bindall"),
		myAddr:            Zero.Conf.GetString("my"),
		portOffset:        Zero.Conf.GetInt("port_offset"),
		nodeId:            uint64(Zero.Conf.GetInt("idx")),
		numReplicas:       Zero.Conf.GetInt("replicas"),
		peer:              Zero.Conf.GetString("peer"),
		w:                 Zero.Conf.GetString("wal"),
		rebalanceInterval: Zero.Conf.GetDuration("rebalance_interval"),
		LudicrousMode:     Zero.Conf.GetBool("ludicrous_mode"),
		totalCache:        int64(Zero.Conf.GetInt("cache_mb")),
		cachePercentage:   Zero.Conf.GetString("cache_percentage"),
		tlsDir:            Zero.Conf.GetString("tls_dir"),
		tlsDisabledRoutes: tlsDisRoutes,
		tlsClientConfig:   tlsConf,
	}

	if opts.nodeId == 0 {
		log.Fatalf("ERROR: idx flag cannot be 0. Please try again with idx as a positive integer")
	}

	x.WorkerConfig = x.WorkerOptions{
		LudicrousMode: Zero.Conf.GetBool("ludicrous_mode"),
	}

	if !enc.EeBuild && Zero.Conf.GetString("enterprise_license") != "" {
		log.Fatalf("ERROR: enterprise_license option cannot be applied to OSS builds. ")
	}

	if opts.numReplicas < 0 || opts.numReplicas%2 == 0 {
		log.Fatalf("ERROR: Number of replicas must be odd for consensus. Found: %d",
			opts.numReplicas)
	}

	if Zero.Conf.GetBool("expose_trace") {
		// TODO: Remove this once we get rid of event logs.
		trace.AuthRequest = func(req *http.Request) (any, sensitive bool) {
			return true, true
		}
	}

	if opts.rebalanceInterval <= 0 {
		log.Fatalf("ERROR: Rebalance interval must be greater than zero. Found: %d",
			opts.rebalanceInterval)
	}

	grpc.EnableTracing = false
	otrace.ApplyConfig(otrace.Config{
		DefaultSampler: otrace.ProbabilitySampler(Zero.Conf.GetFloat64("trace"))})

	addr := "localhost"
	if opts.bindall {
		addr = "0.0.0.0"
	}
	if opts.myAddr == "" {
		opts.myAddr = fmt.Sprintf("localhost:%d", x.PortZeroGrpc+opts.portOffset)
	}

	grpcListener, err := setupListener(addr, x.PortZeroGrpc+opts.portOffset, "grpc")
	x.Check(err)
	httpListener, err := setupListener(addr, x.PortZeroHTTP+opts.portOffset, "http")
	x.Check(err)

	x.AssertTruef(opts.totalCache >= 0, "ERROR: Cache size must be non-negative")

	cachePercent, err := x.GetCachePercentages(opts.cachePercentage, 2)
	x.Check(err)
	blockCacheSz := (cachePercent[0] * (opts.totalCache << 20)) / 100
	indexCacheSz := (cachePercent[1] * (opts.totalCache << 20)) / 100

	// Open raft write-ahead log and initialize raft node.
	x.Checkf(os.MkdirAll(opts.w, 0700), "Error while creating WAL dir.")
	kvOpt := badger.LSMOnlyOptions(opts.w).
		WithSyncWrites(false).
		WithTruncate(true).
		WithValueLogFileSize(64 << 20).
		WithBlockCacheSize(blockCacheSz).
		WithIndexCacheSize(indexCacheSz).
		WithLoadBloomsOnOpen(false)

	compression_level := Zero.Conf.GetInt("badger.compression_level")
	if compression_level > 0 {
		// By default, compression is disabled in badger.
		kvOpt.Compression = bopt.ZSTD
		kvOpt.ZSTDCompressionLevel = compression_level
	}

	// Set loading mode options.
	switch Zero.Conf.GetString("badger.tables") {
	case "mmap":
		kvOpt.TableLoadingMode = bopt.MemoryMap
	case "ram":
		kvOpt.TableLoadingMode = bopt.LoadToRAM
	case "disk":
		kvOpt.TableLoadingMode = bopt.FileIO
	default:
		x.Fatalf("Invalid Badger Tables options")
	}
	switch Zero.Conf.GetString("badger.vlog") {
	case "mmap":
		kvOpt.ValueLogLoadingMode = bopt.MemoryMap
	case "disk":
		kvOpt.ValueLogLoadingMode = bopt.FileIO
	default:
		x.Fatalf("Invalid Badger Value log options")
	}
	glog.Infof("Opening zero BadgerDB with options: %+v\n", kvOpt)

	kv, err := badger.OpenManaged(kvOpt)
	x.Checkf(err, "Error while opening WAL store")
	defer kv.Close()

	gcCloser := z.NewCloser(1) // closer for vLogGC
	go x.RunVlogGC(kv, gcCloser)
	defer gcCloser.SignalAndWait()

	store := raftwal.Init(kv, opts.nodeId, 0)

	// Initialize the servers.
	var st state
	st.serveGRPC(grpcListener, store)
	tlsCfg, err := x.LoadServerTLSConfig(Zero.Conf, "node.crt", "node.key")
	x.Check(err)
	st.startListenHttpAndHttps(httpListener, tlsCfg)

	http.HandleFunc("/health", st.pingResponse)
	http.HandleFunc("/state", st.getState)
	http.HandleFunc("/removeNode", st.removeNode)
	http.HandleFunc("/moveTablet", st.moveTablet)
	http.HandleFunc("/assign", st.assign)
	http.HandleFunc("/enterpriseLicense", st.applyEnterpriseLicense)
	zpages.Handle(http.DefaultServeMux, "/z")

	// This must be here. It does not work if placed before Grpc init.
	x.Check(st.node.initAndStartNode())

	if Zero.Conf.GetBool("telemetry") {
		go st.zero.periodicallyPostTelemetry()
	}

	sdCh := make(chan os.Signal, 1)
	signal.Notify(sdCh, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	// handle signals
	go func() {
		var sigCnt int
		for sig := range sdCh {
			glog.Infof("--- Received %s signal", sig)
			sigCnt++
			if sigCnt == 1 {
				signal.Stop(sdCh)
				st.zero.closer.Signal()
			} else if sigCnt == 3 {
				glog.Infof("--- Got interrupt signal 3rd time. Aborting now.")
				os.Exit(1)
			} else {
				glog.Infof("--- Ignoring interrupt signal.")
			}
		}
	}()

	st.zero.closer.AddRunning(1)

	go func() {
		defer st.zero.closer.Done()
		<-st.zero.closer.HasBeenClosed()
		glog.Infoln("Shutting down...")
		close(sdCh)
		// Close doesn't close already opened connections.

		// Stop all HTTP requests.
		_ = httpListener.Close()
		// Stop Raft.
		st.node.closer.SignalAndWait()
		// Try to generate a snapshot before the shutdown.
		st.node.trySnapshot(0)
		// Stop Raft store.
		store.Closer.SignalAndWait()
		// Stop all internal requests.
		_ = grpcListener.Close()

		x.RemoveCidFile()
	}()

	glog.Infoln("Running Dgraph Zero...")
	st.zero.closer.Wait()
	glog.Infoln("Closer closed.")

	err = kv.Close()
	glog.Infof("Badger closed with err: %v\n", err)

	glog.Infoln("All done. Goodbye!")
}
