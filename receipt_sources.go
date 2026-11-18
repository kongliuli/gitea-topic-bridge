package bridge

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"time"
)

type receiptSourceSelection struct {
	RepositoryID int64  `json:"repository_id"`
	ReleaseID    int64  `json:"release_id"`
	ExpectedSHA  string `json:"expected_sha"`
}

var errReceiptSourceMismatch = errors.New("the selected release could not be verified at the expected commit")

func (client *Gitea) canReadReceiptRelease(ctx context.Context, repo Repository, releaseID int64) error {
	endpoint, err := client.repoURL(repo)
	if err != nil {
		return err
	}
	var current struct {
		ID    int64
		Draft *bool
	}
	if _, err := client.get(ctx, endpoint.JoinPath("releases", strconv.FormatInt(releaseID, 10)), &current); err != nil {
		return err
	}
	if current.ID != releaseID || current.Draft == nil || *current.Draft {
		return ErrNotVisible
	}
	return client.verifyRepoIdentity(ctx, endpoint, repo.ID)
}

func (client *Gitea) receiptSource(ctx context.Context, repo Repository, member TopicMember, selected receiptSourceSelection) (receiptMember, error) {
	result := receiptMember{Repository: repo, SourceRepositoryID: member.SourceRepositoryID, Status: "unverified"}
	endpoint, err := client.repoURL(repo)
	if err != nil {
		return result, err
	}
	evidence := sourceObservation{Repository: repo, Kind: "pull_request", ObjectID: member.PullNumber}
	if selected.ReleaseID > 0 {
		evidence.Kind, evidence.ObjectID = "release", selected.ReleaseID
		if err := client.observeRelease(ctx, endpoint, &evidence); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				return result, err
			}
			if errors.Is(err, ErrNotVisible) || errors.Is(err, errNoAuthoritativeObject) {
				return result, errReceiptSourceMismatch
			}
			return result, err
		}
		if evidence.Commit != selected.ExpectedSHA {
			return result, errReceiptSourceMismatch
		}
	} else {
		if member.PullNumber == 0 {
			return result, nil
		}
		pull, err := client.mergePullDetail(ctx, pullEndpoint(endpoint, member.PullNumber), repo, member)
		if errors.Is(err, errPullChanged) {
			return result, nil
		}
		if err != nil {
			return result, err
		}
		merged, ok := pull.mergedEvidence()
		if !ok {
			return result, nil
		}
		evidence.Status, evidence.Commit, evidence.ActorID, evidence.OccurredAt = "merged", merged.MergeSHA, merged.MergedBy, merged.MergedAt
	}
	evidence.ObservedAt = time.Now().UTC()
	result.Status, result.Source = "source_verified", &evidence
	result.Changes = &receiptChanges{Basis: "verified_commit_vs_first_parent"}
	if paths, err := client.readReceiptChangedPaths(ctx, endpoint, evidence.Commit); err == nil {
		result.Changes.Paths = &paths
	} else if errors.Is(err, ErrUnauthorized) {
		return result, err
	}
	return result, nil
}

func (client *Gitea) readReceiptChangedPaths(ctx context.Context, endpoint *url.URL, sha string) ([]string, error) {
	commit, err := client.readRevertCommit(ctx, endpoint, sha)
	if err != nil {
		return nil, err
	}
	after, err := client.readRevertTree(ctx, endpoint, commit.Commit.Tree.SHA)
	if err != nil {
		return nil, err
	}
	var before map[string]revertTreeEntry
	if len(commit.Parents) > 0 {
		parent, err := client.readRevertCommit(ctx, endpoint, commit.Parents[0].SHA)
		if err != nil {
			return nil, err
		}
		before, err = client.readRevertTree(ctx, endpoint, parent.Commit.Tree.SHA)
		if err != nil {
			return nil, err
		}
	}
	changed := make([]string, 0)
	for path, entry := range after {
		if before[path] != entry {
			changed = append(changed, path)
		}
	}
	for path := range before {
		if _, exists := after[path]; !exists {
			changed = append(changed, path)
		}
	}
	if len(changed) > 200 {
		return nil, errNoAuthoritativeObject
	}
	for _, path := range changed {
		if len(path) > 500 {
			return nil, errNoAuthoritativeObject
		}
	}
	sort.Strings(changed)
	return changed, nil
}
