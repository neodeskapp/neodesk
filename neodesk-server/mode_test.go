package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParseFlagsHelper is the start of the server's configuration in a
// child process of TestSplitModesEndTheStart; it does nothing in a normal
// run.
func TestParseFlagsHelper(t *testing.T) {
	if os.Getenv("BDGO_PARSE_FLAGS_HELPER") != "1" {
		t.Skip("runs only as the child of TestSplitModesEndTheStart")
	}
	cfg := parseFlags()
	fmt.Printf("configuration done, mode %s\n", cfg.Mode)
	os.Exit(0)
}

// MODE=signal and MODE=relay end the start with a message that names the
// combined mode; MODE=all goes on.
func TestSplitModesEndTheStart(t *testing.T) {
	run := func(mode string) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestParseFlagsHelper$")
		cmd.Env = append(os.Environ(), "BDGO_PARSE_FLAGS_HELPER=1", "MODE="+mode)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, mode := range []string{"signal", "relay", "RELAY"} {
		out, err := run(mode)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() == 0 {
			t.Fatalf("MODE=%s: the start went on (%v): %s", mode, err, out)
		}
		lower := strings.ToLower(mode)
		if strings.Contains(out, "configuration done") ||
			!strings.Contains(out, "MODE="+lower+" (-mode "+lower+") is not supported") ||
			!strings.Contains(out, "Use MODE=all (-mode all), the combined mode") {
			t.Fatalf("MODE=%s: output %q does not refuse the mode and name the combined one", mode, out)
		}
	}
	for _, mode := range []string{"all", "node"} {
		out, err := run(mode)
		if err != nil || !strings.Contains(out, "configuration done, mode "+mode) {
			t.Fatalf("MODE=%s: %v: %s", mode, err, out)
		}
	}
}

// TestServerMainHelper runs the server's start in a child process of
// TestNodeModeOpensNoDatabase; it does nothing in a normal run.
func TestServerMainHelper(t *testing.T) {
	if os.Getenv("BDGO_SERVER_MAIN_HELPER") != "1" {
		t.Skip("runs only as the child of TestNodeModeOpensNoDatabase")
	}
	os.Exit(serverMain(nil))
}

// MODE=node starts the relay node before anything of the central server:
// a node that is not enrolled stops with that message, and neither the
// database nor the server key pair was made.
func TestNodeModeOpensNoDatabase(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServerMainHelper$")
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(name, "NODE_"), name == "MODE", name == "DATABASE_URL", name == "DB_URL",
			name == "DB_TYPE", name == "KEY_FILE", name == "NEODESK_ENV_FILE":
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	db := filepath.Join(dir, "server.db")
	key := filepath.Join(dir, "id_ed25519")
	cmd.Env = append(cmd.Env, "BDGO_SERVER_MAIN_HELPER=1", "MODE=node", "DATABASE_URL="+db, "KEY_FILE="+key,
		"NODE_DATA_DIR="+filepath.Join(dir, "node"),
		// Should the start go on to the central server, it stays off the usual ports
		"SIGNAL_PORT=41116", "RELAY_PORT=41117", "GO_API_PORT=41114", "CONSOLE_PORT=0")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("exit %v, want 1: %s", err, out)
	}
	if !strings.Contains(string(out), "this node is not enrolled") {
		t.Fatalf("output does not say the node is not enrolled: %s", out)
	}
	for _, f := range []string{db, key} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("MODE=node made %s", f)
		}
	}
}

func TestCheckModeTakesNode(t *testing.T) {
	if err := checkMode("node"); err != nil {
		t.Fatalf("node: %v", err)
	}
	if err := checkMode("nodes"); err == nil || !strings.Contains(err.Error(), "MODE=node") {
		t.Fatalf("nodes: %v", err)
	}
}
