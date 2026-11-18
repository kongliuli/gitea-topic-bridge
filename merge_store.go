package bridge

import (
	"context"
	"time"
)

type MergeState struct {
	State        string     `json:"state"`
	ResultCode   string     `json:"result_code"`
	ExpectedHead string     `json:"expected_head,omitempty"`
	RequestedBy  int64      `json:"requested_by,omitempty"`
	MergeSHA     string     `json:"merge_sha,omitempty"`
	MergedBy     int64      `json:"merged_by,omitempty"`
	MergedAt     *time.Time `json:"merged_at,omitempty"`
	revision     int64
}

func (store *TopicStore) saveMerge(ctx context.Context, member TopicMember, next MergeState) (bool, error) {
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	tx, err := store.db.BeginTx(persist, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var previous, mergedAt int64
	if member.Merge != nil {
		previous = member.Merge.revision
	}
	if next.MergedAt != nil {
		mergedAt = next.MergedAt.UnixNano()
	}
	result, err := tx.ExecContext(persist, `
INSERT INTO pull_merges(repo_id, pull_number, revision, state, result_code, expected_head, requested_by, merge_sha, merged_by, merged_at_ns)
SELECT ?, ?, ?+1, ?, ?, ?, ?, ?, ?, ?
WHERE ?=0 OR EXISTS (SELECT 1 FROM pull_merges WHERE repo_id=? AND pull_number=?)
ON CONFLICT(repo_id, pull_number) DO UPDATE SET revision=excluded.revision, state=excluded.state, result_code=excluded.result_code,
    expected_head=excluded.expected_head, requested_by=excluded.requested_by, merge_sha=excluded.merge_sha,
    merged_by=excluded.merged_by, merged_at_ns=excluded.merged_at_ns
WHERE pull_merges.revision=? AND pull_merges.state<>'merged'`,
		member.RepositoryID, member.PullNumber, previous, next.State, next.ResultCode, next.ExpectedHead, next.RequestedBy, next.MergeSHA, next.MergedBy, mergedAt,
		previous, member.RepositoryID, member.PullNumber, previous)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return false, err
	}
	if _, err := tx.ExecContext(persist, `
UPDATE topic_changesets SET version=version+1
WHERE id IN (SELECT topic_id FROM topic_members WHERE repo_id=? AND pull_number=?)`, member.RepositoryID, member.PullNumber); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
