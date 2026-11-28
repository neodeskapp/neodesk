package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/neodeskapp/neodesk/neodesk-server/cli"
	"github.com/neodeskapp/neodesk/neodesk-server/config"
	consoleconfig "github.com/neodeskapp/neodesk/neodesk-server/console/config"
	"github.com/neodeskapp/neodesk/neodesk-server/console/updater"
	"github.com/neodeskapp/neodesk/neodesk-server/migrate"
	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/agent"
)

// subcommand is a command of neodesk-server besides running the
// server. run takes the arguments after the command's name and returns
// the exit status.
type subcommand struct {
	usage string
	run   func(args []string, stdin io.Reader, stdout, stderr io.Writer) int
}

// subcommands are the commands a first argument that does not start with
// "-" names (spec updater-installers.md 4.9). Add a command here.
var subcommands = map[string]subcommand{
	"update": {
		usage: "update [--check] [--yes] [--no-backup]   install the newest release of the update channel",
		run:   runUpdate,
	},
	"rollback": {
		usage: "rollback                                 put the binary before the last update back",
		run:   runRollback,
	},
	"upgrade-env": {
		usage: "upgrade-env --from FILE [--write]        turn the old console's env file into the server's",
		run:   withEnv(cli.UpgradeEnv),
	},
	"env": {
		usage: envMergeUsage,
		run:   runEnv,
	},
	"reset-password": {
		usage: "reset-password [USER]                    set a password; the new one is read from standard input",
		run:   withEnv(cli.ResetPassword),
	},
	"config": {
		usage: "config check | config set KEY=VALUE...   check what the panel can do, or set allowlisted settings",
		run:   withEnv(cli.Config),
	},
	"check": {
		usage: "check [--api URL] [--panel URL] [--port HOST:PORT] [--insecure]   check an install's addresses and ports",
		run:   withEnv(cli.Check),
	},
	"cert": {
		usage: "cert --out DIR [--host NAME]... [--days N] [--force]   make an install's self-signed certificate",
		run:   withEnv(cli.Cert),
	},
	"verify-release": {
		usage: verifyReleaseUsage,
		run:   runVerifyRelease,
	},
	"backup": {
		usage: backupUsage,
		run:   runBackup,
	},
	"node": {
		usage: nodeUsage,
		run:   runNodeCommand,
	},
	"migrate": {
		usage: "migrate -mode sqlite2pg|pg2sqlite|rust2go|backup -src X [-dst Y] [-overwrite]   move or back up the database",
		run: func(args []string, _ io.Reader, stdout, stderr io.Writer) int {
			return migrate.Main(args, stdout, stderr)
		},
	},
}

// withEnv runs a command of package cli with the process's environment
// and the given streams.
func withEnv(run func(args []string, env cli.Env) int) func([]string, io.Reader, io.Writer, io.Writer) int {
	return func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
		env := cli.OS()
		env.Stdin, env.Stdout, env.Stderr = stdin, stdout, stderr
		return run(args, env)
	}
}

// runEnv is "env merge"; "env" names a group of commands.
func runEnv(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "merge" {
		fmt.Fprintln(stderr, "neodesk-server: usage: neodesk-server "+envMergeUsage)
		return 2
	}
	return withEnv(cli.EnvMerge)(args[1:], stdin, stdout, stderr)
}

// runSubcommand runs the command args[0] names. handled is false when
// args start with a flag or are empty: then the server runs.
func runSubcommand(args []string) (code int, handled bool) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return 0, false
	}
	cmd, ok := subcommands[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "neodesk-server: unknown command %q\n\nCommands:\n", args[0])
		names := make([]string, 0, len(subcommands))
		for name := range subcommands {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(os.Stderr, "  neodesk-server %s\n", subcommands[name].usage)
		}
		return 2, true
	}
	return cmd.run(args[1:], os.Stdin, os.Stdout, os.Stderr), true
}

// updaterForCommand builds the updater for a command from the console's
// configuration, the same environment the service reads. A command makes
// no data backup: that needs the database, which it does not open.
func updaterForCommand(stderr io.Writer) (*updater.Service, bool) {
	cfg, err := consoleconfig.Load(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "neodesk-server: configuration: %v\n", err)
		return nil, false
	}
	return updater.New(updater.Options{
		DataDir:    cfg.DataDir,
		ConsoleDir: cfg.ConsoleDir,
		EnvFile:    os.Getenv("NEODESK_ENV_FILE"),
		Version:    Version,
		Commit:     Commit,
		Docker:     cfg.Docker,
		Getenv:     os.Getenv,
	}), true
}

func runUpdate(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	svc, ok := updaterForCommand(stderr)
	if !ok {
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return updater.CLI{Service: svc, In: stdin, Out: stdout}.Update(ctx, args)
}

func runRollback(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	svc, ok := updaterForCommand(stderr)
	if !ok {
		return 1
	}
	return updater.CLI{Service: svc, In: stdin, Out: stdout}.Rollback(args)
}

const envMergeUsage = "env merge --target FILE [--fresh] [--subst-file JSON] [--subst-env]   write the env file from the template"

const nodeUsage = "node [--join STRING] [--data-dir DIR] [--lan-addr HOST:PORT] [--public-addr HOST:PORT]   run as a relay node of a site (MODE=node)"

// nodeFlags are the node command's flags; set ones win over NODE_*.
type nodeFlags struct {
	join, dataDir, lanAddr, publicAddr string
}

// runNodeCommand is "neodesk-server node": MODE=node with the flags.
func runNodeCommand(args []string, _ io.Reader, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("node", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f nodeFlags
	fs.StringVar(&f.join, "join", "", "enrollment string from the console (first start)")
	fs.StringVar(&f.dataDir, "data-dir", "", "directory for the node key and state (NODE_DATA_DIR)")
	fs.StringVar(&f.lanAddr, "lan-addr", "", "relay host:port clients on the LAN dial (NODE_LAN_ADDR)")
	fs.StringVar(&f.publicAddr, "public-addr", "", "relay host:port clients outside dial (NODE_PUBLIC_ADDR)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "neodesk-server: usage: neodesk-server "+nodeUsage)
		return 2
	}
	cfg := config.DefaultConfig()
	cfg.LoadEnv()
	cfg.Mode = "node"
	return runRelayNode(cfg, os.Getenv, f, nil, nil)
}

// runRelayNode runs MODE=node until SIGINT, SIGTERM or a closed stop. It
// opens no database and starts neither signal nor console.
func runRelayNode(cfg *config.Config, getenv func(string) string, f nodeFlags, stop <-chan struct{}, ready func()) int {
	acfg := agent.ConfigFromEnv(getenv, cfg)
	for _, o := range []struct {
		flag string
		to   *string
	}{
		{f.join, &acfg.Join}, {f.dataDir, &acfg.DataDir}, {f.lanAddr, &acfg.LanAddr}, {f.publicAddr, &acfg.PublicAddr},
	} {
		if o.flag != "" {
			*o.to = o.flag
		}
	}
	acfg.Version = Version
	acfg.Ready = ready
	a, err := agent.New(acfg)
	if err != nil {
		log.Printf("Relay node: %v", err)
		return 1
	}
	log.Printf("NeoDesk relay node %s: relay :%d (TCP), :%d (WebSocket), data in %s", Version, cfg.RelayPort, cfg.WSRelayPort(), acfg.DataDir)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if stop != nil {
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	if err := a.Run(ctx); err != nil {
		log.Printf("Relay node: %v", err)
		return 1
	}
	log.Printf("Relay node stopped")
	return 0
}
