package bridge

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
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
	fixture, evidencePath := loadSCMFixture(t)
	browser := fixture.browser(t)
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
	evidence := verifySCMReceipt(t, browser, public, batch.ID, fixture.RepositoryID, fixture.AnchorPublicKey)
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
	pull := verifySCMPilot(t, browser, fixture)
	captured := scmRestartEvidence{
		TopicID: batch.ID, TopicVersion: batch.Version, MemberID: batch.Members[0].ID,
		PilotNumber: pull.Number, PilotHead: pull.HeadSHA, Receipt: evidence,
	}
	raw, err := json.Marshal(captured)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidencePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

type scmRestartEvidence struct {
	TopicID      string
	TopicVersion int
	MemberID     int64
	PilotNumber  int64
	PilotHead    string
	Receipt      scmReceiptEvidence
}

type scmReceiptEvidence struct {
	Path      string
	IssuerKey string
	Statement anchorStatement
	Receipt   [sha256.Size]byte
	Proof     [sha256.Size]byte
}

func TestSCMHostedStackAfterRestart(t *testing.T) {
	fixture, evidencePath := loadSCMFixture(t)
	raw, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	var before scmRestartEvidence
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal("invalid pre-restart evidence")
	}
	browser := fixture.browser(t)
	public := fixture.Gateway
	waitForLabProcess(t, "the restarted HTTPS gateway and Bridge", func() bool {
		return labCaddyReady(browser, public+"/") && labCaddyReady(browser, public+"/bridge/")
	})
	loadLabPage(t, browser, public+"/bridge/api/projects", nil, 401)
	authorizeLabBrowser(t, browser, public, public, "labreader", fixture.Password)
	topic := loadLabPage(t, browser, public+"/bridge/api/topics/"+before.TopicID, nil, 200)
	var batch TopicChangeSet
	if err := json.Unmarshal([]byte(topic), &batch); err != nil ||
		batch.ID != before.TopicID || batch.Topic != "CAD-SCM" || batch.ProjectID != "lab" ||
		batch.Version != before.TopicVersion || len(batch.Members) != 1 ||
		batch.Members[0].ID != before.MemberID || batch.Members[0].RepositoryID != fixture.RepositoryID {
		t.Fatalf("the saved topic changed across the SCM restart: %v", err)
	}
	pull := lookupSCMPilot(t, browser, fixture)
	if pull.Number != before.PilotNumber || pull.HeadSHA != before.PilotHead {
		t.Fatal("the imported repository's PR changed across the SCM restart")
	}
	pilotGit := public + "/labowner/pilot.git"
	const pilotRef = "refs/heads/pilot-check"
	head, err := fixture.git(t, t.TempDir(), "labreader", fixture.Password, "ls-remote", "--refs", pilotGit, pilotRef)
	if err != nil || strings.TrimSpace(head) != before.PilotHead+"\t"+pilotRef {
		t.Fatalf("the persisted private Git branch did not match the PR: %v", err)
	}
	receiptURL := public + before.Receipt.Path
	receipt := []byte(loadLabPage(t, browser, receiptURL+"?format=json", nil, 200))
	proof := []byte(loadLabPage(t, browser, receiptURL+"?format=proof", nil, 200))
	if sha256.Sum256(receipt) != before.Receipt.Receipt || sha256.Sum256(proof) != before.Receipt.Proof {
		t.Fatal("the signed receipt or its detached proof changed across the SCM restart")
	}
	if err := VerifyReceiptAnchor(receipt, proof, before.Receipt.IssuerKey, fixture.AnchorPublicKey); err != nil {
		t.Fatalf("the original keys could not verify the restarted stack's receipt and proof: %v", err)
	}
	verifySCMAnchorReplay(t, fixture, before.Receipt)
	t.Log("Restarted all four SCM services; fresh OAuth, original topic, private Git ref and PR, byte-identical receipt and proof, and independent Anchor replay passed.")
}

type scmStackFixture struct {
	Gateway, CA, Password string
	RepositoryID          int64  `json:"repository_id"`
	AnchorPublicKey       string `json:"anchor_public_key"`
	AnchorURL             string `json:"anchor_url"`
	AnchorBearer          string `json:"anchor_bearer"`
	GiteaAPI              string `json:"gitea_api"`
	OwnerToken            string `json:"owner_token"`
	OwnerPassword         string `json:"owner_password"`
	DeveloperToken        string `json:"developer_token"`
	PilotID               int64  `json:"pilot_id"`
}

func loadSCMFixture(t *testing.T) (scmStackFixture, string) {
	t.Helper()
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
	return fixture, filepath.Join(filepath.Dir(path), "restart-evidence.json")
}

func (fixture scmStackFixture) browser(t *testing.T) *http.Client {
	t.Helper()
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
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: transport}
}

func (fixture scmStackFixture) git(t *testing.T, dir, user, password string, args ...string) (string, error) {
	t.Helper()
	tls := []string{"-c", "http.sslVerify=true", "-c", "http.sslCAInfo=" + fixture.CA, "-c", "http.sslBackend=schannel",
		"-c", "http.schannelUseSSLCAInfo=true", "-c", "http.schannelCheckRevoke=false"}
	return labGit(t, dir, fixture.Gateway, user, password, append(tls, args...)...)
}

func verifySCMReceipt(t *testing.T, browser *http.Client, public, topic string, repoID int64, anchorKey string) scmReceiptEvidence {
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
	var signed anchorProof
	if err := json.Unmarshal([]byte(proof), &signed); err != nil {
		t.Fatal(err)
	}
	t.Log("The service account signed the read-only artifact and the independent service anchored its receipt.")
	return scmReceiptEvidence{
		Path: path, IssuerKey: issuer["public_key"], Statement: signed.Record.anchorStatement,
		Receipt: sha256.Sum256([]byte(receipt)), Proof: sha256.Sum256([]byte(proof)),
	}
}

func verifySCMAnchorReplay(t *testing.T, fixture scmStackFixture, before scmReceiptEvidence) {
	t.Helper()
	endpoint, err := url.Parse(fixture.AnchorURL)
	if err != nil || endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" ||
		endpoint.User != nil || endpoint.Path != "/v1/anchors" || !validAnchorBearer(fixture.AnchorBearer) {
		t.Fatal("SCM Anchor fixture must use an isolated internal endpoint and fixture credential")
	}
	raw, err := json.Marshal(before.Statement)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint.String(), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+fixture.AnchorBearer)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	proof, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || response.StatusCode != http.StatusOK || sha256.Sum256(proof) != before.Proof {
		t.Fatalf("independent Anchor did not return the original stored proof (HTTP %d): %v", response.StatusCode, err)
	}
}
