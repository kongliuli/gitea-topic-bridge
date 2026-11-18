package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	bridge "gitea-topic-bridge"
)

type launchSecrets struct {
	OAuthSecret   string `json:"oauth_client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	WebhookToken  string `json:"webhook_reader_token"`
	ReceiptSeed   string `json:"receipt_signing_seed"`
	AnchorSeed    string `json:"anchor_signing_seed"`
	AnchorBearer  string `json:"anchor_bearer"`
}

func loadSecrets(path string) (launchSecrets, error) {
	if path == "" {
		return launchSecrets{
			OAuthSecret: os.Getenv("BRIDGE_OAUTH_CLIENT_SECRET"), WebhookSecret: os.Getenv("BRIDGE_WEBHOOK_SECRET"),
			WebhookToken: os.Getenv("BRIDGE_WEBHOOK_READER_TOKEN"), ReceiptSeed: os.Getenv("BRIDGE_RECEIPT_SIGNING_SEED"),
			AnchorSeed: os.Getenv("BRIDGE_ANCHOR_SIGNING_SEED"), AnchorBearer: os.Getenv("BRIDGE_ANCHOR_BEARER"),
		}, nil
	}
	var secrets launchSecrets
	data, err := readBounded(path, 64<<10)
	if err != nil {
		return secrets, err
	}
	defer clear(data)
	if json.Unmarshal(data, &secrets) != nil {
		return launchSecrets{}, errors.New("invalid Bridge secrets JSON")
	}
	return secrets, nil
}

func loadConfig(path string, secrets launchSecrets) (bridge.ServerConfig, error) {
	var config bridge.ServerConfig
	data, err := readBounded(path, 1<<20)
	if err != nil {
		return config, err
	}
	if json.Unmarshal(data, &config) != nil {
		return config, errors.New("invalid Bridge config JSON")
	}
	config.ClientSecret = secrets.OAuthSecret
	if config.Receipts != nil {
		config.Receipts.SigningSeed = secrets.ReceiptSeed
		if config.Receipts.Anchor != nil {
			config.Receipts.Anchor.Bearer = secrets.AnchorBearer
		}
	}
	return config, nil
}

type launchOptions struct {
	config, secretFile, listen, hookListen, anchorDB string
}

func localAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return errors.New("listen address must be a numeric loopback IP and port")
	}
	return nil
}

func main() {
	var options launchOptions
	flag.StringVar(&options.config, "config", "bridge.json", "path to Bridge JSON configuration")
	flag.StringVar(&options.listen, "listen", "127.0.0.1:3001", "loopback HTTP listen address")
	flag.StringVar(&options.hookListen, "hook-listen", "", "optional dedicated loopback listen address for Gitea webhooks")
	flag.StringVar(&options.secretFile, "secrets", "", "absolute path to an ACL-protected launch secrets JSON file")
	flag.StringVar(&options.anchorDB, "anchor-db", "", "run the independent anchor service using this absolute database path")
	serviceName := flag.String("service", "", "run under this native Windows service name")
	logFile := flag.String("log-file", "", "absolute path to an optional append-only process log")
	verify := flag.String("verify-receipt", "", "verify a receipt JSON file and exit")
	trustedKey := flag.String("trusted-receipt-key", "", "trusted Ed25519 public key (base64) for receipt verification")
	proof := flag.String("anchor-proof", "", "optional detached independent proof JSON file")
	trustedAnchorKey := flag.String("trusted-anchor-key", "", "trusted independent Ed25519 public key (base64)")
	flag.Parse()
	if *verify != "" {
		if err := verifyReceiptFile(*verify, *trustedKey, *proof, *trustedAnchorKey); err != nil {
			log.Fatal(err)
		}
		if *proof != "" {
			log.Print("receipt signature and independent anchor verified")
		} else {
			log.Print("receipt signature verified")
		}
		return
	}
	if options.secretFile != "" && !filepath.IsAbs(options.secretFile) {
		log.Fatal("-secrets must be absolute")
	}
	if *serviceName != "" && options.anchorDB == "" && !filepath.IsAbs(options.config) {
		log.Fatal("service -config must be absolute")
	}
	if *logFile != "" {
		if !filepath.IsAbs(*logFile) {
			log.Fatal("-log-file must be absolute")
		}
		file, err := os.OpenFile(*logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			log.Fatal(err)
		}
		defer file.Close()
		log.SetOutput(file)
	}
	if *serviceName != "" {
		if err := runWindowsService(*serviceName, options.run); err != nil {
			log.Fatal(err)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := options.run(ctx, func() {}); err != nil {
		log.Fatal(err)
	}
}

func (options launchOptions) run(parent context.Context, ready func()) error {
	if err := localAddress(options.listen); err != nil {
		return err
	}
	if options.hookListen != "" {
		if err := localAddress(options.hookListen); err != nil {
			return err
		}
	}
	secrets, err := loadSecrets(options.secretFile)
	if err != nil {
		return err
	}
	if options.anchorDB != "" {
		if options.hookListen != "" {
			return errors.New("an independent anchor cannot host the Gitea webhook")
		}
		return serveAnchor(parent, ready, options.anchorDB, options.listen, secrets)
	}
	config, err := loadConfig(options.config, secrets)
	if err != nil {
		return err
	}
	if config.DatabasePath != "" {
		startup, cancel := context.WithTimeout(parent, 10*time.Second)
		store, err := bridge.OpenTopicStore(startup, config.DatabasePath)
		cancel()
		if err != nil {
			return err
		}
		defer store.Close()
		config.TopicStore = store
	}
	handler, err := bridge.NewServer(config)
	if err != nil {
		return err
	}
	web := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	hosts := []httpEndpoint{{web, options.listen}}
	var workers []func(context.Context)
	if config.Receipts != nil && config.Receipts.Anchor != nil {
		worker, err := bridge.NewReceiptAnchorWorker(config)
		if err != nil {
			return err
		}
		workers = append(workers, worker.Run)
	}
	if options.hookListen != "" {
		hook, err := bridge.NewWebhook(config, secrets.WebhookSecret, secrets.WebhookToken)
		if err != nil {
			return err
		}
		internal := &http.Server{Handler: hook.Handler(), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 8 * time.Second, WriteTimeout: 12 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8 << 10}
		hosts = append(hosts, httpEndpoint{internal, options.hookListen})
		workers = append(workers, hook.Run)
	}
	return serveEndpoints(parent, ready, hosts, workers)
}
