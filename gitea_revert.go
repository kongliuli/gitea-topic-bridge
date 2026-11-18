package bridge

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var (
	errRevertManual = errors.New("revert requires manual preparation")
	revertPath      = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*$`)
)

type revertCommit struct {
	SHA    string
	Commit struct {
		Tree    struct{ SHA string }
		Message string
	}
	Parents []struct{ SHA string }
}

type revertTreeEntry struct {
	Path, Mode, Type, SHA string
	Size                  int64
}

type revertFileChange struct {
	Operation string `json:"operation"`
	Path      string `json:"path"`
	SHA       string `json:"sha,omitempty"`
	Content   string `json:"content,omitempty"`
}

func (client *Gitea) readRevertCommit(ctx context.Context, repoURL *url.URL, sha string) (revertCommit, error) {
	var commit revertCommit
	if !commitSHA.MatchString(sha) {
		return commit, errRevertManual
	}
	endpoint := repoURL.JoinPath("git", "commits", sha)
	endpoint.RawQuery = "stat=false&files=false&verification=false"
	if _, err := client.get(ctx, endpoint, &commit); err != nil {
		return commit, err
	}
	if commit.SHA != sha || !commitSHA.MatchString(commit.Commit.Tree.SHA) {
		return commit, errRevertManual
	}
	return commit, nil
}

func (client *Gitea) readRevertTree(ctx context.Context, repoURL *url.URL, sha string) (map[string]revertTreeEntry, error) {
	endpoint := repoURL.JoinPath("git", "trees", sha)
	endpoint.RawQuery = "recursive=true&per_page=1000"
	var tree struct {
		SHA        string
		Truncated  *bool
		TotalCount *int `json:"total_count"`
		Tree       []revertTreeEntry
	}
	headers, err := client.get(ctx, endpoint, &tree)
	if err != nil {
		return nil, err
	}
	if tree.SHA != sha || tree.Truncated == nil || *tree.Truncated || tree.Tree == nil || len(tree.Tree) > 1000 ||
		tree.TotalCount == nil || *tree.TotalCount != len(tree.Tree) || strings.Contains(headers.Get("Link"), `rel="next"`) {
		return nil, errRevertManual
	}
	entries := make(map[string]revertTreeEntry)
	for _, entry := range tree.Tree {
		if entry.Type == "tree" {
			continue
		}
		if _, exists := entries[entry.Path]; exists || !commitSHA.MatchString(entry.SHA) {
			return nil, errRevertManual
		}
		entries[entry.Path] = entry
	}
	return entries, nil
}

func validInverseEntry(exists bool, entry revertTreeEntry) bool {
	if !exists {
		return true
	}
	if len(entry.Path) > 500 || !revertPath.MatchString(entry.Path) || entry.Mode != "100644" || entry.Type != "blob" || entry.Size < 0 || entry.Size > 128<<10 {
		return false
	}
	for _, segment := range strings.Split(entry.Path, "/") {
		if segment == "." || strings.EqualFold(segment, ".git") || strings.HasSuffix(segment, ".") {
			return false
		}
	}
	return true
}

func (client *Gitea) readRevertBlob(ctx context.Context, repoURL *url.URL, entry revertTreeEntry) (string, error) {
	var blob struct {
		SHA, Content, Encoding string
		Size                   int64
	}
	if _, err := client.get(ctx, repoURL.JoinPath("git", "blobs", entry.SHA), &blob); err != nil {
		return "", err
	}
	if blob.Encoding != "base64" || blob.SHA != entry.SHA || blob.Size != entry.Size {
		return "", errRevertManual
	}
	decoded, err := base64.StdEncoding.DecodeString(blob.Content)
	if err != nil || int64(len(decoded)) != entry.Size || strings.HasPrefix(string(decoded), "version https://git-lfs.github.com/spec/v1") {
		return "", errRevertManual
	}
	gitBlob := append([]byte(fmt.Sprintf("blob %d\x00", len(decoded))), decoded...)
	var hash string
	if len(entry.SHA) == 40 {
		sum := sha1.Sum(gitBlob)
		hash = hex.EncodeToString(sum[:])
	} else {
		sum := sha256.Sum256(gitBlob)
		hash = hex.EncodeToString(sum[:])
	}
	if hash != entry.SHA {
		return "", errRevertManual
	}
	return base64.StdEncoding.EncodeToString(decoded), nil
}

func (client *Gitea) inverseChanges(ctx context.Context, repoURL *url.URL, mergeTree, parentTree string) ([]revertFileChange, error) {
	after, err := client.readRevertTree(ctx, repoURL, mergeTree)
	if err != nil {
		return nil, err
	}
	before, err := client.readRevertTree(ctx, repoURL, parentTree)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]bool)
	for path := range before {
		paths[path] = true
	}
	for path := range after {
		paths[path] = true
	}
	changed := make([]string, 0)
	for path := range paths {
		if before[path] != after[path] {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	if len(changed) < 1 || len(changed) > 16 {
		return nil, errRevertManual
	}
	var size int64
	result := make([]revertFileChange, 0, len(changed))
	for _, path := range changed {
		prior, was := before[path]
		current, exists := after[path]
		if !validInverseEntry(was, prior) || !validInverseEntry(exists, current) {
			return nil, errRevertManual
		}
		for _, candidate := range changed {
			if path != candidate && (strings.EqualFold(path, candidate) || strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(path)+"/")) {
				return nil, errRevertManual
			}
		}
		change := revertFileChange{Path: path}
		switch {
		case !was:
			change.Operation, change.SHA = "delete", current.SHA
		case !exists:
			change.Operation = "create"
		default:
			change.Operation, change.SHA = "update", current.SHA
		}
		if was {
			size += prior.Size
			if size > 256<<10 {
				return nil, errRevertManual
			}
			change.Content, err = client.readRevertBlob(ctx, repoURL, prior)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, change)
	}
	return result, nil
}

type revertBranch struct {
	Name              string
	Protected         *bool
	RequiredApprovals int   `json:"required_approvals"`
	UserCanPush       *bool `json:"user_can_push"`
	Commit            struct{ ID string }
}

func (client *Gitea) readRevertBranch(ctx context.Context, repoURL *url.URL, name string) (revertBranch, error) {
	var branch revertBranch
	if !validBranch(name) {
		return branch, errRevertManual
	}
	if _, err := client.get(ctx, repoURL.JoinPath("branches", name), &branch); err != nil {
		return branch, err
	}
	if branch.Name != name || !commitSHA.MatchString(branch.Commit.ID) {
		return branch, errRevertManual
	}
	return branch, nil
}

func (client *Gitea) createInverseBranch(ctx context.Context, repoURL *url.URL, target, branch, message string, changes []revertFileChange) (int, error) {
	if !validBranch(target) || !validBranch(branch) || strings.EqualFold(target, branch) || !strings.HasPrefix(branch, "bridge-revert-") || len(changes) == 0 {
		return 0, errRevertManual
	}
	response, err := client.postJSON(ctx, repoURL.JoinPath("contents"), map[string]any{
		"branch": target, "new_branch": branch, "force_push": false, "message": message, "files": changes,
	})
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func (client *Gitea) createInversePull(ctx context.Context, repoURL *url.URL, topic, branch, target, marker string) (int, error) {
	response, err := client.postJSON(ctx, repoURL.JoinPath("pulls"), map[string]any{
		"base": target, "head": branch, "title": "[Topic: " + topic + "] Revert merged change", "body": marker, "allow_maintainer_edit": false,
	})
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return response.StatusCode, ErrUnauthorized
	}
	return response.StatusCode, nil
}

func (client *Gitea) findInversePull(ctx context.Context, repo Repository, branch, target, inverse, marker string, maxPages int) (int64, error) {
	var found int64
	for index := 1; index <= maxPages; index++ {
		page, err := client.PullRequests(ctx, repo, index)
		if err != nil {
			return 0, err
		}
		for _, pull := range page.PullRequests {
			if pull.sourceRepoID != repo.ID || pull.sourceBranch != branch || pull.targetBranch != target {
				continue
			}
			if pull.HeadSHA != inverse || !hasMemberMarker(pull.Body, marker) ||
				(found > 0 && found != pull.Number) {
				return 0, errRevertManual
			}
			found = pull.Number
		}
		if !page.HasMore {
			return found, nil
		}
	}
	return 0, errPullScanLimit
}
