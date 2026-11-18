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
	path := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(dir, "caddy.log"))
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
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if !installed {
			pem, _ := os.ReadFile(ca)
			installed = roots.AppendCertsFromPEM(pem)
		}
		if installed {
			res, err := client.Get("https://" + gateway + "/bridge/")
			if err == nil {
				res.Body.Close()
				if res.StatusCode == 200 {
					return ca, client
				}
			}
		}
	}
	log, _ := os.ReadFile(filepath.Join(dir, "caddy.log"))
	t.Fatalf("the isolated gateway did not start: %s", log)
	return "", nil
}
