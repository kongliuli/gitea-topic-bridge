package bridge

import (
	"context"
	"errors"
	"net/http"
)

func (member TopicMember) pullSourceID() int64 {
	if member.SourceRepositoryID > 0 {
		return member.SourceRepositoryID
	}
	return member.RepositoryID
}

func (project Project) fork(target, source int64) (Repository, bool) {
	for _, entry := range project.Forks {
		if entry.TargetID == target && entry.Source.ID == source {
			return entry.Source, true
		}
	}
	return Repository{}, false
}

func (client *Gitea) verifySourceFork(ctx context.Context, source Repository, target int64, write bool) error {
	endpoint, err := client.repoURL(source)
	if err != nil {
		return err
	}
	var data struct {
		ID               int64
		Fork             bool
		Parent           struct{ ID int64 }
		Mirror, Archived *bool
		Permissions      struct{ Pull, Push bool }
	}
	if _, err := client.get(ctx, endpoint, &data); err != nil {
		return err
	}
	if data.ID != source.ID || !data.Fork || data.Parent.ID != target || !data.Permissions.Pull {
		return ErrNotVisible
	}
	if write && (data.Mirror == nil || *data.Mirror || data.Archived == nil || *data.Archived || !data.Permissions.Push) {
		return errCannotWriteRepo
	}
	return nil
}

func (client *Gitea) memberFork(ctx context.Context, project Project, member TopicMember, write bool) (Repository, error) {
	fork, ok := project.fork(member.RepositoryID, member.SourceRepositoryID)
	if !ok {
		return Repository{}, ErrNotVisible
	}
	return fork, client.verifySourceFork(ctx, fork, member.RepositoryID, write)
}

func (server *Server) visibleForkPulls(ctx context.Context, client *Gitea, project Project, results []RepositoryResult) ([]RepositoryResult, error) {
	checked := make(map[int64]bool)
	for index := range results {
		result := &results[index]
		filtered := result.PullRequests[:0]
		for _, pull := range result.PullRequests {
			if pull.sourceRepoID == result.Repository.ID {
				filtered = append(filtered, pull)
				continue
			}
			source, ok := project.fork(result.Repository.ID, pull.sourceRepoID)
			if !ok {
				continue
			}
			visible, known := checked[source.ID]
			if !known {
				err := client.verifySourceFork(ctx, source, result.Repository.ID, false)
				if err != nil && !errors.Is(err, ErrNotVisible) {
					return nil, err
				}
				visible = err == nil
				checked[source.ID] = visible
			}
			if visible {
				filtered = append(filtered, pull)
			}
		}
		result.PullRequests = filtered
	}
	return results, nil
}

func (server *Server) listProjectForks(writer http.ResponseWriter, request *http.Request, client *Gitea, _ GiteaUser) {
	project, ok := server.projects[request.PathValue("id")]
	if !ok {
		server.topicProblem(writer, request, ErrNotVisible)
		return
	}
	visible := make(map[int64]bool)
	result := make([]ProjectFork, 0)
	for _, repo := range project.Repositories {
		allowed, err := client.CanReadPullRequests(request.Context(), repo)
		if err != nil {
			server.topicProblem(writer, request, err)
			return
		}
		visible[repo.ID] = allowed
	}
	for _, entry := range project.Forks {
		if !visible[entry.TargetID] {
			continue
		}
		err := client.verifySourceFork(request.Context(), entry.Source, entry.TargetID, true)
		if errors.Is(err, ErrNotVisible) || errors.Is(err, errCannotWriteRepo) {
			continue
		}
		if err != nil {
			server.topicProblem(writer, request, err)
			return
		}
		result = append(result, entry)
	}
	responseJSON(writer, 200, result)
}
