// Copyright 2019 ChainSafe Systems (ON) Corp.
// This file is part of gossamer.
//
// The gossamer library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The gossamer library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the gossamer library. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"os"

	"github.com/ChainSafe/gossamer/dot"
	"github.com/ChainSafe/gossamer/lib/keystore"
	"github.com/ChainSafe/gossamer/lib/utils"

	log "github.com/ChainSafe/log15"
	"github.com/urfave/cli"
)

// app is the cli application
var app = cli.NewApp()

var (
	// exportCommand defines the "export" subcommand (ie, `gossamer export`)
	exportCommand = cli.Command{
		Action:    FixFlagOrder(exportAction),
		Name:      "export",
		Usage:     "Export configuration values to TOML configuration file",
		ArgsUsage: "",
		Flags:     CLIFlags,
		Category:  "CONFIGURATION",
		Description: "The export command exports configuration values from the command flags to a TOML configuration file.\n" +
			"\tUsage: gossamer export --config node/custom/config.toml --datadir ~/.gossamer/custom --protocol /gossamer/custom/0",
	}
	// initCommand defines the "init" subcommand (ie, `gossamer init`)
	initCommand = cli.Command{
		Action:    FixFlagOrder(initAction),
		Name:      "init",
		Usage:     "Initialize node databases and load genesis data to state",
		ArgsUsage: "",
		Flags:     InitFlags,
		Category:  "INITIALIZATION",
		Description: "The init command initializes the node databases and loads the genesis data from the genesis configuration file to state.\n" +
			"\tUsage: gossamer init --genesis genesis.json",
	}
	// accountCommand defines the "account" subcommand (ie, `gossamer account`)
	accountCommand = cli.Command{
		Action:   FixFlagOrder(accountAction),
		Name:     "account",
		Usage:    "manage gossamer keystore",
		Flags:    AccountFlags,
		Category: "KEYSTORE",
		Description: "The account command is used to manage the gossamer keystore.\n" +
			"\tTo generate a new sr25519 account: gossamer account --generate\n" +
			"\tTo generate a new ed25519 account: gossamer account --generate --ed25519\n" +
			"\tTo generate a new secp256k1 account: gossamer account --generate --secp256k1\n" +
			"\tTo import a keystore file: gossamer account --import=path/to/file\n" +
			"\tTo list keys: gossamer account --list",
	}
)

// init initializes the cli application
func init() {
	app.Action = gossamerAction
	app.Copyright = "Copyright 2019 ChainSafe Systems Authors"
	app.Name = "gossamer"
	app.Usage = "Official gossamer command-line interface"
	app.Author = "ChainSafe Systems 2019"
	app.Version = "0.0.1"
	app.Commands = []cli.Command{
		exportCommand,
		initCommand,
		accountCommand,
	}
	app.Flags = CLIFlags
}

// main runs the cli application
func main() {
	if err := app.Run(os.Args); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// gossamerAction is the root action for the gossamer command, creates a node
// configuration, loads the keystore, initializes the node if not initialized,
// then creates and starts the node and node services
func gossamerAction(ctx *cli.Context) error {

	// check for unknown command arguments
	if arguments := ctx.Args(); len(arguments) > 0 {
		return fmt.Errorf("failed to read command argument: %q", arguments[0])
	}

	// start gossamer logger
	err := startLogger(ctx)
	if err != nil {
		log.Error("[cmd] Failed to start logger", "error", err)
		return err
	}

	// create new dot configuration (the dot configuration is created within the
	// cli application from the flag values provided)
	cfg, err := createDotConfig(ctx)
	if err != nil {
		log.Error("[cmd] Failed to create node configuration", "error", err)
		return err
	}

	// expand data directory and update node configuration (performed separate
	// from createDotConfig because dot config should not include expanded path)
	cfg.Global.DataDir = utils.ExpandDir(cfg.Global.DataDir)

	// check if node has not been initialized
	if !dot.NodeInitialized(cfg) {

		log.Warn("[cmd] Node has not been initialized, initializing new node...")

		err = dot.InitNode(cfg)
		if err != nil {
			log.Error("[cmd] Failed to initialize node", "error", err)
			return err
		}
	}

	ks, err := keystore.LoadKeystore(cfg.Account.Key)
	if err != nil {
		log.Error("[cmd] Failed to load keystore", "error", err)
		return err
	}

	err = unlockKeystore(ks, cfg.Global.DataDir, cfg.Account.Unlock, ctx.String(PasswordFlag.Name))
	if err != nil {
		log.Error("[cmd] Failed to unlock keystore", "error", err)
		return err
	}

	node, err := dot.NewNode(cfg, ks)
	if err != nil {
		log.Error("[cmd] Failed to create node services", "error", err)
		return err
	}

	log.Info("[cmd] Starting node...", "name", node.Name)

	// start node
	node.Start()

	return nil
}

// initAction is the action for the "init" subcommand, initializes the trie and
// state databases and loads initial state from the configured genesis file
func initAction(ctx *cli.Context) error {
	err := startLogger(ctx)
	if err != nil {
		log.Error("[cmd] Failed to start logger", "error", err)
		return err
	}

	cfg, err := createInitConfig(ctx)
	if err != nil {
		log.Error("[cmd] Failed to create node configuration", "error", err)
		return err
	}

	// expand data directory and update node configuration (performed separate
	// from createDotConfig because dot config should not include expanded path)
	cfg.Global.DataDir = utils.ExpandDir(cfg.Global.DataDir)

	// check if node has been initialized
	if dot.NodeInitialized(cfg) {

		// TODO: do we want to handle initialized node differently?
		log.Warn("[cmd] Node has already been initialized, reinitializing node")

	}

	// initialize node (initialize databases and load genesis data)
	err = dot.InitNode(cfg)
	if err != nil {
		log.Error("[cmd] Failed to initialize node", "error", err)
		return err
	}

	return nil
}