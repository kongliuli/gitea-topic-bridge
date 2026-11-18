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
	base, command := startLocalGitea(t, binary)
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
	browser, bridgeURL := loginLab(t, base, ownerToken, readerPassword, repos)
	lookupURL := bridgeURL + "/bridge/api/topics/lookup?project_id=lab&topic=CAD-01"
	data := loadLabPage(t, browser, lookupURL, nil, 200)
	if !strings.Contains(data, `"name":"conflict"`) || !strings.Contains(data, `"lifecycle":"merged"`) || strings.Contains(data, `"name":"hidden"`) {
		t.Fatal("OAuth lookup did not use the reader's repository visibility")
	}
	api("DELETE", "/repos/labowner/conflict/collaborators/labreader", nil, nil, 204)
	results = lookup(3)
	if len(results) != 2 || results[1].Repository.Name != "no-match" {
		t.Fatalf("lookup after revocation: %#v", results)
	}
	if strings.Contains(loadLabPage(t, browser, lookupURL, nil, 200), `"name":"conflict"`) {
		t.Fatal("OAuth lookup returned a repository after access was revoked")
	}
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
}

func loginLab(t *testing.T, giteaURL, ownerToken, readerPassword string, repos []Repository) (*http.Client, string) {
	t.Helper()
	app := httptest.NewUnstartedServer(nil)
	bridgeURL := "http://" + app.Listener.Addr().String()
	var creds struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	labAPI(t, giteaURL, ownerToken, "POST", "/user/applications/oauth2", map[string]any{
		"name": "Topic Bridge Lab", "confidential_client": true, "redirect_uris": []string{bridgeURL + "/bridge/auth/callback"},
	}, &creds, 201)
	handler, err := NewServer(ServerConfig{PublicBaseURL: bridgeURL + "/bridge/", GiteaPublicURL: giteaURL, GiteaAPIURL: giteaURL,
		ClientID: creds.ID, ClientSecret: creds.Secret, AllowLoopbackHTTP: true, PageSize: 1, MaxPages: 3,
		Projects: []Project{{ID: "lab", Name: "Lab", Repositories: repos}, {ID: "secret", Name: "Secret", Repositories: repos[3:]}},
	})
	if err != nil {
		app.Listener.Close()
		t.Fatal(err)
	}
	app.Config.Handler = handler
	app.Start()
	t.Cleanup(app.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	loginURL := giteaURL + "/user/login"
	page := loadLabPage(t, browser, loginURL, nil, 200)
	loginForm := url.Values{"user_name": {"labreader"}, "password": {readerPassword}}
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
	if projects := loadLabPage(t, browser, bridgeURL+"/bridge/api/projects", nil, 200); !strings.Contains(projects, `"user":"labreader"`) || strings.Contains(projects, "Secret") {
		t.Fatal("OAuth session used the wrong user or exposed a hidden project")
	}
	return browser, bridgeURL
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
