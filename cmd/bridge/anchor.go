package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	bridge "gitea-topic-bridge"
)

func serveAnchor(path, listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return errors.New("anchor -listen must be a numeric loopback address and port")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ledger, err := bridge.OpenAnchorLedger(startup, path, os.Getenv("BRIDGE_ANCHOR_SIGNING_SEED"), os.Getenv("BRIDGE_ANCHOR_BEARER"))
	if err != nil {
		return err
	}
	defer ledger.Close()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: ledger.Handler(), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 8 * time.Second, WriteTimeout: 12 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	log.Printf("independent anchor listening on %s; Ed25519 public key: %s", listener.Addr(), ledger.PublicKey())
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			return err
		}
		return nil
	}
}
