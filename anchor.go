package bridge

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

const anchorSignatureDomain = "gitea-topic-bridge/anchor/v1\x00"

var anchorDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type anchorStatement struct {
	ReceiptID    string `json:"receipt_id"`
	ReceiptSHA   string `json:"receipt_sha256"`
	ReceiptKeyID string `json:"receipt_key_id"`
}

type anchorRecord struct {
	anchorStatement
	RecordedAt time.Time `json:"recorded_at"`
}

type anchorProof struct {
	Record    anchorRecord `json:"record"`
	KeyID     string       `json:"key_id"`
	Signature string       `json:"signature"`
}

func (statement anchorStatement) valid() bool {
	return topicIDPattern.MatchString(statement.ReceiptID) &&
		anchorDigestPattern.MatchString(statement.ReceiptSHA) && anchorDigestPattern.MatchString(statement.ReceiptKeyID)
}

func anchorSignable(record anchorRecord) ([]byte, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return append([]byte(anchorSignatureDomain), encoded...), nil
}

func verifyAnchorProof(raw []byte, statement anchorStatement, trusted ed25519.PublicKey) error {
	var proof anchorProof
	if json.Unmarshal(raw, &proof) != nil || proof.Record.anchorStatement != statement ||
		proof.Record.RecordedAt.IsZero() || proof.KeyID != receiptKeyID(trusted) {
		return errors.New("independent proof does not match the receipt or trusted anchor")
	}
	message, err := anchorSignable(proof.Record)
	signature, decodeErr := base64.StdEncoding.DecodeString(proof.Signature)
	if err != nil || decodeErr != nil || !ed25519.Verify(trusted, message, signature) {
		return errors.New("independent proof signature is invalid")
	}
	return nil
}

func decodeReceiptPublicKey(encoded string) (ed25519.PublicKey, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("invalid trusted Ed25519 public key")
	}
	return ed25519.PublicKey(key), nil
}

func VerifyReceiptAnchor(receipt, proof []byte, trustedReceiptKey, trustedAnchorKey string) error {
	issuer, err := decodeReceiptPublicKey(trustedReceiptKey)
	if err != nil {
		return err
	}
	anchor, err := decodeReceiptPublicKey(trustedAnchorKey)
	if err != nil {
		return err
	}
	if issuer.Equal(anchor) {
		return errors.New("anchor key must be independent of the receipt key")
	}
	envelope, doc, err := readVerifiedReceipt(receipt, issuer)
	if err != nil {
		return err
	}
	return verifyAnchorProof(proof, anchorStatement{doc.ID, envelope.Digest, envelope.KeyID}, anchor)
}
