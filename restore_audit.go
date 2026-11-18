package bridge

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RestoreAudit struct {
	Topics             int `json:"topics"`
	WebhookEvents      int `json:"webhook_events"`
	Receipts           int `json:"receipts"`
	Artifacts          int `json:"artifacts"`
	VerifiedAnchors    int `json:"verified_anchors"`
	PendingAnchors     int `json:"pending_anchors"`
	UnanchoredReceipts int `json:"unanchored_receipts"`
}

func openAuditDatabase(ctx context.Context, path string) (*sql.DB, error) {
	if !filepath.IsAbs(path) || strings.HasPrefix(filepath.ToSlash(path), "//") {
		return nil, errors.New("audit database path must be an absolute local path")
	}
	file, err := openArtifact(filepath.Dir(path), []string{filepath.Base(path)})
	if err != nil {
		return nil, err
	}
	defer file.Close()
	header := make([]byte, 16)
	if _, err := io.ReadFull(file, header); err != nil || string(header) != "SQLite format 3\x00" {
		return nil, errors.New("audit input is not a SQLite database")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); err == nil || !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("database still has SQLite sidecars; cleanly close all database connections before copying")
		}
	}
	slash := filepath.ToSlash(filepath.Clean(path))
	if !strings.HasPrefix(slash, "/") {
		slash = "/" + slash
	}
	address := url.URL{Scheme: "file", Path: slash, RawQuery: "mode=ro&immutable=1"}
	db, err := sql.Open("sqlite", address.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func auditDatabase(ctx context.Context, db *sql.DB, application, version int) error {
	var actual, schema int
	if err := db.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&actual); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&schema); err != nil {
		return err
	}
	if actual != application || schema != version {
		return errors.New("unexpected database application or schema version")
	}
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check(1)`).Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return errors.New("SQLite integrity check failed")
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("SQLite foreign key check failed")
	}
	return rows.Err()
}

func AuditRestoredBridge(ctx context.Context, database, snapshots, receiptKey, anchorKey, anchorDatabase string) (RestoreAudit, error) {
	var report RestoreAudit
	issuer, err := decodeReceiptPublicKey(receiptKey)
	if err != nil {
		return report, err
	}
	var anchor ed25519.PublicKey
	if anchorKey != "" {
		anchor, err = decodeReceiptPublicKey(anchorKey)
		if err != nil {
			return report, err
		}
		if issuer.Equal(anchor) {
			return report, errors.New("anchor key must be separate from receipt key")
		}
	}
	var ledger *sql.DB
	if anchorDatabase != "" {
		if anchor == nil {
			return report, errors.New("a trusted anchor key is required to audit the independent ledger")
		}
		ledger, err = openAuditDatabase(ctx, anchorDatabase)
		if err != nil {
			return report, err
		}
		defer ledger.Close()
		if err := auditDatabase(ctx, ledger, anchorApplicationID, anchorSchemaVersion); err != nil {
			return report, err
		}
		var key string
		if err := ledger.QueryRowContext(ctx, `SELECT public_key FROM anchor_identity`).Scan(&key); err != nil {
			return report, err
		}
		if key != anchorKey {
			return report, errors.New("restored ledger identity does not match the trusted key")
		}
	}
	if !filepath.IsAbs(snapshots) || strings.HasPrefix(filepath.ToSlash(snapshots), "//") {
		return report, errors.New("snapshot root must be an absolute local path")
	}
	root, err := os.Lstat(snapshots)
	if err != nil || !root.IsDir() {
		return report, errors.New("snapshot root must be an existing directory")
	}
	db, err := openAuditDatabase(ctx, database)
	if err != nil {
		return report, err
	}
	defer db.Close()
	if err := auditDatabase(ctx, db, bridgeApplicationID, bridgeSchemaVersion); err != nil {
		return report, err
	}
	if err := db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM topic_changesets),(SELECT COUNT(*) FROM webhook_inbox)`).Scan(&report.Topics, &report.WebhookEvents); err != nil {
		return report, err
	}
	rows, err := db.QueryContext(ctx, `
SELECT receipt.id, receipt.topic_id, receipt.issuer_id, receipt.envelope, receipt.previous_id,
       previous.topic_id, previous.envelope, pending.receipt_id, pending.proof
FROM delivery_receipts AS receipt
LEFT JOIN delivery_receipts AS previous ON previous.id=receipt.previous_id
LEFT JOIN receipt_anchor_outbox AS pending ON pending.receipt_id=receipt.id
ORDER BY receipt.rowid`)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, topicID, raw string
		var issuerID int64
		var previousID, previousTopic, previousRaw, pendingID, proof sql.NullString
		if err := rows.Scan(&id, &topicID, &issuerID, &raw, &previousID, &previousTopic, &previousRaw, &pendingID, &proof); err != nil {
			return report, err
		}
		envelope, doc, err := readVerifiedReceipt([]byte(raw), issuer)
		if err != nil || doc.ID != id || doc.TopicID != topicID || doc.IssuerID != issuerID || doc.PreviousID != previousID.String {
			return report, fmt.Errorf("receipt %q failed signature or row consistency checks", id)
		}
		switch {
		case previousID.Valid:
			prior, priorDoc, err := readVerifiedReceipt([]byte(previousRaw.String), issuer)
			if err != nil || previousTopic.String != topicID || priorDoc.TopicID != topicID || priorDoc.ID != previousID.String ||
				doc.Version != priorDoc.Version+1 || prior.Digest != doc.PreviousDigest {
				return report, fmt.Errorf("receipt %s has an invalid version link", id)
			}
		case doc.Version != 1 || doc.PreviousDigest != "":
			return report, fmt.Errorf("receipt %s has an invalid initial version", id)
		}
		switch {
		case proof.Valid:
			if anchor == nil || verifyAnchorProof([]byte(proof.String), anchorStatement{id, envelope.Digest, envelope.KeyID}, anchor) != nil {
				return report, fmt.Errorf("receipt %s has an invalid or untrusted independent proof", id)
			}
			if ledger != nil {
				if err := auditLedgerEntry(ctx, ledger, id, proof.String); err != nil {
					return report, err
				}
			}
			report.VerifiedAnchors++
		case pendingID.Valid:
			report.PendingAnchors++
		case doc.Scope.IndependentAnchor == "separate_proof_required":
			return report, fmt.Errorf("receipt %s is missing its required anchor queue entry", id)
		default:
			report.UnanchoredReceipts++
		}
		if len(doc.Artifacts) > 8 {
			return report, fmt.Errorf("receipt %s has too many artifacts", id)
		}
		for index, artifact := range doc.Artifacts {
			if artifact.Slot != index+1 || artifact.Size < 0 || artifact.Size > receiptArtifactLimit || !anchorDigestPattern.MatchString(artifact.SHA256) {
				return report, fmt.Errorf("receipt %s has invalid artifact metadata", id)
			}
			if err := auditSnapshot(ctx, snapshots, id, artifact); err != nil {
				return report, err
			}
			report.Artifacts++
		}
		report.Receipts++
	}
	return report, rows.Err()
}

func auditLedgerEntry(ctx context.Context, ledger *sql.DB, id, raw string) error {
	var proof anchorProof
	if err := json.Unmarshal([]byte(raw), &proof); err != nil {
		return err
	}
	var digest, keyID, recordedAt, signature string
	err := ledger.QueryRowContext(ctx, `SELECT receipt_sha,receipt_key_id,recorded_at,signature FROM anchors WHERE receipt_id=?`, id).
		Scan(&digest, &keyID, &recordedAt, &signature)
	if err != nil || digest != proof.Record.ReceiptSHA || keyID != proof.Record.ReceiptKeyID ||
		recordedAt != proof.Record.RecordedAt.Format(time.RFC3339Nano) || signature != proof.Signature {
		return fmt.Errorf("restored independent ledger does not match receipt %s", id)
	}
	return nil
}

func auditSnapshot(ctx context.Context, root, id string, artifact artifactSnapshot) error {
	name := fmt.Sprintf("%d.blob", artifact.Slot)
	file, err := openArtifact(root, []string{id, name})
	if err != nil {
		return fmt.Errorf("receipt %s artifact %d cannot be opened safely", id, artifact.Slot)
	}
	defer file.Close()
	hash := sha256.New()
	copied, err := io.Copy(hash, io.LimitReader(contextReader{ctx, file}, artifact.Size+1))
	if err != nil {
		return err
	}
	if copied != artifact.Size || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		return fmt.Errorf("receipt %s artifact %d has a different size or digest", id, artifact.Slot)
	}
	return nil
}
