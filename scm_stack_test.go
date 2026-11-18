package bridge

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSCMHostedStack(t *testing.T) {
	path := os.Getenv("BRIDGE_TEST_SCM_STACK")
	if path == "" {
		t.Skip("executed by the Windows service lab with an isolated stack")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Gateway, CA, Password string
		RepositoryID          int64 `json:"repository_id"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal("invalid SCM stack fixture JSON")
	}
	gateway, err := url.Parse(fixture.Gateway)
	if err != nil || gateway.Scheme != "https" || gateway.Hostname() != "127.0.0.1" ||
		gateway.User != nil || fixture.RepositoryID < 1 || fixture.Password == "" || !filepath.IsAbs(fixture.CA) {
		t.Fatal("SCM stack fixture must identify the isolated HTTPS site and test account")
	}
	roots := x509.NewCertPool()
	certificate, err := os.ReadFile(fixture.CA)
	if err != nil || !roots.AppendCertsFromPEM(certificate) {
		t.Fatalf("the service gateway CA was not available: %v", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer transport.CloseIdleConnections()
	browser := &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: transport}
	public := fixture.Gateway
	home := loadLabPage(t, browser, public+"/", nil, 200)
	if !strings.Contains(home, `href="/bridge/">多仓联调</a>`) {
		t.Fatal("the service gateway did not deliver Gitea's Bridge menu")
	}
	loadLabPage(t, browser, public+"/bridge/api/projects", nil, 401)
	loadLabPage(t, browser, public+"/api/v1/repos/labowner/fixture", nil, 404)
	authorizeLabBrowser(t, browser, public, public, "labreader", fixture.Password)
	var overview struct{ Repositories []projectRepositoryOverview }
	overviewJSON := loadLabPage(t, browser, public+"/bridge/api/projects?overview=lab", nil, 200)
	if err := json.Unmarshal([]byte(overviewJSON), &overview); err != nil || len(overview.Repositories) != 1 {
		t.Fatalf("could not read the SCM-hosted project overview: %v", err)
	}
	row := overview.Repositories[0]
	if row.Repository.ID != fixture.RepositoryID || row.Commit == nil ||
		row.Commit.URL != public+"/labowner/fixture/commit/"+row.Commit.ID {
		t.Fatal("the project overview did not expose the expected Gitea repository and canonical commit link")
	}
	batch := postLabTopic(t, browser, public+"/bridge/api/topics", public, "scm-fixture", `{"project_id":"lab","topic":"CAD-SCM"}`, 201)
	if len(batch.Members) != 1 || batch.Members[0].RepositoryID != fixture.RepositoryID {
		t.Fatal("the SCM-hosted Bridge did not persist the expected repository")
	}
	repoURL := public + "/labowner/fixture.git"
	page := loadLabPage(t, browser, public+"/labowner/fixture", nil, 200)
	if !strings.Contains(page, repoURL) {
		t.Fatal("the protected Gitea page did not advertise the canonical Git HTTPS URL")
	}
	output, err := labGit(t, t.TempDir(), public, "labreader", fixture.Password,
		"-c", "http.sslVerify=true", "-c", "http.sslCAInfo="+fixture.CA, "-c", "http.sslBackend=schannel",
		"-c", "http.schannelUseSSLCAInfo=true", "-c", "http.schannelCheckRevoke=false",
		"ls-remote", repoURL, "refs/heads/main")
	if err != nil || strings.TrimSpace(output) != row.Commit.ID+"\trefs/heads/main" {
		t.Fatalf("private Git HTTPS did not return the same head as Bridge: %v\n%s", err, output)
	}
	response, err := browser.Post(public+"/internal/hooks/gitea", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatalf("public gateway accepted the internal webhook: %d", response.StatusCode)
	}
	t.Log("Gitea, Caddy and Bridge under SCM passed canonical HTTPS, private Git and OAuth verification.")
}
