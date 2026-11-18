package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type recoveredFixture struct {
	id             string
	receipt, proof []byte
}

type receiptRecoveryLab struct {
	bridgeDB, snapshots, anchorDB                   string
	receiptKey, anchorKey, anchorSeed, anchorBearer string
	versions                                        []recoveredFixture
	stop                                            func()
}

func (lab receiptRecoveryLab) verifyRestored(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		return
	}
	lab.stop()
	script, err := filepath.Abs("examples/Copy-StoppedDatabase.ps1")
	if err != nil {
		t.Fatal(err)
	}
	copyStopped := func(database, snapshots, destination string, wantSuccess bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		args := []string{"-NoProfile", "-NonInteractive", "-File", script, "-Database", database, "-Destination", destination}
		if snapshots != "" {
			args = append(args, "-Snapshots", snapshots)
		}
		output, err := exec.CommandContext(ctx, "powershell.exe", args...).CombinedOutput()
		if (err == nil) != wantSuccess {
			t.Fatalf("copy: %v, %s", err, output)
		}
	}
	backup, anchorBackup := t.TempDir(), t.TempDir()
	copyStopped(lab.bridgeDB, lab.snapshots, backup, true)
	copyStopped(lab.anchorDB, "", anchorBackup, true)
	copyStopped(lab.bridgeDB, lab.snapshots, backup, false)
	restored, anchorRestored := t.TempDir(), t.TempDir()
	const databaseFile = "state.sqlite3"
	copyStopped(filepath.Join(backup, databaseFile), filepath.Join(backup, "snapshots"), restored, true)
	copyStopped(filepath.Join(anchorBackup, databaseFile), "", anchorRestored, true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	database, snapshots, ledgerPath := filepath.Join(restored, databaseFile), filepath.Join(restored, "snapshots"), filepath.Join(anchorRestored, databaseFile)
	read := func(path string) []byte {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before, ledgerBefore := sha256.Sum256(read(database)), sha256.Sum256(read(ledgerPath))
	audit := func() (RestoreAudit, error) {
		return AuditRestoredBridge(ctx, database, snapshots, lab.receiptKey, lab.anchorKey, ledgerPath)
	}
	result, err := audit()
	if err != nil || result.Receipts != 2 || result.Artifacts != 2 || result.Topics != 1 || result.VerifiedAnchors != 2 || result.PendingAnchors != 0 {
		t.Fatalf("restored evidence: %+v, %v", result, err)
	}
	if sha256.Sum256(read(database)) != before || sha256.Sum256(read(ledgerPath)) != ledgerBefore {
		t.Fatal("offline audit changed database bytes")
	}
	for _, path := range []string{database, ledgerPath} {
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
				t.Fatalf("audit left database sidecars: %v", err)
			}
		}
	}
	store, err := OpenTopicStore(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ledger, err := OpenAnchorLedger(ctx, ledgerPath, lab.anchorSeed, lab.anchorBearer)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	for _, version := range lab.versions {
		raw, err := store.receiptByID(ctx, version.id)
		if err != nil || !bytes.Equal(raw, version.receipt) {
			t.Fatal("restored receipt differs from the original export")
		}
		var proof anchorProof
		if err := json.Unmarshal(version.proof, &proof); err != nil {
			t.Fatal(err)
		}
		replayed, created, err := ledger.append(ctx, proof.Record.anchorStatement)
		if err != nil || created || replayed.Signature != proof.Signature || !replayed.Record.RecordedAt.Equal(proof.Record.RecordedAt) {
			t.Fatal("restored independent ledger did not return the original proof")
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(snapshots, lab.versions[0].id, "1.blob")
	corrupt := bytes.Repeat([]byte{'x'}, len(read(file)))
	if err := os.WriteFile(file, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := audit(); err == nil || !strings.Contains(err.Error(), "different size or digest") {
		t.Fatalf("restored snapshot corruption was not rejected: %v", err)
	}
	sidecar, err := os.OpenFile(database+"-wal", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(sidecar, "simulated uncheckpointed state"); err != nil {
		t.Fatal(err)
	}
	sidecar.Close()
	if _, err := audit(); err == nil || !strings.Contains(err.Error(), "sidecars") {
		t.Fatalf("audit ignored SQLite WAL: %v", err)
	}
	copyStopped(database, snapshots, t.TempDir(), false)
}
