package bridge

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func verifySCMPilot(t *testing.T, browser *http.Client, fixture scmStackFixture) PullRequest {
	t.Helper()
	api, err := url.Parse(fixture.GiteaAPI)
	if err != nil || api.Scheme != "http" || api.Hostname() != "127.0.0.1" || api.User != nil ||
		fixture.PilotID <= 0 || fixture.OwnerToken == "" || fixture.OwnerPassword == "" || fixture.DeveloperToken == "" {
		t.Fatal("SCM pilot must use an isolated internal Gitea API and fixture credentials")
	}
	source, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	git := func(dir, user, password string, args ...string) string {
		t.Helper()
		result, err := fixture.git(t, dir, user, password, args...)
		if err != nil {
			t.Fatalf("Git %s in the SCM pilot: %v\n%s", args[0], err, result)
		}
		return strings.TrimSpace(strings.ReplaceAll(result, "\r\n", "\n"))
	}
	sourceGit := func(dir string, args ...string) string { return git(dir, "labowner", fixture.OwnerPassword, args...) }
	const defaultBranch = "refs/remotes/origin/main"
	head := sourceGit(source, "rev-parse", "--verify", defaultBranch)
	history := sourceGit(source, "rev-list", "--count", defaultBranch)
	if count, err := strconv.Atoi(history); !commitSHA.MatchString(head) || err != nil || count < 2 {
		t.Fatal("the pilot source must have the full public main history")
	}
	before := sourceGit(source, "show-ref", "--head")
	staging := filepath.Join(t.TempDir(), "staging.git")
	sourceGit(source, "clone", "--mirror", "--no-local", source, staging)
	remote := fixture.Gateway + "/labowner/pilot.git"
	push := []string{"push", "--atomic", remote, defaultBranch + ":refs/heads/main"}
	expected := []string{head + "\trefs/heads/main"}
	tags := sourceGit(staging, "for-each-ref", "--format=%(objectname)%09%(refname)", "refs/tags/")
	if tags != "" {
		push = append(push, "refs/tags/*:refs/tags/*")
		expected = append(expected, strings.Split(tags, "\n")...)
	}
	sourceGit(staging, push...)
	actual := strings.Split(sourceGit(staging, "ls-remote", "--refs", remote, "refs/heads/*", "refs/tags/*"), "\n")
	slices.Sort(expected)
	slices.Sort(actual)
	if !slices.Equal(expected, actual) {
		t.Fatal("the pilot branch and tag object IDs did not match the source")
	}
	clone := t.TempDir()
	git(clone, "labreader", fixture.Password, "clone", "--branch", "main", remote, ".")
	if count := git(clone, "labreader", fixture.Password, "rev-list", "--count", "HEAD"); count != history {
		t.Fatal("the read-only developer clone did not contain the complete source history")
	}
	const path = "/repos/labowner/pilot"
	labAPI(t, fixture.GiteaAPI, fixture.OwnerToken, "POST", path+"/branch_protections", map[string]any{
		"rule_name": "main", "enable_push": false, "required_approvals": 1,
		"enable_approvals_whitelist": true, "approvals_whitelist_username": []string{"labowner"}, "block_admin_merge_override": true,
	}, nil, 201)
	labAPI(t, fixture.GiteaAPI, fixture.DeveloperToken, "POST", path+"/branches", map[string]string{
		"old_branch_name": "main", "new_branch_name": "pilot-check",
	}, nil, 201)
	labAPI(t, fixture.GiteaAPI, fixture.DeveloperToken, "POST", path+"/contents/scm-migration-fixture.txt", map[string]string{
		"branch": "pilot-check", "message": "Trial after public-history import",
		"content": base64.StdEncoding.EncodeToString([]byte("SCM migration verification\n")),
	}, nil, 201)
	var pull struct {
		Number int64
		Head   apiPullBranch
	}
	labAPI(t, fixture.GiteaAPI, fixture.DeveloperToken, "POST", path+"/pulls", map[string]string{
		"title": "[Topic: CAD-PILOT] Verify imported history", "base": "main", "head": "pilot-check",
	}, &pull, 201)
	match := lookupSCMPilot(t, browser, fixture)
	if match.Number != pull.Number || !commitSHA.MatchString(match.HeadSHA) || match.HeadSHA != pull.Head.SHA {
		t.Fatal("the Bridge pilot PR did not match Gitea")
	}
	if after := sourceGit(source, "show-ref", "--head"); after != before {
		t.Fatal("the original checkout's refs changed during the pilot")
	}
	t.Logf("Imported public main %s (%s commits, %d tags); private HTTPS clone and PR #%d verified, source refs unchanged.",
		head, history, len(expected)-1, pull.Number)
	return match
}

func lookupSCMPilot(t *testing.T, browser *http.Client, fixture scmStackFixture) PullRequest {
	t.Helper()
	raw := loadLabPage(t, browser, fixture.Gateway+"/bridge/api/topics/lookup?project_id=pilot&topic=CAD-PILOT", nil, 200)
	var result struct {
		Repositories []RepositoryResult
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || len(result.Repositories) != 1 ||
		result.Repositories[0].Repository.ID != fixture.PilotID || len(result.Repositories[0].PullRequests) != 1 {
		t.Fatalf("the read-only Bridge pilot lookup did not return the imported repo and PR: %v", err)
	}
	return result.Repositories[0].PullRequests[0]
}
