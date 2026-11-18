package bridge

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
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
	withAssets := os.Getenv("BRIDGE_TEST_GITEA_RESTORE_ASSETS") == "1"
	settings := ""
	if withAssets {
		settings = "[server]\nLFS_START_SERVER = true\n[attachment]\nENABLED = true\n"
	}
	base, command := prepareLocalGitea(t, binary, "", "", settings)
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
	var assets restoreAssets
	if withAssets {
		assets = seedGiteaRestoreAssets(t, base, owner, password)
	}
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
	if withAssets {
		git("clone", "--no-checkout", "--branch", "main", address+"/labowner/restore-fixture.git", ".")
		git("lfs", "install", "--local")
		git("checkout", "main")
		git("lfs", "pull")
		assets.verify(t, address, writer, password, dir)
	} else {
		git("clone", "--branch", "main", address+"/labowner/restore-fixture.git", ".")
	}
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

type restoreAssets struct {
	releaseID   int64
	attachment  int64
	lfsDigest   [sha256.Size]byte
	assetDigest [sha256.Size]byte
}

func seedGiteaRestoreAssets(t *testing.T, base, owner, password string) restoreAssets {
	t.Helper()
	dir := t.TempDir()
	const repoURL = "/labowner/restore-fixture.git"
	git := func(args ...string) string {
		t.Helper()
		result, err := labGit(t, dir, base, "labwriter", password, args...)
		if err != nil {
			t.Fatalf("Git %s while creating restore assets: %v\n%s", args[0], err, result)
		}
		return result
	}
	git("clone", "--branch", "main", base+repoURL, ".")
	git("lfs", "install", "--local")
	git("lfs", "track", "*.bin")
	lfs := []byte(strings.Repeat("isolated LFS content\x00", 4096))
	if err := os.WriteFile(filepath.Join(dir, "fixture.bin"), lfs, 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".gitattributes", "fixture.bin")
	git("-c", "user.name=Restore Fixture", "-c", "user.email=restore@example.invalid", "commit", "-m", "Add a Git LFS fixture")
	digest := sha256.Sum256(lfs)
	if pointer := git("show", "HEAD:fixture.bin"); !strings.Contains(pointer, fmt.Sprintf("oid sha256:%x", digest)) || len(pointer) > 200 {
		t.Fatal("Git did not store an LFS pointer")
	}
	git("push", "origin", "HEAD:refs/heads/main")

	const apiPath = "/repos/labowner/restore-fixture/releases"
	var release struct{ ID int64 }
	labAPI(t, base, owner, "POST", apiPath, map[string]string{
		"tag_name": "fixture-v1", "target_commitish": "main", "name": "Restore fixture",
	}, &release, 201)
	attachment := "isolated release attachment\n"
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1"+fmt.Sprintf("%s/%d/assets?name=attachment.txt", apiPath, release.ID), strings.NewReader(attachment))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+owner)
	req.Header.Set("Content-Type", "application/octet-stream")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		t.Fatalf("attach the release fixture: status %d, %s", response.StatusCode, body)
	}
	var asset struct{ ID int64 }
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&asset); err != nil || asset.ID < 1 {
		t.Fatalf("could not read the fixture attachment ID: %v", err)
	}
	return restoreAssets{release.ID, asset.ID, digest, sha256.Sum256([]byte(attachment))}
}

func (assets restoreAssets) verify(t *testing.T, base, token, password, dir string) {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join(dir, "fixture.bin"))
	if err != nil || sha256.Sum256(fixture) != assets.lfsDigest {
		t.Fatalf("restored clone did not fetch the original LFS bytes: %v", err)
	}
	var asset struct {
		ID  int64
		URL string `json:"browser_download_url"`
	}
	endpoint := fmt.Sprintf("/repos/labowner/restore-fixture/releases/%d/assets/%d", assets.releaseID, assets.attachment)
	labAPI(t, base, token, "GET", endpoint, nil, &asset, 200)
	download, err := url.Parse(asset.URL)
	if err != nil || asset.ID != assets.attachment || !strings.HasPrefix(asset.URL, base+"/") || download.User != nil {
		t.Fatalf("restored release attachment has an unexpected ID or download URL: %d %q", asset.ID, asset.URL)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	form := url.Values{"user_name": {"labwriter"}, "password": {password}}
	loginURL := base + "/user/login"
	addLabCSRF(loadLabPage(t, browser, loginURL, nil, 200), form)
	loadLabPage(t, browser, loginURL, form, 200)
	data := loadLabPage(t, browser, asset.URL, nil, 200)
	if sha256.Sum256([]byte(data)) != assets.assetDigest {
		t.Fatal("restored release attachment bytes did not match the original")
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
