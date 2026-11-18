package bridge

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

var topicPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{0,63}$`)

type Repository struct {
	ID   int64
	Name string
}

type PullRequest struct {
	Number    int64
	Title     string
	Body      string `json:"-"`
	Lifecycle string
	HeadSHA   string
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
	Repository   Repository
	Status       string
	PullRequests []PullRequest
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
			return nil, errors.New("cannot verify repository permissions")
		}
		if !visible {
			continue
		}
		result := RepositoryResult{Repository: repo, Status: "incomplete", PullRequests: make([]PullRequest, 0)}
		seenPRs := make(map[int64]bool)
		for pageNumber := 1; pageNumber <= maxPages && ctx.Err() == nil; pageNumber++ {
			page, err := source.PullRequests(ctx, repo, pageNumber)
			if err != nil {
				if ctx.Err() == nil {
					result.Status = "failed"
				}
				break
			}
			for _, pull := range page.PullRequests {
				if !seenPRs[pull.Number] && (strings.Contains(pull.Title, marker) || strings.Contains(pull.Body, marker)) {
					seenPRs[pull.Number] = true
					pull.Body = ""
					result.PullRequests = append(result.PullRequests, pull)
				}
			}
			if !page.HasMore {
				result.Status = "complete"
				break
			}
		}
		results = append(results, result)
	}
	return results, nil
}
