package bridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIndependentAnchorSurvivesLostReplyAndRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	bridgePath, ledgerPath := filepath.Join(root, "bridge.db"), filepath.Join(root, "independent.db")
	issuer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	anchorSeed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	bearer := strings.Repeat("a", 32)
	ledger, err := OpenAnchorLedger(ctx, ledgerPath, anchorSeed, bearer)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	var discard atomic.Bool
	discard.Store(true)
	handler := ledger.Handler()
	sink := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if discard.Load() {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != 201 {
				t.Errorf("independent write failed: %d", recorder.Code)
			}
			writer.WriteHeader(503)
			return
		}
		handler.ServeHTTP(writer, request)
	}))
	defer sink.Close()
	if _, err := newAnchorClient(ReceiptAnchorConfig{URL: sink.URL + "/v1/anchors", PublicKey: base64.StdEncoding.EncodeToString(issuer.Public().(ed25519.PublicKey)), Bearer: bearer}, issuer.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("receipt signing key accepted as independent key")
	}
	client, err := newAnchorClient(ReceiptAnchorConfig{URL: sink.URL + "/v1/anchors", PublicKey: ledger.PublicKey(), Bearer: bearer}, issuer.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenTopicStore(ctx, bridgePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	topic := TopicChangeSet{ID: strings.Repeat("t", 43), ProjectID: "lab", Topic: "LAB", CreatorID: 8, CreatedAt: time.Now(),
		Members: []TopicMember{{RepositoryID: 9}}}
	if _, _, err := store.create(ctx, topic, "topic-key", "hash"); err != nil {
		t.Fatal(err)
	}
	doc := receiptDocument{Schema: 1, ID: strings.Repeat("r", 43), TopicID: topic.ID, Version: 1, IssuerID: 8, Members: []receiptMember{{Repository: Repository{ID: 9}}}}
	envelope, err := signReceipt(doc, issuer)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.insertReceipt(ctx, doc, "receipt-key", "hash", raw, true); err != nil {
		t.Fatal(err)
	}
	call := func(worker *ReceiptAnchorWorker, now time.Time) {
		t.Helper()
		if done, err := worker.workOnce(ctx, now); !done || err != nil {
			t.Fatalf("anchor worker: %v %v", done, err)
		}
	}
	call(&ReceiptAnchorWorker{store, issuer.Public().(ed25519.PublicKey), client}, time.Now())
	var failed string
	if err := store.db.QueryRow(`SELECT last_error FROM receipt_anchor_outbox WHERE receipt_id=? AND proof IS NULL`, doc.ID).Scan(&failed); err != nil || failed != "unavailable" {
		t.Fatalf("lost response was treated as verified: %s %v", failed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := OpenTopicStore(ctx, bridgePath)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	discard.Store(false)
	call(&ReceiptAnchorWorker{recovered, issuer.Public().(ed25519.PublicKey), client}, time.Now().Add(time.Minute))
	var proof string
	if err := recovered.db.QueryRow(`SELECT proof FROM receipt_anchor_outbox WHERE receipt_id=? AND attempts=2`, doc.ID).Scan(&proof); err != nil {
		t.Fatal(err)
	}
	issuerPublic := base64.StdEncoding.EncodeToString(issuer.Public().(ed25519.PublicKey))
	if err := VerifyReceiptAnchor(raw, []byte(proof), issuerPublic, ledger.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if err := VerifyReceiptAnchor(raw, bytes.ReplaceAll([]byte(proof), []byte(envelope.Digest), []byte(strings.Repeat("f", 64))), issuerPublic, ledger.PublicKey()); err == nil {
		t.Fatal("tampered anchor proof verified")
	}
	if _, code := client.append(ctx, anchorStatement{doc.ID, strings.Repeat("f", 64), envelope.KeyID}); code != "digest_conflict" {
		t.Fatalf("anchor accepted a digest replacement: %s", code)
	}
	for _, statement := range []string{"DELETE FROM anchors", "UPDATE anchors SET receipt_sha='different'", "INSERT OR REPLACE INTO anchors SELECT * FROM anchors"} {
		if _, err := ledger.db.Exec(statement); err == nil {
			t.Fatalf("mutable ledger: %s", statement)
		}
	}
	var count int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM anchors`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("anchor count %d: %v", count, err)
	}
	after, err := recovered.receiptByID(ctx, doc.ID)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("anchor worker changed signed receipt bytes")
	}
}
