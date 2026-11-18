package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArtifactCopiesBeforeHashAndDeniesHardLinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "artifacts")
	repoDirectory := filepath.Join(root, "23")
	if err := os.MkdirAll(repoDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repoDirectory, "bundle.txt")
	original := []byte("first version\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	destination, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	receiptID := strings.Repeat("A", 43)
	if err := destination.Mkdir(receiptID, 0700); err != nil {
		t.Fatal(err)
	}
	artifact, err := copyArtifact(context.Background(), root, destination, receiptID, artifactSelection{23, "bundle.txt"}, 1, 1024)
	digest := sha256.Sum256(original)
	if err != nil || artifact.Size != int64(len(original)) || artifact.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("snapshot failed: %+v, %v", artifact, err)
	}
	if err := os.WriteFile(path, []byte("replacement\n"), 0600); err != nil {
		t.Fatal(err)
	}
	copied, err := destination.ReadFile(filepath.Join(receiptID, "1.blob"))
	if err != nil || !bytes.Equal(copied, original) {
		t.Fatalf("snapshot changed: %q, %v", copied, err)
	}
	for _, unsafe := range []string{"../bundle.txt", "/bundle.txt", "C:/bundle.txt", "bundle.txt:stream", "nested/../bundle.txt", "nested/./bundle.txt"} {
		if _, err := copyArtifact(context.Background(), root, destination, receiptID, artifactSelection{23, unsafe}, 2, 1024); !errors.Is(err, errUnsafeArtifact) {
			t.Errorf("unsafe path %s was not denied", unsafe)
		}
	}
	if err := os.Link(path, filepath.Join(repoDirectory, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := copyArtifact(context.Background(), root, destination, receiptID, artifactSelection{23, "linked.txt"}, 2, 1024); !errors.Is(err, errUnsafeArtifact) {
		t.Fatal("hard link was not denied")
	}
}

func verifyReceiptLab(t *testing.T, gitea, ownerToken string, browser *http.Client, bridgeURL, topicID string, repos []Repository, releaseID int64, releasedSHA, mergedSHA string, recovery *receiptRecoveryLab) {
	t.Helper()
	keyJSON := loadLabPage(t, browser, bridgeURL+"/bridge/api/receipt-key", nil, 200)
	var key map[string]string
	if err := json.Unmarshal([]byte(keyJSON), &key); err != nil {
		t.Fatal(err)
	}
	type response struct {
		envelope receiptEnvelope
		document receiptDocument
		raw      []byte
	}
	issue := func(requestKey, previous string, status int) response {
		t.Helper()
		payload, err := json.Marshal(receiptRequest{TopicID: topicID, PreviousID: previous,
			Sources:   []receiptSourceSelection{{RepositoryID: repos[0].ID, ReleaseID: releaseID, ExpectedSHA: releasedSHA}},
			Artifacts: []artifactSelection{{RepositoryID: repos[2].ID, Path: "bundle.txt"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest("POST", bridgeURL+"/bridge/api/receipts", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", bridgeURL)
		request.Header.Set("Idempotency-Key", requestKey)
		reply, err := browser.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer reply.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(reply.Body, 1<<20))
		if err != nil || reply.StatusCode != status {
			t.Fatalf("issue receipt: got %d, wanted %d: %s", reply.StatusCode, status, raw)
		}
		record := response{raw: raw}
		if status < 300 {
			if VerifyReceipt(raw, key["public_key"]) != nil || json.Unmarshal(raw, &record.envelope) != nil ||
				json.Unmarshal(record.envelope.Snapshot, &record.document) != nil {
				t.Fatal("could not verify signed receipt")
			}
		}
		return record
	}
	first := issue("first-receipt", "", 201)
	if repeated := issue("first-receipt", "", 200); !bytes.Equal(first.raw, repeated.raw) {
		t.Fatal("replayed request changed receipt bytes")
	}
	issue("another-first", "", 409)
	second := issue("corrected", first.document.ID, 201)
	issue("parallel-correction", first.document.ID, 409)
	if second.document.Version != 2 || second.document.PreviousDigest != first.envelope.Digest || second.document.PreviousID != first.document.ID {
		t.Fatal("correction did not link to the original digest and ID")
	}
	center := loadLabPage(t, browser, bridgeURL+"/bridge/receipts/", nil, 200)
	if !strings.Contains(center, `src="/bridge/receipts.js"`) {
		t.Fatal("receipt center is missing its embedded script")
	}
	historyURL := bridgeURL + "/bridge/api/receipts?topic_id=" + topicID
	var listing struct {
		Versions []receiptVersion
		CanIssue bool `json:"can_issue"`
	}
	if err := json.Unmarshal([]byte(loadLabPage(t, browser, historyURL, nil, 200)), &listing); err != nil || !listing.CanIssue ||
		len(listing.Versions) != 2 || listing.Versions[0].ID != second.document.ID || listing.Versions[1].ID != first.document.ID {
		t.Fatalf("unexpected receipt history: %+v %v", listing, err)
	}
	digest := sha256.Sum256([]byte("fixture delivery\n"))
	doc := first.document
	if len(doc.Artifacts) != 1 || doc.Artifacts[0].SHA256 != hex.EncodeToString(digest[:]) || len(doc.Members) != 3 ||
		doc.Members[0].Source == nil || doc.Members[0].Source.Commit != releasedSHA || doc.Members[0].Source.Tag != "build/v1" ||
		doc.Members[1].Status != "unverified" || doc.Members[1].Source != nil ||
		doc.Members[2].Source == nil || doc.Members[2].Source.Commit != mergedSHA ||
		doc.Members[2].Changes == nil || doc.Members[2].Changes.Paths == nil {
		t.Fatalf("incorrect evidence binding: %+v", doc)
	}
	if VerifyReceipt(bytes.Replace(first.raw, []byte(`"version":1`), []byte(`"version":9`), 1), key["public_key"]) == nil {
		t.Fatal("tampered receipt verified")
	}
	url := fmt.Sprintf("%s/bridge/api/receipts/%s", bridgeURL, doc.ID)
	anchorKey := recovery.anchorKey
	if anchorKey == "" {
		t.Fatal("independent test key is unavailable")
	}
	recovery.receiptKey = key["public_key"]
	for _, receipt := range []response{first, second} {
		proofURL := fmt.Sprintf("%s/bridge/api/receipts/%s?format=proof", bridgeURL, receipt.document.ID)
		deadline := time.Now().Add(20 * time.Second)
		for {
			response, err := browser.Get(proofURL)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := io.ReadAll(io.LimitReader(response.Body, 4096))
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode == 200 {
				if response.Header.Get("X-Bridge-Anchor-Status") != "verified" || VerifyReceiptAnchor(receipt.raw, proof, key["public_key"], anchorKey) != nil {
					t.Fatal("anchored receipt failed offline verification")
				}
				recovery.versions = append(recovery.versions, recoveredFixture{receipt.document.ID, receipt.raw, proof})
				break
			}
			if response.StatusCode != 409 || time.Now().After(deadline) {
				t.Fatalf("proof did not become available: %d", response.StatusCode)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if export := loadLabPage(t, browser, url+"?format=json", nil, 200); export != string(first.raw) {
		t.Fatal("correction overwrote old snapshot")
	}
	if markdown := loadLabPage(t, browser, url+"?format=markdown", nil, 200); !strings.Contains(markdown, doc.Artifacts[0].SHA256) ||
		!strings.Contains(markdown, first.envelope.Digest) || !strings.Contains(markdown, "未验证") {
		t.Fatal("markdown and signed snapshot differ")
	}
	collaborator := "/repos/labowner/conflict/collaborators/labreviewer"
	labAPI(t, gitea, ownerToken, "DELETE", collaborator, nil, nil, 204)
	loadLabPage(t, browser, url+"?format=json", nil, 404)
	loadLabPage(t, browser, url+"?format=markdown", nil, 404)
	loadLabPage(t, browser, url+"?format=proof", nil, 404)
	loadLabPage(t, browser, historyURL, nil, 404)
	issue("first-receipt", "", 404)
	labAPI(t, gitea, ownerToken, "PUT", collaborator, map[string]string{"permission": "write"}, nil, 204)
}
