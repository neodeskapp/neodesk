package main

import (
	"fmt"
	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/hub"

	"github.com/neodeskapp/neodesk/neodesk-server/config"
	consoleconfig "github.com/neodeskapp/neodesk/neodesk-server/console/config"
)

// listener is a TCP port the start opens and the setting that names it.
type listener struct {
	port    int
	setting string
}

// startListeners are the TCP ports this process opens: the server's, and
// the console's when it is on. ccfg is nil when the console's
// configuration does not load (its start reports why). getenv tells which
// variable gave the server's API its port.
func startListeners(cfg *config.Config, ccfg *consoleconfig.Config, getenv func(string) string) []listener {
	api := "GO_API_PORT (-api-port, the server's API)"
	if getenv("GO_API_PORT") == "" && getenv("API_PORT") != "" {
		api = "API_PORT (the server's API, as GO_API_PORT is not set)"
	}
	out := []listener{
		{cfg.SignalPort, "SIGNAL_PORT (-port, the ID server)"},
		{cfg.NATTestPort(), "SIGNAL_PORT-1 (the ID server's NAT test)"},
		{cfg.WSSignalPort(), "SIGNAL_PORT+2 (the ID server's WebSocket)"},
		{cfg.RelayPort, "RELAY_PORT (-relay-port, the relay)"},
		{cfg.WSRelayPort(), "RELAY_PORT+2 (the relay's WebSocket)"},
		{cfg.APIPort, api},
	}
	if port, err := hub.ControlPort(getenv); err == nil && port != 0 {
		out = append(out, listener{port, "NODE_CONTROL_PORT (relay nodes' control connections)"})
	}
	if cfg.CDAPEnabled {
		out = append(out, listener{cfg.CDAPPort, "CDAP_PORT (-cdap-port, the CDAP gateway)"})
	}
	if cfg.AdminPort != 0 {
		out = append(out, listener{cfg.AdminPort, "ADMIN_PORT (-admin-port, the admin interface)"})
	}
	if ccfg != nil && ccfg.Port != 0 {
		out = append(out, listener{ccfg.Port, "CONSOLE_PORT (the console)"})
		if ccfg.HTTPS && ccfg.CertFile != "" && ccfg.KeyFile != "" {
			out = append(out, listener{ccfg.HTTPSPort, "HTTPS_PORT (the console over HTTPS)"})
		}
		if ccfg.APIEnabled {
			out = append(out, listener{ccfg.APIPort, "API_PORT (the console's client API)"})
		}
	}
	return out
}

// checkPorts refuses a start in which two listeners would take the same
// port, and names the settings of both.
func checkPorts(listeners []listener) error {
	taken := map[int]string{}
	for _, l := range listeners {
		if l.port <= 0 {
			continue
		}
		if first, ok := taken[l.port]; ok {
			return fmt.Errorf("%s and %s would both listen on port %d: give one of them another port", first, l.setting, l.port)
		}
		taken[l.port] = l.setting
	}
	return nil
}

// checkStartPorts is checkPorts on the listeners of this start.
func checkStartPorts(cfg *config.Config, getenv func(string) string) error {
	ccfg, err := consoleconfig.LoadEnv()
	if err != nil {
		ccfg = nil
	}
	return checkPorts(startListeners(cfg, ccfg, getenv))
}

// derivedClientPort is the port a stock RustDesk client with an empty API
// server setting calls (the ID server's port minus 2), for the server's
// API to answer there too; 0 when the API already listens there or
// another of the start's listeners has the port.
func derivedClientPort(cfg *config.Config, listeners []listener) int {
	port := cfg.SignalPort - 2
	if port <= 0 || port == cfg.APIPort {
		return 0
	}
	for _, l := range listeners {
		if l.port == port {
			return 0
		}
	}
	return port
}

// startDerivedClientPort is derivedClientPort for the listeners of this start.
func startDerivedClientPort(cfg *config.Config, getenv func(string) string) int {
	ccfg, err := consoleconfig.LoadEnv()
	if err != nil {
		ccfg = nil
	}
	return derivedClientPort(cfg, startListeners(cfg, ccfg, getenv))
}

// consoleClientAPI reports whether the console's client API port
// (API_ENABLED, API_PORT) will run in this process, so outside callers
// on the server's API port get what it answers (api.Server.LimitOutside).
// ccfg is nil when the console's configuration does not load.
func consoleClientAPI(ccfg *consoleconfig.Config) bool {
	return ccfg != nil && ccfg.Port != 0 && ccfg.APIEnabled
}

// startConsoleClientAPI is consoleClientAPI for this start.
func startConsoleClientAPI() bool {
	ccfg, err := consoleconfig.LoadEnv()
	if err != nil {
		ccfg = nil
	}
	return consoleClientAPI(ccfg)
}
