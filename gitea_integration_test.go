package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestGiteaLocalLab(t *testing.T) {
	binary := os.Getenv("BRIDGE_TEST_GITEA_BINARY")
	if binary == "" {
		t.Skip("set BRIDGE_TEST_GITEA_BINARY to an absolute Gitea binary path")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("BRIDGE_TEST_GITEA_BINARY must be an absolute path")
	}
	internal := httptest.NewUnstartedServer(nil)
	t.Cleanup(internal.Close)
	base, command := startLocalGitea(t, binary, internal.Listener.Addr().String())
	home := loadLabPage(t, &http.Client{Timeout: 10 * time.Second}, base+"/", nil, 200)
	for _, item := range []string{`href="/bridge/">多仓联调</a>`, `href="/bridge/receipts/">交付回执</a>`} {
		if strings.Count(home, item) != 1 {
			t.Fatalf("Gitea did not render exactly one extension link %q", item)
		}
	}
	createUser := func(name, scopes string, opts ...string) (string, string) {
		t.Helper()
		args := []string{"admin", "user", "create", "--username", name, "--email", name + "@example.invalid", "--random-password", "--must-change-password=false", "--access-token", "--access-token-name", "bridge-lab", "--access-token-scopes", scopes}
		output, err := command(append(args, opts...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("create temporary user %s: %v", name, err)
		}
		match := regexp.MustCompile(`(?i)access token was successfully created[.:]+\s+([a-f0-9]{40})`).FindSubmatch(output)
		if len(match) != 2 {
			t.Fatal("Gitea CLI did not return a fixture access token")
		}
		password := regexp.MustCompile(`generated random password is '([^\r\n]+)'`).FindSubmatch(output)
		if len(password) != 2 {
			t.Fatal("Gitea CLI did not return a fixture password")
		}
		return string(match[1]), string(password[1])
	}
	ownerToken, _ := createUser("labowner", "all", "--admin")
	readerToken, readerPassword := createUser("labreader", "read:repository,read:user")
	reviewerToken, reviewerPassword := createUser("labreviewer", "write:repository,read:user")
	api := func(method, path string, data, out any, status int) {
		t.Helper()
		labAPI(t, base, ownerToken, method, path, data, out, status)
	}
	repos := make([]Repository, 0, 4)
	for _, name := range []string{"merged", "conflict", "no-match", "hidden"} {
		var repo struct{ ID int64 }
		api("POST", "/user/repos", map[string]any{"name": name, "private": true, "auto_init": true, "default_branch": "main"}, &repo, 201)
		repos = append(repos, Repository{ID: repo.ID, Owner: "labowner", Name: name})
		if name != "hidden" {
			api("PUT", "/repos/labowner/"+name+"/collaborators/labreader", map[string]string{"permission": "read"}, nil, 204)
		}
	}
	hookStore, err := OpenTopicStore(context.Background(), filepath.Join(t.TempDir(), "webhooks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hookStore.Close() })
	secret, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	hook, err := NewWebhook(ServerConfig{TopicStore: hookStore, Projects: []Project{{Repositories: repos}}, GiteaAPIURL: base, GiteaPublicURL: base}, secret, readerToken)
	if err != nil {
		t.Fatal(err)
	}
	internal.Config.Handler = hook.Handler()
	internal.Start()
	hookCtx, stopHook := context.WithCancel(context.Background())
	doneHook := make(chan struct{})
	go func() { defer close(doneHook); hook.Run(hookCtx) }()
	t.Cleanup(func() { stopHook(); <-doneHook })
	api("POST", "/repos/labowner/merged/hooks", map[string]any{
		"type": "gitea", "active": true, "events": []string{"pull_request", "release"},
		"config": map[string]string{"url": internal.URL + "/internal/hooks/gitea", "content_type": "json", "secret": secret},
	}, nil, 201)
	seedBranch := func(repo, branch string) {
		t.Helper()
		prefix := "/repos/labowner/" + repo
		api("POST", prefix+"/branches", map[string]string{"new_branch_name": branch, "old_branch_name": "main"}, nil, 201)
		api("POST", prefix+"/contents/"+branch+".txt", map[string]string{"branch": branch, "message": "Synthetic fixture", "content": base64.StdEncoding.EncodeToString([]byte("test\n"))}, nil, 201)
	}
	seedPull := func(repo, branch, title, body string) int64 {
		t.Helper()
		seedBranch(repo, branch)
		prefix := "/repos/labowner/" + repo
		var pull struct{ Number int64 }
		api("POST", prefix+"/pulls", map[string]string{"title": title, "body": body, "base": "main", "head": branch}, &pull, 201)
		return pull.Number
	}
	merged := seedPull("merged", "complete", "[Topic: CAD-01] Merged fixture", "")
	api("POST", fmt.Sprintf("/repos/labowner/merged/pulls/%d/merge", merged), map[string]string{"Do": "merge"}, nil, 200)
	var actualMerge struct {
		SHA string `json:"merge_commit_sha"`
	}
	api("GET", fmt.Sprintf("/repos/labowner/merged/pulls/%d", merged), nil, &actualMerge, 200)
	api("POST", "/repos/labowner/merged/tags", map[string]string{
		"tag_name": "build/v1", "tag_message": "Annotated fixture", "target": actualMerge.SHA,
	}, nil, 201)
	var release struct{ ID int64 }
	api("POST", "/repos/labowner/merged/releases", map[string]string{"tag_name": "build/v1", "name": "Fixture release"}, &release, 201)
	conflicting := seedPull("conflict", "changes", "Open fixture", "[Topic: CAD-01]")
	api("POST", "/repos/labowner/conflict/contents/changes.txt", map[string]string{"branch": "main", "message": "Conflicting fixture", "content": "b3RoZXIK"}, nil, 201)
	approved := seedPull("conflict", "followup", "[Topic: CAD-01] Second fixture", "")
	seedPull("no-match", "other", "[Topic: CAD-010] Other fixture", "")
	seedPull("hidden", "secret", "[Topic: CAD-01] Hidden fixture", "")
	client, err := NewGitea(base, base, readerToken, 1)
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(pageLimit int) []RepositoryResult {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		results, err := Lookup(ctx, client, repos, "CAD-01", pageLimit)
		if err != nil {
			t.Fatal(err)
		}
		return results
	}
	results := lookup(1)
	if len(results) != 3 || results[1].Repository.Name != "conflict" || results[1].Status != "incomplete" || len(results[1].PullRequests) != 1 {
		t.Fatalf("bounded lookup: %#v", results)
	}
	results = lookup(3)
	if len(results) != 3 || len(results[0].PullRequests) != 1 || results[0].PullRequests[0].Lifecycle != "merged" ||
		results[1].Status != "complete" || len(results[1].PullRequests) != 2 || results[1].PullRequests[0].Lifecycle != "open" ||
		results[1].PullRequests[0].HeadSHA == "" || results[2].Status != "complete" || len(results[2].PullRequests) != 0 {
		t.Fatalf("full lookup: %#v", results)
	}
	for _, pull := range results[1].PullRequests {
		state := "failure"
		if pull.Number == approved {
			state = "success"
		}
		api("POST", "/repos/labowner/conflict/statuses/"+pull.HeadSHA, map[string]string{"state": state, "context": "fixture/check"}, nil, 201)
	}
	api("PUT", "/repos/labowner/conflict/collaborators/labreviewer", map[string]string{"permission": "write"}, nil, 204)
	labAPI(t, base, reviewerToken, "POST", fmt.Sprintf("/repos/labowner/conflict/pulls/%d/reviews", approved), map[string]string{"event": "APPROVED", "body": "Synthetic verified review"}, nil, 200)
	browser, bridgeURL := loginLab(t, base, ownerToken, "labreader", readerPassword, repos)
	lookupURL := bridgeURL + "/bridge/api/topics/lookup?project_id=lab&topic=CAD-01"
	data := loadLabPage(t, browser, lookupURL, nil, 200)
	if !strings.Contains(data, `"name":"conflict"`) || !strings.Contains(data, `"lifecycle":"merged"`) || strings.Contains(data, `"name":"hidden"`) {
		t.Fatal("OAuth lookup did not use the reader's repository visibility")
	}
	var detailed struct{ Repositories []RepositoryResult }
	if err := json.Unmarshal([]byte(data), &detailed); err != nil {
		t.Fatal(err)
	}
	checks := make(map[int64]PullRequest)
	for _, repo := range detailed.Repositories {
		if repo.Repository.Name == "conflict" {
			for _, pull := range repo.PullRequests {
				checks[pull.Number] = pull
			}
		}
	}
	valid := checks[approved]
	if valid.CI != "success" || valid.ChangedFiles == nil || *valid.ChangedFiles != 1 || valid.Conflict != "none_reported" || valid.Reviews == nil || valid.Reviews.Approved != 1 {
		t.Fatalf("clean reviewed PR did not expose verified checks: %+v", valid)
	}
	if blocked := checks[conflicting]; blocked.CI != "failure" || blocked.Conflict != "not_mergeable_reason_unknown" {
		t.Fatalf("conflicting fixture was not distinguished from clean PR: %+v", blocked)
	}
	saveURL := bridgeURL + "/bridge/api/topics"
	selection := fmt.Sprintf(`{"project_id":"lab","topic":"CAD-01","members":[{"repository_id":%d,"source_branch":"changes","target_branch":"main"}]}`, repos[1].ID)
	postLabTopic(t, browser, saveURL, "https://untrusted.invalid", "cad-fictional", selection, 403)
	postLabTopic(t, browser, saveURL, bridgeURL, "whole-group", `{"project_id":"lab","topic":"CAD-01"}`, 404)
	saved := postLabTopic(t, browser, saveURL, bridgeURL, "cad-fixture", selection, 201)
	retried := postLabTopic(t, browser, saveURL, bridgeURL, "cad-fixture", selection, 200)
	if saved.ID == "" || saved.ID != retried.ID || saved.Version != 1 || len(saved.Members) != 1 || saved.Members[0].RepositoryID != repos[1].ID || saved.Members[0].SourceBranch != "changes" {
		t.Fatalf("unexpected saved topic: %+v / %+v", saved, retried)
	}
	postLabTopic(t, browser, saveURL, bridgeURL, "cad-fixture", strings.Replace(selection, "CAD-01", "CAD-02", 1), 409)
	savedURL := saveURL + "/" + saved.ID
	if data := loadLabPage(t, browser, savedURL, nil, 200); !strings.Contains(data, `"name":"conflict"`) || strings.Contains(data, `"name":"merged"`) {
		t.Fatal("saved topic did not preserve the specific repository selection")
	}
	readerAttempt := postLabTopic(t, browser, savedURL+"/pulls", bridgeURL, "", fmt.Sprintf(`{"confirm":true,"member_ids":[%d]}`, saved.Members[0].ID), 200)
	if readerAttempt.Members[0].State != "pending" {
		t.Fatal("read-only user changed the saved operation")
	}
	api("PUT", "/repos/labowner/no-match/collaborators/labreviewer", map[string]string{"permission": "write"}, nil, 204)
	api("PUT", "/repos/labowner/merged/collaborators/labreviewer", map[string]string{"permission": "write"}, nil, 204)
	seedBranch("no-match", "bridge-source")
	writer, writerURL := loginLab(t, base, ownerToken, "labreviewer", reviewerPassword, repos)
	writeSelection := fmt.Sprintf(`{"project_id":"lab","topic":"CAD-01","members":[
{"repository_id":%d,"source_branch":"missing","target_branch":"main"},
{"repository_id":%d,"source_branch":"changes","target_branch":"main"},
{"repository_id":%d,"source_branch":"bridge-source","target_branch":"main"}]}`, repos[0].ID, repos[1].ID, repos[2].ID)
	batchTopic := postLabTopic(t, writer, writerURL+"/bridge/api/topics", writerURL, "batch", writeSelection, 201)
	batchURL := writerURL + "/bridge/api/topics/" + batchTopic.ID + "/pulls"
	confirmation := fmt.Sprintf(`{"confirm":true,"member_ids":[%d,%d,%d]}`, batchTopic.Members[0].ID, batchTopic.Members[1].ID, batchTopic.Members[2].ID)
	postLabTopic(t, writer, batchURL, writerURL, "", strings.Replace(confirmation, "true", "false", 1), 400)
	batch := postLabTopic(t, writer, batchURL, writerURL, "", confirmation, 200)
	if batch.Members[0].ResultCode != "gitea_rejected" || batch.Members[1].PullNumber != conflicting || batch.Members[1].ResultCode != "existing" || batch.Members[2].ResultCode != "created" || batch.Members[2].PullNumber <= 0 {
		t.Fatalf("expected independent rejected, reused and created results: %+v", batch.Members)
	}
	repeatBatch := postLabTopic(t, writer, batchURL, writerURL, "", confirmation, 200)
	if repeatBatch.Members[2].PullNumber != batch.Members[2].PullNumber || repeatBatch.Members[2].ResultCode != "created" {
		t.Fatal("a retry changed an already successful repository")
	}
	seedBranch("merged", "missing")
	batch = postLabTopic(t, writer, batchURL, writerURL, "", confirmation, 200)
	for _, name := range []string{"merged", "conflict", "no-match"} {
		rule := map[string]any{
			"rule_name": "main", "enable_push": false, "required_approvals": 1,
			"enable_approvals_whitelist": true, "approvals_whitelist_username": []string{"labowner"},
			"block_admin_merge_override": true,
		}
		if name == "no-match" {
			rule["enable_status_check"], rule["status_check_contexts"] = true, []string{"fixture/required"}
		}
		api("POST", "/repos/labowner/"+name+"/branch_protections", rule, nil, 201)
	}
	api("POST", "/repos/labowner/no-match/contents/direct.txt", map[string]string{
		"branch": "main", "message": "Synthetic protected write", "content": "dGVzdAo=",
	}, nil, 403)
	type labMergeTarget struct {
		Number    int64
		Head      struct{ SHA string }
		Mergeable bool
		Merged    bool
		MergeSHA  string             `json:"merge_commit_sha"`
		MergedBy  struct{ ID int64 } `json:"merged_by"`
	}
	readMergeTarget := func(repo Repository, member TopicMember, ready bool) labMergeTarget {
		t.Helper()
		endpoint := fmt.Sprintf("/repos/labowner/%s/pulls/%d", repo.Name, member.PullNumber)
		deadline := time.Now().Add(15 * time.Second)
		for {
			var detail labMergeTarget
			api("GET", endpoint, nil, &detail, 200)
			if !ready || detail.Mergeable || time.Now().After(deadline) {
				return detail
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	mergeURL := func(member TopicMember) string {
		return fmt.Sprintf("%s/bridge/api/topics/%s/members/%d/merge", writerURL, batch.ID, member.ID)
	}
	confirmMerge := func(head string) string {
		return fmt.Sprintf(`{"confirm":true,"expected_head":%q}`, head)
	}
	first := readMergeTarget(repos[0], batch.Members[0], true)
	second := readMergeTarget(repos[2], batch.Members[2], true)
	if !first.Mergeable || !second.Mergeable {
		t.Fatal("Gitea did not finish checks for clean PRs")
	}
	postLabTopic(t, writer, mergeURL(batch.Members[2]), writerURL, "", confirmMerge(strings.Repeat("a", 40)), 409)
	missingApproval := postLabTopic(t, writer, mergeURL(batch.Members[2]), writerURL, "", confirmMerge(second.Head.SHA), 200)
	if missingApproval.Members[2].Merge == nil || missingApproval.Members[2].Merge.ResultCode != "approval_missing" {
		var policy map[string]any
		labAPI(t, base, reviewerToken, "GET", "/repos/labowner/no-match/branches/main", nil, &policy, 200)
		delete(policy, "commit")
		t.Fatalf("unexpected merge gate: %+v; target branch metadata: %+v", missingApproval.Members[2].Merge, policy)
	}
	for _, index := range []int{0, 2} {
		api("POST", fmt.Sprintf("/repos/labowner/%s/pulls/%d/reviews", repos[index].Name, batch.Members[index].PullNumber),
			map[string]string{"event": "APPROVED", "body": "Synthetic approval"}, nil, 200)
	}
	api("PUT", "/repos/labowner/no-match/collaborators/labreviewer", map[string]string{"permission": "read"}, nil, 204)
	postLabTopic(t, writer, mergeURL(batch.Members[2]), writerURL, "", confirmMerge(second.Head.SHA), 403)
	api("PUT", "/repos/labowner/no-match/collaborators/labreviewer", map[string]string{"permission": "write"}, nil, 204)
	postLabTopic(t, writer, mergeURL(batch.Members[2]), "https://untrusted.invalid", "", confirmMerge(second.Head.SHA), 403)
	statusBlocked := postLabTopic(t, writer, mergeURL(batch.Members[2]), writerURL, "", confirmMerge(second.Head.SHA), 200)
	if statusBlocked.Members[2].Merge == nil || statusBlocked.Members[2].Merge.ResultCode != "gitea_rejected_merge" ||
		readMergeTarget(repos[2], batch.Members[2], false).Merged {
		t.Fatalf("Gitea did not enforce the required status check: %+v", statusBlocked.Members[2].Merge)
	}
	api("POST", "/repos/labowner/no-match/statuses/"+second.Head.SHA, map[string]string{"state": "success", "context": "fixture/required"}, nil, 201)
	postLabTopic(t, writer, mergeURL(batch.Members[0]), writerURL, "", confirmMerge(first.Head.SHA), 200)
	conflictPull := readMergeTarget(repos[1], batch.Members[1], false)
	postLabTopic(t, writer, mergeURL(batch.Members[1]), writerURL, "", confirmMerge(conflictPull.Head.SHA), 200)
	mergedBatch := postLabTopic(t, writer, mergeURL(batch.Members[2]), writerURL, "", confirmMerge(second.Head.SHA), 200)
	for _, index := range []int{0, 2} {
		actual := readMergeTarget(repos[index], batch.Members[index], false)
		evidence := mergedBatch.Members[index].Merge
		if !actual.Merged || evidence == nil || evidence.State != "merged" || evidence.MergeSHA != actual.MergeSHA ||
			evidence.MergedBy != actual.MergedBy.ID || evidence.MergedBy != evidence.RequestedBy || evidence.MergedAt == nil {
			t.Fatalf("merged evidence for repo %s did not match Gitea: %+v", repos[index].Name, evidence)
		}
	}
	remaining := mergedBatch.Members[1].Merge
	if remaining == nil || remaining.State != "blocked" || remaining.ResultCode != "pull_not_ready" || readMergeTarget(repos[1], batch.Members[1], false).Merged {
		t.Fatalf("conflicting repository did not stay blocked: %+v", remaining)
	}
	verifyReceiptLab(t, base, ownerToken, writer, writerURL, batch.ID, repos, release.ID, actualMerge.SHA, mergedBatch.Members[2].Merge.MergeSHA)
	revertURL := func(member TopicMember) string {
		return fmt.Sprintf("%s/bridge/api/topics/%s/members/%d/revert-pr", writerURL, batch.ID, member.ID)
	}
	revertBody := func(member TopicMember) string {
		return fmt.Sprintf(`{"confirm":true,"expected_merge":%q}`, member.Merge.MergeSHA)
	}
	planned := mergedBatch.Members[0]
	api("PATCH", "/repos/labowner/merged/branch_protections/main", map[string]bool{"enable_push": true}, nil, 200)
	api("POST", "/repos/labowner/merged/contents/later.txt", map[string]string{
		"branch": "main", "message": "Advance the branch after merging", "content": "dGVzdAo=",
	}, nil, 201)
	api("PATCH", "/repos/labowner/merged/branch_protections/main", map[string]bool{"enable_push": false}, nil, 200)
	postLabTopic(t, writer, revertURL(planned), writerURL, "", revertBody(planned), 409)
	advancedBranch, _ := revertIdentity(repos[0], planned)
	api("GET", "/repos/labowner/merged/branches/"+advancedBranch, nil, nil, 404)

	planned = mergedBatch.Members[2]
	postLabTopic(t, writer, revertURL(planned), "https://untrusted.invalid", "", revertBody(planned), 403)
	opened := postLabTopic(t, writer, revertURL(planned), writerURL, "", revertBody(planned), 200)
	revert := opened.Members[2].Revert
	if revert == nil || revert.State != "opened" || revert.PullNumber < 1 || revert.InverseSHA == "" {
		t.Fatalf("reverse PR not created: %+v", revert)
	}
	repeated := postLabTopic(t, writer, revertURL(planned), writerURL, "", revertBody(planned), 200)
	if repeated.Members[2].Revert.PullNumber != revert.PullNumber || repeated.Version != opened.Version {
		t.Fatal("reverse PR confirmation was not idempotent")
	}
	var target revertBranch
	api("GET", "/repos/labowner/no-match/branches/main", nil, &target, 200)
	if target.Commit.ID != planned.Merge.MergeSHA {
		t.Fatal("reverse PR changed the protected branch")
	}
	inverseTarget := readMergeTarget(repos[2], TopicMember{PullNumber: revert.PullNumber}, true)
	if inverseTarget.Head.SHA != revert.InverseSHA || !inverseTarget.Mergeable || inverseTarget.Merged {
		t.Fatalf("unexpected reverse PR state: %+v", inverseTarget)
	}
	inverseMergePath := fmt.Sprintf("/repos/labowner/no-match/pulls/%d/merge", revert.PullNumber)
	api("POST", "/repos/labowner/no-match/statuses/"+revert.InverseSHA, map[string]string{"state": "success", "context": "fixture/required"}, nil, 201)
	labAPI(t, base, reviewerToken, "POST", inverseMergePath, map[string]string{"do": "merge", "head_commit_id": revert.InverseSHA}, nil, 405)
	api("GET", "/repos/labowner/no-match/branches/main", nil, &target, 200)
	if target.Commit.ID != planned.Merge.MergeSHA {
		t.Fatal("reverse PR could update main without approval")
	}
	api("POST", fmt.Sprintf("/repos/labowner/no-match/pulls/%d/reviews", revert.PullNumber),
		map[string]string{"event": "APPROVED", "body": "Independent approval for reverse PR"}, nil, 200)
	labAPI(t, base, reviewerToken, "POST", inverseMergePath, map[string]string{"do": "merge", "head_commit_id": revert.InverseSHA}, nil, 200)
	api("GET", "/repos/labowner/no-match/contents/bridge-source.txt?ref=main", nil, nil, 404)
	api("DELETE", "/repos/labowner/conflict/collaborators/labreader", nil, nil, 204)
	results = lookup(3)
	if len(results) != 2 || results[1].Repository.Name != "no-match" {
		t.Fatalf("lookup after revocation: %#v", results)
	}
	if strings.Contains(loadLabPage(t, browser, lookupURL, nil, 200), `"name":"conflict"`) {
		t.Fatal("OAuth lookup returned a repository after access was revoked")
	}
	if data := loadLabPage(t, browser, savedURL, nil, 404); data != loadLabPage(t, browser, saveURL+"/"+strings.Repeat("A", 43), nil, 404) {
		t.Fatal("a revoked topic was distinguishable from an unknown topic")
	}
	postLabTopic(t, browser, saveURL, bridgeURL, "cad-fixture", selection, 404)
	logout, err := http.NewRequest("POST", bridgeURL+"/bridge/auth/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	logout.Header.Set("Origin", bridgeURL)
	logoutResponse, err := browser.Do(logout)
	if err != nil {
		t.Fatal(err)
	}
	logoutResponse.Body.Close()
	if logoutResponse.StatusCode != 204 {
		t.Fatalf("logout returned %d", logoutResponse.StatusCode)
	}
	loadLabPage(t, browser, lookupURL, nil, 401)
	checkHookLab(t, hookStore, "pull_request", repos[0].ID, merged, actualMerge.SHA, "")
	checkHookLab(t, hookStore, "release", repos[0].ID, release.ID, actualMerge.SHA, "build/v1")
}

func checkHookLab(t *testing.T, store *TopicStore, kind string, repoID, objectID int64, sha, tag string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var snapshot string
		err := store.db.QueryRow(`
SELECT observation FROM webhook_inbox
WHERE kind=? AND repo_id=? AND object_id=? AND json_extract(observation, '$.commit_sha')=? AND
json_extract(observation, '$.status') IN ('merged', 'published') LIMIT 1`, kind, repoID, objectID, sha).Scan(&snapshot)
		if err == nil {
			var result sourceObservation
			if json.Unmarshal([]byte(snapshot), &result) != nil || result.ActorID <= 0 || result.OccurredAt == nil || result.Tag != tag || result.Repository.ID != repoID {
				t.Fatalf("invalid %s evidence", kind)
			}
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	var jobs int
	var failure string
	store.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(last_error), '') FROM webhook_inbox WHERE kind=?`, kind).Scan(&jobs, &failure)
	t.Fatalf("Gitea %s callback not verified: %d queued; last error %q", kind, jobs, failure)
}

func loginLab(t *testing.T, giteaURL, ownerToken, username, password string, repos []Repository) (*http.Client, string) {
	t.Helper()
	store, err := OpenTopicStore(context.Background(), filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	app := httptest.NewUnstartedServer(nil)
	bridgeURL := "http://" + app.Listener.Addr().String()
	var anchorPublic string
	var receipts *ReceiptConfig
	if username == "labreviewer" {
		artifactRoot := t.TempDir()
		repoFolder := filepath.Join(artifactRoot, fmt.Sprint(repos[2].ID))
		if err := os.Mkdir(repoFolder, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repoFolder, "bundle.txt"), []byte("fixture delivery\n"), 0600); err != nil {
			t.Fatal(err)
		}
		var owner struct{ ID int64 }
		labAPI(t, giteaURL, ownerToken, "GET", "/users/labreviewer", nil, &owner, 200)
		receipts = &ReceiptConfig{ArtifactRoot: artifactRoot, SnapshotRoot: t.TempDir(), Issuers: []int64{owner.ID},
			SigningSeed: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{17}, 32))}
		anchorBearer, err := randomID()
		if err != nil {
			t.Fatal(err)
		}
		ledger, err := OpenAnchorLedger(context.Background(), filepath.Join(t.TempDir(), "anchor.db"), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{23}, 32)), anchorBearer)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ledger.Close() })
		anchorServer := httptest.NewServer(ledger.Handler())
		t.Cleanup(anchorServer.Close)
		anchorPublic = ledger.PublicKey()
		receipts.Anchor = &ReceiptAnchorConfig{URL: anchorServer.URL + "/v1/anchors", PublicKey: anchorPublic, Bearer: anchorBearer}
		t.Setenv("BRIDGE_TEST_TRUSTED_ANCHOR_KEY", anchorPublic)
	}
	var creds struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	labAPI(t, giteaURL, ownerToken, "POST", "/user/applications/oauth2", map[string]any{
		"name": "Topic Bridge Lab", "confidential_client": true, "redirect_uris": []string{bridgeURL + "/bridge/auth/callback"},
	}, &creds, 201)
	config := ServerConfig{PublicBaseURL: bridgeURL + "/bridge/", GiteaPublicURL: giteaURL, GiteaAPIURL: giteaURL,
		ClientID: creds.ID, ClientSecret: creds.Secret, AllowLoopbackHTTP: true, PageSize: 1, MaxPages: 3,
		TopicStore: store, EnablePullCreation: true, EnableMerge: true, EnableRevert: true,
		Receipts: receipts,
		Projects: []Project{{ID: "lab", Name: "Lab", Repositories: repos}, {ID: "secret", Name: "Secret", Repositories: repos[3:]}},
	}
	handler, err := NewServer(config)
	if err != nil {
		app.Listener.Close()
		t.Fatal(err)
	}
	app.Config.Handler = handler
	app.Start()
	t.Cleanup(app.Close)
	if anchorPublic != "" {
		worker, err := NewReceiptAnchorWorker(config)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); worker.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	loginURL := giteaURL + "/user/login"
	page := loadLabPage(t, browser, loginURL, nil, 200)
	loginForm := url.Values{"user_name": {username}, "password": {password}}
	addLabCSRF(page, loginForm)
	loadLabPage(t, browser, loginURL, loginForm, 200)
	consent := loadLabPage(t, browser, bridgeURL+"/bridge/auth/login?topic=CAD-01&project_id=lab", nil, 200)
	grantForm := regexp.MustCompile(`(?s)<form[^>]*action="/login/oauth/grant"[^>]*>(.*?)</form>`).FindStringSubmatch(consent)
	if len(grantForm) != 2 {
		t.Fatal("Gitea did not show OAuth consent")
	}
	values := url.Values{"granted": {"true"}}
	for _, input := range regexp.MustCompile(`<input\b[^>]*>`).FindAllString(grantForm[1], -1) {
		attributes := make(map[string]string)
		for _, attribute := range regexp.MustCompile(`([a-z_-]+)="([^"]*)"`).FindAllStringSubmatch(input, -1) {
			attributes[attribute[1]] = html.UnescapeString(attribute[2])
		}
		if attributes["type"] == "hidden" && attributes["name"] != "" {
			values.Add(attributes["name"], attributes["value"])
		}
	}
	addLabCSRF(consent, values)
	if dashboard := loadLabPage(t, browser, giteaURL+"/login/oauth/grant", values, 200); !strings.Contains(dashboard, "/bridge/app.js") {
		t.Fatal("OAuth consent did not return to the Bridge page")
	}
	if projects := loadLabPage(t, browser, bridgeURL+"/bridge/api/projects", nil, 200); !strings.Contains(projects, fmt.Sprintf(`"user":%q`, username)) || strings.Contains(projects, "Secret") {
		t.Fatal("OAuth session used the wrong user or exposed a hidden project")
	}
	return browser, bridgeURL
}

func postLabTopic(t *testing.T, browser *http.Client, address, origin, key, body string, status int) TopicChangeSet {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, address, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("Origin", origin)
	response, err := browser.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 200))
		t.Fatalf("create saved topic: wanted %d, got %d: %s", status, response.StatusCode, data)
	}
	var topic TopicChangeSet
	if status < 300 {
		if err := json.NewDecoder(response.Body).Decode(&topic); err != nil {
			t.Fatal(err)
		}
	}
	return topic
}

func addLabCSRF(page string, form url.Values) {
	if token := regexp.MustCompile(`"csrfToken"\s*:\s*"([^"]+)"`).FindStringSubmatch(page); form.Get("_csrf") == "" && len(token) == 2 {
		form.Set("_csrf", token[1])
	}
}

func loadLabPage(t *testing.T, browser *http.Client, address string, form url.Values, status int) string {
	t.Helper()
	method, body := http.MethodGet, ""
	if form != nil {
		method, body = http.MethodPost, form.Encode()
	}
	request, err := http.NewRequest(method, address, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", request.URL.Scheme+"://"+request.URL.Host)
		request.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	response, err := browser.Do(request)
	if err != nil {
		t.Fatalf("lab web request failed on %s", request.URL.Path)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		t.Fatalf("%s %s redirected to %s: expected %d, got %d", method, request.URL.Path, response.Request.URL.Path, status, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func startLocalGitea(t *testing.T, binary, webhookAddress string) (string, func(...string) *exec.Cmd) {
	t.Helper()
	root := t.TempDir()
	override := filepath.Join(root, "custom", "templates", "custom")
	if err := os.MkdirAll(override, 0700); err != nil {
		t.Fatal(err)
	}
	linkTemplate, err := os.ReadFile("examples/gitea/extra_links.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(override, "extra_links.tmpl"), linkTemplate, 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	config := filepath.Join(root, "app.ini")
	settings := fmt.Sprintf(`[server]
HTTP_ADDR = 127.0.0.1
HTTP_PORT = %d
ROOT_URL = %s/
DISABLE_SSH = true
OFFLINE_MODE = true
APP_DATA_PATH = %s/data
[database]
DB_TYPE = sqlite3
PATH = %s/data/gitea.db
[repository]
ROOT = %s/repos
[security]
INSTALL_LOCK = true
ALLOWED_HOST_LIST = %s
[service]
DISABLE_REGISTRATION = true
[actions]
ENABLED = false
[log]
MODE = console
LEVEL = warn
`, port, base, filepath.ToSlash(root), filepath.ToSlash(root), filepath.ToSlash(root), webhookAddress)
	if err := os.WriteFile(config, []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) *exec.Cmd {
		cmd := exec.Command(binary, append([]string{"--work-path", root, "--config", config}, args...)...)
		cmd.Dir = root
		for _, variable := range os.Environ() {
			if !strings.HasPrefix(strings.ToUpper(variable), "GITEA") {
				cmd.Env = append(cmd.Env, variable)
			}
		}
		return cmd
	}
	output, err := os.Create(filepath.Join(root, "gitea.log"))
	if err != nil {
		t.Fatal(err)
	}
	server := command("web")
	server.Stdout, server.Stderr = output, output
	if err := server.Start(); err != nil {
		output.Close()
		t.Fatal(err)
	}
	output.Close()
	t.Cleanup(func() { server.Process.Kill(); server.Wait() })
	httpClient := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		response, err := httpClient.Get(base + "/api/v1/version")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return base, command
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	logOutput, _ := os.ReadFile(filepath.Join(root, "gitea.log"))
	t.Fatalf("local Gitea did not become ready: %s", logOutput)
	return "", nil
}

func labAPI(t *testing.T, base, token, method, path string, data, out any, status int) {
	t.Helper()
	payload, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, base+"/api/v1"+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1000))
		t.Fatalf("%s %s: wanted %d, got %d: %s", method, path, status, response.StatusCode, body)
	}
	if out != nil {
		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}
