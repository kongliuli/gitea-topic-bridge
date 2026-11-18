package main

import (
	"context"
	"log"
	"net/http"
	"time"

	bridge "gitea-topic-bridge"
)

func serveAnchor(ctx context.Context, ready func(), path, listen string, secrets launchSecrets) error {
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	ledger, err := bridge.OpenAnchorLedger(startup, path, secrets.AnchorSeed, secrets.AnchorBearer)
	cancel()
	if err != nil {
		return err
	}
	defer ledger.Close()
	server := &http.Server{Handler: ledger.Handler(), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 8 * time.Second, WriteTimeout: 12 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
	return serveEndpoints(ctx, func() {
		log.Printf("independent anchor Ed25519 public key: %s", ledger.PublicKey())
		ready()
	}, []httpEndpoint{{server, listen}}, nil)
}
