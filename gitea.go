package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var repositorySegment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

type Gitea struct {
	apiBase    *url.URL
	publicBase *url.URL
	token      string
	pageSize   int
	http       *http.Client
}

func baseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("invalid service URL")
	}
	ip := net.ParseIP(parsed.Hostname())
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, errors.New("service URL requires HTTPS or a loopback IP")
	}
	return parsed, nil
}

func NewGitea(apiBase, publicBase, token string, pageSize int) (*Gitea, error) {
	internal, err := baseURL(apiBase)
	if err != nil {
		return nil, err
	}
	public, err := baseURL(publicBase)
	if err != nil {
		return nil, err
	}
	if token == "" || pageSize < 1 || pageSize > 100 {
		return nil, errors.New("invalid token or page size")
	}
	return &Gitea{apiBase: internal, publicBase: public, token: token, pageSize: pageSize, http: newUpstreamClient()}, nil
}

func newUpstreamClient() *http.Client {
	return &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type GiteaUser struct {
	ID    int64
	Login string
}

func (client *Gitea) CurrentUser(ctx context.Context) (GiteaUser, error) {
	var user GiteaUser
	if _, err := client.get(ctx, client.apiBase.JoinPath("api", "v1", "user"), &user); err != nil {
		if errors.Is(err, ErrNotVisible) {
			return GiteaUser{}, ErrUnauthorized
		}
		return GiteaUser{}, err
	}
	if user.ID <= 0 || user.Login == "" {
		return GiteaUser{}, ErrUnauthorized
	}
	return user, nil
}

func (client *Gitea) get(ctx context.Context, endpoint *url.URL, value any) (http.Header, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case http.StatusForbidden, http.StatusNotFound:
		return nil, ErrNotVisible
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("unexpected Gitea response: %d", response.StatusCode)
	}
	const maxJSON = 16 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxJSON+1))
	if err != nil || len(data) > maxJSON || json.Unmarshal(data, value) != nil {
		return nil, errors.New("invalid Gitea response")
	}
	return response.Header, nil
}

func (client *Gitea) repoURL(repo Repository) (*url.URL, error) {
	for _, segment := range []string{repo.Owner, repo.Name} {
		if !repositorySegment.MatchString(segment) || segment == "." || segment == ".." {
			return nil, errors.New("invalid repository path")
		}
	}
	if repo.ID <= 0 {
		return nil, errors.New("invalid repository ID")
	}
	return client.apiBase.JoinPath("api", "v1", "repos", repo.Owner, repo.Name), nil
}

func (client *Gitea) CanReadPullRequests(ctx context.Context, repo Repository) (bool, error) {
	endpoint, err := client.repoURL(repo)
	if err != nil {
		return false, err
	}
	var info struct{ ID int64 }
	if _, err := client.get(ctx, endpoint, &info); err != nil {
		if errors.Is(err, ErrNotVisible) {
			return false, nil
		}
		return false, err
	}
	if info.ID != repo.ID {
		return false, nil
	}
	_, err = client.readPage(ctx, repo, 1, 1)
	if errors.Is(err, ErrNotVisible) {
		return false, nil
	}
	return err == nil, err
}

func (client *Gitea) PullRequests(ctx context.Context, repo Repository, page int) (Page, error) {
	return client.readPage(ctx, repo, page, client.pageSize)
}

func (client *Gitea) readPage(ctx context.Context, repo Repository, page, limit int) (Page, error) {
	endpoint, err := client.repoURL(repo)
	if err != nil {
		return Page{}, err
	}
	endpoint = endpoint.JoinPath("pulls")
	endpoint.RawQuery = url.Values{"state": {"all"}, "sort": {"oldest"}, "page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}.Encode()
	var rows []struct {
		Number             int64 `json:"number"`
		Title, Body, State string
		Merged             bool
		Head               apiPullBranch
		Base               apiPullBranch
	}
	headers, err := client.get(ctx, endpoint, &rows)
	if err != nil {
		return Page{}, err
	}
	total, err := strconv.Atoi(headers.Get("X-Total-Count"))
	if err != nil || total < 0 || rows == nil || (page == 1 && len(rows) == 0 && total > 0) {
		return Page{}, errors.New("invalid Gitea pagination")
	}
	hasNext := false
	for _, link := range strings.Split(headers.Get("Link"), ",") {
		if strings.HasSuffix(strings.TrimSpace(link), `; rel="next"`) {
			hasNext = true
		}
	}
	result := Page{HasMore: hasNext || total > page*limit}
	for _, row := range rows {
		if row.Base.RepoID != repo.ID {
			return Page{}, ErrNotVisible
		}
		lifecycle := "unknown"
		switch {
		case row.Merged:
			lifecycle = "merged"
		case row.State == "open":
			lifecycle = "open"
		case row.State == "closed":
			lifecycle = "closed"
		}
		if row.Number <= 0 {
			return Page{}, errors.New("invalid Gitea pull request number")
		}
		result.PullRequests = append(result.PullRequests, PullRequest{Number: row.Number, Title: row.Title, Body: row.Body,
			Lifecycle: lifecycle, HeadSHA: row.Head.SHA,
			URL:          client.publicBase.JoinPath(repo.Owner, repo.Name, "pulls", strconv.FormatInt(row.Number, 10)).String(),
			sourceRepoID: row.Head.RepoID, sourceBranch: row.Head.Label, targetBranch: row.Base.Ref,
		})
	}
	return result, nil
}
