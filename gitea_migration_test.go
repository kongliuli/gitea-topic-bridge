package bridge

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGiteaPilotMigrationLab(t *testing.T) {
	binary := os.Getenv("BRIDGE_TEST_GITEA_BINARY")
	if binary == "" {
		t.Skip("set BRIDGE_TEST_GITEA_BINARY for the isolated pilot import")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("Gitea binary path must be absolute")
	}
	base, command := startLocalGitea(t, binary, "")
	admin, password := createLabUser(t, command, "labowner", "all", "--admin")
	reader, _ := createLabUser(t, command, "labreader", "read:repository,read:user")
	const repoPath = "/repos/labowner/pilot"
	var repo struct{ ID int64 }
	labAPI(t, base, admin, "POST", "/user/repos", map[string]any{
		"name": "pilot", "private": true, "default_branch": "main",
	}, &repo, 201)
	labAPI(t, base, admin, "PUT", repoPath+"/collaborators/labreader", map[string]string{"permission": "read"}, nil, 204)
	runGit := func(dir string, args ...string) string {
		t.Helper()
		output, err := labGit(t, dir, base, "labowner", password, args...)
		if err != nil {
			t.Fatalf("Git %s in the pilot lab: %v\n%s", args[0], err, output)
		}
		return strings.TrimSpace(output)
	}

	work := t.TempDir()
	runGit(work, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(work, "original.txt"), []byte("legacy content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(work, "add", "original.txt")
	runGit(work, "-c", "user.name=Pilot Fixture", "-c", "user.email=pilot@example.invalid", "commit", "-m", "Seed legacy history")
	runGit(work, "branch", "maintenance/legacy")
	runGit(work, "-c", "user.name=Pilot Fixture", "-c", "user.email=pilot@example.invalid", "tag", "-a", "legacy-v1", "-m", "Original tag")
	root := t.TempDir()
	original := filepath.Join(root, "legacy.git")
	scratch := filepath.Join(root, "staging.git")
	runGit(root, "clone", "--bare", work, original)
	runGit(root, "clone", "--mirror", original, scratch)
	refs := func(dir string) []string {
		rows := strings.Split(strings.ReplaceAll(runGit(dir, "for-each-ref", "--format=%(objectname)%09%(refname)", "refs/heads/", "refs/tags/"), "\r\n", "\n"), "\n")
		slices.Sort(rows)
		return rows
	}
	before := refs(original)
	if len(before) != 3 || !slices.Equal(before, refs(scratch)) {
		t.Fatal("the staging repository is missing original branches or tag objects")
	}
	const repoURL = "/labowner/pilot.git"
	runGit(scratch, "push", "--atomic", base+repoURL, "refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*")
	remote := strings.Split(strings.ReplaceAll(runGit(scratch, "ls-remote", "--refs", base+repoURL, "refs/heads/*", "refs/tags/*"), "\r\n", "\n"), "\n")
	slices.Sort(remote)
	if !slices.Equal(before, remote) {
		t.Fatalf("imported refs differ from the legacy repository: %v vs %v", before, remote)
	}
	labAPI(t, base, admin, "POST", repoPath+"/branch_protections", map[string]any{
		"rule_name": "main", "enable_push": false, "required_approvals": 1,
		"enable_approvals_whitelist": true, "approvals_whitelist_username": []string{"labowner"}, "block_admin_merge_override": true,
	}, nil, 201)
	labAPI(t, base, admin, "POST", repoPath+"/branches", map[string]string{
		"old_branch_name": "main", "new_branch_name": "pilot-check",
	}, nil, 201)
	labAPI(t, base, admin, "POST", repoPath+"/contents/pilot.txt", map[string]string{
		"branch": "pilot-check", "message": "Pilot smoke fixture", "content": base64.StdEncoding.EncodeToString([]byte("bridge trial\n")),
	}, nil, 201)
	var created struct {
		Number int64
		Head   apiPullBranch
	}
	labAPI(t, base, admin, "POST", repoPath+"/pulls", map[string]string{
		"title": "[Topic: CAD-PILOT] Test imported repository", "base": "main", "head": "pilot-check",
	}, &created, 201)

	client, err := NewGitea(base, base, reader, 10)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repositories := []Repository{{ID: repo.ID, Owner: "labowner", Name: "pilot"}}
	results, err := Lookup(ctx, client, repositories, "CAD-PILOT", 2)
	if err != nil || len(results) != 1 || results[0].Status != "complete" || len(results[0].PullRequests) != 1 {
		t.Fatalf("Bridge could not query the pilot as a read-only user: %+v, %v", results, err)
	}
	pull := results[0].PullRequests[0]
	if pull.Number != created.Number || pull.HeadSHA != created.Head.SHA || !commitSHA.MatchString(pull.HeadSHA) {
		t.Fatalf("the imported repository's PR number or commit SHA did not match Gitea: %+v", pull)
	}
	if !slices.Equal(before, refs(original)) {
		t.Fatal("the legacy repository's branch or tag refs changed during the pilot")
	}
	runGit(original, "fsck", "--full")
	t.Logf("pilot verified: new repository ID %d, PR #%d at %s, %d unchanged legacy refs", repo.ID, pull.Number, pull.HeadSHA, len(before))
}
