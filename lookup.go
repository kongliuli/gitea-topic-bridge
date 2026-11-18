package bridge

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

var topicPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{0,63}$`)

var (
	ErrNotVisible   = errors.New("repository is not visible")
	ErrUnauthorized = errors.New("upstream authorization expired or invalid")
)

type Repository struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

type PullRequest struct {
	Number       int64           `json:"number"`
	Title        string          `json:"title"`
	Body         string          `json:"-"`
	Lifecycle    string          `json:"lifecycle"`
	HeadSHA      string          `json:"head_sha"`
	URL          string          `json:"url"`
	ReadAt       time.Time       `json:"read_at"`
	ChangedFiles *int            `json:"changed_files"`
	CI           string          `json:"ci,omitempty"`
	Conflict     string          `json:"conflict,omitempty"`
	Reviews      *ReviewEvidence `json:"reviews"`
}

type ReviewEvidence struct {
	Approved         int `json:"approved"`
	ChangesRequested int `json:"changes_requested"`
}

type Page struct {
	PullRequests []PullRequest
	HasMore      bool
}

type Source interface {
	CanReadPullRequests(context.Context, Repository) (bool, error)
	PullRequests(context.Context, Repository, int) (Page, error)
}

type RepositoryResult struct {
	Repository   Repository    `json:"repository"`
	Status       string        `json:"status"`
	PullRequests []PullRequest `json:"pull_requests"`
}

func Lookup(ctx context.Context, source Source, repositories []Repository, topic string, maxPages int) ([]RepositoryResult, error) {
	if !topicPattern.MatchString(topic) || maxPages < 1 {
		return nil, errors.New("invalid topic or page limit")
	}
	marker := "[Topic: " + topic + "]"
	results := make([]RepositoryResult, 0)
	seenRepos := make(map[int64]bool)
	for _, repo := range repositories {
		if seenRepos[repo.ID] {
			continue
		}
		seenRepos[repo.ID] = true
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		visible, err := source.CanReadPullRequests(ctx, repo)
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				return nil, ErrUnauthorized
			}
			return nil, errors.New("cannot verify repository permissions")
		}
		if visible {
			results = append(results, RepositoryResult{Repository: repo, Status: "incomplete", PullRequests: make([]PullRequest, 0)})
		}
	}
	filtered := results[:0]
	for _, result := range results {
		repo := result.Repository
		seenPRs := make(map[int64]bool)
		for pageNumber := 1; pageNumber <= maxPages && ctx.Err() == nil; pageNumber++ {
			page, err := source.PullRequests(ctx, repo, pageNumber)
			if err != nil {
				if errors.Is(err, ErrUnauthorized) {
					return nil, ErrUnauthorized
				}
				if errors.Is(err, ErrNotVisible) {
					result.Status = "hidden"
					break
				}
				if ctx.Err() == nil {
					result.Status = "failed"
				}
				break
			}
			for _, pull := range page.PullRequests {
				if !seenPRs[pull.Number] && (strings.Contains(pull.Title, marker) || strings.Contains(pull.Body, marker)) {
					seenPRs[pull.Number] = true
					pull.Body = ""
					pull.ReadAt = time.Now().UTC()
					result.PullRequests = append(result.PullRequests, pull)
				}
			}
			if !page.HasMore {
				result.Status = "complete"
				break
			}
		}
		if result.Status != "hidden" {
			filtered = append(filtered, result)
		}
	}
	return filtered, nil
}
