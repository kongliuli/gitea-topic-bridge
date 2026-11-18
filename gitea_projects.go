package bridge

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

type projectRepositoryInfo struct {
	ID                      int64
	DefaultBranch           string `json:"default_branch"`
	Empty, Mirror, Archived bool
	Permissions             struct{ Pull, Admin bool }
}

type projectBranchInfo struct {
	Name              string
	Commit            *projectCommit
	Protected         *bool
	RequiredApprovals *int   `json:"required_approvals"`
	UserCanPush       *bool  `json:"user_can_push"`
	RuleName          string `json:"effective_branch_protection_name"`
}

type projectCommit struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	URL       string    `json:"url"`
}

type projectRepositoryOverview struct {
	Repository Repository        `json:"repository"`
	URL        string            `json:"url"`
	Branch     string            `json:"default_branch,omitempty"`
	Commit     *projectCommit    `json:"latest_commit,omitempty"`
	Protection projectProtection `json:"protection"`
}

type projectProtection struct {
	Status            string   `json:"status"`
	Protected         *bool    `json:"protected,omitempty"`
	RequiredApprovals *int     `json:"required_approvals,omitempty"`
	UserCanPush       *bool    `json:"user_can_push,omitempty"`
	Findings          []string `json:"findings,omitempty"`
}

func (client *Gitea) projectRepository(ctx context.Context, repo Repository) (projectRepositoryInfo, error) {
	endpoint, err := client.repoURL(repo)
	if err != nil {
		return projectRepositoryInfo{}, err
	}
	var info projectRepositoryInfo
	if _, err := client.get(ctx, endpoint, &info); err != nil {
		return projectRepositoryInfo{}, err
	}
	if info.ID != repo.ID {
		return projectRepositoryInfo{}, ErrNotVisible
	}
	// Code and PR permissions are independent in Gitea.
	if !info.Permissions.Pull {
		if _, err := client.readPage(ctx, repo, 1, 1); err != nil {
			return projectRepositoryInfo{}, err
		}
	}
	return info, nil
}

func (client *Gitea) repositoryOverview(ctx context.Context, repo Repository) (projectRepositoryOverview, error) {
	info, err := client.projectRepository(ctx, repo)
	if err != nil {
		return projectRepositoryOverview{}, err
	}
	public := client.publicBase.JoinPath(repo.Owner, repo.Name)
	overview := projectRepositoryOverview{Repository: repo, URL: public.String(), Protection: projectProtection{Status: "unknown"}}
	if !info.Permissions.Pull {
		return overview, nil
	}
	overview.Branch = info.DefaultBranch
	if info.Mirror || info.Archived {
		overview.Protection.Status = "not_applicable"
	}
	if info.Empty || info.DefaultBranch == "" || info.DefaultBranch == "." || info.DefaultBranch == ".." {
		return overview, nil
	}
	endpoint, err := client.repoURL(repo)
	if err != nil {
		return projectRepositoryOverview{}, err
	}
	var branch projectBranchInfo
	if _, err := client.get(ctx, endpoint.JoinPath("branches", url.PathEscape(info.DefaultBranch)), &branch); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return projectRepositoryOverview{}, err
		}
		if errors.Is(err, ErrNotVisible) {
			// Recheck visibility in case permission changed during the read.
			latest, checkErr := client.projectRepository(ctx, repo)
			if checkErr != nil {
				return projectRepositoryOverview{}, checkErr
			}
			if !latest.Permissions.Pull {
				overview.Branch = ""
			}
		}
		return overview, nil
	}
	if branch.Name != info.DefaultBranch {
		return overview, nil
	}
	if branch.Commit != nil && commitSHA.MatchString(branch.Commit.ID) {
		branch.Commit.URL = public.JoinPath("commit", branch.Commit.ID).String()
		overview.Commit = branch.Commit
	}
	if overview.Protection.Status != "not_applicable" {
		overview.Protection, err = client.inspectProjectProtection(ctx, endpoint, branch, info.Permissions.Admin)
	}
	return overview, err
}

func (client *Gitea) inspectProjectProtection(ctx context.Context, endpoint *url.URL, branch projectBranchInfo, admin bool) (projectProtection, error) {
	result := projectProtection{Status: "unknown", Protected: branch.Protected, RequiredApprovals: branch.RequiredApprovals, UserCanPush: branch.UserCanPush}
	if branch.Protected == nil || branch.RequiredApprovals == nil || *branch.RequiredApprovals < 0 {
		return result, nil
	}
	if !*branch.Protected {
		result.Findings = append(result.Findings, "unprotected")
	}
	if *branch.RequiredApprovals == 0 {
		result.Findings = append(result.Findings, "no_required_approval")
	}
	if len(result.Findings) != 0 {
		result.Status = "needs_attention"
		return result, nil
	}
	result.Status = "partial"
	if !admin || branch.RuleName == "" || branch.RuleName == "." || branch.RuleName == ".." {
		return result, nil
	}
	var rule struct {
		Name               string   `json:"rule_name"`
		RequiredApprovals  *int     `json:"required_approvals"`
		EnablePush         *bool    `json:"enable_push"`
		EnableForcePush    *bool    `json:"enable_force_push"`
		EnableBypass       *bool    `json:"enable_bypass_allowlist"`
		BlockAdminOverride *bool    `json:"block_admin_merge_override"`
		ReviewAllowlist    *bool    `json:"enable_approvals_whitelist"`
		AllowedReviewUsers []string `json:"approvals_whitelist_username"`
		AllowedReviewTeams []string `json:"approvals_whitelist_teams"`
	}
	_, err := client.get(ctx, endpoint.JoinPath("branch_protections", url.PathEscape(branch.RuleName)), &rule)
	if errors.Is(err, ErrUnauthorized) {
		return projectProtection{}, err
	}
	if err != nil || rule.Name != branch.RuleName || rule.RequiredApprovals == nil || *rule.RequiredApprovals != *branch.RequiredApprovals {
		return result, nil
	}
	if rule.EnablePush == nil || rule.EnableForcePush == nil || rule.EnableBypass == nil || rule.BlockAdminOverride == nil || rule.ReviewAllowlist == nil {
		return result, nil
	}
	if *rule.EnablePush || *rule.EnableForcePush {
		result.Findings = append(result.Findings, "push_enabled")
	}
	if !*rule.ReviewAllowlist || len(rule.AllowedReviewUsers)+len(rule.AllowedReviewTeams) == 0 {
		result.Findings = append(result.Findings, "reviewers_not_designated")
	}
	if *rule.EnableBypass || !*rule.BlockAdminOverride {
		result.Findings = append(result.Findings, "approval_bypass_enabled")
	}
	result.Status = "meets_baseline"
	if len(result.Findings) != 0 {
		result.Status = "needs_attention"
	}
	return result, nil
}

func (server *Server) projectOverview(writer http.ResponseWriter, request *http.Request, client *Gitea, id string) {
	project, exists := server.projects[id]
	if !exists {
		responseProblem(writer, http.StatusNotFound, "project_not_found")
		return
	}
	repos := make([]projectRepositoryOverview, 0, len(project.Repositories))
	for _, repo := range project.Repositories {
		result, err := client.repositoryOverview(request.Context(), repo)
		if errors.Is(err, ErrNotVisible) {
			continue
		}
		if err != nil {
			server.upstreamProblem(writer, request, err)
			return
		}
		repos = append(repos, result)
	}
	if len(repos) == 0 {
		responseProblem(writer, http.StatusNotFound, "project_not_found")
		return
	}
	responseJSON(writer, http.StatusOK, struct {
		ID           string                      `json:"id"`
		Name         string                      `json:"name"`
		Repositories []projectRepositoryOverview `json:"repositories"`
		ReadAt       time.Time                   `json:"read_at"`
	}{project.ID, project.Name, repos, time.Now().UTC()})
}
