package bridge

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestTopicStorePersistsAndReplays(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "台账 #1.db")
	store, err := OpenTopicStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var journal string
	if err := store.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("expected WAL: %q, %v", journal, err)
	}
	draft := TopicChangeSet{ID: "record-1", ProjectID: "cad", Topic: "CAD-01", CreatorID: 8, CreatedAt: time.Now().UTC(), Members: []TopicMember{{RepositoryID: 22, SourceBranch: "feature/cad", TargetBranch: "main"}, {RepositoryID: 23}}}
	differentUser := draft
	differentUser.CreatorID = 9
	differentUser.ID = "record-2"
	differentUser.Members = append([]TopicMember(nil), draft.Members...)
	created, isNew, err := store.create(ctx, draft, "send-1", "hash-1")
	if err != nil || !isNew || created.Version != 1 || created.Members[0].ID == 0 {
		t.Fatalf("create returned %#v, %v, %v", created, isNew, err)
	}
	if _, isNew, err := store.create(ctx, differentUser, "send-1", "hash-1"); err != nil || !isNew {
		t.Fatalf("same idempotency key under another actor: %v, %v", isNew, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenTopicStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.get(ctx, draft.ID)
	if err != nil || !reflect.DeepEqual(loaded, created) {
		t.Fatalf("reopened result: %#v, %v", loaded, err)
	}
	draft.ID = "retry"
	if repeated, isNew, err := reopened.create(ctx, draft, "send-1", "hash-1"); err != nil || isNew || !reflect.DeepEqual(repeated, loaded) {
		t.Fatalf("retry returned %#v, %v, %v", repeated, isNew, err)
	}
	if _, _, err := reopened.create(ctx, draft, "send-1", "changed"); !errors.Is(err, errIdempotencyConflict) {
		t.Fatalf("changed payload reused old key: %v", err)
	}
	if _, err := reopened.get(ctx, "retry"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("duplicate request wrote another topic: %v", err)
	}
	draft.Members = []TopicMember{{RepositoryID: 22}, {RepositoryID: 22}}
	if _, _, err := reopened.create(ctx, draft, "atomic", "hash"); err == nil {
		t.Fatal("invalid members did not fail")
	}
	if _, _, err := reopened.findRequest(ctx, draft.CreatorID, "atomic"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed insert left an idempotency record: %v", err)
	}
}

func TestTopicStoreDoesNotAlterAnUnownedDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "other.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE unrelated (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if store, err := OpenTopicStore(ctx, path); err == nil {
		store.Close()
		t.Fatal("opened an existing database without a Bridge application ID")
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "delete" {
		t.Fatalf("foreign database journal mode changed: %s, %v", mode, err)
	}
}
