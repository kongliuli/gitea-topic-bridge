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
	developer, developerPassword := createLabUser(t, command, "labdeveloper", "write:repository,read:user")
	const repoPath = "/repos/labowner/pilot"
	var repo struct{ ID int64 }
	labAPI(t, base, admin, "POST", "/user/repos", map[string]any{
		"name": "pilot", "private": true, "default_branch": "main",
	}, &repo, 201)
	labAPI(t, base, admin, "PUT", repoPath+"/collaborators/labreader", map[string]string{"permission": "read"}, nil, 204)
	labAPI(t, base, admin, "PUT", repoPath+"/collaborators/labdeveloper", map[string]string{"permission": "write"}, nil, 204)
	runGit := migrationGit(t, base, "labowner", password)

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
	labAPI(t, base, developer, "POST", repoPath+"/branches", map[string]string{
		"old_branch_name": "main", "new_branch_name": "pilot-check",
	}, nil, 201)
	labAPI(t, base, developer, "POST", repoPath+"/contents/pilot.txt", map[string]string{
		"branch": "pilot-check", "message": "Pilot smoke fixture", "content": base64.StdEncoding.EncodeToString([]byte("bridge trial\n")),
	}, nil, 201)
	var created struct {
		Number int64
		Head   apiPullBranch
	}
	labAPI(t, base, developer, "POST", repoPath+"/pulls", map[string]string{
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

	t.Run("staged cutover and rollback", func(t *testing.T) {
		verifyMigrationCutover(t, base, admin, password, developer, developerPassword)
	})
}

func migrationGit(t *testing.T, base, user, password string) func(string, ...string) string {
	t.Helper()
	return func(dir string, args ...string) string {
		t.Helper()
		output, err := labGit(t, dir, base, user, password, args...)
		if err != nil {
			t.Fatalf("Git %s as %s in the migration lab: %v\n%s", args[0], user, err, output)
		}
		return strings.TrimSpace(output)
	}
}

func verifyMigrationCutover(t *testing.T, base, admin, ownerPassword, developer, developerPassword string) {
	t.Helper()
	runGit := migrationGit(t, base, "labowner", ownerPassword)
	writerGit := migrationGit(t, base, "labdeveloper", developerPassword)
	const mainRef = "refs/heads/main"
	const acceptedRef = "refs/heads/after-cutover"
	root := t.TempDir()
	refs := func(remote string) []string {
		output := runGit(root, "ls-remote", "--refs", remote, "refs/heads/*", "refs/tags/*")
		rows := strings.FieldsFunc(output, func(r rune) bool { return r == '\r' || r == '\n' })
		slices.Sort(rows)
		return rows
	}
	commitFile := func(git func(string, ...string) string, dir, name, contents string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		git(dir, "add", name)
		git(dir, "-c", "user.name=Migration Fixture", "-c", "user.email=migration@example.invalid", "commit", "-m", contents)
		return git(dir, "rev-parse", "HEAD")
	}

	seed := t.TempDir()
	runGit(seed, "init", "--initial-branch=main")
	commitFile(runGit, seed, "original.txt", "Seed the follow-up repository")
	runGit(seed, "branch", "maintenance")
	runGit(seed, "-c", "user.name=Migration Fixture", "-c", "user.email=migration@example.invalid", "tag", "-a", "legacy-v1", "-m", "Original tag")
	original := filepath.Join(root, "source.git")
	staging := filepath.Join(root, "staging.git")
	consumer := t.TempDir()
	runGit(root, "clone", "--bare", seed, original)
	runGit(root, "clone", "--mirror", original, staging)
	writerGit(consumer, "clone", "--branch", "main", original, ".")

	lastOldHead := commitFile(runGit, seed, "last-old.txt", "Accept a change before the freeze")
	runGit(seed, "push", original, "HEAD:"+mainRef)
	frozen := refs(original)
	if len(frozen) != 3 || slices.Equal(frozen, refs(staging)) {
		t.Fatal("the cutover comparison did not detect the source commit added after staging")
	}
	hook := filepath.Join(original, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'migration freeze is active' >&2\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	message, err := labGit(t, seed, base, "labdeveloper", developerPassword, "push", original, "HEAD:refs/heads/freeze-probe")
	if err == nil || !strings.Contains(message, "migration freeze is active") || !slices.Equal(frozen, refs(original)) {
		t.Fatalf("the frozen source accepted a push or changed its refs: %v\n%s", err, message)
	}
	runGit(staging, "remote", "update")
	if !slices.Equal(frozen, refs(staging)) {
		t.Fatal("the refreshed staging refs did not match the frozen source")
	}

	const repoPath = "/repos/labowner/followup"
	destination := base + "/labowner/followup.git"
	var repo struct{ ID int64 }
	labAPI(t, base, admin, "POST", "/user/repos", map[string]any{"name": "followup", "private": true, "default_branch": "main"}, &repo, 201)
	runGit(staging, "push", "--atomic", destination, "refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*")
	if repo.ID < 1 || !slices.Equal(frozen, refs(destination)) {
		t.Fatal("the follow-up repository did not exactly match the frozen source")
	}
	labAPI(t, base, admin, "POST", repoPath+"/branch_protections", map[string]any{
		"rule_name": "main", "enable_push": false, "required_approvals": 1,
		"enable_approvals_whitelist": true, "approvals_whitelist_username": []string{"labowner"}, "block_admin_merge_override": true,
	}, nil, 201)
	labAPI(t, base, admin, "PUT", repoPath+"/collaborators/labdeveloper", map[string]string{"permission": "write"}, nil, 204)
	writerGit(consumer, "remote", "set-url", "origin", destination)
	writerGit(consumer, "pull", "--ff-only", "origin", "main")
	if writerGit(consumer, "rev-parse", "HEAD") != lastOldHead {
		t.Fatal("the switched checkout missed the last commit from the old repository")
	}
	writerGit(consumer, "switch", "-c", "after-cutover")
	accepted := commitFile(writerGit, consumer, "after.txt", "Accept new work after the cutover")
	writerGit(consumer, "push", "origin", "HEAD:"+acceptedRef)
	var branch struct{ Commit struct{ ID string } }
	labAPI(t, base, developer, "GET", repoPath+"/branches/after-cutover", nil, &branch, 200)
	if branch.Commit.ID != accepted {
		t.Fatal("the new Gitea remote did not acknowledge the post-cutover commit")
	}
	message, err = labGit(t, consumer, base, "labdeveloper", developerPassword, "push", "origin", "HEAD:"+mainRef)
	if err == nil || !strings.Contains(strings.ToLower(message), "protected branch") {
		t.Fatalf("Gitea did not reject the post-cutover push to protected main: %v\n%s", err, message)
	}

	targetRefs := refs(destination)
	archive := filepath.Join(root, "accepted.git")
	bundle := filepath.Join(root, "accepted.bundle")
	restored := filepath.Join(root, "bundle-check.git")
	runGit(root, "clone", "--mirror", destination, archive)
	runGit(archive, "bundle", "create", bundle, "--branches", "--tags", "HEAD")
	runGit(archive, "bundle", "verify", bundle)
	runGit(root, "clone", "--mirror", bundle, restored)
	runGit(restored, "fsck", "--full")
	if !slices.Equal(targetRefs, refs(restored)) || runGit(restored, "symbolic-ref", "HEAD") != mainRef ||
		runGit(restored, "rev-parse", mainRef) != lastOldHead || runGit(restored, "rev-parse", acceptedRef) != accepted {
		t.Fatal("the independent rollback bundle did not preserve the destination refs and default branch")
	}

	writerGit(consumer, "remote", "set-url", "origin", original)
	writerGit(consumer, "fetch", "--prune", "origin")
	if writerGit(consumer, "rev-parse", "origin/main") != lastOldHead ||
		!slices.Equal(frozen, refs(original)) || !slices.Equal(targetRefs, refs(destination)) {
		t.Fatal("rollback did not leave both the frozen source and the new server's accepted work intact")
	}
	runGit(original, "fsck", "--full")
	t.Logf("Follow-up cutover verified: Gitea repo ID %d, frozen main %s, post-cutover commit %s. labdeveloper fetched the original remote after a bundle restored every new branch and tag; both servers' refs were unchanged by rollback.",
		repo.ID, lastOldHead, accepted)
}
