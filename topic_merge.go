package bridge

import (
	"context"
	"errors"
	"net/http"
	"strconv"
)

type mergeSelection struct {
	Confirm      bool   `json:"confirm"`
	ExpectedHead string `json:"expected_head"`
	Reconcile    bool   `json:"reconcile"`
}

func (server *Server) mergeTopicMember(writer http.ResponseWriter, request *http.Request, client *Gitea, user GiteaUser) {
	if !server.checkOrigin(writer, request) {
		return
	}
	var input mergeSelection
	if !decodeRequest(writer, request, &input) {
		return
	}
	if (!input.Reconcile && (!input.Confirm || !commitSHA.MatchString(input.ExpectedHead))) ||
		(input.Reconcile && (input.Confirm || input.ExpectedHead != "")) {
		responseProblem(writer, http.StatusBadRequest, "invalid_merge_confirmation")
		return
	}
	topicID := request.PathValue("id")
	memberID, err := strconv.ParseInt(request.PathValue("member"), 10, 64)
	if err != nil || memberID < 1 || !topicIDPattern.MatchString(topicID) {
		server.topicProblem(writer, request, ErrNotVisible)
		return
	}
	ctx := request.Context()
	store := server.config.TopicStore
	topic, err := store.get(ctx, topicID)
	if err != nil {
		server.topicProblem(writer, request, err)
		return
	}
	repos, err := server.authorizedTopicRepos(ctx, client, topic)
	if err != nil {
		server.topicProblem(writer, request, err)
		return
	}
	for index, member := range topic.Members {
		if member.ID != memberID {
			continue
		}
		if member.PullNumber <= 0 || !validBranch(member.SourceBranch) || !validBranch(member.TargetBranch) {
			responseProblem(writer, http.StatusConflict, "pull_not_linked")
			return
		}
		err := server.processMemberMerge(ctx, client, user, repos[index], topic, member, input)
		if errors.Is(err, errPullChanged) {
			responseProblem(writer, http.StatusConflict, "pull_changed")
			return
		}
		if err != nil {
			server.topicProblem(writer, request, err)
			return
		}
		topic, err := store.get(ctx, topicID)
		if err == nil {
			_, err = server.authorizedTopicRepos(ctx, client, topic)
		}
		if err != nil {
			server.topicProblem(writer, request, err)
			return
		}
		writeSavedTopic(writer, http.StatusOK, topic)
		return
	}
	server.topicProblem(writer, request, ErrNotVisible)
}

func (server *Server) processMemberMerge(ctx context.Context, client *Gitea, user GiteaUser, repo Repository, topic TopicChangeSet, member TopicMember, input mergeSelection) error {
	store := server.config.TopicStore
	repoURL, err := client.repoURL(repo)
	if err != nil {
		return err
	}
	pullURL := pullEndpoint(repoURL, member.PullNumber)
	pull, err := client.mergePullDetail(ctx, pullURL, repo, member)
	if err != nil {
		return err
	}
	if member.Merge != nil && member.Merge.State == "merged" {
		return nil
	}
	saveEvidence := func(pull mergePullDetail) (bool, error) {
		observed, ok := pull.mergedEvidence()
		if !ok {
			return false, nil
		}
		if member.Merge != nil {
			observed.ExpectedHead, observed.RequestedBy = member.Merge.ExpectedHead, member.Merge.RequestedBy
		}
		_, err := store.saveMerge(ctx, member, observed)
		return true, err
	}
	if found, err := saveEvidence(pull); found || err != nil {
		return err
	}
	if input.Reconcile || (member.Merge != nil && member.Merge.State == "unconfirmed") {
		return nil
	}
	if pull.Head.SHA != input.ExpectedHead {
		return errPullChanged
	}
	if err := client.verifyRepoWrite(ctx, repo); err != nil {
		return err
	}
	next := MergeState{ExpectedHead: input.ExpectedHead, RequestedBy: user.ID}
	block := func(reason string) error {
		next.State, next.ResultCode = "blocked", reason
		_, err := store.saveMerge(ctx, member, next)
		return err
	}
	reason, err := client.checkMergePolicy(ctx, repoURL, pullURL, pull, topic.Topic)
	if err != nil {
		return err
	}
	if reason != "" {
		return block(reason)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	next.State, next.ResultCode = "unconfirmed", "needs_reconciliation"
	claimed, err := store.saveMerge(ctx, member, next)
	if err != nil || !claimed {
		return err
	}
	next.revision = 1
	if member.Merge != nil {
		next.revision = member.Merge.revision + 1
	}
	member.Merge = &next
	status, postErr := client.mergeMemberPull(ctx, pullURL, next.ExpectedHead)
	after, readErr := client.mergePullDetail(ctx, pullURL, repo, member)
	if readErr == nil {
		if found, err := saveEvidence(after); found || err != nil {
			return err
		}
		if postErr == nil && isMergeRejection(status) && after.Merged != nil && !*after.Merged {
			blocked := next
			blocked.State, blocked.ResultCode = "blocked", "gitea_rejected_merge"
			_, err := store.saveMerge(ctx, member, blocked)
			return err
		}
	}
	if status == http.StatusUnauthorized || errors.Is(readErr, ErrUnauthorized) {
		return ErrUnauthorized
	}
	return nil
}
