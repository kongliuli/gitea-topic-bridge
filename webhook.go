package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	webhookBodyLimit    = 2 << 20
	webhookLease        = 90 * time.Second
	webhookPendingLimit = 4096
)

var deliveryPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

var (
	errDeliveryConflict = errors.New("delivery ID conflicts with an existing event")
	errInboxFull        = errors.New("webhook inbox is full")
)

type webhookJob struct {
	Delivery, Kind, BodySHA, Lease string
	RepoID, ObjectID               int64
	Attempts                       int
}

type Webhook struct {
	store  *TopicStore
	client *Gitea
	secret []byte
	repos  map[int64]Repository
	wake   chan struct{}
}

func NewWebhook(config ServerConfig, secret, readerToken string) (*Webhook, error) {
	if config.TopicStore == nil || len(secret) < 32 || len(secret) > 1024 || strings.TrimSpace(readerToken) == "" {
		return nil, errors.New("webhooks require a Bridge database, a signing secret of at least 32 bytes and a read-only Gitea token")
	}
	client, err := NewGitea(config.GiteaAPIURL, config.GiteaPublicURL, readerToken, 1)
	if err != nil {
		return nil, err
	}
	hook := &Webhook{store: config.TopicStore, client: client, secret: []byte(secret), repos: make(map[int64]Repository), wake: make(chan struct{}, 1)}
	for _, project := range config.Projects {
		for _, repo := range project.Repositories {
			if _, err := client.repoURL(repo); err != nil {
				return nil, err
			}
			if existing, ok := hook.repos[repo.ID]; ok && existing != repo {
				return nil, errors.New("conflicting webhook repository allowlist entries")
			}
			hook.repos[repo.ID] = repo
		}
	}
	if len(hook.repos) == 0 {
		return nil, errors.New("webhook repository allowlist is empty")
	}
	return hook, nil
}

func (hook *Webhook) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/hooks/gitea", hook.receive)
	return mux
}

func (hook *Webhook) receive(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if ip := net.ParseIP(host); err != nil || !ip.IsLoopback() {
		http.NotFound(writer, request)
		return
	}
	media, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || request.Header.Get("Content-Encoding") != "" {
		responseProblem(writer, http.StatusUnsupportedMediaType, "json_required")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, webhookBodyLimit))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			responseProblem(writer, http.StatusRequestEntityTooLarge, "webhook_too_large")
		} else {
			responseProblem(writer, http.StatusBadRequest, "invalid_webhook")
		}
		return
	}
	provided, err := hex.DecodeString(request.Header.Get("X-Gitea-Signature"))
	mac := hmac.New(sha256.New, hook.secret)
	mac.Write(raw)
	if err != nil || len(request.Header.Values("X-Gitea-Signature")) != 1 || !hmac.Equal(mac.Sum(nil), provided) {
		responseProblem(writer, http.StatusUnauthorized, "invalid_webhook_signature")
		return
	}
	job := webhookJob{Delivery: strings.ToLower(request.Header.Get("X-Gitea-Delivery")), Kind: request.Header.Get("X-Gitea-Event")}
	if !deliveryPattern.MatchString(job.Delivery) || len(request.Header.Values("X-Gitea-Delivery")) != 1 || len(request.Header.Values("X-Gitea-Event")) != 1 {
		responseProblem(writer, http.StatusBadRequest, "invalid_webhook_headers")
		return
	}
	if job.Kind != "pull_request" && job.Kind != "release" {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	var payload struct {
		Repository struct{ ID int64 }
		Number     int64
		Pull       *struct {
			Number int64
			Base   apiPullBranch
		} `json:"pull_request"`
		Release *struct{ ID int64 }
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		responseProblem(writer, http.StatusBadRequest, "invalid_webhook")
		return
	}
	job.RepoID = payload.Repository.ID
	switch {
	case job.Kind == "pull_request" && payload.Pull != nil && payload.Release == nil &&
		payload.Number == payload.Pull.Number && payload.Pull.Base.RepoID == job.RepoID:
		job.ObjectID = payload.Number
	case job.Kind == "release" && payload.Release != nil && payload.Pull == nil:
		job.ObjectID = payload.Release.ID
	}
	if job.ObjectID <= 0 || job.RepoID <= 0 {
		responseProblem(writer, http.StatusBadRequest, "invalid_webhook")
		return
	}
	if _, allowed := hook.repos[job.RepoID]; !allowed {
		http.NotFound(writer, request)
		return
	}
	sum := sha256.Sum256(raw)
	job.BodySHA = hex.EncodeToString(sum[:])
	switch err := hook.store.enqueueWebhook(request.Context(), job, time.Now()); {
	case errors.Is(err, errDeliveryConflict):
		responseProblem(writer, http.StatusConflict, "webhook_delivery_conflict")
		return
	case err != nil:
		writer.Header().Set("Retry-After", "10")
		responseProblem(writer, http.StatusServiceUnavailable, "webhook_not_queued")
		return
	}
	select {
	case hook.wake <- struct{}{}:
	default:
	}
	writer.WriteHeader(http.StatusAccepted)
}

func (store *TopicStore) enqueueWebhook(ctx context.Context, job webhookJob, now time.Time) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing webhookJob
	err = tx.QueryRowContext(ctx, `SELECT kind, repo_id, object_id, body_sha FROM webhook_inbox WHERE delivery_id=?`, job.Delivery).
		Scan(&existing.Kind, &existing.RepoID, &existing.ObjectID, &existing.BodySHA)
	if err == nil {
		if existing.Kind != job.Kind || existing.RepoID != job.RepoID || existing.ObjectID != job.ObjectID || existing.BodySHA != job.BodySHA {
			return errDeliveryConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM webhook_inbox WHERE observation IS NULL`).Scan(&pending); err != nil {
		return err
	}
	if pending >= webhookPendingLimit {
		return errInboxFull
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO webhook_inbox(delivery_id, kind, repo_id, object_id, body_sha, received_at_ns, available_at_ns)
VALUES (?, ?, ?, ?, ?, ?, ?)`, job.Delivery, job.Kind, job.RepoID, job.ObjectID, job.BodySHA, now.UnixNano(), now.UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *TopicStore) claimWebhook(ctx context.Context, now time.Time) (webhookJob, error) {
	var job webhookJob
	lease, err := randomID()
	if err != nil {
		return job, err
	}
	job.Lease = lease
	err = store.db.QueryRowContext(ctx, `
UPDATE webhook_inbox SET lease=?, available_at_ns=?, attempts=attempts+1
WHERE delivery_id=(SELECT delivery_id FROM webhook_inbox WHERE observation IS NULL AND available_at_ns<=?
    ORDER BY available_at_ns, received_at_ns, delivery_id LIMIT 1)
RETURNING delivery_id, kind, repo_id, object_id, attempts`, lease, now.Add(webhookLease).UnixNano(), now.UnixNano()).
		Scan(&job.Delivery, &job.Kind, &job.RepoID, &job.ObjectID, &job.Attempts)
	return job, err
}

func (hook *Webhook) workOnce(ctx context.Context, now time.Time) (bool, error) {
	job, err := hook.store.claimWebhook(ctx, now)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	readCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	observation, readErr := hook.observe(readCtx, job)
	cancel()
	persist, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	if readErr == nil {
		data, err := json.Marshal(observation)
		if err != nil {
			return false, err
		}
		_, err = hook.store.db.ExecContext(persist, `
UPDATE webhook_inbox SET observation=?, lease='', last_error='' WHERE delivery_id=? AND lease=?`, string(data), job.Delivery, job.Lease)
		return true, err
	}
	code := "upstream_unavailable"
	switch {
	case errors.Is(readErr, ErrUnauthorized), errors.Is(readErr, ErrNotVisible):
		code = "reader_access_required"
	case ctx.Err() != nil:
		code = "interrupted"
	}
	delay := min(10*time.Second<<min(job.Attempts-1, 9), time.Hour)
	if code == "interrupted" {
		delay = 0
	}
	_, err = hook.store.db.ExecContext(persist, `
UPDATE webhook_inbox SET lease='', last_error=?, available_at_ns=? WHERE delivery_id=? AND lease=?`,
		code, time.Now().Add(delay).UnixNano(), job.Delivery, job.Lease)
	if err == nil && (job.Attempts == 1 || job.Attempts%10 == 0) && ctx.Err() == nil {
		log.Printf("Gitea webhook %s waiting for retry (%s; attempt %d)", job.Delivery, code, job.Attempts)
	}
	return true, err
}

func (hook *Webhook) Run(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		worked, err := hook.workOnce(ctx, time.Now())
		if err != nil && ctx.Err() == nil {
			log.Print("Gitea webhook worker could not update its inbox; will retry")
		}
		if worked && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-hook.wake:
		}
	}
}
