package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

type revertSelection struct {
	Confirm       bool   `json:"confirm"`
	ExpectedMerge string `json:"expected_merge"`
	Reconcile     bool   `json:"reconcile"`
}

func (server *Server) revertTopicMember(writer http.ResponseWriter, request *http.Request, client *Gitea, _ GiteaUser) {
	if !server.checkOrigin(writer, request) {
		return
	}
	var input revertSelection
	if !decodeRequest(writer, request, &input) {
		return
	}
	if input.Confirm == input.Reconcile || !commitSHA.MatchString(input.ExpectedMerge) {
		responseProblem(writer, http.StatusBadRequest, "invalid_revert_confirmation")
		return
	}
	ctx := request.Context()
	id := request.PathValue("id")
	memberID, err := strconv.ParseInt(request.PathValue("member"), 10, 64)
	if err != nil || memberID < 1 || !topicIDPattern.MatchString(id) {
		server.topicProblem(writer, request, ErrNotVisible)
		return
	}
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
	for index, member := range topic.Members {
		if member.ID != memberID {
			continue
		}
		if member.PullNumber <= 0 || member.Merge == nil || member.Merge.State != "merged" ||
			member.Merge.MergeSHA != input.ExpectedMerge || !validBranch(member.SourceBranch) || !validBranch(member.TargetBranch) {
			responseProblem(writer, http.StatusConflict, "merge_not_verified")
			return
		}
		err := server.processMemberRevert(ctx, client, repos[index], topic, member, input.Reconcile)
		switch {
		case errors.Is(err, errPullChanged):
			responseProblem(writer, http.StatusConflict, "pull_changed")
			return
		case errors.Is(err, errRevertManual):
			responseProblem(writer, http.StatusConflict, "manual_revert_required")
			return
		case err != nil:
			server.topicProblem(writer, request, err)
			return
		}
		topic, err = store.get(ctx, id)
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

func revertIdentity(repo Repository, member TopicMember) (string, string) {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%s", repo.ID, member.PullNumber, member.Merge.MergeSHA)))
	return "bridge-revert-" + hex.EncodeToString(digest[:16]),
		fmt.Sprintf("Bridge revert of #%d at %s", member.PullNumber, member.Merge.MergeSHA)
}

func (server *Server) processMemberRevert(ctx context.Context, client *Gitea, repo Repository, topic TopicChangeSet, member TopicMember, reconcile bool) error {
	repoURL, err := client.repoURL(repo)
	if err != nil {
		return err
	}
	original, err := client.mergePullDetail(ctx, pullEndpoint(repoURL, member.PullNumber), repo, member)
	if err != nil {
		return err
	}
	merged, ok := original.mergedEvidence()
	if !ok || merged.MergeSHA != member.Merge.MergeSHA {
		return errPullChanged
	}
	state := RevertState{}
	if member.Revert != nil {
		state = *member.Revert
		if state.State == "opened" || state.State == "manual" {
			return nil
		}
	}
	if reconcile && state.State == "" {
		return nil
	}
	if !reconcile {
		if err := client.verifyRepoWrite(ctx, repo); err != nil {
			return err
		}
	}
	branch, marker := revertIdentity(repo, member)
	if state.Branch != "" && state.Branch != branch {
		return errRevertManual
	}
	state.Branch = branch
	save := func(status string) (bool, error) {
		state.State = status
		return server.config.TopicStore.saveRevert(ctx, member, &state)
	}
	manual := func() error {
		_, err := save("manual")
		return err
	}
	readPull := func() (bool, error) {
		number, err := client.findInversePull(ctx, repo, branch, member.TargetBranch, state.InverseSHA, marker, server.config.MaxPages)
		if err == nil && number > 0 {
			state.PullNumber = number
			_, err = save("opened")
			return true, err
		}
		return false, err
	}
	if state.State == "pull_pending" {
		_, err := readPull()
		return err
	}
	merge, err := client.readRevertCommit(ctx, repoURL, merged.MergeSHA)
	if err != nil {
		return err
	}
	if len(merge.Parents) != 2 || merge.Parents[1].SHA != original.Head.SHA || !commitSHA.MatchString(original.Head.SHA) {
		return errRevertManual
	}
	parent, err := client.readRevertCommit(ctx, repoURL, merge.Parents[0].SHA)
	if err != nil {
		return err
	}
	if state.State == "" {
		if err := checkRevertTarget(ctx, client, repoURL, member.TargetBranch, merge.SHA); err != nil {
			return err
		}
		changes, err := client.inverseChanges(ctx, repoURL, merge.Commit.Tree.SHA, parent.Commit.Tree.SHA)
		if err != nil {
			return err
		}
		if err := checkRevertTarget(ctx, client, repoURL, member.TargetBranch, merge.SHA); err != nil {
			return err
		}
		claimed, err := save("branch_pending")
		if err != nil || !claimed {
			return err
		}
		status, err := client.createInverseBranch(ctx, repoURL, member.TargetBranch, branch, marker, changes)
		if status == http.StatusUnauthorized {
			return ErrUnauthorized
		}
		if err == nil && status != http.StatusCreated && isMergeRejection(status) {
			return manual()
		}
	}
	inverseBranch, err := client.readRevertBranch(ctx, repoURL, branch)
	if errors.Is(err, ErrNotVisible) {
		return nil
	}
	if err != nil {
		return err
	}
	inverse, err := client.readRevertCommit(ctx, repoURL, inverseBranch.Commit.ID)
	if err != nil {
		return err
	}
	if inverse.Commit.Message != marker && inverse.Commit.Message != marker+"\n" ||
		len(inverse.Parents) != 1 || inverse.Parents[0].SHA != merge.SHA || inverse.Commit.Tree.SHA != parent.Commit.Tree.SHA {
		return manual()
	}
	if reconcile {
		return nil
	}
	if err := checkRevertTarget(ctx, client, repoURL, member.TargetBranch, merge.SHA); err != nil {
		if errors.Is(err, errRevertManual) {
			return manual()
		}
		return err
	}
	state.InverseSHA = inverse.SHA
	if found, err := readPull(); err != nil || found {
		return err
	}
	claimed, err := save("pull_pending")
	if err != nil || !claimed {
		return err
	}
	status, err := client.createInversePull(ctx, repoURL, topic.Topic, branch, member.TargetBranch, marker)
	if errors.Is(err, ErrUnauthorized) {
		return err
	}
	if found, readErr := readPull(); found || readErr != nil {
		return readErr
	}
	if err == nil && status != http.StatusConflict && isMergeRejection(status) {
		return manual()
	}
	return nil
}

func checkRevertTarget(ctx context.Context, client *Gitea, repoURL *url.URL, target, expected string) error {
	branch, err := client.readRevertBranch(ctx, repoURL, target)
	if err != nil {
		return err
	}
	if branch.Commit.ID != expected || branch.Protected == nil || !*branch.Protected || branch.RequiredApprovals < 1 ||
		branch.UserCanPush == nil || *branch.UserCanPush {
		return errRevertManual
	}
	return nil
}
