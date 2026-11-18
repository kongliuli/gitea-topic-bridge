package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnconfirmedPullDoesNotRepostAcrossRestart(t *testing.T) {
	ctx := context.Background()
	repo := Repository{ID: 12, Owner: "team", Name: "private"}
	draft := TopicChangeSet{ID: "fixture-topic", ProjectID: "lab", Topic: "CAD-01", CreatorID: 91, CreatedAt: time.Now().UTC(),
		Members: []TopicMember{{RepositoryID: repo.ID, SourceBranch: "feature/example", TargetBranch: "main"}}}
	databasePath := filepath.Join(t.TempDir(), "bridge.db")
	store, err := OpenTopicStore(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	topic, _, err := store.create(ctx, draft, "request-key", "request-hash")
	if err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	var showUpstreamResult atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		const endpoint = "/api/v1/repos/team/private"
		if request.Header.Get("Authorization") != "Bearer member-token" {
			t.Error("operation did not use the actor's token")
		}
		switch {
		case request.URL.Path == endpoint && request.Method == http.MethodGet:
			writer.Write([]byte(`{"id":12,"mirror":false,"archived":false,"permissions":{"push":true}}`))
		case request.URL.Path == endpoint+"/pulls" && request.Method == http.MethodGet:
			rows := make([]any, 0)
			total := "0"
			if showUpstreamResult.Load() {
				total = "1"
				rows = append(rows, map[string]any{"number": 42, "state": "closed", "merged": true, "title": "[Topic: CAD-01] fixture",
					"body": memberMarker(topic.ID, topic.Members[0]),
					"head": map[string]any{"repo_id": repo.ID, "label": "feature/example", "ref": "refs/pull/42/head"},
					"base": map[string]any{"repo_id": repo.ID, "ref": "main"}})
			}
			writer.Header().Set("X-Total-Count", total)
			json.NewEncoder(writer).Encode(rows)
		case request.URL.Path == endpoint+"/pulls" && request.Method == http.MethodPost:
			posts.Add(1)
			writer.WriteHeader(http.StatusGatewayTimeout)
		default:
			t.Errorf("unexpected upstream request: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	client, err := NewGitea(upstream.URL, upstream.URL, "member-token", 10)
	if err != nil {
		t.Fatal(err)
	}
	apply := func(store *TopicStore, topic TopicChangeSet) TopicChangeSet {
		t.Helper()
		server := &Server{config: ServerConfig{TopicStore: store, MaxPages: 2}}
		if err := server.processMemberPull(ctx, client, 91, repo, topic, topic.Members[0]); err != nil {
			t.Fatal(err)
		}
		reloaded, err := store.get(ctx, topic.ID)
		if err != nil {
			t.Fatal(err)
		}
		return reloaded
	}
	unconfirmed := apply(store, topic)
	if unconfirmed.Members[0].State != "unconfirmed" || posts.Load() != 1 || unconfirmed.Version != 2 {
		t.Fatalf("did not persist uncertainty: %+v", unconfirmed)
	}
	stale := apply(store, topic)
	if posts.Load() != 1 || stale.Version != unconfirmed.Version {
		t.Fatal("a stale claimed operation caused a second POST")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenTopicStore(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	unchanged := apply(restarted, unconfirmed)
	if unchanged.Members[0].State != "unconfirmed" || posts.Load() != 1 {
		t.Fatal("an empty readback caused a second POST")
	}
	showUpstreamResult.Store(true)
	recovered := apply(restarted, unchanged)
	if recovered.Members[0].PullNumber != 42 || recovered.Members[0].State != "linked" || recovered.Version != 3 || posts.Load() != 1 {
		t.Fatalf("readback did not link the original PR: %+v", recovered)
	}
}
