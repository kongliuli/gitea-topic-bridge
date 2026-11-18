package bridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBridgeProcessAnchorRecoveryLab(t *testing.T) {
	gitea := os.Getenv("BRIDGE_TEST_GITEA_BINARY")
	binary := os.Getenv("BRIDGE_TEST_BRIDGE_BINARY")
	if gitea == "" || binary == "" {
		t.Skip("set BRIDGE_TEST_GITEA_BINARY and BRIDGE_TEST_BRIDGE_BINARY for the executable recovery test")
	}
	if !filepath.IsAbs(gitea) || !filepath.IsAbs(binary) {
		t.Fatal("test binaries must use absolute paths")
	}
	giteaURL, command := startLocalGitea(t, gitea, "")
	owner, _ := createLabUser(t, command, "labowner", "all", "--admin")
	reader, password := createLabUser(t, command, "labreader", "read:repository,read:user")
	var issuer struct{ ID int64 }
	labAPI(t, giteaURL, reader, "GET", "/user", nil, &issuer, 200)
	var repo struct{ ID int64 }
	labAPI(t, giteaURL, owner, "POST", "/user/repos", map[string]any{
		"name": "fixture", "private": true, "auto_init": true, "default_branch": "main",
	}, &repo, 201)
	labAPI(t, giteaURL, owner, "PUT", "/repos/labowner/fixture/collaborators/labreader", map[string]string{"permission": "read"}, nil, 204)
	bridgeAddress, anchorAddress := labProcessAddresses(t)
	public := "http://" + bridgeAddress
	var oauth struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	labAPI(t, giteaURL, owner, "POST", "/user/applications/oauth2", map[string]any{
		"name": "Executable recovery lab", "confidential_client": true, "redirect_uris": []string{public + "/bridge/auth/callback"},
	}, &oauth, 201)

	bridgeDir, anchorDir := t.TempDir(), t.TempDir()
	inbox, snapshots := t.TempDir(), t.TempDir()
	artifactDir := filepath.Join(inbox, fmt.Sprint(repo.ID))
	if err := os.Mkdir(artifactDir, 0700); err != nil {
		t.Fatal(err)
	}
	artifact := []byte("process recovery fixture\n")
	if err := os.WriteFile(filepath.Join(artifactDir, "artifact.txt"), artifact, 0600); err != nil {
		t.Fatal(err)
	}
	issuerKey, issuerSigner, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	anchorKey, anchorSigner, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	bearer, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	anchorConfig := ReceiptAnchorConfig{
		URL: "http://" + anchorAddress + "/v1/anchors", PublicKey: base64.StdEncoding.EncodeToString(anchorKey),
	}
	writeLabJSON(t, filepath.Join(bridgeDir, "bridge.json"), ServerConfig{
		PublicBaseURL: public + "/bridge/", GiteaPublicURL: giteaURL, GiteaAPIURL: giteaURL, ClientID: oauth.ID,
		AllowLoopbackHTTP: true, DatabasePath: filepath.Join(bridgeDir, "bridge.db"), PageSize: 5, MaxPages: 2,
		Projects: []Project{{ID: "lab", Name: "Lab", Repositories: []Repository{{ID: repo.ID, Owner: "labowner", Name: "fixture"}}}},
		Receipts: &ReceiptConfig{ArtifactRoot: inbox, SnapshotRoot: snapshots, Issuers: []int64{issuer.ID}, Anchor: &anchorConfig},
	})
	writeLabJSON(t, filepath.Join(bridgeDir, "secrets.json"), map[string]string{
		"oauth_client_secret": oauth.Secret, "receipt_signing_seed": base64.StdEncoding.EncodeToString(issuerSigner.Seed()), "anchor_bearer": bearer,
	})
	writeLabJSON(t, filepath.Join(anchorDir, "secrets.json"), map[string]string{
		"anchor_signing_seed": base64.StdEncoding.EncodeToString(anchorSigner.Seed()), "anchor_bearer": bearer,
	})
	bridgeLog := filepath.Join(bridgeDir, "bridge.log")
	startBridge := func() func() {
		return startBridgeLabProcess(t, binary, bridgeDir, public+"/bridge/", 200,
			"-config", filepath.Join(bridgeDir, "bridge.json"), "-listen", bridgeAddress,
			"-secrets", filepath.Join(bridgeDir, "secrets.json"), "-log-file", bridgeLog)
	}
	startAnchor := func() func() {
		return startBridgeLabProcess(t, binary, anchorDir, "http://"+anchorAddress+"/v1/anchors", 405,
			"-anchor-db", filepath.Join(anchorDir, "ledger.db"), "-listen", anchorAddress,
			"-secrets", filepath.Join(anchorDir, "secrets.json"), "-log-file", filepath.Join(anchorDir, "anchor.log"))
	}
	stopBridge := startBridge()
	browser := labProcessBrowser(t)
	authorizeLabBrowser(t, browser, giteaURL, public, "labreader", password)
	topic := postLabTopic(t, browser, public+"/bridge/api/topics", public, "process-topic", `{"project_id":"lab","topic":"CAD-PROCESS"}`, 201)
	path := issueSingleArtifactLabReceipt(t, browser, public, topic.ID, repo.ID, issuerKey, fmt.Sprintf("%x", sha256.Sum256(artifact)))
	download := public + path
	receipt := []byte(loadLabPage(t, browser, download+"?format=json", nil, 200))
	loadLabPage(t, browser, download+"?format=proof", nil, 409)
	waitForLabProcess(t, "outbox records the service outage", func() bool {
		log, _ := os.ReadFile(bridgeLog)
		return strings.Contains(string(log), "awaiting retry (unavailable; attempt 1)")
	})
	stopBridge()
	stopAnchor := startAnchor()
	startBridge()
	loadLabPage(t, browser, download, nil, 401)
	browser = labProcessBrowser(t)
	authorizeLabBrowser(t, browser, giteaURL, public, "labreader", password)

	proofURL := download + "?format=proof"
	waitForLabProcess(t, "restarted Bridge exports the resumed proof", func() bool {
		return labCaddyReady(browser, proofURL)
	})
	proof := []byte(loadLabPage(t, browser, proofURL, nil, 200))
	if err := VerifyReceiptAnchor(receipt, proof, base64.StdEncoding.EncodeToString(issuerKey), anchorConfig.PublicKey); err != nil {
		t.Fatal(err)
	}
	if observed := loadLabPage(t, browser, download+"?format=json", nil, 200); observed != string(receipt) {
		t.Fatal("resumed delivery changed the original receipt bytes")
	}
	stopAnchor()
	startAnchor()
	envelope, document, err := readVerifiedReceipt(receipt, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	anchorConfig.Bearer = bearer
	client, err := newAnchorClient(anchorConfig, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	again, result := client.append(context.Background(), anchorStatement{document.ID, envelope.Digest, envelope.KeyID})
	if result != "" || !bytes.Equal(proof, again) {
		t.Fatalf("replay after restarting the ledger changed the proof: %s", result)
	}
}

func labProcessAddresses(t *testing.T) (string, string) {
	t.Helper()
	bind := func() net.Listener {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return listener
	}
	bridge, anchor := bind(), bind()
	defer bridge.Close()
	defer anchor.Close()
	return bridge.Addr().String(), anchor.Addr().String()
}

func writeLabJSON(t *testing.T, path string, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func labProcessBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 5 * time.Second}
}

func startBridgeLabProcess(t *testing.T, binary, directory, probe string, wantStatus int, args ...string) func() {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Dir = directory
	stderr, err := os.CreateTemp(directory, "startup-*.log")
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = stderr
	err = command.Start()
	stderr.Close()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = command.Wait()
		close(done)
	}()
	stop := sync.OnceFunc(func() { command.Process.Kill(); <-done })
	t.Cleanup(stop)
	client := &http.Client{Timeout: time.Second}
	waitForLabProcess(t, "start "+probe, func() bool {
		select {
		case <-done:
			log, _ := os.ReadFile(stderr.Name())
			t.Fatalf("Bridge executable exited before readiness: %v\n%s", exitErr, log)
		default:
		}
		response, err := client.Get(probe)
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == wantStatus
	})
	return stop
}

func waitForLabProcess(t *testing.T, description string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(25 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", description)
}
