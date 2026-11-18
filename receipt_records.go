package bridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var errReceiptSuperseded = errors.New("a correction has already been issued for this version")

type receiptDocument struct {
	Schema         int                `json:"schema"`
	ID             string             `json:"id"`
	PreviousID     string             `json:"previous_id,omitempty"`
	PreviousDigest string             `json:"previous_sha256,omitempty"`
	Version        int                `json:"version"`
	TopicID        string             `json:"topic_id"`
	Topic          string             `json:"topic"`
	TopicVersion   int                `json:"topic_version"`
	ProjectID      string             `json:"project_id"`
	IssuerID       int64              `json:"issuer_id"`
	IssuedAt       time.Time          `json:"issued_at"`
	Members        []receiptMember    `json:"members"`
	Artifacts      []artifactSnapshot `json:"artifacts"`
	Scope          receiptScope       `json:"verification_scope"`
}

type receiptScope struct {
	ArtifactProvenance string `json:"artifact_provenance"`
	TestResults        string `json:"test_results"`
	IndependentAnchor  string `json:"independent_anchor"`
}

type receiptMember struct {
	Repository         Repository         `json:"repository"`
	SourceRepositoryID int64              `json:"source_repository_id,omitempty"`
	Status             string             `json:"status"`
	Source             *sourceObservation `json:"source,omitempty"`
	Changes            *receiptChanges    `json:"changes,omitempty"`
}

type receiptChanges struct {
	Basis string    `json:"basis"`
	Paths *[]string `json:"paths"`
}

type receiptEnvelope struct {
	Snapshot  json.RawMessage `json:"snapshot"`
	Digest    string          `json:"sha256"`
	KeyID     string          `json:"key_id"`
	Signature string          `json:"signature"`
}

const receiptSignatureDomain = "gitea-topic-bridge/receipt/v1\x00"

func receiptKeyID(public ed25519.PublicKey) string {
	digest := sha256.Sum256(public)
	return hex.EncodeToString(digest[:])
}

func signReceipt(snapshot receiptDocument, private ed25519.PrivateKey) (receiptEnvelope, error) {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return receiptEnvelope{}, err
	}
	digest := sha256.Sum256(raw)
	signature := ed25519.Sign(private, append([]byte(receiptSignatureDomain), digest[:]...))
	return receiptEnvelope{Snapshot: raw, Digest: hex.EncodeToString(digest[:]), KeyID: receiptKeyID(private.Public().(ed25519.PublicKey)),
		Signature: base64.StdEncoding.EncodeToString(signature)}, nil
}

func VerifyReceipt(data []byte, trustedPublicKey string) error {
	key, err := base64.StdEncoding.DecodeString(trustedPublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("invalid trusted Ed25519 public key")
	}
	_, _, err = readVerifiedReceipt(data, ed25519.PublicKey(key))
	return err
}

func readVerifiedReceipt(data []byte, key ed25519.PublicKey) (receiptEnvelope, receiptDocument, error) {
	var envelope receiptEnvelope
	var document receiptDocument
	var normalized bytes.Buffer
	if json.Unmarshal(data, &envelope) != nil || json.Compact(&normalized, envelope.Snapshot) != nil || envelope.KeyID != receiptKeyID(key) {
		return envelope, document, errors.New("invalid receipt or wrong signing key")
	}
	digest := sha256.Sum256(normalized.Bytes())
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || hex.EncodeToString(digest[:]) != envelope.Digest ||
		!ed25519.Verify(key, append([]byte(receiptSignatureDomain), digest[:]...), signature) {
		return envelope, document, errors.New("receipt signature or digest does not match")
	}
	if err := json.Unmarshal(normalized.Bytes(), &document); err != nil || document.Schema != 1 || !topicIDPattern.MatchString(document.ID) ||
		!topicIDPattern.MatchString(document.TopicID) || len(document.Members) == 0 || document.Version < 1 {
		return envelope, document, errors.New("invalid signed receipt document")
	}
	envelope.Snapshot = append([]byte(nil), normalized.Bytes()...)
	return envelope, document, nil
}

func (store *TopicStore) receiptByID(ctx context.Context, id string) ([]byte, error) {
	var data string
	err := store.db.QueryRowContext(ctx, `SELECT envelope FROM delivery_receipts WHERE id=?`, id).Scan(&data)
	return []byte(data), err
}

func (store *TopicStore) receiptIDs(ctx context.Context, topicID, before string) ([]string, string, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT id FROM delivery_receipts WHERE topic_id=? AND (?='' OR rowid<(SELECT rowid FROM delivery_receipts WHERE topic_id=? AND id=?))
ORDER BY rowid DESC LIMIT 13`, topicID, before, topicID, before)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, "", err
		}
		ids = append(ids, id)
	}
	if len(ids) > 12 {
		return ids[:12], ids[11], rows.Err()
	}
	return ids, "", rows.Err()
}

func (store *TopicStore) receiptByRequest(ctx context.Context, who int64, key string) ([]byte, string, error) {
	var data, hash string
	err := store.db.QueryRowContext(ctx, `SELECT envelope, request_hash FROM delivery_receipts WHERE issuer_id=? AND request_key=?`, who, key).Scan(&data, &hash)
	return []byte(data), hash, err
}

func (store *TopicStore) insertReceipt(ctx context.Context, document receiptDocument, key, hash string, data []byte, anchor bool) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM delivery_receipts WHERE issuer_id=? AND request_key=?`, document.IssuerID, key).Scan(&existing); err == nil {
		return errIdempotencyConflict
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var previous any
	if document.PreviousID != "" {
		previous = document.PreviousID
		if err := tx.QueryRowContext(ctx, `SELECT id FROM delivery_receipts WHERE previous_id=?`, previous).Scan(&existing); err == nil {
			return errReceiptSuperseded
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	} else if err := tx.QueryRowContext(ctx, `SELECT id FROM delivery_receipts WHERE topic_id=? AND previous_id IS NULL`, document.TopicID).Scan(&existing); err == nil {
		return errReceiptSuperseded
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO delivery_receipts(id, topic_id, previous_id, issuer_id, request_key, request_hash, envelope)
VALUES (?, ?, ?, ?, ?, ?, ?)`, document.ID, document.TopicID, previous, document.IssuerID, key, hash, string(data)); err != nil {
		return err
	}
	if anchor {
		if _, err := tx.ExecContext(ctx, `INSERT INTO receipt_anchor_outbox(receipt_id) VALUES(?)`, document.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
