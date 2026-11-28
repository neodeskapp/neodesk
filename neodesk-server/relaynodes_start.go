package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/neodeskapp/neodesk/neodesk-server/billing"
	"github.com/neodeskapp/neodesk/neodesk-server/config"
	"github.com/neodeskapp/neodesk/neodesk-server/crypto"
	"github.com/neodeskapp/neodesk/neodesk-server/db"
	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/central"
	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/hub"
	sigServer "github.com/neodeskapp/neodesk/neodesk-server/signal"
)

// startRelayNodeHub opens the control listener relay nodes connect to and
// turns on relay node routing in signal. It returns nil when
// NODE_CONTROL_PORT=0 or the listener can't start: sessions then keep the
// central relay.
func startRelayNodeHub(ctx context.Context, cfg *config.Config, kp *crypto.KeyPair, database db.Database, billingSvc *billing.Service, sig *sigServer.Server) *central.Glue {
	port, err := hub.ControlPort(os.Getenv)
	if err != nil {
		log.Printf("[relay-node] %v; relay nodes are off", err)
		return nil
	}
	if port == 0 {
		log.Printf("[relay-node] NODE_CONTROL_PORT=0: relay nodes are off")
		return nil
	}
	ccfg := central.Config{
		Addr: fmt.Sprintf(":%d", port), CertDir: filepath.Dir(cfg.KeyFile),
		ServerPK: kp.PublicKeyBase64(), DB: database, Signal: sig,
	}
	if billingSvc != nil {
		ccfg.Billing = billingSvc
	}
	glue, err := central.Start(ctx, ccfg)
	if err != nil {
		log.Printf("[relay-node] %v; relay nodes are off", err)
		return nil
	}
	central.SetDefault(glue)
	log.Printf("[relay-node] control listener on :%d (pin %s)", port, glue.Pin)
	return glue
}
