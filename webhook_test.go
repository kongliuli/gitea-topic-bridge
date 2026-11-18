package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookAuthenticatesDeduplicatesAndReadsCurrentState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bridge.db")
	store, err := OpenTopicStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var reads atomic.Int32
	var available atomic.Bool
	actualSHA := strings.Repeat("b", 40)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != "GET" || request.Header.Get("Authorization") != "Bearer read-token" {
			t.Errorf("unexpected webhook worker request: %s %s", request.Method, request.URL.Path)
		}
		switch request.URL.Path {
		case "/api/v1/repos/team/cad":
			fmt.Fprint(writer, `{"id":1}`)
		case "/api/v1/repos/team/cad/pulls/7":
			reads.Add(1)
			if !available.Load() {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprintf(writer, `{"number":7,"base":{"repo_id":1},"merged":true,"merge_commit_sha":%q,"merged_by":{"id":6},"merged_at":"2026-11-19T00:00:00Z"}`, actualSHA)
		default:
			t.Errorf("unexpected upstream path %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	config := ServerConfig{TopicStore: store, GiteaAPIURL: upstream.URL, GiteaPublicURL: upstream.URL,
		Projects: []Project{{Repositories: []Repository{{ID: 1, Owner: "team", Name: "cad"}}}}}
	secret := strings.Repeat("s", 32)
	hook, err := NewWebhook(config, secret, "read-token")
	if err != nil {
		t.Fatal(err)
	}
	handler := hook.Handler()
	deliver := func(id, kind, body, signed, peer string, expected int) {
		t.Helper()
		req := httptest.NewRequest("POST", "/internal/hooks/gitea", strings.NewReader(body))
		req.RemoteAddr = peer
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Gitea-Delivery", id)
		req.Header.Set("X-Gitea-Event", kind)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(signed))
		req.Header.Set("X-Gitea-Signature", hex.EncodeToString(mac.Sum(nil)))
		writer := httptest.NewRecorder()
		handler.ServeHTTP(writer, req)
		if writer.Code != expected {
			t.Fatalf("webhook status %d, expected %d: %s", writer.Code, expected, writer.Body)
		}
	}
	const first = "afd01334-326c-4c90-ba52-b6895274e1d1"
	const older = "afd01334-326c-4c90-ba52-b6895274e1d2"
	const localhost = "127.0.0.1:18000"
	mergedPayload := `{"action":"closed","repository":{"id":1},"number":7,"pull_request":{"number":7,"base":{"repo_id":1},"merged":true,"merge_commit_sha":"untrusted"}}`
	openedPayload := strings.Replace(mergedPayload, "closed", "opened", 1)
	deliver(first, "pull_request", mergedPayload+" ", mergedPayload, localhost, 401)
	deliver(first, "pull_request", mergedPayload, mergedPayload, "192.0.2.1:8080", 404)
	deliver(first, "release", mergedPayload, mergedPayload, localhost, 400)
	deliver(first, "pull_request", mergedPayload, mergedPayload, localhost, 202)
	deliver(strings.ToUpper(first), "pull_request", mergedPayload, mergedPayload, localhost, 202)
	deliver(first, "pull_request", openedPayload, openedPayload, localhost, 409)
	unknownRepo := strings.ReplaceAll(mergedPayload, ":1", ":2")
	deliver(older, "pull_request", unknownRepo, unknownRepo, localhost, 404)
	deliver(older, "pull_request", openedPayload, openedPayload, localhost, 202)
	if reads.Load() != 0 {
		t.Fatal("receiver contacted Gitea before acknowledgment")
	}
	if worked, err := hook.workOnce(ctx, time.Now()); err != nil || !worked {
		t.Fatalf("first work: %t, %v", worked, err)
	}
	var failedID, failure string
	if err := store.db.QueryRow(`SELECT delivery_id, last_error FROM webhook_inbox WHERE attempts=1`).Scan(&failedID, &failure); err != nil || failedID != first || failure == "" {
		t.Fatalf("failed attempt was not persisted: %s %s %v", failedID, failure, err)
	}
	job, err := store.claimWebhook(ctx, time.Now())
	if err != nil || job.Delivery != older {
		t.Fatalf("other ready job not claimable during backoff: %+v %v", job, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenTopicStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	config.TopicStore = reopened
	hook, err = NewWebhook(config, secret, "read-token")
	if err != nil {
		t.Fatal(err)
	}
	available.Store(true)
	for _, step := range []time.Duration{30 * time.Second, webhookLease + 10*time.Second} {
		if worked, err := hook.workOnce(ctx, time.Now().Add(step)); err != nil || !worked {
			t.Fatalf("restart work: %t, %v", worked, err)
		}
	}
	var jobs, verified int
	if err := reopened.db.QueryRow(`
SELECT COUNT(*), SUM(json_extract(observation,'$.commit_sha')=? AND json_extract(observation,'$.status')='merged')
FROM webhook_inbox`, actualSHA).Scan(&jobs, &verified); err != nil || jobs != 2 || verified != 2 || reads.Load() != 3 {
		t.Fatalf("dedupe/retry/ordering mismatch: jobs=%d verified=%d reads=%d %v", jobs, verified, reads.Load(), err)
	}
	var observation string
	if err := reopened.db.QueryRow(`SELECT observation FROM webhook_inbox WHERE delivery_id=?`, older).Scan(&observation); err != nil {
		t.Fatal(err)
	}
	var data sourceObservation
	if json.Unmarshal([]byte(observation), &data) != nil || data.Commit != actualSHA || data.ActorID != 6 || data.OccurredAt == nil {
		t.Fatalf("evidence came from the notice rather than Gitea: %+v", data)
	}
}
