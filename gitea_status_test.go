package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInspectPullUsesCoherentEvidence(t *testing.T) {
	sha := strings.Repeat("a", 40)
	otherSHA := strings.Repeat("b", 40)
	const repoPath = "/api/v1/repos/team/cad"
	for _, fixture := range []struct {
		name, laterSHA, ciExpected string
		statusCount                int
		shouldInspect              bool
	}{
		{"current commit", sha, "failure", 1, true},
		{"truncated status page", sha, "", 2, true},
		{"head moved", otherSHA, "", 1, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case repoPath + "/pulls/1":
					fmt.Fprintf(writer, `{"number":1,"state":"open","head":{"sha":%q},"base":{"repo_id":2},"changed_files":0,"mergeable":false}`, fixture.laterSHA)
				case repoPath + "/commits/" + sha + "/status":
					if !fixture.shouldInspect {
						t.Error("read CI after the head changed")
					}
					writer.Header().Set("X-Total-Count", fmt.Sprint(fixture.statusCount))
					fmt.Fprintf(writer, `{"sha":%q,"repository":{"id":2},"state":"failure","statuses":[{"status":"failure"}]}`, sha)
				case repoPath + "/pulls/1/reviews":
					if !fixture.shouldInspect {
						t.Error("read reviews after the head changed")
					}
					makeReview := func(id, user int, state, commit string, dismissed, stale, official bool) map[string]any {
						return map[string]any{"id": id, "user": map[string]int{"id": user}, "state": state, "commit_id": commit,
							"dismissed": dismissed, "stale": stale, "official": official}
					}
					reviews := []map[string]any{
						makeReview(1, 10, "APPROVED", sha, false, false, true),
						makeReview(2, 10, "REQUEST_CHANGES", sha, false, false, true),
						makeReview(3, 11, "APPROVED", sha, true, false, true),
						makeReview(4, 12, "APPROVED", sha, false, true, true),
						makeReview(5, 13, "APPROVED", sha, false, false, false),
						makeReview(6, 14, "APPROVED", otherSHA, false, false, true),
						makeReview(7, 15, "APPROVED", sha, false, false, true),
					}
					writer.Header().Set("X-Total-Count", fmt.Sprint(len(reviews)))
					if err := json.NewEncoder(writer).Encode(reviews); err != nil {
						t.Error(err)
					}
				default:
					t.Errorf("unexpected path %s", request.URL.Path)
					writer.WriteHeader(500)
				}
			}))
			defer upstream.Close()
			client, err := NewGitea(upstream.URL, upstream.URL, "reader-token", 1)
			if err != nil {
				t.Fatal(err)
			}
			results := []RepositoryResult{{Repository: Repository{ID: 2, Owner: "team", Name: "cad"}, PullRequests: []PullRequest{{Number: 1, HeadSHA: sha, Lifecycle: "open"}}}}
			results, err = client.inspectMatches(context.Background(), results)
			if err != nil || len(results) != 1 {
				t.Fatalf("inspect failed: %v", err)
			}
			pull := results[0].PullRequests[0]
			if pull.CI != fixture.ciExpected {
				t.Fatalf("CI = %q, want %q", pull.CI, fixture.ciExpected)
			}
			if fixture.shouldInspect {
				if pull.Conflict != "not_mergeable_reason_unknown" || pull.ChangedFiles == nil || *pull.ChangedFiles != 0 || pull.Reviews == nil || *pull.Reviews != (ReviewEvidence{Approved: 1, ChangesRequested: 1}) {
					t.Fatalf("wrong PR evidence: %+v", pull)
				}
			} else if pull.Conflict != "" || pull.ChangedFiles != nil || pull.Reviews != nil {
				t.Fatalf("stale metadata was not suppressed: %+v", pull)
			}
		})
	}
}
