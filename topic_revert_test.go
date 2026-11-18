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

func TestReversePullResumesOnlyFromVerifiedBranch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bridge.db")
	store, err := OpenTopicStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo := Repository{ID: 3, Owner: "team", Name: "part"}
	topic, _, err := store.create(ctx, TopicChangeSet{ID: "topic", ProjectID: "team", Topic: "PART-1", CreatorID: 5, CreatedAt: time.Now().UTC(),
		Members: []TopicMember{{RepositoryID: repo.ID, SourceBranch: "feature", TargetBranch: "main"}}}, "key", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.updateMember(ctx, topic.ID, &topic.Members[0], "linked", "existing", 4, 5); !ok || err != nil {
		t.Fatalf("fixture link: %v", err)
	}
	headSHA, mergeSHA, parentSHA := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40)
	mergeTree, parentTree, inverseSHA := strings.Repeat("4", 40), strings.Repeat("5", 40), strings.Repeat("6", 40)
	initial := MergeState{State: "merged", MergeSHA: mergeSHA}
	if ok, err := store.saveMerge(ctx, topic.Members[0], initial); !ok || err != nil {
		t.Fatalf("fixture merge evidence: %v", err)
	}
	member := topic.Members[0]
	member.Merge = &initial
	branch, marker := revertIdentity(repo, member)
	var branchPosts, pullPosts atomic.Int32
	var showBranch, showPull atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		const base = "/api/v1/repos/team/part"
		if request.Header.Get("Authorization") != "Bearer writer-token" {
			t.Error("wrong actor token")
		}
		writeCommit := func(sha, tree, message string, parents ...string) {
			items := make([]map[string]string, 0, len(parents))
			for _, parent := range parents {
				items = append(items, map[string]string{"sha": parent})
			}
			json.NewEncoder(writer).Encode(map[string]any{"sha": sha, "parents": items, "commit": map[string]any{"message": message, "tree": map[string]string{"sha": tree}}})
		}
		switch request.URL.Path {
		case base:
			fmt.Fprint(writer, `{"id":3,"archived":false,"mirror":false,"permissions":{"push":true}}`)
		case base + "/pulls/4":
			fmt.Fprintf(writer, `{"number":4,"merged":true,"merge_commit_sha":%q,"merged_at":"2026-11-18T00:00:00Z","merged_by":{"id":5},"head":{"repo_id":3,"label":"feature","sha":%q},"base":{"repo_id":3,"ref":"main"}}`, mergeSHA, headSHA)
		case base + "/branches/main":
			fmt.Fprintf(writer, `{"name":"main","protected":true,"required_approvals":1,"user_can_push":false,"commit":{"id":%q}}`, mergeSHA)
		case base + "/branches/" + branch:
			if !showBranch.Load() {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprintf(writer, `{"name":%q,"commit":{"id":%q}}`, branch, inverseSHA)
		case base + "/git/commits/" + mergeSHA:
			writeCommit(mergeSHA, mergeTree, "", parentSHA, headSHA)
		case base + "/git/commits/" + parentSHA:
			writeCommit(parentSHA, parentTree, "")
		case base + "/git/commits/" + inverseSHA:
			writeCommit(inverseSHA, parentTree, marker, mergeSHA)
		case base + "/git/trees/" + mergeTree:
			fmt.Fprintf(writer, `{"sha":%q,"truncated":false,"total_count":1,"tree":[{"path":"added.txt","type":"blob","mode":"100644","size":3,"sha":%q}]}`, mergeTree, headSHA)
		case base + "/git/trees/" + parentTree:
			fmt.Fprintf(writer, `{"sha":%q,"truncated":false,"total_count":0,"tree":[]}`, parentTree)
		case base + "/contents":
			branchPosts.Add(1)
			var input struct {
				Branch    string
				NewBranch string `json:"new_branch"`
				ForcePush bool   `json:"force_push"`
				Files     []revertFileChange
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || request.Method != "POST" || input.Branch != "main" || input.NewBranch != branch || input.ForcePush ||
				len(input.Files) != 1 || input.Files[0].Operation != "delete" || input.Files[0].SHA != headSHA {
				t.Errorf("invalid inverse branch request: %+v", input)
			}
			writer.WriteHeader(http.StatusGatewayTimeout)
		case base + "/pulls":
			if request.Method == "POST" {
				pullPosts.Add(1)
				writer.WriteHeader(http.StatusGatewayTimeout)
				return
			}
			if !showPull.Load() {
				writer.Header().Set("X-Total-Count", "0")
				fmt.Fprint(writer, `[]`)
				return
			}
			writer.Header().Set("X-Total-Count", "1")
			fmt.Fprintf(writer, `[{"number":8,"state":"open","body":%q,"head":{"repo_id":3,"sha":%q,"label":%q},"base":{"repo_id":3,"ref":"main"}}]`, marker, inverseSHA, branch)
		default:
			t.Errorf("unexpected upstream request: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	client, err := NewGitea(upstream.URL, upstream.URL, "writer-token", 2)
	if err != nil {
		t.Fatal(err)
	}
	call := func(store *TopicStore, reconcile bool, state string, branches, pulls int32) *RevertState {
		t.Helper()
		loaded, err := store.get(ctx, topic.ID)
		if err != nil {
			t.Fatal(err)
		}
		server := &Server{config: ServerConfig{TopicStore: store, MaxPages: 2}}
		if err := server.processMemberRevert(ctx, client, repo, loaded, loaded.Members[0], reconcile); err != nil {
			t.Fatal(err)
		}
		record, err := store.get(ctx, topic.ID)
		if err != nil {
			t.Fatal(err)
		}
		revert := record.Members[0].Revert
		if revert == nil || revert.State != state || branchPosts.Load() != branches || pullPosts.Load() != pulls {
			t.Fatalf("unexpected state: %+v; branch requests=%d, PR requests=%d", revert, branchPosts.Load(), pullPosts.Load())
		}
		return revert
	}
	call(store, false, "branch_pending", 1, 0)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenTopicStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	call(reopened, false, "branch_pending", 1, 0)
	showBranch.Store(true)
	call(reopened, true, "branch_pending", 1, 0)
	call(reopened, false, "pull_pending", 1, 1)
	call(reopened, false, "pull_pending", 1, 1)
	showPull.Store(true)
	opened := call(reopened, true, "opened", 1, 1)
	if opened.PullNumber != 8 || opened.InverseSHA != inverseSHA {
		t.Fatalf("readback did not confirm the original reverse PR: %+v", opened)
	}
}
