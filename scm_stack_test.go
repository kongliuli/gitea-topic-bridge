package bridge

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
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
	var fixture scmStackFixture
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
	verifySCMReceipt(t, browser, public, batch.ID, fixture.RepositoryID, fixture.AnchorPublicKey)
	repoURL := public + "/labowner/fixture.git"
	page := loadLabPage(t, browser, public+"/labowner/fixture", nil, 200)
	if !strings.Contains(page, repoURL) {
		t.Fatal("the protected Gitea page did not advertise the canonical Git HTTPS URL")
	}
	clone := t.TempDir()
	output, err := fixture.git(t, clone, "labreader", fixture.Password, "clone", "--branch", "main", repoURL, ".")
	if err != nil {
		t.Fatalf("private Git HTTPS clone through the SCM gateway: %v\n%s", err, output)
	}
	head, err := labGit(t, clone, public, "labreader", fixture.Password, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != row.Commit.ID {
		t.Fatalf("private Git HTTPS clone did not return the same head as Bridge: %v", err)
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
	verifySCMPilot(t, browser, fixture)
}

type scmStackFixture struct {
	Gateway, CA, Password string
	RepositoryID          int64  `json:"repository_id"`
	AnchorPublicKey       string `json:"anchor_public_key"`
	GiteaAPI              string `json:"gitea_api"`
	OwnerToken            string `json:"owner_token"`
	OwnerPassword         string `json:"owner_password"`
	DeveloperToken        string `json:"developer_token"`
	PilotID               int64  `json:"pilot_id"`
}

func (fixture scmStackFixture) git(t *testing.T, dir, user, password string, args ...string) (string, error) {
	t.Helper()
	tls := []string{"-c", "http.sslVerify=true", "-c", "http.sslCAInfo=" + fixture.CA, "-c", "http.sslBackend=schannel",
		"-c", "http.schannelUseSSLCAInfo=true", "-c", "http.schannelCheckRevoke=false"}
	return labGit(t, dir, fixture.Gateway, user, password, append(tls, args...)...)
}

func verifySCMReceipt(t *testing.T, browser *http.Client, public, topic string, repoID int64, anchorKey string) {
	t.Helper()
	var issuer map[string]string
	rawKey := loadLabPage(t, browser, public+"/bridge/api/receipt-key", nil, 200)
	if err := json.Unmarshal([]byte(rawKey), &issuer); err != nil {
		t.Fatal("could not read the laboratory receipt public key")
	}
	key, err := decodeReceiptPublicKey(issuer["public_key"])
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("SCM inbox fixture")))
	path := issueSingleArtifactLabReceipt(t, browser, public, topic, repoID, key, digest)
	proofURL := public + path + "?format=proof"
	waitForLabProcess(t, "independent Windows service to sign the receipt digest", func() bool {
		return labCaddyReady(browser, proofURL)
	})
	proof := loadLabPage(t, browser, proofURL, nil, 200)
	receipt := loadLabPage(t, browser, public+path+"?format=json", nil, 200)
	if err := VerifyReceiptAnchor([]byte(receipt), []byte(proof), issuer["public_key"], anchorKey); err != nil {
		t.Fatalf("SCM receipt and detached proof verify together: %v", err)
	}
	t.Log("The service account signed the read-only artifact and the independent service anchored its receipt.")
}
