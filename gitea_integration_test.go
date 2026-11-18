package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
	base, command := startLocalGitea(t, binary)
	createUser := func(name, scopes string, opts ...string) string {
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
		return string(match[1])
	}
	ownerToken := createUser("labowner", "all", "--admin")
	readerToken := createUser("labreader", "read:repository,read:user")
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
	seedPull := func(repo, branch, title, body string) int64 {
		t.Helper()
		prefix := "/repos/labowner/" + repo
		api("POST", prefix+"/branches", map[string]string{"new_branch_name": branch, "old_branch_name": "main"}, nil, 201)
		api("POST", prefix+"/contents/"+branch+".txt", map[string]string{"branch": branch, "message": "Synthetic fixture", "content": base64.StdEncoding.EncodeToString([]byte("test\n"))}, nil, 201)
		var pull struct{ Number int64 }
		api("POST", prefix+"/pulls", map[string]string{"title": title, "body": body, "base": "main", "head": branch}, &pull, 201)
		return pull.Number
	}
	merged := seedPull("merged", "complete", "[Topic: CAD-01] Merged fixture", "")
	api("POST", fmt.Sprintf("/repos/labowner/merged/pulls/%d/merge", merged), map[string]string{"Do": "merge"}, nil, 200)
	seedPull("conflict", "changes", "Open fixture", "[Topic: CAD-01]")
	api("POST", "/repos/labowner/conflict/contents/changes.txt", map[string]string{"branch": "main", "message": "Conflicting fixture", "content": "b3RoZXIK"}, nil, 201)
	seedPull("conflict", "followup", "[Topic: CAD-01] Second fixture", "")
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
	api("DELETE", "/repos/labowner/conflict/collaborators/labreader", nil, nil, 204)
	results = lookup(3)
	if len(results) != 2 || results[1].Repository.Name != "no-match" {
		t.Fatalf("lookup after revocation: %#v", results)
	}
}

func startLocalGitea(t *testing.T, binary string) (string, func(...string) *exec.Cmd) {
	t.Helper()
	root := t.TempDir()
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
[service]
DISABLE_REGISTRATION = true
[actions]
ENABLED = false
[log]
MODE = console
LEVEL = warn
`, port, base, filepath.ToSlash(root), filepath.ToSlash(root), filepath.ToSlash(root))
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
