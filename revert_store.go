package bridge

import (
	"context"
	"time"
)

type RevertState struct {
	State      string `json:"state"`
	Branch     string `json:"branch,omitempty"`
	PullNumber int64  `json:"pull_number,omitempty"`
	InverseSHA string `json:"inverse_sha,omitempty"`
	revision   int64
}

func (store *TopicStore) saveRevert(ctx context.Context, member TopicMember, next *RevertState) (bool, error) {
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	tx, err := store.db.BeginTx(persist, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(persist, `
INSERT INTO pull_reverts(repo_id, original_pull, merge_sha, branch, revision, state, pull_number, inverse_sha)
VALUES(?, ?, ?, ?, ?+1, ?, ?, ?)
ON CONFLICT(repo_id, original_pull, merge_sha) DO UPDATE SET revision=excluded.revision,
    state=excluded.state, pull_number=excluded.pull_number, inverse_sha=excluded.inverse_sha
WHERE pull_reverts.revision=? AND pull_reverts.state NOT IN ('opened','manual')`,
		member.RepositoryID, member.PullNumber, member.Merge.MergeSHA, next.Branch, next.revision, next.State, next.PullNumber, next.InverseSHA, next.revision)
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
	if err := tx.Commit(); err != nil {
		return false, err
	}
	next.revision++
	return true, nil
}
