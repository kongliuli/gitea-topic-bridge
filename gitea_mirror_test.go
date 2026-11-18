package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGiteaPullMirrorLab(t *testing.T) {
	binary := os.Getenv("BRIDGE_TEST_GITEA_BINARY")
	if binary == "" || os.Getenv("BRIDGE_TEST_PUBLIC_MIRROR") != "1" {
		t.Skip("set BRIDGE_TEST_GITEA_BINARY and BRIDGE_TEST_PUBLIC_MIRROR=1 for the public HTTPS pull mirror check")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("BRIDGE_TEST_GITEA_BINARY must be an absolute path")
	}
	base, command := startLocalGitea(t, binary, "", `[migrations]
EGRESS_MODE = strict
ALLOWED_HOST_LIST = github.com:443
`)
	admin, _ := createLabUser(t, command, "labowner", "all", "--admin")
	writer, password := createLabUser(t, command, "labwriter", "write:repository,read:user")
	const upstream = "https://github.com/octocat/Hello-World.git"
	const path = "/repos/labowner/upstream"
	var imported struct {
		ID     int64
		Mirror bool
		Branch string `json:"default_branch"`
	}
	labAPIWithTimeout(t, base, admin, "POST", "/repos/migrate", map[string]any{
		"clone_addr": upstream, "repo_name": "upstream", "service": "git", "mirror": true, "private": true,
	}, &imported, 201, 60*time.Second)
	if !imported.Mirror || imported.ID <= 0 || imported.Branch == "" {
		t.Fatal("import did not create a pull mirror with a default branch")
	}
	labAPI(t, base, admin, "PUT", path+"/collaborators/labwriter", map[string]string{"permission": "write"}, nil, 204)
	var access struct{ Permissions struct{ Push bool } }
	labAPI(t, base, writer, "GET", path, nil, &access, 200)
	if !access.Permissions.Push {
		t.Fatal("fixture account needs ordinary repository write access before testing mirror push restrictions")
	}
	lastSync := func() time.Time {
		t.Helper()
		var info struct {
			Last time.Time `json:"mirror_last_sync_at"`
		}
		labAPI(t, base, admin, "GET", path, nil, &info, 200)
		return info.Last
	}
	beforeSync := lastSync()
	client, err := NewGitea(base, base, writer, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	row, err := client.repositoryOverview(ctx, Repository{ID: imported.ID, Owner: "labowner", Name: "upstream"})
	if err != nil || row.Protection.Status != "not_applicable" || row.Commit == nil {
		t.Fatalf("mirror overview should include its commit and skip branch protection inspection: %+v, %v", row, err)
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		result, err := labGit(t, dir, base, "labwriter", password, args...)
		if err != nil {
			t.Fatalf("Git %s: %v\n%s", args[0], err, result)
		}
		return strings.TrimSpace(result)
	}
	git("clone", "--branch", imported.Branch, base+"/labowner/upstream.git", ".")
	if git("rev-parse", "HEAD") != row.Commit.ID {
		t.Fatal("the overview commit does not match the cloned mirror")
	}
	git("merge-base", "--is-ancestor", "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d", "HEAD")
	git("-c", "user.name=Lab Writer", "-c", "user.email=lab@example.invalid", "commit", "--allow-empty", "-m", "Mirror push fixture")
	result, err := labGit(t, dir, base, "labwriter", password, "push", "origin", "HEAD:refs/heads/bridge-mirror-proof")
	if err == nil || !strings.Contains(strings.ToLower(result), "mirror repository is read-only") {
		t.Fatalf("expected mirror-specific push rejection: %v\n%s", err, result)
	}
	if git("ls-remote", "origin", "refs/heads/bridge-mirror-proof") != "" {
		t.Fatal("the rejected push created a ref")
	}
	// Gitea records the sync time with second precision.
	if until := time.Until(beforeSync.Add(time.Second)); until > 0 {
		time.Sleep(until)
	}
	labAPI(t, base, admin, "POST", path+"/mirror-sync", nil, nil, 200)
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if lastSync().After(beforeSync) {
			return
		}
	}
	t.Fatal("the queued pull mirror sync did not complete")
}
