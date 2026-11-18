package bridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGiteaGatewayTLS(t *testing.T) {
	gitea := os.Getenv("BRIDGE_TEST_GITEA_BINARY")
	caddy := os.Getenv("BRIDGE_TEST_CADDY_BINARY")
	if gitea == "" || caddy == "" {
		t.Skip("set BRIDGE_TEST_GITEA_BINARY and BRIDGE_TEST_CADDY_BINARY for the isolated HTTPS gateway check")
	}
	if !filepath.IsAbs(gitea) || !filepath.IsAbs(caddy) {
		t.Fatal("test binaries must use absolute paths")
	}
	for _, name := range []string{"git.office.example", "127.0.0.1"} {
		t.Run(name, func(t *testing.T) {
			checkGiteaGateway(t, gitea, caddy, name)
		})
	}
}

func checkGiteaGateway(t *testing.T, gitea, caddy, name string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	localAddr := listener.Addr().String()
	gatewayAddr := fmt.Sprintf("%s:%d", name, listener.Addr().(*net.TCPAddr).Port)
	listener.Close()
	public := "https://" + gatewayAddr
	internal, command := startLocalGiteaForPublicURL(t, gitea, "", public)
	admin, _ := createLabUser(t, command, "labowner", "all", "--admin")
	_, password := createLabUser(t, command, "labreader", "read:repository,read:user")
	var fixture struct {
		ID       int64
		CloneURL string `json:"clone_url"`
	}
	labAPI(t, internal, admin, "POST", "/user/repos", map[string]any{
		"name": "fixture", "private": true, "auto_init": true, "default_branch": "main",
	}, &fixture, 201)
	labAPI(t, internal, admin, "PUT", "/repos/labowner/fixture/collaborators/labreader", map[string]string{"permission": "read"}, nil, 204)
	var creds struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	labAPI(t, internal, admin, "POST", "/user/applications/oauth2", map[string]any{
		"name": "Gateway Lab", "confidential_client": true, "redirect_uris": []string{public + "/bridge/auth/callback"},
	}, &creds, 201)
	project := Project{ID: "lab", Name: "Gateway test", Repositories: []Repository{{ID: fixture.ID, Owner: "labowner", Name: "fixture"}}}
	bridge, err := NewServer(ServerConfig{
		PublicBaseURL: public + "/bridge/", GiteaPublicURL: public, GiteaAPIURL: internal,
		ClientID: creds.ID, ClientSecret: creds.Secret, PageSize: 1, MaxPages: 2, Projects: []Project{project},
	})
	if err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(bridge)
	t.Cleanup(app.Close)
	ca, browser := startLabCaddy(t, caddy, gatewayAddr, localAddr, internal, app.Listener.Addr().String())
	home := loadLabPage(t, browser, public+"/", nil, 200)
	if !strings.Contains(home, `href="/bridge/">多仓联调</a>`) {
		t.Fatal("Gitea did not render the Bridge navigation through the gateway")
	}
	if body := loadLabPage(t, browser, public+"/internal/hooks/gitea", nil, 404); body != "" {
		t.Fatal("gateway should terminate the internal route at its own 404")
	}
	loadLabPage(t, browser, public+"/bridge/api/projects", nil, 401)
	var secureSession bool
	browser.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme+"://"+next.URL.Host != public || len(via) > 12 {
			return fmt.Errorf("unexpected redirect away from the canonical gateway or too many redirects")
		}
		if next.Response.Request.URL.Path == "/bridge/auth/callback" {
			if next.URL.Path != "/bridge/" || next.URL.Query().Get("project_id") != "lab" || next.URL.Query().Get("topic") != "CAD-01" {
				return fmt.Errorf("OAuth did not restore the original Bridge page")
			}
			for _, cookie := range next.Response.Cookies() {
				if cookie.Name == sessionCookie && cookie.HttpOnly && cookie.Secure && cookie.SameSite == http.SameSiteLaxMode {
					secureSession = true
				}
			}
		}
		return nil
	}
	authorizeLabBrowser(t, browser, public, public, "labreader", password)
	if !secureSession {
		t.Fatal("HTTPS OAuth did not issue a secure session cookie")
	}
	if overview := loadLabPage(t, browser, public+"/bridge/api/projects?overview=lab", nil, 200); !strings.Contains(overview, public+"/labowner/fixture/commit/") {
		t.Fatal("overview commit link did not use the canonical HTTPS site")
	}
	if fixture.CloneURL != public+"/labowner/fixture.git" {
		t.Fatalf("Gitea advertised the wrong Git HTTPS URL: %q", fixture.CloneURL)
	}
	repoPage := loadLabPage(t, browser, public+"/labowner/fixture", nil, 200)
	if !strings.Contains(repoPage, fixture.CloneURL) {
		t.Fatal("repository page did not include the canonical Git HTTPS URL")
	}
	options := []string{"-c", "http.sslVerify=true", "-c", "http.sslCAInfo=" + ca}
	if runtime.GOOS == "windows" {
		// The temporary CA has no revocation endpoint. Keep chain and name verification enabled.
		options = append(options, "-c", "http.sslBackend=schannel", "-c", "http.schannelUseSSLCAInfo=true", "-c", "http.schannelCheckRevoke=false")
	}
	gitOutput, err := labGit(t, t.TempDir(), public, "labreader", password, append(options, "clone", fixture.CloneURL, ".")...)
	if err != nil {
		t.Fatalf("Git HTTPS clone through the trusted gateway failed: %v\n%s", err, gitOutput)
	}
}

func startLabCaddy(t *testing.T, binary, gateway, dialAddress, gitea, bridge string) (string, *http.Client) {
	t.Helper()
	dir := t.TempDir()
	storage := filepath.Join(dir, "storage")
	data, err := os.ReadFile("examples/Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{
	admin off
	persist_config off
	skip_install_trust
	auto_https disable_redirects
	default_bind 127.0.0.1
	servers {
		protocols h1 h2
	}
	storage file_system {
		root "%s"
	}
}
`, filepath.ToSlash(storage))
	config += strings.NewReplacer("git.office.example:8443", gateway, "127.0.0.1:3000", strings.TrimPrefix(gitea, "http://"), "127.0.0.1:3001", bridge).Replace(string(data))
	logPath := runLabCaddy(t, binary, dir, config)
	ca := filepath.Join(storage, "pki", "authorities", "local", "root.crt")
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != gateway {
			return nil, fmt.Errorf("unexpected gateway destination %q", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, dialAddress)
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Jar: jar, Timeout: 8 * time.Second, Transport: transport}
	installed := false
	waitForLabCaddy(t, logPath, func() bool {
		if !installed {
			pem, _ := os.ReadFile(ca)
			installed = roots.AppendCertsFromPEM(pem)
		}
		return installed && labCaddyReady(client, "https://"+gateway+"/bridge/")
	})
	return ca, client
}

func runLabCaddy(t *testing.T, binary, dir, config string) string {
	t.Helper()
	path := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "caddy.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "run", "--config", path, "--adapter", "caddyfile")
	command.Dir = dir
	command.Stdout, command.Stderr = logFile, logFile
	err = command.Start()
	logFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command.Process.Kill(); command.Wait() })
	return logPath
}

func labCaddyReady(client *http.Client, address string) bool {
	response, err := client.Get(address)
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == 200
}

func waitForLabCaddy(t *testing.T, logPath string, ready func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if ready() {
			return
		}
	}
	log, _ := os.ReadFile(logPath)
	t.Fatalf("the isolated gateway did not start: %s", log)
}

func TestCaddyTailnetLoopback(t *testing.T) {
	binary := os.Getenv("BRIDGE_TEST_CADDY_BINARY")
	if binary == "" {
		t.Skip("set BRIDGE_TEST_CADDY_BINARY for loopback gateway validation")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("Caddy binary path must be absolute")
	}
	upstream := func(name string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			fmt.Fprintf(writer, "%s %s", name, request.URL.RequestURI())
		}))
		t.Cleanup(server.Close)
		return server
	}
	gitea, bridge := upstream("gitea"), upstream("bridge")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	data, err := os.ReadFile("examples/Caddyfile.tailnet")
	if err != nil {
		t.Fatal(err)
	}
	config := "{\n\tadmin off\n\tpersist_config off\n}\n"
	config += strings.NewReplacer("127.0.0.1:8080", address, "127.0.0.1:3000", gitea.Listener.Addr().String(), "127.0.0.1:3001", bridge.Listener.Addr().String()).Replace(string(data))
	logPath := runLabCaddy(t, binary, t.TempDir(), config)
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + address
	waitForLabCaddy(t, logPath, func() bool { return labCaddyReady(client, base+"/") })
	if got := loadLabPage(t, client, base+"/lab/fixture.git/info/refs?service=git-upload-pack", nil, 200); got != "gitea /lab/fixture.git/info/refs?service=git-upload-pack" {
		t.Fatal("expected Git HTTPS paths received from Serve to route to Gitea over local HTTP")
	}
	if got := loadLabPage(t, client, base+"/bridge/auth/callback?state=fixture", nil, 200); got != "bridge /bridge/auth/callback?state=fixture" {
		t.Fatal("expected Bridge callback path and query to reach Bridge unchanged")
	}
	if got := loadLabPage(t, client, base+"/internal/hooks/gitea", nil, 404); got != "" {
		t.Fatal("expected the loopback gateway to block the internal webhook path")
	}
	response, err := client.Get(base + "/bridge")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 308 || response.Header.Get("Location") != "/bridge/" {
		t.Fatal("expected /bridge to redirect to /bridge/")
	}
}
