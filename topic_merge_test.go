package bridge

import (
	"context"
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

func TestMergeTimeoutReconcilesWithoutRepostingAcrossTopics(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bridge.db")
	store, err := OpenTopicStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo := Repository{ID: 17, Owner: "team", Name: "cad"}
	linked := func(id string) TopicChangeSet {
		t.Helper()
		topic, _, err := store.create(ctx, TopicChangeSet{ID: id, ProjectID: "cad", Topic: "CAD-01", CreatorID: 9, CreatedAt: time.Now().UTC(),
			Members: []TopicMember{{RepositoryID: repo.ID, SourceBranch: "feature", TargetBranch: "main"}}}, id, id)
		if err != nil {
			t.Fatal(err)
		}
		if updated, err := store.updateMember(ctx, id, &topic.Members[0], "linked", "existing", 42, 9); err != nil || !updated {
			t.Fatalf("fixture link failed: %v", err)
		}
		return topic
	}
	original, duplicate := linked("original"), linked("duplicate")
	sha, mergedSHA := strings.Repeat("1", 40), strings.Repeat("2", 40)
	var posts atomic.Int32
	var upstreamMerged atomic.Bool
	var targetProtected atomic.Bool
	targetProtected.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer user-token" {
			t.Error("merge did not use current user token")
		}
		const repoPath = "/api/v1/repos/team/cad"
		switch request.URL.Path {
		case repoPath:
			fmt.Fprint(writer, `{"id":17,"mirror":false,"archived":false,"permissions":{"push":true}}`)
		case repoPath + "/branches/main":
			fmt.Fprintf(writer, `{"name":"main","protected":%t,"required_approvals":1,"user_can_push":false,"user_can_merge":true}`, targetProtected.Load())
		case repoPath + "/pulls/42":
			pull := map[string]any{"number": 42, "state": "open", "draft": false, "mergeable": true, "merged": false, "title": "[Topic: CAD-01]",
				"head": map[string]any{"sha": sha, "repo_id": 17, "label": "feature"}, "base": map[string]any{"repo_id": 17, "ref": "main"}}
			if upstreamMerged.Load() {
				pull["state"], pull["merged"], pull["merge_commit_sha"] = "closed", true, mergedSHA
				pull["merged_by"], pull["merged_at"] = map[string]int{"id": 9}, "2026-11-18T12:00:00Z"
			}
			json.NewEncoder(writer).Encode(pull)
		case repoPath + "/pulls/42/reviews":
			writer.Header().Set("X-Total-Count", "1")
			fmt.Fprintf(writer, `[{"id":7,"user":{"id":5},"state":"APPROVED","commit_id":%q,"official":true,"stale":false,"dismissed":false}]`, sha)
		case repoPath + "/pulls/42/merge":
			posts.Add(1)
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || request.Method != "POST" ||
				body["do"] != "merge" || body["head_commit_id"] != sha || body["force_merge"] != false ||
				body["merge_when_checks_succeed"] != false || body["delete_branch_after_merge"] != false {
				t.Errorf("unsafe merge request: %v", body)
			}
			writer.WriteHeader(http.StatusGatewayTimeout)
		default:
			t.Errorf("unexpected upstream call: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	client, err := NewGitea(upstream.URL, upstream.URL, "user-token", 5)
	if err != nil {
		t.Fatal(err)
	}
	load := func(store *TopicStore, topic TopicChangeSet) TopicChangeSet {
		t.Helper()
		reloaded, err := store.get(ctx, topic.ID)
		if err != nil {
			t.Fatal(err)
		}
		return reloaded
	}
	process := func(store *TopicStore, topic TopicChangeSet, input mergeSelection) {
		t.Helper()
		server := &Server{config: ServerConfig{TopicStore: store}}
		if err := server.processMemberMerge(ctx, client, GiteaUser{ID: 9}, repo, topic, topic.Members[0], input); err != nil {
			t.Fatal(err)
		}
	}
	process(store, original, mergeSelection{Reconcile: true})
	if posts.Load() != 0 {
		t.Fatal("readback-only request performed a merge")
	}
	targetProtected.Store(false)
	process(store, original, mergeSelection{Confirm: true, ExpectedHead: sha})
	blocked := load(store, original)
	if blocked.Members[0].Merge == nil || blocked.Members[0].Merge.ResultCode != "branch_policy_unsafe" || posts.Load() != 0 {
		t.Fatal("an unprotected target was not blocked")
	}
	targetProtected.Store(true)
	process(store, blocked, mergeSelection{Confirm: true, ExpectedHead: sha})
	process(store, duplicate, mergeSelection{Confirm: true, ExpectedHead: sha})
	pending := load(store, duplicate)
	if pending.Members[0].Merge == nil || pending.Members[0].Merge.State != "unconfirmed" || posts.Load() != 1 {
		t.Fatalf("timeout did not persist a shared claim: %+v", pending.Members[0].Merge)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenTopicStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	process(reopened, pending, mergeSelection{Confirm: true, ExpectedHead: sha})
	upstreamMerged.Store(true)
	process(reopened, pending, mergeSelection{Reconcile: true})
	for _, topic := range []TopicChangeSet{original, duplicate} {
		record := load(reopened, topic)
		merged := record.Members[0].Merge
		if merged == nil || merged.State != "merged" || merged.MergeSHA != mergedSHA || merged.ExpectedHead != sha ||
			merged.MergedBy != 9 || merged.RequestedBy != 9 || merged.MergedAt == nil || posts.Load() != 1 || record.Version != 5 {
			t.Fatalf("reconciled response lost evidence or reposted: %+v", merged)
		}
	}
}
