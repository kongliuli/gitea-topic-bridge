package bridge

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var errNoAuthoritativeObject = errors.New("webhook object could not be verified")

type sourceObservation struct {
	Delivery   string     `json:"delivery_id"`
	Repository Repository `json:"repository"`
	Kind       string     `json:"kind"`
	ObjectID   int64      `json:"object_id"`
	Status     string     `json:"status"`
	Tag        string     `json:"tag,omitempty"`
	Commit     string     `json:"commit_sha,omitempty"`
	ActorID    int64      `json:"actor_id,omitempty"`
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
	ObservedAt time.Time  `json:"observed_at"`
}

func (client *Gitea) verifyRepoIdentity(ctx context.Context, endpoint *url.URL, repoID int64) error {
	var data struct{ ID int64 }
	if _, err := client.get(ctx, endpoint, &data); err != nil {
		return err
	}
	if data.ID != repoID {
		return ErrNotVisible
	}
	return nil
}

func (hook *Webhook) observe(ctx context.Context, job webhookJob) (sourceObservation, error) {
	observation := sourceObservation{Delivery: job.Delivery, Kind: job.Kind, ObjectID: job.ObjectID}
	repo, allowed := hook.repos[job.RepoID]
	if !allowed {
		return observation, ErrNotVisible
	}
	observation.Repository = repo
	endpoint, err := hook.client.repoURL(repo)
	if err != nil {
		return observation, err
	}
	if err := hook.client.verifyRepoIdentity(ctx, endpoint, repo.ID); err != nil {
		return observation, err
	}
	switch job.Kind {
	case "pull_request":
		err = hook.client.observePull(ctx, endpoint, &observation)
	case "release":
		err = hook.client.observeRelease(ctx, endpoint, &observation)
	default:
		err = errNoAuthoritativeObject
	}
	if err == nil {
		err = hook.client.verifyRepoIdentity(ctx, endpoint, repo.ID)
	}
	observation.ObservedAt = time.Now().UTC()
	return observation, err
}

func (client *Gitea) observePull(ctx context.Context, endpoint *url.URL, observation *sourceObservation) error {
	var pull mergePullDetail
	if _, err := client.get(ctx, pullEndpoint(endpoint, observation.ObjectID), &pull); err != nil {
		return err
	}
	if pull.Base.RepoID != observation.Repository.ID || pull.Number != observation.ObjectID || pull.Merged == nil {
		return errNoAuthoritativeObject
	}
	switch {
	case *pull.Merged:
		merged, ok := pull.mergedEvidence()
		if !ok {
			return errNoAuthoritativeObject
		}
		observation.Status, observation.Commit, observation.ActorID, observation.OccurredAt = "merged", merged.MergeSHA, merged.MergedBy, merged.MergedAt
	case pull.State == "open" || pull.State == "closed":
		observation.Status = pull.State
		if commitSHA.MatchString(pull.Head.SHA) {
			observation.Commit = pull.Head.SHA
		}
	default:
		return errNoAuthoritativeObject
	}
	return nil
}

func validObservedTag(tag string) bool {
	if len(tag) > 200 || tag == "" || tag == "@" || strings.HasSuffix(tag, ".") ||
		strings.Contains(tag, "..") || strings.Contains(tag, "@{") || strings.ContainsAny(tag, " ~^:?*[\\%") {
		return false
	}
	for _, char := range tag {
		if char < 0x21 || char == 0x7f {
			return false
		}
	}
	for _, segment := range strings.Split(tag, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return false
		}
	}
	return true
}

func (client *Gitea) observeRelease(ctx context.Context, endpoint *url.URL, observation *sourceObservation) error {
	var release struct {
		ID          int64
		Draft       *bool
		Tag         string `json:"tag_name"`
		Author      struct{ ID int64 }
		PublishedAt *time.Time `json:"published_at"`
	}
	if _, err := client.get(ctx, endpoint.JoinPath("releases", strconv.FormatInt(observation.ObjectID, 10)), &release); err != nil {
		return err
	}
	if release.ID != observation.ObjectID || release.Draft == nil || *release.Draft || !validObservedTag(release.Tag) ||
		release.Author.ID <= 0 || release.PublishedAt == nil || release.PublishedAt.IsZero() {
		return errNoAuthoritativeObject
	}
	var tag struct {
		Name   string
		Commit struct{ SHA string }
	}
	if _, err := client.get(ctx, endpoint.JoinPath("tags", release.Tag), &tag); err != nil {
		return err
	}
	if tag.Name != release.Tag || !commitSHA.MatchString(tag.Commit.SHA) {
		return errNoAuthoritativeObject
	}
	if _, err := client.readRevertCommit(ctx, endpoint, tag.Commit.SHA); err != nil {
		return err
	}
	when := release.PublishedAt.UTC()
	observation.Status, observation.Tag, observation.Commit = "published", release.Tag, tag.Commit.SHA
	observation.ActorID, observation.OccurredAt = release.Author.ID, &when
	return nil
}
