package bridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndependentAnchorHTTPS(t *testing.T) {
	caddy := os.Getenv("BRIDGE_TEST_CADDY_BINARY")
	if caddy == "" {
		t.Skip("set BRIDGE_TEST_CADDY_BINARY for the independent HTTPS gateway test")
	}
	if !filepath.IsAbs(caddy) {
		t.Fatal("Caddy path must be absolute")
	}
	ctx := context.Background()
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	bearer := strings.Repeat("local-fixture-", 3)
	ledger, err := OpenAnchorLedger(ctx, filepath.Join(t.TempDir(), "anchor.db"), seed, bearer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })
	backend := httptest.NewServer(ledger.Handler())
	t.Cleanup(backend.Close)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	sample, err := os.ReadFile("examples/Caddyfile.anchor")
	if err != nil {
		t.Fatal(err)
	}
	config := strings.NewReplacer("anchor.office.example:8443", address, "127.0.0.1:3003", backend.Listener.Addr().String()).Replace(string(sample))
	ca, probe := startLabTLSCaddy(t, caddy, address, address, config, "/v1/anchors", http.StatusNotFound)
	public := "https://" + address
	for _, path := range []string{"/", "/v1/anchors", "/bridge/", "/internal/hooks/gitea"} {
		loadLabPage(t, probe, public+path, nil, http.StatusNotFound)
	}
	badPath, err := probe.Post(public+"/v1/anchors/extra", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	badPath.Body.Close()
	if badPath.StatusCode != http.StatusNotFound {
		t.Fatal("the independent gateway exposed a path other than its append endpoint")
	}
	issuer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	key := issuer.Public().(ed25519.PublicKey)
	doc := receiptDocument{Schema: 1, ID: strings.Repeat("r", 43), TopicID: strings.Repeat("t", 43), Version: 1, IssuerID: 8,
		Members: []receiptMember{{Repository: Repository{ID: 9, Owner: "lab", Name: "fixture"}}}}
	envelope, err := signReceipt(doc, issuer)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	statement := anchorStatement{doc.ID, envelope.Digest, envelope.KeyID}
	settings := ReceiptAnchorConfig{URL: public + "/v1/anchors", PublicKey: ledger.PublicKey(), Bearer: bearer}
	untrusted, err := newAnchorClient(settings, key)
	if err != nil {
		t.Fatal(err)
	}
	if proof, result := untrusted.append(ctx, statement); proof != nil || result != "unavailable" {
		t.Fatalf("untrusted private CA was not rejected: %s", result)
	}
	settings.CAFile, settings.Bearer = ca, strings.Repeat("incorrect-", 4)
	unauthorized, err := newAnchorClient(settings, key)
	if err != nil {
		t.Fatal(err)
	}
	if proof, result := unauthorized.append(ctx, statement); proof != nil || result != "unavailable" {
		t.Fatalf("wrong anchor credential was accepted: %s", result)
	}
	var count int
	if err := ledger.db.QueryRow("SELECT COUNT(*) FROM anchors").Scan(&count); err != nil || count != 0 {
		t.Fatalf("untrusted or unauthenticated requests changed the independent ledger: %d %v", count, err)
	}
	settings.Bearer = bearer
	client, err := newAnchorClient(settings, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.http.CloseIdleConnections)
	proof, result := client.append(ctx, statement)
	if err := VerifyReceiptAnchor(signed, proof, base64.StdEncoding.EncodeToString(key), ledger.PublicKey()); result != "" || err != nil {
		t.Fatalf("HTTPS append did not return a verifiable independent proof: %s (%v)", result, err)
	}
	again, result := client.append(ctx, statement)
	if result != "" || !bytes.Equal(proof, again) {
		t.Fatal("HTTPS replay changed the signed proof")
	}
}

func TestAnchorCAFileConfiguration(t *testing.T) {
	issuer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	anchor := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))
	certPath := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	valid := ReceiptAnchorConfig{
		URL: "https://anchor.example.com/v1/anchors", Bearer: strings.Repeat("test-", 8),
		PublicKey: base64.StdEncoding.EncodeToString(anchor.Public().(ed25519.PublicKey)),
	}
	for _, test := range []struct{ url, path string }{
		{valid.URL, "relative.pem"},
		{valid.URL, certPath},
		{"http://127.0.0.1:3003/v1/anchors", certPath},
	} {
		config := valid
		config.URL, config.CAFile = test.url, test.path
		if _, err := newAnchorClient(config, issuer.Public().(ed25519.PublicKey)); err == nil {
			t.Fatalf("invalid CA configuration accepted: %+v", test)
		}
	}
}
