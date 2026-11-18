package bridge

import (
	"errors"
	"net/http"
	"time"
)

type receiptVersion struct {
	ID       string    `json:"id"`
	Version  int       `json:"version"`
	IssuedAt time.Time `json:"issued_at"`
}

func (server *Server) listReceipts(writer http.ResponseWriter, request *http.Request, client *Gitea, user GiteaUser) {
	ctx := request.Context()
	store := server.config.TopicStore
	query := request.URL.Query()
	topicID := query.Get("topic_id")
	if !topicIDPattern.MatchString(topicID) {
		server.receiptProblem(writer, request, ErrNotVisible)
		return
	}
	before := query.Get("before")
	if before != "" && !topicIDPattern.MatchString(before) {
		responseProblem(writer, 400, "invalid_receipt_cursor")
		return
	}
	topic, err := store.get(ctx, topicID)
	if err != nil {
		server.receiptProblem(writer, request, err)
		return
	}
	repos, err := server.authorizedTopicRepos(ctx, client, topic)
	if err != nil {
		server.receiptProblem(writer, request, err)
		return
	}
	if before != "" {
		raw, err := store.receiptByID(ctx, before)
		if err != nil {
			server.receiptProblem(writer, request, err)
			return
		}
		_, prior, err := server.loadAuthorizedReceipt(ctx, client, before, raw)
		if err != nil {
			server.receiptProblem(writer, request, err)
			return
		}
		if prior.TopicID != topicID {
			server.receiptProblem(writer, request, ErrNotVisible)
			return
		}
	}
	ids, next, err := store.receiptIDs(ctx, topicID, before)
	if err != nil {
		server.receiptProblem(writer, request, err)
		return
	}
	versions := make([]receiptVersion, 0, len(ids))
	allowed := make(map[int64]Repository, len(repos))
	for _, repo := range repos {
		allowed[repo.ID] = repo
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			server.receiptProblem(writer, request, ctx.Err())
			return
		}
		raw, err := store.receiptByID(ctx, id)
		if err != nil {
			server.receiptProblem(writer, request, err)
			return
		}
		_, doc, err := readVerifiedReceipt(raw, server.receipts.public)
		if err != nil {
			server.receiptProblem(writer, request, err)
			return
		}
		if doc.TopicID != topicID || doc.ID != id || doc.ProjectID != topic.ProjectID {
			continue
		}
		visible := true
		for _, member := range doc.Members {
			repo, ok := allowed[member.Repository.ID]
			if !ok {
				visible = false
				break
			}
			if member.SourceRepositoryID != 0 {
				_, err := client.memberFork(ctx, server.projects[topic.ProjectID], TopicMember{RepositoryID: repo.ID, SourceRepositoryID: member.SourceRepositoryID}, false)
				if errors.Is(err, ErrNotVisible) {
					visible = false
					break
				}
				if err != nil {
					server.receiptProblem(writer, request, err)
					return
				}
			}
			if member.Source != nil && member.Source.Kind == "release" {
				err := client.canReadReceiptRelease(ctx, repo, member.Source.ObjectID)
				if errors.Is(err, ErrNotVisible) {
					visible = false
					break
				}
				if err != nil {
					server.receiptProblem(writer, request, err)
					return
				}
			}
		}
		if visible {
			versions = append(versions, receiptVersion{id, doc.Version, doc.IssuedAt})
		}
	}
	if len(versions) != len(ids) {
		server.receiptProblem(writer, request, ErrNotVisible)
		return
	}
	responseJSON(writer, 200, struct {
		Topic        string           `json:"topic"`
		Repositories []Repository     `json:"repositories"`
		Versions     []receiptVersion `json:"versions"`
		Before       string           `json:"before,omitempty"`
		CanIssue     bool             `json:"can_issue"`
	}{topic.Topic, repos, versions, next, server.receipts.issuers[user.ID] && before == ""})
}
