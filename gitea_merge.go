package bridge

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type mergePullDetail struct {
	Number                   int64
	Title, Body, State       string
	Draft, Mergeable, Merged *bool
	Head, Base               apiPullBranch
	MergeSHA                 string             `json:"merge_commit_sha"`
	MergedBy                 struct{ ID int64 } `json:"merged_by"`
	MergedAt                 *time.Time         `json:"merged_at"`
}

var errPullChanged = errors.New("linked pull request or confirmed commit changed")

func (client *Gitea) mergePullDetail(ctx context.Context, endpoint *url.URL, repo Repository, member TopicMember) (mergePullDetail, error) {
	var pull mergePullDetail
	if _, err := client.get(ctx, endpoint, &pull); err != nil {
		return mergePullDetail{}, err
	}
	if pull.Base.RepoID != repo.ID {
		return mergePullDetail{}, ErrNotVisible
	}
	if pull.Number != member.PullNumber || pull.Head.RepoID != member.pullSourceID() || pull.Head.Label != member.SourceBranch || pull.Base.Ref != member.TargetBranch {
		return mergePullDetail{}, errPullChanged
	}
	return pull, nil
}

func (pull mergePullDetail) mergedEvidence() (MergeState, bool) {
	if pull.Merged == nil || !*pull.Merged || !commitSHA.MatchString(pull.MergeSHA) ||
		pull.MergedAt == nil || pull.MergedAt.IsZero() || pull.MergedBy.ID <= 0 {
		return MergeState{}, false
	}
	at := pull.MergedAt.UTC()
	return MergeState{State: "merged", ResultCode: "verified", MergeSHA: pull.MergeSHA, MergedBy: pull.MergedBy.ID, MergedAt: &at}, true
}

func (client *Gitea) checkMergePolicy(ctx context.Context, repoURL, pullURL *url.URL, pull mergePullDetail, topic string) (string, error) {
	marker := "[Topic: " + topic + "]"
	if !strings.Contains(pull.Title, marker) && !strings.Contains(pull.Body, marker) {
		return "", errPullChanged
	}
	if pull.State != "open" || pull.Draft == nil || *pull.Draft || pull.Mergeable == nil || !*pull.Mergeable || pull.Merged == nil || *pull.Merged {
		return "pull_not_ready", nil
	}
	var branch struct {
		Name              string
		Protected         *bool
		UserCanPush       *bool `json:"user_can_push"`
		UserCanMerge      *bool `json:"user_can_merge"`
		RequiredApprovals int   `json:"required_approvals"`
	}
	if _, err := client.get(ctx, repoURL.JoinPath("branches", pull.Base.Ref), &branch); err != nil {
		if errors.Is(err, ErrNotVisible) {
			return "branch_policy_unknown", nil
		}
		return "", err
	}
	if branch.Name != pull.Base.Ref || branch.Protected == nil || !*branch.Protected || branch.RequiredApprovals < 1 ||
		branch.UserCanPush == nil || *branch.UserCanPush {
		return "branch_policy_unsafe", nil
	}
	if branch.UserCanMerge == nil || !*branch.UserCanMerge {
		return "", errCannotWriteRepo
	}
	reviews, err := client.reviewEvidence(ctx, pullURL, pull.Head.SHA)
	if err != nil && !errors.Is(err, ErrNotVisible) {
		return "", err
	}
	if reviews == nil || reviews.Approved < branch.RequiredApprovals || reviews.ChangesRequested != 0 {
		return "approval_missing", nil
	}
	return "", nil
}

func (client *Gitea) mergeMemberPull(ctx context.Context, pullURL *url.URL, headSHA string) (int, error) {
	response, err := client.postJSON(ctx, pullURL.JoinPath("merge"), map[string]any{
		"do": "merge", "head_commit_id": headSHA,
		"force_merge": false, "merge_when_checks_succeed": false, "delete_branch_after_merge": false,
	})
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func pullEndpoint(repoURL *url.URL, number int64) *url.URL {
	return repoURL.JoinPath("pulls", strconv.FormatInt(number, 10))
}

func isMergeRejection(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed,
		http.StatusConflict, http.StatusUnprocessableEntity, http.StatusLocked:
		return true
	default:
		return false
	}
}
