package bridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

type ReceiptAnchorConfig struct {
	URL       string `json:"url"`
	PublicKey string `json:"public_key"`
	Bearer    string `json:"-"`
}

type anchorClient struct {
	endpoint *url.URL
	public   ed25519.PublicKey
	bearer   string
	http     *http.Client
}

func newAnchorClient(config ReceiptAnchorConfig, issuer ed25519.PublicKey) (*anchorClient, error) {
	endpoint, err := baseURL(config.URL)
	if err != nil || endpoint.Path != "/v1/anchors" || endpoint.RawPath != "" || !validAnchorBearer(config.Bearer) {
		return nil, errors.New("anchor URL must be a fixed /v1/anchors endpoint with bearer credentials")
	}
	public, err := decodeReceiptPublicKey(config.PublicKey)
	if err != nil || issuer.Equal(public) {
		return nil, errors.New("configure a distinct trusted Ed25519 anchor key")
	}
	return &anchorClient{endpoint, public, config.Bearer, newUpstreamClient()}, nil
}

func (client *anchorClient) append(ctx context.Context, statement anchorStatement) ([]byte, string) {
	raw, err := json.Marshal(statement)
	if err != nil {
		return nil, "payload_invalid"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint.String(), bytes.NewReader(raw))
	if err != nil {
		return nil, "payload_invalid"
	}
	request.Header.Set("Authorization", "Bearer "+client.bearer)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return nil, "unavailable"
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		return nil, "digest_conflict"
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return nil, "unavailable"
	}
	proof, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(proof) > 4096 || verifyAnchorProof(proof, statement, client.public) != nil {
		return nil, "proof_invalid"
	}
	return proof, ""
}

type ReceiptAnchorWorker struct {
	store  *TopicStore
	issuer ed25519.PublicKey
	client *anchorClient
}

func NewReceiptAnchorWorker(config ServerConfig) (*ReceiptAnchorWorker, error) {
	if config.Receipts == nil || config.Receipts.Anchor == nil || config.TopicStore == nil {
		return nil, errors.New("receipt anchoring is not configured")
	}
	service, err := newReceiptService(*config.Receipts)
	if err != nil {
		return nil, err
	}
	clear(service.private)
	return &ReceiptAnchorWorker{config.TopicStore, service.public, service.anchor}, nil
}

func (worker *ReceiptAnchorWorker) workOnce(ctx context.Context, now time.Time) (bool, error) {
	lease, err := randomID()
	if err != nil {
		return false, err
	}
	var id string
	var attempts int
	err = worker.store.db.QueryRowContext(ctx, `
UPDATE receipt_anchor_outbox SET lease=?, attempts=attempts+1, available_at_ns=?
WHERE receipt_id=(SELECT receipt_id FROM receipt_anchor_outbox WHERE proof IS NULL AND available_at_ns<=? ORDER BY available_at_ns,receipt_id LIMIT 1)
RETURNING receipt_id,attempts`, lease, now.Add(time.Minute).UnixNano(), now.UnixNano()).Scan(&id, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	raw, err := worker.store.receiptByID(ctx, id)
	if err != nil {
		return true, err
	}
	envelope, doc, err := readVerifiedReceipt(raw, worker.issuer)
	var proof []byte
	failure := "snapshot_invalid"
	if err == nil && doc.ID == id {
		proof, failure = worker.client.append(ctx, anchorStatement{id, envelope.Digest, envelope.KeyID})
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if failure == "" {
		_, err = worker.store.db.ExecContext(persist, `
UPDATE receipt_anchor_outbox SET lease='',proof=?,last_error='' WHERE receipt_id=? AND lease=?`, string(proof), id, lease)
		return true, err
	}
	delay := min(10*time.Second<<min(attempts-1, 9), time.Hour)
	if ctx.Err() != nil {
		delay = 0
	}
	_, err = worker.store.db.ExecContext(persist, `
UPDATE receipt_anchor_outbox SET lease='',last_error=?,available_at_ns=? WHERE receipt_id=? AND lease=?`,
		failure, time.Now().Add(delay).UnixNano(), id, lease)
	if err == nil && ctx.Err() == nil && (attempts == 1 || attempts%10 == 0) {
		log.Printf("receipt anchor %s awaiting retry (%s; attempt %d)", id, failure, attempts)
	}
	return true, err
}

func (worker *ReceiptAnchorWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	initialized := false
	for ctx.Err() == nil {
		var worked bool
		var err error
		if !initialized {
			_, err = worker.store.db.ExecContext(ctx, `
INSERT INTO receipt_anchor_outbox(receipt_id)
SELECT id FROM delivery_receipts WHERE true ON CONFLICT(receipt_id) DO NOTHING`)
			initialized = err == nil
		}
		if initialized {
			worked, err = worker.workOnce(ctx, time.Now())
		}
		if err == nil && worked {
			continue
		}
		if err != nil && ctx.Err() == nil {
			log.Print("receipt anchor outbox could not be processed; will retry")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
