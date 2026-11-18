package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	bridge "gitea-topic-bridge"
)

func loadConfig(path string) (bridge.ServerConfig, error) {
	var config bridge.ServerConfig
	file, err := os.Open(path)
	if err != nil {
		return config, err
	}
	defer file.Close()
	const maxConfig = 1 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxConfig+1))
	if err != nil || len(data) > maxConfig {
		return config, errors.New("could not read the Bridge config within 1 MiB")
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, errors.New("invalid Bridge config JSON")
	}
	config.ClientSecret = os.Getenv("BRIDGE_OAUTH_CLIENT_SECRET")
	return config, nil
}

func main() {
	configPath := flag.String("config", "bridge.json", "path to Bridge JSON configuration")
	listen := flag.String("listen", "127.0.0.1:3001", "loopback HTTP listen address")
	hookListen := flag.String("hook-listen", "", "optional dedicated loopback listen address for Gitea webhooks")
	flag.Parse()
	host, _, err := net.SplitHostPort(*listen)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		log.Fatal("-listen must be a numeric loopback address and port")
	}
	if *hookListen != "" {
		host, _, err := net.SplitHostPort(*hookListen)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			log.Fatal("-hook-listen must be a numeric loopback address and port")
		}
	}
	config, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	if config.DatabasePath != "" {
		startup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		store, err := bridge.OpenTopicStore(startup, config.DatabasePath)
		cancel()
		if err != nil {
			log.Fatalf("open Bridge database: %v", err)
		}
		defer store.Close()
		config.TopicStore = store
	}
	handler, err := bridge.NewServer(config)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 2)
	if *hookListen != "" {
		hook, err := bridge.NewWebhook(config, os.Getenv("BRIDGE_WEBHOOK_SECRET"), os.Getenv("BRIDGE_WEBHOOK_READER_TOKEN"))
		if err != nil {
			log.Fatal(err)
		}
		internal, err := net.Listen("tcp", *hookListen)
		if err != nil {
			log.Fatal(err)
		}
		internalServer := &http.Server{Handler: hook.Handler(), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 8 * time.Second, WriteTimeout: 12 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8 << 10}
		workerDone := make(chan struct{})
		defer func() {
			stop()
			shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := internalServer.Shutdown(shutdown); err != nil {
				internalServer.Close()
			}
			<-workerDone
		}()
		go func() { defer close(workerDone); hook.Run(ctx) }()
		go func() { serveErr <- internalServer.Serve(internal) }()
		log.Printf("Gitea webhook listening on %s", internal.Addr())
	}
	go func() { serveErr <- server.Serve(listener) }()
	log.Printf("Bridge listening on %s", listener.Addr())
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Fatal(err)
		}
	}
}
