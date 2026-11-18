package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type apiPullBranch struct {
	SHA    string
	Ref    string
	Label  string
	RepoID int64 `json:"repo_id"`
}

var (
	errCannotWriteRepo = errors.New("repository writes not permitted")
	errOtherTopicPull  = errors.New("branches already have a different pull request")
	errPullScanLimit   = errors.New("could not verify existing pull requests")
)

func (client *Gitea) verifyRepoWrite(ctx context.Context, repo Repository) error {
	endpoint, err := client.repoURL(repo)
	if err != nil {
		return err
	}
	var details struct {
		ID               int64
		Mirror, Archived *bool
		Permissions      struct{ Push bool }
	}
	if _, err := client.get(ctx, endpoint, &details); err != nil {
		return err
	}
	if details.ID != repo.ID {
		return ErrNotVisible
	}
	if details.Mirror == nil || details.Archived == nil || *details.Mirror || *details.Archived || !details.Permissions.Push {
		return errCannotWriteRepo
	}
	return nil
}

func memberMarker(topicID string, member TopicMember) string {
	return "Bridge operation: " + topicID + "/" + strconv.FormatInt(member.ID, 10)
}

func hasMemberMarker(body, marker string) bool {
	return strings.Contains("\n"+strings.ReplaceAll(body, "\r\n", "\n")+"\n", "\n"+marker+"\n")
}

func (client *Gitea) findMemberPull(ctx context.Context, repo Repository, topic TopicChangeSet, member TopicMember, maxPages int) (int64, error) {
	marker := memberMarker(topic.ID, member)
	tag := "[Topic: " + topic.Topic + "]"
	var open, recovered int64
	var foreign bool
	for pageNumber := 1; pageNumber <= maxPages; pageNumber++ {
		page, err := client.PullRequests(ctx, repo, pageNumber)
		if err != nil {
			return 0, err
		}
		for _, pull := range page.PullRequests {
			if pull.sourceRepoID != repo.ID || pull.sourceBranch != member.SourceBranch || pull.targetBranch != member.TargetBranch {
				continue
			}
			matching := strings.Contains(pull.Title, tag) || strings.Contains(pull.Body, tag)
			if matching && hasMemberMarker(pull.Body, marker) {
				if recovered != 0 && recovered != pull.Number {
					return 0, errOtherTopicPull
				}
				recovered = pull.Number
			}
			if pull.Lifecycle == "open" {
				if !matching || (open != 0 && open != pull.Number) {
					foreign = true
				} else {
					open = pull.Number
				}
			}
		}
		if !page.HasMore {
			switch {
			case recovered != 0:
				return recovered, nil
			case foreign:
				return 0, errOtherTopicPull
			default:
				return open, nil
			}
		}
	}
	return 0, errPullScanLimit
}

func (client *Gitea) createMemberPull(ctx context.Context, repo Repository, topic TopicChangeSet, member TopicMember, actorID int64) (int64, int, error) {
	endpoint, err := client.repoURL(repo)
	if err != nil {
		return 0, 0, err
	}
	marker := memberMarker(topic.ID, member)
	response, err := client.postJSON(ctx, endpoint.JoinPath("pulls"), map[string]any{
		"title": "[Topic: " + topic.Topic + "] " + member.SourceBranch + " → " + member.TargetBranch,
		"body":  marker, "base": member.TargetBranch, "head": member.SourceBranch,
		"allow_maintainer_edit": false,
	})
	if err != nil {
		return 0, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return 0, response.StatusCode, nil
	}
	const maxPayload = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPayload+1))
	var row struct {
		Number     int64
		Body       string
		User       struct{ ID int64 }
		Base, Head apiPullBranch
	}
	if err != nil || len(body) > maxPayload || json.Unmarshal(body, &row) != nil || row.Number <= 0 || row.User.ID != actorID ||
		row.Base.RepoID != repo.ID || row.Head.RepoID != repo.ID || row.Base.Ref != member.TargetBranch || row.Head.Label != member.SourceBranch ||
		!hasMemberMarker(row.Body, marker) {
		return 0, response.StatusCode, errors.New("Gitea did not confirm creation for the expected branches and user")
	}
	return row.Number, response.StatusCode, nil
}

func (client *Gitea) postJSON(ctx context.Context, endpoint *url.URL, value any) (*http.Response, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+client.token)
	return client.http.Do(request)
}
