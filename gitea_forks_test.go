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

func TestLookupDoesNotExposeUnverifiedForkPulls(t *testing.T) {
	repo := Repository{ID: 1, Owner: "lab", Name: "target"}
	fork := Repository{ID: 2, Owner: "dev", Name: "fork"}
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/repos/dev/fork" {
			t.Errorf("unexpected source lookup: %s", request.URL.Path)
		}
		fmt.Fprint(writer, `{"id":2,"fork":true,"parent":{"id":1},"permissions":{"pull":false}}`)
	}))
	defer upstream.Close()
	client, err := NewGitea(upstream.URL, upstream.URL, "fixture-token", 1)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	result, err := server.visibleForkPulls(context.Background(), client, Project{Forks: []ProjectFork{{TargetID: 1, Source: fork}}},
		[]RepositoryResult{{Repository: repo, Status: "complete", PullRequests: []PullRequest{
			{Number: 1, sourceRepoID: 1}, {Number: 2, sourceRepoID: 2}, {Number: 3, sourceRepoID: 3}, {Number: 4},
		}}})
	if err != nil || len(result) != 1 || len(result[0].PullRequests) != 1 || result[0].PullRequests[0].Number != 1 {
		t.Fatalf("unverified source escaped filtering: %+v, %v", result, err)
	}
}

func verifyForkLab(t *testing.T, base, ownerToken, forkToken, forkPassword string, repos []Repository) {
	t.Helper()
	target := repos[2]
	owner := func(method, path string, data, out any, status int) {
		t.Helper()
		labAPI(t, base, ownerToken, method, path, data, out, status)
	}
	root := "/repos/" + target.Owner + "/" + target.Name
	owner("PUT", root+"/collaborators/labfork", map[string]string{"permission": "read"}, nil, 204)
	var fork struct{ ID int64 }
	labAPI(t, base, forkToken, "POST", root+"/forks", map[string]string{"name": "different-fork-name"}, &fork, 202)
	source := Repository{ID: fork.ID, Owner: "labfork", Name: "different-fork-name"}
	sourcePath := "/repos/" + source.Owner + "/" + source.Name
	labAPI(t, base, forkToken, "POST", sourcePath+"/contents/cross-fork.txt", map[string]string{
		"branch": "main", "message": "Fork-only change", "content": "Zml4dHVyZQo=",
	}, nil, 201)
	var unrelated struct{ ID int64 }
	owner("POST", "/user/repos", map[string]any{"name": "not-a-fork", "private": true, "auto_init": true}, &unrelated, 201)
	owner("PUT", "/repos/labowner/not-a-fork/collaborators/labfork", map[string]string{"permission": "write"}, nil, 204)
	browser, address := loginLab(t, base, ownerToken, "labfork", forkPassword, repos, nil,
		ProjectFork{TargetID: target.ID, Source: source}, ProjectFork{TargetID: target.ID, Source: Repository{ID: unrelated.ID, Owner: "labowner", Name: "not-a-fork"}})
	api := address + "/bridge/api"
	var options []ProjectFork
	if err := json.Unmarshal([]byte(loadLabPage(t, browser, api+"/projects/lab/forks", nil, 200)), &options); err != nil || len(options) != 1 || options[0].Source != source {
		t.Fatalf("unexpected eligible forks: %+v %v", options, err)
	}
	selection := func(sourceID int64) string {
		return fmt.Sprintf(`{"project_id":"lab","topic":"CAD-FORK","members":[{"repository_id":%d,"source_repository_id":%d,"source_branch":"main","target_branch":"main"}]}`, target.ID, sourceID)
	}
	postLabTopic(t, browser, api+"/topics", address, "unrelated", selection(unrelated.ID), 404)
	topic := postLabTopic(t, browser, api+"/topics", address, "fork", selection(source.ID), 201)
	topicURL := api + "/topics/" + topic.ID
	confirm := fmt.Sprintf(`{"confirm":true,"member_ids":[%d]}`, topic.Members[0].ID)
	opened := postLabTopic(t, browser, topicURL+"/pulls", address, "", confirm, 200)
	member := opened.Members[0]
	if member.SourceRepositoryID != source.ID || member.PullNumber < 1 || member.ResultCode != "created" {
		t.Fatalf("cross-fork PR was not created with the selected identity: %+v", member)
	}
	pullPath := fmt.Sprintf("%s/pulls/%d", root, member.PullNumber)
	var actual struct{ Base, Head apiPullBranch }
	owner("GET", pullPath, nil, &actual, 200)
	if actual.Base.RepoID != target.ID || actual.Head.RepoID != source.ID || actual.Base.Ref != "main" || actual.Head.Label != "main" {
		t.Fatalf("unexpected Gitea PR endpoints: %+v", actual)
	}
	reused := postLabTopic(t, browser, api+"/topics", address, "second-fork-record", selection(source.ID), 201)
	reuseSelection := fmt.Sprintf(`{"confirm":true,"member_ids":[%d]}`, reused.Members[0].ID)
	reused = postLabTopic(t, browser, api+"/topics/"+reused.ID+"/pulls", address, "", reuseSelection, 200)
	if reused.Members[0].PullNumber != member.PullNumber || reused.Members[0].ResultCode != "existing" {
		t.Fatalf("matching fork PR was not reused: %+v", reused.Members[0])
	}
	if got := loadLabPage(t, browser, topicURL, nil, 200); !strings.Contains(got, "CAD-FORK") || !strings.Contains(got, fmt.Sprintf(`"source_repository_id":%d`, source.ID)) {
		t.Fatal("saved Fork topic was not displayed to its author")
	}
	labAPI(t, base, forkToken, "DELETE", sourcePath, nil, nil, 204)
	loadLabPage(t, browser, topicURL, nil, 404)
	postLabTopic(t, browser, api+"/topics", address, "fork", selection(source.ID), 404)
	if got := loadLabPage(t, browser, api+"/topics/lookup?project_id=lab&topic=CAD-FORK", nil, 200); strings.Contains(got, "CAD-FORK") {
		t.Fatal("lookup returned a PR without a currently visible source")
	}
}
