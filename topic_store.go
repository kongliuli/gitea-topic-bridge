package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var errIdempotencyConflict = errors.New("idempotency key was used by a different request")

type TopicStore struct{ db *sql.DB }

type TopicChangeSet struct {
	ID        string        `json:"id"`
	ProjectID string        `json:"project_id"`
	Topic     string        `json:"topic"`
	Version   int           `json:"version"`
	CreatorID int64         `json:"creator_id"`
	CreatedAt time.Time     `json:"created_at"`
	Members   []TopicMember `json:"members"`
}

type TopicMember struct {
	ID           int64       `json:"id"`
	RepositoryID int64       `json:"repository_id"`
	SourceBranch string      `json:"source_branch,omitempty"`
	TargetBranch string      `json:"target_branch,omitempty"`
	State        string      `json:"state"`
	PullNumber   int64       `json:"pull_number,omitempty"`
	ResultCode   string      `json:"result_code,omitempty"`
	ActorID      int64       `json:"actor_id,omitempty"`
	UpdatedAt    *time.Time  `json:"updated_at,omitempty"`
	Merge        *MergeState `json:"merge,omitempty"`
}

func OpenTopicStore(ctx context.Context, path string) (*TopicStore, error) {
	if !filepath.IsAbs(path) || strings.HasPrefix(filepath.ToSlash(path), "//") {
		return nil, errors.New("database path must be an absolute local path")
	}
	slashPath := filepath.ToSlash(filepath.Clean(path))
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	filename := &url.URL{Scheme: "file", Path: slashPath}
	filename.RawQuery = url.Values{"_pragma": {"busy_timeout(5000)", "foreign_keys(1)", "synchronous(FULL)"}, "_txlock": {"immediate"}}.Encode()
	db, err := sql.Open("sqlite", filename.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := prepareTopicStore(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &TopicStore{db: db}, nil
}

func prepareTopicStore(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const bridgeApplicationID = 0x47544252
	var version, applicationID int
	if err := tx.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&applicationID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	switch version {
	case 0:
		if applicationID != 0 {
			return errors.New("refusing to initialize a database owned by another application")
		}
		var objects int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'`).Scan(&objects); err != nil {
			return err
		}
		if objects != 0 {
			return errors.New("refusing to use a nonempty database without a Bridge schema version")
		}
		if _, err := tx.ExecContext(ctx, `
CREATE TABLE topic_changesets (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    topic TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version >= 1),
    creator_id INTEGER NOT NULL CHECK (creator_id > 0),
    created_at_ns INTEGER NOT NULL,
    request_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    UNIQUE (creator_id, request_key)
) STRICT;
CREATE TABLE topic_members (
    id INTEGER PRIMARY KEY,
    topic_id TEXT NOT NULL REFERENCES topic_changesets(id),
    repo_id INTEGER NOT NULL CHECK (repo_id > 0),
    source_branch TEXT NOT NULL,
    target_branch TEXT NOT NULL,
    state TEXT NOT NULL,
    UNIQUE (topic_id, repo_id)
) STRICT;
PRAGMA application_id=0x47544252;
PRAGMA user_version=1;`); err != nil {
			return err
		}
		fallthrough
	case 1, 2, 3:
		if version != 0 && applicationID != bridgeApplicationID {
			return errors.New("database is not owned by Bridge")
		}
	default:
		return fmt.Errorf("unsupported Bridge database version %d", version)
	}
	if version < 2 {
		if _, err := tx.ExecContext(ctx, `
ALTER TABLE topic_members ADD COLUMN pull_number INTEGER NOT NULL DEFAULT 0;
ALTER TABLE topic_members ADD COLUMN result_code TEXT NOT NULL DEFAULT '';
ALTER TABLE topic_members ADD COLUMN actor_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE topic_members ADD COLUMN updated_at_ns INTEGER NOT NULL DEFAULT 0;
PRAGMA user_version=2;`); err != nil {
			return err
		}
	}
	if version < 3 {
		if _, err := tx.ExecContext(ctx, `
CREATE TABLE pull_merges (
    repo_id INTEGER NOT NULL,
    pull_number INTEGER NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0),
    state TEXT NOT NULL CHECK (state IN ('unconfirmed', 'blocked', 'merged')),
    result_code TEXT NOT NULL,
    expected_head TEXT NOT NULL,
    requested_by INTEGER NOT NULL,
    merge_sha TEXT NOT NULL,
    merged_by INTEGER NOT NULL,
    merged_at_ns INTEGER NOT NULL,
    PRIMARY KEY (repo_id, pull_number)
) STRICT;
CREATE INDEX topic_members_pull ON topic_members(repo_id, pull_number);
PRAGMA user_version=3;`); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var journal string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&journal); err != nil {
		return err
	}
	if journal != "wal" {
		return errors.New("SQLite could not enable WAL for Bridge storage")
	}
	return nil
}

func (store *TopicStore) Close() error { return store.db.Close() }

type topicReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const selectTopic = `SELECT id, project_id, topic, version, creator_id, created_at_ns, request_hash FROM topic_changesets`

func readTopic(ctx context.Context, reader topicReader, filter string, args ...any) (TopicChangeSet, string, error) {
	var topic TopicChangeSet
	var nanos int64
	var requestHash string
	if err := reader.QueryRowContext(ctx, selectTopic+filter, args...).Scan(&topic.ID, &topic.ProjectID, &topic.Topic, &topic.Version, &topic.CreatorID, &nanos, &requestHash); err != nil {
		return TopicChangeSet{}, "", err
	}
	topic.CreatedAt = time.Unix(0, nanos).UTC()
	rows, err := reader.QueryContext(ctx, `
SELECT members.id, members.repo_id, members.source_branch, members.target_branch, members.state,
    members.pull_number, members.result_code, members.actor_id, members.updated_at_ns,
    COALESCE(merge.revision, 0), COALESCE(merge.state, ''), COALESCE(merge.result_code, ''),
    COALESCE(merge.expected_head, ''), COALESCE(merge.requested_by, 0), COALESCE(merge.merge_sha, ''),
    COALESCE(merge.merged_by, 0), COALESCE(merge.merged_at_ns, 0)
FROM topic_members AS members
LEFT JOIN pull_merges AS merge ON merge.repo_id = members.repo_id AND merge.pull_number = members.pull_number
WHERE members.topic_id = ? ORDER BY members.id`, topic.ID)
	if err != nil {
		return TopicChangeSet{}, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var member TopicMember
		var merge MergeState
		var updated, mergedAt int64
		if err := rows.Scan(&member.ID, &member.RepositoryID, &member.SourceBranch, &member.TargetBranch, &member.State, &member.PullNumber, &member.ResultCode, &member.ActorID, &updated,
			&merge.revision, &merge.State, &merge.ResultCode, &merge.ExpectedHead, &merge.RequestedBy, &merge.MergeSHA, &merge.MergedBy, &mergedAt); err != nil {
			return TopicChangeSet{}, "", err
		}
		if merge.revision > 0 {
			if mergedAt != 0 {
				value := time.Unix(0, mergedAt).UTC()
				merge.MergedAt = &value
			}
			member.Merge = &merge
		}
		if updated != 0 {
			value := time.Unix(0, updated).UTC()
			member.UpdatedAt = &value
		}
		topic.Members = append(topic.Members, member)
	}
	if err := rows.Err(); err != nil {
		return TopicChangeSet{}, "", err
	}
	if len(topic.Members) == 0 {
		return TopicChangeSet{}, "", errors.New("persisted topic has no members")
	}
	return topic, requestHash, nil
}

func (store *TopicStore) get(ctx context.Context, id string) (TopicChangeSet, error) {
	topic, _, err := store.read(ctx, ` WHERE id = ?`, id)
	return topic, err
}

func (store *TopicStore) findRequest(ctx context.Context, actorID int64, key string) (TopicChangeSet, string, error) {
	return store.read(ctx, ` WHERE creator_id = ? AND request_key = ?`, actorID, key)
}

func (store *TopicStore) read(ctx context.Context, filter string, args ...any) (TopicChangeSet, string, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return TopicChangeSet{}, "", err
	}
	defer tx.Rollback()
	return readTopic(ctx, tx, filter, args...)
}

func (store *TopicStore) updateMember(ctx context.Context, topicID string, member *TopicMember, state, code string, number, actorID int64) (bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var previous int64
	if member.UpdatedAt != nil {
		previous = member.UpdatedAt.UnixNano()
	}
	now := time.Now().UTC()
	if now.UnixNano() <= previous {
		now = time.Unix(0, previous+1).UTC()
	}
	result, err := tx.ExecContext(ctx, `UPDATE topic_members
SET state=?, result_code=?, pull_number=?, actor_id=?, updated_at_ns=?
WHERE id=? AND topic_id=? AND state=? AND updated_at_ns=?`, state, code, number, actorID, now.UnixNano(), member.ID, topicID, member.State, previous)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE topic_changesets SET version=version+1 WHERE id=?`, topicID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	member.State, member.ResultCode, member.PullNumber, member.ActorID, member.UpdatedAt = state, code, number, actorID, &now
	return true, nil
}

func (store *TopicStore) create(ctx context.Context, topic TopicChangeSet, key, hash string) (TopicChangeSet, bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return TopicChangeSet{}, false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO topic_changesets(id, project_id, topic, version, creator_id, created_at_ns, request_key, request_hash)
VALUES(?, ?, ?, 1, ?, ?, ?, ?) ON CONFLICT(creator_id, request_key) DO NOTHING`, topic.ID, topic.ProjectID, topic.Topic, topic.CreatorID, topic.CreatedAt.UnixNano(), key, hash)
	if err != nil {
		return TopicChangeSet{}, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return TopicChangeSet{}, false, err
	}
	if inserted == 0 {
		existing, existingHash, err := readTopic(ctx, tx, ` WHERE creator_id = ? AND request_key = ?`, topic.CreatorID, key)
		if err == nil && existingHash != hash {
			err = errIdempotencyConflict
		}
		return existing, false, err
	}
	for index := range topic.Members {
		member := &topic.Members[index]
		result, err := tx.ExecContext(ctx, `INSERT INTO topic_members(topic_id, repo_id, source_branch, target_branch, state) VALUES(?, ?, ?, ?, 'pending')`, topic.ID, member.RepositoryID, member.SourceBranch, member.TargetBranch)
		if err != nil {
			return TopicChangeSet{}, false, err
		}
		member.ID, err = result.LastInsertId()
		if err != nil {
			return TopicChangeSet{}, false, err
		}
		member.State = "pending"
	}
	if err := tx.Commit(); err != nil {
		return TopicChangeSet{}, false, err
	}
	topic.Version = 1
	return topic, true, nil
}
