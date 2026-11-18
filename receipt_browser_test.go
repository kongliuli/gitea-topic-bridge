package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReceiptCenterEdge(t *testing.T) {
	edge := os.Getenv("BRIDGE_TEST_EDGE_BINARY")
	if edge == "" {
		t.Skip("set BRIDGE_TEST_EDGE_BINARY for the isolated headless Edge check")
	}
	if !filepath.IsAbs(edge) {
		t.Fatal("browser path must be absolute")
	}
	id, topicID := strings.Repeat("A", 43), strings.Repeat("B", 43)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bridge/receipts/", serveAsset("receipts.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /bridge/receipts.js", serveAsset("receipts.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /bridge/style.css", serveAsset("style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /bridge/api/receipts/"+id, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Bridge-Anchor-Status", "verified")
		responseJSON(writer, 200, receiptEnvelope{Digest: strings.Repeat("f", 64), Snapshot: json.RawMessage(`{"id":"` + id + `","topic_id":"` + topicID + `","topic":"CAD-01","version":2,"issued_at":"2026-11-19T00:00:00Z","issuer_id":3,"members":[{"repository":{"owner":"lab","name":"fixture"},"status":"unverified"}],"artifacts":[]}`)})
	})
	mux.HandleFunc("GET /bridge/api/receipts", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("topic_id") != topicID {
			t.Error("wrong topic loaded")
		}
		responseJSON(writer, 200, map[string]any{"topic": "CAD-01", "can_issue": true, "repositories": []Repository{{ID: 3, Owner: "lab", Name: "fixture"}},
			"versions": []receiptVersion{{ID: id, Version: 2, IssuedAt: time.Now()}}})
	})
	lab := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		mux.ServeHTTP(writer, request)
	}))
	defer lab.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, edge, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-background-networking", "--user-data-dir="+t.TempDir(), "--virtual-time-budget=5000", "--dump-dom", lab.URL+"/bridge/receipts/?id="+id)
	rendered, err := command.Output()
	if err != nil {
		t.Fatalf("headless Edge could not render the receipt center: %v", err)
	}
	for _, expected := range []string{"CAD-01 · v2", "独立存证已核验", "未核验源提交", "CAD-01 · 版本历史", "更正 v2（旧版保留）", "?format=proof", "?format=markdown"} {
		if !strings.Contains(string(rendered), expected) {
			t.Errorf("receipt center did not render %q", expected)
		}
	}
}
