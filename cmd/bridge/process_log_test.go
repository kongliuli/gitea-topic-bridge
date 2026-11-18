package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bridge "gitea-topic-bridge"
)

func TestProcessLogRejectsDeploymentData(t *testing.T) {
	root := t.TempDir()
	artifacts, snapshots := filepath.Join(root, "artifacts"), filepath.Join(root, "snapshots")
	for _, dir := range []string{artifacts, snapshots} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config, secret, database, ca := filepath.Join(root, "bridge.json"), filepath.Join(root, "secret.json"),
		filepath.Join(root, "bridge.db"), filepath.Join(root, "ca.pem")
	data, err := json.Marshal(bridge.ServerConfig{DatabasePath: database, Receipts: &bridge.ReceiptConfig{
		ArtifactRoot: artifacts, SnapshotRoot: snapshots, Anchor: &bridge.ReceiptAnchorConfig{CAFile: ca},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string][]byte{
		config: data, secret: []byte("fixture secret"), ca: []byte("fixture CA"),
	} {
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "hardlink.log")
	if err := os.Link(secret, alias); err != nil {
		t.Fatal(err)
	}
	options := launchOptions{config: config, secretFile: secret}
	for _, path := range []string{config, secret, ca, alias, database, database + "-wal", database + "-shm", database + "-journal",
		filepath.Join(artifacts, "bad.log"), filepath.Join(snapshots, "bad.log")} {
		file, err := options.openProcessLog(path)
		if err == nil {
			file.Close()
			t.Errorf("opened a deployment file as the process log: %s", path)
		}
	}
	if bytes, err := os.ReadFile(secret); err != nil || string(bytes) != "fixture secret" {
		t.Fatalf("secret bytes changed while checking the log path: %v", err)
	}
	if _, err := os.Stat(database); !os.IsNotExist(err) {
		t.Fatal("log validation created a database file")
	}
	anchorDB := filepath.Join(root, "anchor.db")
	for _, path := range []string{anchorDB, anchorDB + "-wal", secret} {
		file, err := (launchOptions{anchorDB: anchorDB, secretFile: secret}).openProcessLog(path)
		if err == nil {
			file.Close()
			t.Errorf("opened an independent ledger or secret as the log: %s", path)
		}
	}
	log := filepath.Join(root, "normal.log")
	for _, text := range []string{"start\n", "stop\n"} {
		file, err := options.openProcessLog(log)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(text); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if data, err := os.ReadFile(log); err != nil || strings.TrimSpace(string(data)) != "start\nstop" {
		t.Fatalf("process log did not retain both writes: %v", err)
	}
}
