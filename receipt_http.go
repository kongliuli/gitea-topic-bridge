package bridge

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const receiptArtifactLimit = 256 << 20

type ReceiptConfig struct {
	ArtifactRoot string               `json:"artifact_root"`
	SnapshotRoot string               `json:"snapshot_root"`
	Issuers      []int64              `json:"issuers"`
	SigningSeed  string               `json:"-"`
	Anchor       *ReceiptAnchorConfig `json:"anchor,omitempty"`
}

type receiptService struct {
	artifactRoot, snapshotRoot string
	private                    ed25519.PrivateKey
	public                     ed25519.PublicKey
	issuers                    map[int64]bool
	anchor                     *anchorClient
}

func newReceiptService(config ReceiptConfig) (*receiptService, error) {
	seed, err := base64.StdEncoding.DecodeString(config.SigningSeed)
	if err != nil || len(seed) != ed25519.SeedSize || len(config.Issuers) == 0 {
		return nil, errors.New("receipts require a base64 Ed25519 seed and at least one issuer")
	}
	defer clear(seed)
	resolve := func(path string) (string, error) {
		if !filepath.IsAbs(path) || strings.HasPrefix(filepath.ToSlash(path), "//") {
			return "", errors.New("receipt directories must be absolute local paths")
		}
		return filepath.EvalSymlinks(path)
	}
	input, err := resolve(config.ArtifactRoot)
	if err != nil {
		return nil, err
	}
	output, err := resolve(config.SnapshotRoot)
	if err != nil {
		return nil, err
	}
	within := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && filepath.IsLocal(rel)
	}
	if within(input, output) || within(output, input) {
		return nil, errors.New("artifact and snapshot roots must not overlap")
	}
	for _, root := range []string{config.ArtifactRoot, output} {
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return nil, errors.New("receipt roots must be existing directories")
		}
	}
	private := ed25519.NewKeyFromSeed(seed)
	service := &receiptService{artifactRoot: config.ArtifactRoot, snapshotRoot: output, private: private, public: private.Public().(ed25519.PublicKey), issuers: make(map[int64]bool)}
	if config.Anchor != nil {
		service.anchor, err = newAnchorClient(*config.Anchor, service.public)
		if err != nil {
			return nil, err
		}
	}
	for _, id := range config.Issuers {
		if id <= 0 {
			return nil, errors.New("receipt issuer IDs must be positive")
		}
		service.issuers[id] = true
	}
	return service, nil
}

type receiptRequest struct {
	TopicID    string                   `json:"topic_id"`
	PreviousID string                   `json:"previous_id"`
	Sources    []receiptSourceSelection `json:"sources"`
	Artifacts  []artifactSelection      `json:"artifacts"`
}

func (server *Server) receiptProblem(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, ErrNotVisible):
		responseProblem(writer, 404, "receipt_not_found")
	case errors.Is(err, errIdempotencyConflict):
		responseProblem(writer, 409, "idempotency_conflict")
	case errors.Is(err, errReceiptSuperseded):
		responseProblem(writer, 409, "receipt_superseded")
	case errors.Is(err, errReceiptSourceMismatch):
		responseProblem(writer, 409, "release_source_changed")
	case errors.Is(err, errUnsafeArtifact):
		responseProblem(writer, 422, "artifact_unavailable")
	default:
		server.upstreamProblem(writer, request, err)
	}
}

func (server *Server) loadAuthorizedReceipt(ctx context.Context, client *Gitea, id string, raw []byte) (receiptEnvelope, receiptDocument, error) {
	envelope, doc, err := readVerifiedReceipt(raw, server.receipts.public)
	if err != nil {
		return receiptEnvelope{}, receiptDocument{}, err
	}
	if doc.ID != id {
		return receiptEnvelope{}, receiptDocument{}, ErrNotVisible
	}
	members := make([]TopicMember, len(doc.Members))
	for index, member := range doc.Members {
		members[index].RepositoryID = member.Repository.ID
		members[index].SourceRepositoryID = member.SourceRepositoryID
	}
	repos, err := server.authorizedTopicRepos(ctx, client, TopicChangeSet{ProjectID: doc.ProjectID, Members: members})
	if err != nil {
		return receiptEnvelope{}, receiptDocument{}, err
	}
	for index, member := range doc.Members {
		if member.Source != nil && member.Source.Kind == "release" {
			if err := client.canReadReceiptRelease(ctx, repos[index], member.Source.ObjectID); err != nil {
				return receiptEnvelope{}, receiptDocument{}, err
			}
		}
	}
	return envelope, doc, nil
}

func (server *Server) existingReceipt(writer http.ResponseWriter, request *http.Request, client *Gitea, actor int64, key, hash string) bool {
	raw, storedHash, err := server.config.TopicStore.receiptByRequest(request.Context(), actor, key)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		server.receiptProblem(writer, request, err)
		return true
	}
	_, record, err := readVerifiedReceipt(raw, server.receipts.public)
	if err == nil {
		_, _, err = server.loadAuthorizedReceipt(request.Context(), client, record.ID, raw)
	}
	if err == nil && hash != storedHash {
		err = errIdempotencyConflict
	}
	if err != nil {
		server.receiptProblem(writer, request, err)
	} else {
		writeReceipt(writer, 200, record.ID, raw)
	}
	return true
}

func (server *Server) createReceipt(writer http.ResponseWriter, request *http.Request, client *Gitea, user GiteaUser) {
	if !server.checkOrigin(writer, request) {
		return
	}
	if !server.receipts.issuers[user.ID] {
		responseProblem(writer, 403, "receipt_issuer_required")
		return
	}
	key := request.Header.Get("Idempotency-Key")
	var input receiptRequest
	if !requestKeyPattern.MatchString(key) {
		responseProblem(writer, 400, "invalid_idempotency_key")
		return
	}
	if !decodeRequest(writer, request, &input) {
		return
	}
	if !topicIDPattern.MatchString(input.TopicID) || input.PreviousID != "" && !topicIDPattern.MatchString(input.PreviousID) ||
		len(input.Artifacts) > 8 || len(input.Sources) > 16 {
		responseProblem(writer, 400, "invalid_receipt")
		return
	}
	requestBytes, err := json.Marshal(input)
	if err != nil {
		server.receiptProblem(writer, request, err)
		return
	}
	requestSum := sha256.Sum256(requestBytes)
	hash := hex.EncodeToString(requestSum[:])
	if server.existingReceipt(writer, request, client, user.ID, key, hash) {
		return
	}
	ctx := request.Context()
	store := server.config.TopicStore
	topic, err := store.get(ctx, input.TopicID)
	if err != nil {
		server.receiptProblem(writer, request, err)
		return
	}
	repos, err := server.authorizedTopicRepos(ctx, client, topic)
	if err != nil {
		server.receiptProblem(writer, request, err)
		return
	}
	version := 1
	previousDigest := ""
	if input.PreviousID != "" {
		raw, err := store.receiptByID(ctx, input.PreviousID)
		if err != nil {
			server.receiptProblem(writer, request, err)
			return
		}
		priorEnvelope, prior, err := server.loadAuthorizedReceipt(ctx, client, input.PreviousID, raw)
		if err != nil {
			server.receiptProblem(writer, request, err)
			return
		}
		if prior.TopicID != topic.ID {
			responseProblem(writer, 409, "receipt_topic_mismatch")
			return
		}
		version = prior.Version + 1
		previousDigest = priorEnvelope.Digest
	}
	allowed := make(map[int64]bool)
	for _, repo := range repos {
		allowed[repo.ID] = true
	}
	selections := make(map[int64]receiptSourceSelection)
	for _, source := range input.Sources {
		if !allowed[source.RepositoryID] || selections[source.RepositoryID].ReleaseID != 0 || source.ReleaseID < 1 || !commitSHA.MatchString(source.ExpectedSHA) {
			responseProblem(writer, 400, "invalid_receipt_sources")
			return
		}
		selections[source.RepositoryID] = source
	}
	seenArtifacts := make(map[string]bool)
	for _, artifact := range input.Artifacts {
		artifactKey := strconv.FormatInt(artifact.RepositoryID, 10) + "/" + strings.ToLower(artifact.Path)
		if _, ok := artifactParts(artifact.Path); !ok || !allowed[artifact.RepositoryID] || seenArtifacts[artifactKey] {
			responseProblem(writer, 400, "invalid_receipt_artifacts")
			return
		}
		seenArtifacts[artifactKey] = true
	}
	id, err := randomID()
	if err != nil {
		server.receiptProblem(writer, request, err)
		return
	}
	doc := receiptDocument{Schema: 1, ID: id, PreviousID: input.PreviousID, PreviousDigest: previousDigest, Version: version, TopicID: topic.ID, Topic: topic.Topic,
		TopicVersion: topic.Version, ProjectID: topic.ProjectID, IssuerID: user.ID, Artifacts: make([]artifactSnapshot, 0),
		Scope: receiptScope{ArtifactProvenance: "unverified", TestResults: "unverified", IndependentAnchor: "not_configured"}}
	if server.receipts.anchor != nil {
		doc.Scope.IndependentAnchor = "separate_proof_required"
	}
	for index, repo := range repos {
		member, err := client.receiptSource(ctx, repo, topic.Members[index], selections[repo.ID])
		if err != nil {
			server.receiptProblem(writer, request, err)
			return
		}
		doc.Members = append(doc.Members, member)
	}
	issued, err := server.snapshotAndSaveReceipt(ctx, client, topic, doc, input.Artifacts, key, hash)
	if errors.Is(err, errIdempotencyConflict) && server.existingReceipt(writer, request, client, user.ID, key, hash) {
		return
	}
	if err != nil {
		server.receiptProblem(writer, request, err)
		return
	}
	writeReceipt(writer, 201, id, issued)
}

func (server *Server) snapshotAndSaveReceipt(ctx context.Context, client *Gitea, topic TopicChangeSet, doc receiptDocument, selections []artifactSelection, key, hash string) ([]byte, error) {
	local, err := os.OpenRoot(server.receipts.snapshotRoot)
	if err != nil {
		return nil, err
	}
	defer local.Close()
	if err := local.Mkdir(doc.ID, 0700); err != nil {
		return nil, err
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			local.RemoveAll(doc.ID)
		}
	}()
	var remaining int64 = receiptArtifactLimit
	for index, selection := range selections {
		evidence, err := copyArtifact(ctx, server.receipts.artifactRoot, local, doc.ID, selection, index+1, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= evidence.Size
		doc.Artifacts = append(doc.Artifacts, evidence)
	}
	if _, err := server.authorizedTopicRepos(ctx, client, topic); err != nil {
		return nil, err
	}
	doc.IssuedAt = time.Now().UTC()
	envelope, err := signReceipt(doc, server.receipts.private)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	if len(raw) > 16<<20 {
		return nil, errors.New("receipt exceeds 16 MiB")
	}
	removeStaging = false
	err = server.config.TopicStore.insertReceipt(ctx, doc, key, hash, raw, server.receipts.anchor != nil)
	if errors.Is(err, errIdempotencyConflict) || errors.Is(err, errReceiptSuperseded) {
		removeStaging = true
	}
	return raw, err
}

func writeReceipt(writer http.ResponseWriter, status int, id string, raw []byte) {
	writer.Header().Set("Location", "/bridge/api/receipts/"+id)
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	writer.Write(raw)
}

func (server *Server) receiptKey(writer http.ResponseWriter, _ *http.Request) {
	responseJSON(writer, 200, map[string]string{
		"algorithm": "Ed25519", "key_id": receiptKeyID(server.receipts.public),
		"public_key": base64.StdEncoding.EncodeToString(server.receipts.public),
	})
}

func (server *Server) getReceipt(writer http.ResponseWriter, request *http.Request, client *Gitea, _ GiteaUser) {
	id := request.PathValue("id")
	if !topicIDPattern.MatchString(id) {
		server.receiptProblem(writer, request, ErrNotVisible)
		return
	}
	raw, err := server.config.TopicStore.receiptByID(request.Context(), id)
	if err != nil {
		server.receiptProblem(writer, request, err)
		return
	}
	envelope, doc, err := server.loadAuthorizedReceipt(request.Context(), client, id, raw)
	if err != nil {
		server.receiptProblem(writer, request, err)
		return
	}
	format := request.URL.Query().Get("format")
	status, proof, err := server.receiptAnchorStatus(request.Context(), doc.ID, envelope)
	if err != nil {
		server.receiptProblem(writer, request, err)
		return
	}
	writer.Header().Set("X-Bridge-Anchor-Status", status)
	switch format {
	case "proof":
		if proof == nil {
			responseProblem(writer, 409, "anchor_proof_unavailable")
			return
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.Header().Set("Content-Disposition", `attachment; filename="receipt-`+id+`.proof.json"`)
		writer.Write(proof)
	case "markdown":
		writer.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		writer.Header().Set("Content-Disposition", `attachment; filename="receipt-`+id+`.md"`)
		writer.Write(receiptMarkdown(doc, envelope, status))
	case "json", "":
		if request.URL.Query().Get("format") == "json" {
			writer.Header().Set("Content-Disposition", `attachment; filename="receipt-`+id+`.json"`)
		}
		writeReceipt(writer, 200, id, raw)
	default:
		responseProblem(writer, 400, "invalid_receipt_format")
	}
}

func (server *Server) receiptAnchorStatus(ctx context.Context, id string, envelope receiptEnvelope) (string, []byte, error) {
	var proof sql.NullString
	err := server.config.TopicStore.db.QueryRowContext(ctx, `SELECT proof FROM receipt_anchor_outbox WHERE receipt_id=?`, id).Scan(&proof)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "not_configured", nil, nil
	case err != nil:
		return "", nil, err
	case server.receipts.anchor == nil:
		return "check_unavailable", nil, nil
	case !proof.Valid:
		return "pending", nil, nil
	case verifyAnchorProof([]byte(proof.String), anchorStatement{id, envelope.Digest, envelope.KeyID}, server.receipts.anchor.public) != nil:
		return "proof_invalid", nil, nil
	default:
		return "verified", []byte(proof.String), nil
	}
}

func receiptMarkdown(doc receiptDocument, envelope receiptEnvelope, anchorStatus string) []byte {
	var output strings.Builder
	fmt.Fprintf(&output, "# 交付回执 %s · v%d\n\n- 回执：%s\n- 联调单：%s · v%d\n- 签发账号：%d\n- 签发时间：%s\n- JSON 快照 SHA-256：%s\n- 签名密钥：%s\n- Ed25519 签名：%s\n",
		doc.Topic, doc.Version, doc.ID, doc.TopicID, doc.TopicVersion, doc.IssuerID, doc.IssuedAt.Format(time.RFC3339), envelope.Digest, envelope.KeyID, envelope.Signature)
	if doc.PreviousID != "" {
		fmt.Fprintf(&output, "- 更正自：%s（SHA-256 %s）\n", doc.PreviousID, doc.PreviousDigest)
	}
	output.WriteString("\n## 仓库与变更\n\n")
	for _, member := range doc.Members {
		fmt.Fprintf(&output, "### %s/%s\n\n", member.Repository.Owner, member.Repository.Name)
		if member.Source == nil {
			output.WriteString("未验证源提交。\n\n")
			continue
		}
		source := member.Source
		fmt.Fprintf(&output, "- %s #%d · 提交 %s\n- 执行人：%d\n- 执行时间：%s\n", source.Kind, source.ObjectID, source.Commit, source.ActorID, source.OccurredAt.Format(time.RFC3339))
		if source.Tag != "" {
			fmt.Fprintf(&output, "- 标签：%s\n", strconv.QuoteToASCII(source.Tag))
		}
		if member.Changes == nil || member.Changes.Paths == nil {
			output.WriteString("- 变更清单：未验证\n\n")
			continue
		}
		output.WriteString("- 变更范围：源提交相对第一父提交（根提交相对空树）\n\n```text\n")
		for _, path := range *member.Changes.Paths {
			fmt.Fprintln(&output, strconv.QuoteToASCII(path))
		}
		output.WriteString("```\n\n")
	}
	output.WriteString("## 制品快照\n\n")
	if len(doc.Artifacts) == 0 {
		output.WriteString("未提供制品。\n")
	}
	for _, artifact := range doc.Artifacts {
		fmt.Fprintf(&output, "- 仓库 %d · %s · %d 字节 · SHA-256 %s\n", artifact.RepositoryID, strconv.QuoteToASCII(artifact.Path), artifact.Size, artifact.SHA256)
	}
	fmt.Fprintf(&output, "\n独立存证状态：%s。独立签名凭证需单独下载和验证。\n", anchorStatus)
	output.WriteString("\n签名对应 JSON 快照。制品哈希证明复制的字节，不证明制品由所列提交构建；未验证测试范围。\n")
	return []byte(output.String())
}
