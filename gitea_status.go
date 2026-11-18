package bridge

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var commitSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

const maxInspectedPulls = 32

func (client *Gitea) inspectMatches(ctx context.Context, results []RepositoryResult) ([]RepositoryResult, error) {
	inspected := 0
	visible := results[:0]
	for _, result := range results {
		repoURL, err := client.repoURL(result.Repository)
		if err != nil {
			return nil, err
		}
		hidden := false
		for index := range result.PullRequests {
			if inspected == maxInspectedPulls || ctx.Err() != nil {
				break
			}
			pull := &result.PullRequests[index]
			if !commitSHA.MatchString(pull.HeadSHA) {
				continue
			}
			inspected++
			err := client.inspectPull(ctx, repoURL, result.Repository.ID, pull)
			if errors.Is(err, ErrNotVisible) {
				var canRead bool
				canRead, err = client.CanReadPullRequests(ctx, result.Repository)
				if err != nil {
					return nil, err
				}
				if !canRead {
					hidden = true
					break
				}
			}
			if errors.Is(err, ErrUnauthorized) {
				return nil, err
			}
		}
		if !hidden {
			visible = append(visible, result)
		}
	}
	return visible, nil
}

func (client *Gitea) inspectPull(ctx context.Context, repoURL *url.URL, repoID int64, pull *PullRequest) error {
	endpoint := repoURL.JoinPath("pulls", strconv.FormatInt(pull.Number, 10))
	var detail struct {
		Number       int64
		State        string
		Merged       bool
		Draft        bool
		Mergeable    *bool
		ChangedFiles *int `json:"changed_files"`
		Base         struct {
			RepoID int64 `json:"repo_id"`
		}
		Head struct{ SHA string }
	}
	if _, err := client.get(ctx, endpoint, &detail); err != nil {
		return err
	}
	if detail.Base.RepoID != repoID {
		return ErrNotVisible
	}
	if detail.Number != pull.Number || detail.Head.SHA != pull.HeadSHA || detail.Merged != (pull.Lifecycle == "merged") || (pull.Lifecycle == "open") != (detail.State == "open") {
		return errors.New("pull request changed between list and detail")
	}
	if detail.ChangedFiles != nil && *detail.ChangedFiles >= 0 {
		pull.ChangedFiles = detail.ChangedFiles
	}
	switch {
	case pull.Lifecycle != "open":
		pull.Conflict = "not_applicable"
	case detail.Mergeable != nil && *detail.Mergeable && !detail.Draft:
		pull.Conflict = "none_reported"
	case detail.Mergeable != nil && !*detail.Mergeable:
		pull.Conflict = "not_mergeable_reason_unknown"
	}
	status, statusErr := client.commitCheck(ctx, repoURL, repoID, pull.HeadSHA)
	if errors.Is(statusErr, ErrUnauthorized) {
		return statusErr
	}
	reviews, reviewErr := client.reviewEvidence(ctx, endpoint, pull.HeadSHA)
	if errors.Is(reviewErr, ErrUnauthorized) || errors.Is(reviewErr, ErrNotVisible) {
		return reviewErr
	}
	pull.CI, pull.Reviews = status, reviews
	return nil
}

func (client *Gitea) commitCheck(ctx context.Context, repoURL *url.URL, repoID int64, sha string) (string, error) {
	endpoint := repoURL.JoinPath("commits", sha, "status")
	endpoint.RawQuery = "limit=100"
	var status struct {
		SHA, State string
		Repository struct{ ID int64 }
		Statuses   []struct{ Status string }
	}
	headers, err := client.get(ctx, endpoint, &status)
	if err != nil {
		return "", err
	}
	if status.SHA != sha || status.Repository.ID != repoID || status.Statuses == nil || !wholePage(headers, len(status.Statuses)) {
		return "", nil
	}
	if len(status.Statuses) == 0 {
		return "not_reported", nil
	}
	state := strings.ToLower(status.State)
	for _, check := range status.Statuses {
		itemState := strings.ToLower(check.Status)
		switch itemState {
		case "success", "pending", "warning", "failure", "error", "skipped":
		default:
			return "", nil
		}
		if state == "success" && itemState != "success" {
			return "", nil
		}
	}
	switch state {
	case "success", "pending", "warning", "failure", "error", "skipped":
		return state, nil
	default:
		return "", nil
	}
}

func wholePage(headers http.Header, count int) bool {
	total, err := strconv.Atoi(headers.Get("X-Total-Count"))
	return err == nil && total == count
}

func (client *Gitea) reviewEvidence(ctx context.Context, pullURL *url.URL, sha string) (*ReviewEvidence, error) {
	endpoint := pullURL.JoinPath("reviews")
	endpoint.RawQuery = "limit=100"
	var reviews []struct {
		ID                         int64
		User                       struct{ ID int64 }
		State                      string
		CommitID                   string `json:"commit_id"`
		Stale, Dismissed, Official *bool
	}
	headers, err := client.get(ctx, endpoint, &reviews)
	if err != nil {
		return nil, err
	}
	if reviews == nil || !wholePage(headers, len(reviews)) {
		return nil, nil
	}
	type decision struct {
		id    int64
		state string
	}
	latest := make(map[int64]decision)
	for _, review := range reviews {
		if review.State != "APPROVED" && review.State != "REQUEST_CHANGES" {
			continue
		}
		if review.Stale == nil || review.Dismissed == nil || review.Official == nil {
			return nil, nil
		}
		if *review.Stale || *review.Dismissed || !*review.Official || review.CommitID != sha {
			continue
		}
		if review.ID <= 0 || review.User.ID <= 0 {
			return nil, nil
		}
		if review.ID > latest[review.User.ID].id {
			latest[review.User.ID] = decision{review.ID, review.State}
		}
	}
	evidence := &ReviewEvidence{}
	for _, review := range latest {
		if review.state == "APPROVED" {
			evidence.Approved++
		} else {
			evidence.ChangesRequested++
		}
	}
	return evidence, nil
}
