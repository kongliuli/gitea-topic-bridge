package bridge

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"
)

type AnchorLedger struct {
	db         *sql.DB
	private    ed25519.PrivateKey
	secretHash [32]byte
}

const (
	anchorApplicationID = 0x4754414C
	anchorSchemaVersion = 1
)

func OpenAnchorLedger(ctx context.Context, path, seed, bearer string) (*AnchorLedger, error) {
	key, err := base64.StdEncoding.DecodeString(seed)
	if err != nil || len(key) != ed25519.SeedSize || !validAnchorBearer(bearer) {
		return nil, errors.New("anchor needs a 32-byte base64 signing seed and a nonempty bearer secret")
	}
	defer clear(key)
	db, err := openLocalDatabase(path)
	if err != nil {
		return nil, err
	}
	ledger := &AnchorLedger{db: db, private: ed25519.NewKeyFromSeed(key), secretHash: sha256.Sum256([]byte("Bearer " + bearer))}
	if err := ledger.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return ledger, nil
}

func (ledger *AnchorLedger) Close() error { return ledger.db.Close() }

func (ledger *AnchorLedger) PublicKey() string {
	return base64.StdEncoding.EncodeToString(ledger.private.Public().(ed25519.PublicKey))
}

func (ledger *AnchorLedger) initialize(ctx context.Context) error {
	tx, err := ledger.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var application, version int
	if err := tx.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&application); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	switch {
	case application == 0 && version == 0:
		var tables int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'`).Scan(&tables); err != nil {
			return err
		}
		if tables != 0 {
			return errors.New("not an empty anchor database")
		}
		if _, err := tx.ExecContext(ctx, `
CREATE TABLE anchor_identity (public_key TEXT PRIMARY KEY) STRICT;
CREATE TABLE anchors (
    receipt_id TEXT PRIMARY KEY,
    receipt_sha TEXT NOT NULL,
    receipt_key_id TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    signature TEXT NOT NULL
) STRICT;
CREATE TRIGGER anchor_no_update BEFORE UPDATE ON anchors BEGIN SELECT RAISE(ABORT,'append only'); END;
CREATE TRIGGER anchor_no_delete BEFORE DELETE ON anchors BEGIN SELECT RAISE(ABORT,'append only'); END;
CREATE TRIGGER anchor_no_replace BEFORE INSERT ON anchors
WHEN EXISTS (SELECT 1 FROM anchors WHERE receipt_id=NEW.receipt_id)
BEGIN SELECT RAISE(ABORT,'append only'); END;
PRAGMA application_id = 0x4754414C;
PRAGMA user_version = 1;`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO anchor_identity VALUES(?)`, ledger.PublicKey()); err != nil {
			return err
		}
	case application == anchorApplicationID && version == anchorSchemaVersion:
		var public string
		if err := tx.QueryRowContext(ctx, `SELECT public_key FROM anchor_identity`).Scan(&public); err != nil {
			return err
		}
		if public != ledger.PublicKey() {
			return errors.New("anchor key does not match the ledger")
		}
	default:
		return errors.New("database does not belong to the independent anchor ledger")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return enableWAL(ctx, ledger.db)
}

func validAnchorBearer(bearer string) bool {
	return len(bearer) >= 32 && len(bearer) <= 512 && !strings.ContainsFunc(bearer, func(char rune) bool { return char < 0x21 || char > 0x7e })
}

var errAnchorConflict = errors.New("receipt ID already anchored to a different digest")

func (ledger *AnchorLedger) append(ctx context.Context, statement anchorStatement) (anchorProof, bool, error) {
	proof := anchorProof{KeyID: receiptKeyID(ledger.private.Public().(ed25519.PublicKey))}
	tx, err := ledger.db.BeginTx(ctx, nil)
	if err != nil {
		return proof, false, err
	}
	defer tx.Rollback()
	var recorded string
	proof.Record.ReceiptID = statement.ReceiptID
	err = tx.QueryRowContext(ctx, `SELECT receipt_sha, receipt_key_id, recorded_at, signature FROM anchors WHERE receipt_id=?`, statement.ReceiptID).
		Scan(&proof.Record.ReceiptSHA, &proof.Record.ReceiptKeyID, &recorded, &proof.Signature)
	if err == nil {
		if proof.Record.anchorStatement != statement {
			return proof, false, errAnchorConflict
		}
		proof.Record.RecordedAt, err = time.Parse(time.RFC3339Nano, recorded)
		return proof, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return proof, false, err
	}
	proof.Record = anchorRecord{statement, time.Now().UTC()}
	signable, err := anchorSignable(proof.Record)
	if err != nil {
		return proof, false, err
	}
	proof.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(ledger.private, signable))
	if _, err := tx.ExecContext(ctx, `INSERT INTO anchors VALUES(?, ?, ?, ?, ?)`,
		statement.ReceiptID, statement.ReceiptSHA, statement.ReceiptKeyID, proof.Record.RecordedAt.Format(time.RFC3339Nano), proof.Signature); err != nil {
		return proof, false, err
	}
	return proof, true, tx.Commit()
}

func (ledger *AnchorLedger) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/anchors", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		supplied := sha256.Sum256([]byte(request.Header.Get("Authorization")))
		if len(request.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare(supplied[:], ledger.secretHash[:]) != 1 {
			responseProblem(writer, 401, "bad_anchor_credentials")
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, 2048)
		var input anchorStatement
		if !decodeRequest(writer, request, &input) {
			return
		}
		if !input.valid() {
			responseProblem(writer, 400, "invalid_anchor_statement")
			return
		}
		proof, created, err := ledger.append(request.Context(), input)
		switch {
		case errors.Is(err, errAnchorConflict):
			responseProblem(writer, 409, "anchor_digest_conflict")
		case err != nil:
			responseProblem(writer, 503, "anchor_unavailable")
		case created:
			responseJSON(writer, 201, proof)
		default:
			responseJSON(writer, 200, proof)
		}
	})
	return mux
}
