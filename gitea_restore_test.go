package bridge

import (
	"encoding/base64"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGiteaColdRestoreLab(t *testing.T) {
	binary := os.Getenv("BRIDGE_TEST_GITEA_BINARY")
	if binary == "" {
		t.Skip("set BRIDGE_TEST_GITEA_BINARY for the isolated Gitea cold restore")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("Gitea binary path must be absolute")
	}
	base, command := prepareLocalGitea(t, binary, "", "")
	proc := command("web")
	done, result := launchLocalGitea(t, base, proc)
	owner, _ := createLabUser(t, command, "labowner", "all", "--admin")
	writer, password := createLabUser(t, command, "labwriter", "write:repository,read:user")
	const repoPath = "/repos/labowner/restore-fixture"
	var repo struct{ ID int64 }
	labAPI(t, base, owner, "POST", "/user/repos", map[string]any{
		"name": "restore-fixture", "private": true, "auto_init": true, "default_branch": "main",
	}, &repo, 201)
	labAPI(t, base, owner, "PUT", repoPath+"/collaborators/labwriter", map[string]string{"permission": "write"}, nil, 204)
	const payload = "isolated bare repository recovery\n"
	labAPI(t, base, owner, "POST", repoPath+"/contents/fixture.txt", map[string]string{
		"branch": "main", "message": "Restore fixture", "content": base64.StdEncoding.EncodeToString([]byte(payload)),
	}, nil, 201)
	var before struct{ Commit struct{ ID string } }
	labAPI(t, base, writer, "GET", repoPath+"/branches/main", nil, &before, 200)
	if !commitSHA.MatchString(before.Commit.ID) {
		t.Fatal("Gitea fixture did not return a valid default branch SHA")
	}
	labAPI(t, base, owner, "POST", repoPath+"/branch_protections", map[string]any{
		"rule_name": "main", "enable_push": false, "required_approvals": 1,
	}, nil, 201)
	if out, err := command("manager", "shutdown").CombinedOutput(); err != nil {
		t.Fatalf("stop fixture Gitea: %v\n%s", err, out)
	}
	select {
	case <-done:
		if err := result(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Gitea did not exit before backup")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(filepath.Join(proc.Dir, "data", "gitea.db"+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("SQLite sidecar must be absent before copying the fixture: %s (%v)", suffix, err)
		}
	}
	backup, restore := t.TempDir(), t.TempDir()
	copyLabGiteaState(t, proc.Dir, backup)
	copyLabGiteaState(t, backup, restore)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "http://" + listener.Addr().String()
	listener.Close()
	_, oldPort, _ := strings.Cut(base, "127.0.0.1:")
	_, newPort, _ := strings.Cut(address, "127.0.0.1:")
	configPath := filepath.Join(restore, "app.ini")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	relocated := strings.NewReplacer(
		filepath.ToSlash(proc.Dir), filepath.ToSlash(restore), proc.Dir, restore,
		"HTTP_PORT = "+oldPort, "HTTP_PORT = "+newPort, base, address,
	).Replace(string(config))
	if err := os.WriteFile(configPath, []byte(relocated), 0600); err != nil {
		t.Fatal(err)
	}
	recoveredCommand := labGiteaCommand(binary, restore, configPath)
	if out, err := recoveredCommand("admin", "regenerate", "hooks").CombinedOutput(); err != nil {
		t.Fatalf("regenerate Git hooks in the restored repository: %v\n%s", err, out)
	}
	launchLocalGitea(t, address, recoveredCommand("web"))

	var restored struct{ ID int64 }
	labAPI(t, address, writer, "GET", repoPath, nil, &restored, 200)
	var branch struct {
		Commit            struct{ ID string }
		Protected         bool
		RequiredApprovals int `json:"required_approvals"`
	}
	labAPI(t, address, writer, "GET", repoPath+"/branches/main", nil, &branch, 200)
	if restored.ID != repo.ID || branch.Commit.ID != before.Commit.ID || !branch.Protected || branch.RequiredApprovals != 1 {
		t.Fatal("restored repository or its branch protection did not match the backup")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		out, err := labGit(t, dir, address, "labwriter", password, args...)
		if err != nil {
			t.Fatalf("Git %s on the restored instance: %v\n%s", args[0], err, out)
		}
	}
	git("clone", "--branch", "main", address+"/labowner/restore-fixture.git", ".")
	data, err := os.ReadFile(filepath.Join(dir, "fixture.txt"))
	if err != nil || strings.ReplaceAll(string(data), "\r\n", "\n") != payload {
		t.Fatalf("original file did not survive the restore: %v", err)
	}
	git("push", "origin", "HEAD:refs/heads/restore-proof")
	labAPI(t, address, writer, "GET", repoPath+"/branches/restore-proof", nil, &branch, 200)
	if branch.Commit.ID != before.Commit.ID {
		t.Fatal("restored Git hooks failed to record the new branch")
	}
}

func copyLabGiteaState(t *testing.T, source, target string) {
	t.Helper()
	for _, dir := range []string{"repos", "data", "custom"} {
		if err := os.CopyFS(filepath.Join(target, dir), os.DirFS(filepath.Join(source, dir))); err != nil {
			t.Fatal(err)
		}
	}
	config, err := os.ReadFile(filepath.Join(source, "app.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "app.ini"), config, 0600); err != nil {
		t.Fatal(err)
	}
}
