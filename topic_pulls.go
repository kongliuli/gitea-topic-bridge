package bridge

import (
	"context"
	"errors"
	"net/http"
	"time"
)

func (server *Server) createTopicPulls(writer http.ResponseWriter, request *http.Request, client *Gitea, user GiteaUser) {
	if !server.checkOrigin(writer, request) {
		return
	}
	var input struct {
		Confirm   bool    `json:"confirm"`
		MemberIDs []int64 `json:"member_ids"`
	}
	if !decodeRequest(writer, request, &input) {
		return
	}
	id := request.PathValue("id")
	if !topicIDPattern.MatchString(id) {
		server.topicProblem(writer, request, ErrNotVisible)
		return
	}
	if !input.Confirm || len(input.MemberIDs) == 0 || len(input.MemberIDs) > 8 {
		responseProblem(writer, http.StatusBadRequest, "invalid_pull_selection")
		return
	}
	ctx := request.Context()
	store := server.config.TopicStore
	topic, err := store.get(ctx, id)
	if err != nil {
		server.topicProblem(writer, request, err)
		return
	}
	repos, err := server.authorizedTopicRepos(ctx, client, topic)
	if err != nil {
		server.topicProblem(writer, request, err)
		return
	}
	indexByMember := make(map[int64]int, len(topic.Members))
	for index, member := range topic.Members {
		indexByMember[member.ID] = index
	}
	seen := make(map[int64]bool)
	denied := make([]int64, 0)
	for _, memberID := range input.MemberIDs {
		index, exists := indexByMember[memberID]
		if !exists || seen[memberID] || !validBranch(topic.Members[index].SourceBranch) || !validBranch(topic.Members[index].TargetBranch) ||
			(topic.Members[index].SourceRepositoryID == 0 && topic.Members[index].SourceBranch == topic.Members[index].TargetBranch) {
			responseProblem(writer, http.StatusBadRequest, "invalid_pull_selection")
			return
		}
		seen[memberID] = true
	}
	for _, memberID := range input.MemberIDs {
		if ctx.Err() != nil {
			break
		}
		index := indexByMember[memberID]
		if err := server.processMemberPull(ctx, client, user.ID, repos[index], topic, topic.Members[index]); err != nil {
			if errors.Is(err, errCannotWriteRepo) {
				denied = append(denied, memberID)
				continue
			}
			server.topicProblem(writer, request, err)
			return
		}
	}
	topic, err = store.get(ctx, id)
	if err == nil {
		_, err = server.authorizedTopicRepos(ctx, client, topic)
	}
	if err != nil {
		server.topicProblem(writer, request, err)
		return
	}
	responseJSON(writer, http.StatusOK, struct {
		TopicChangeSet
		WriteDenied []int64 `json:"write_denied"`
	}{topic, denied})
}

func (server *Server) processMemberPull(ctx context.Context, client *Gitea, actorID int64, repo Repository, topic TopicChangeSet, member TopicMember) error {
	if member.PullNumber > 0 {
		return nil
	}
	save := func(state, code string, number int64) (bool, error) {
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		return server.config.TopicStore.updateMember(persist, topic.ID, &member, state, code, number, actorID)
	}
	fail := func(code string) error {
		if member.State == "unconfirmed" {
			return nil
		}
		_, err := save("failed", code, 0)
		return err
	}
	source := repo
	permission := func() (bool, error) {
		err := client.verifyPullDestination(ctx, repo, member.SourceRepositoryID == 0)
		if err == nil && member.SourceRepositoryID > 0 {
			source, err = client.memberFork(ctx, server.projects[topic.ProjectID], member, true)
		}
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, ErrNotVisible), errors.Is(err, ErrUnauthorized):
			return false, err
		case errors.Is(err, errCannotWriteRepo):
			// Denied attempts cannot overwrite the shared operation result or
			// invalidate another user's pending claim.
			return false, err
		default:
			return false, fail("precheck_failed")
		}
	}
	if allowed, err := permission(); !allowed {
		return err
	}
	number, err := client.findMemberPull(ctx, repo, topic, member, server.config.MaxPages)
	switch {
	case errors.Is(err, ErrUnauthorized), errors.Is(err, ErrNotVisible):
		return err
	case errors.Is(err, errOtherTopicPull):
		return fail("branches_in_use")
	case errors.Is(err, errPullScanLimit):
		return fail("scan_incomplete")
	case err != nil:
		return fail("precheck_failed")
	case number != 0:
		_, err := save("linked", "existing", number)
		return err
	case member.State == "unconfirmed", ctx.Err() != nil:
		return nil
	}
	if allowed, err := permission(); !allowed {
		return err
	}
	claimed, err := save("unconfirmed", "needs_reconciliation", 0)
	if err != nil || !claimed {
		return err
	}
	number, status, err := client.createMemberPull(ctx, repo, source, topic, member, actorID)
	switch {
	case err != nil:
		return nil
	case number > 0:
		_, err = save("linked", "created", number)
	case status == http.StatusUnauthorized:
		if _, err = save("failed", "login_required", 0); err == nil {
			err = ErrUnauthorized
		}
	case status == http.StatusForbidden || status == http.StatusLocked:
		_, err = save("failed", "write_denied", 0)
	case status == http.StatusUnprocessableEntity || status == http.StatusNotFound || status == http.StatusBadRequest:
		_, err = save("failed", "gitea_rejected", 0)
	}
	return err
}
