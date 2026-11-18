package bridge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	requestKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	topicIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	branchPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

type topicSelection struct {
	ProjectID string `json:"project_id"`
	Topic     string `json:"topic"`
	Members   []struct {
		RepositoryID int64  `json:"repository_id"`
		SourceBranch string `json:"source_branch,omitempty"`
		TargetBranch string `json:"target_branch,omitempty"`
	} `json:"members,omitempty"`
}

func validBranch(branch string) bool {
	if !branchPattern.MatchString(branch) || strings.Contains(branch, "..") || strings.HasSuffix(branch, ".") {
		return false
	}
	for _, component := range strings.Split(branch, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return branch != "HEAD"
}

func decodeRequest(writer http.ResponseWriter, request *http.Request, value any) bool {
	contentType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		responseProblem(writer, http.StatusUnsupportedMediaType, "json_required")
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil || decoder.Decode(new(any)) != io.EOF {
		responseProblem(writer, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}

func (server *Server) authorizedTopicRepos(ctx context.Context, client *Gitea, topic TopicChangeSet) ([]Repository, error) {
	project, exists := server.projects[topic.ProjectID]
	if !exists {
		return nil, ErrNotVisible
	}
	allowed := make(map[int64]Repository, len(project.Repositories))
	for _, repo := range project.Repositories {
		allowed[repo.ID] = repo
	}
	repos := make([]Repository, 0, len(topic.Members))
	for _, member := range topic.Members {
		repo, exists := allowed[member.RepositoryID]
		if !exists {
			return nil, ErrNotVisible
		}
		visible, err := client.CanReadPullRequests(ctx, repo)
		if err != nil {
			return nil, err
		}
		if !visible {
			return nil, ErrNotVisible
		}
		repos = append(repos, repo)
	}
	return repos, nil
}

func (server *Server) topicProblem(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, ErrNotVisible):
		responseProblem(writer, http.StatusNotFound, "topic_not_found")
	case errors.Is(err, errIdempotencyConflict):
		responseProblem(writer, http.StatusConflict, "idempotency_conflict")
	default:
		server.upstreamProblem(writer, request, err)
	}
}

func (server *Server) createTopic(writer http.ResponseWriter, request *http.Request, client *Gitea, user GiteaUser) {
	if !server.checkOrigin(writer, request) {
		return
	}
	key := request.Header.Get("Idempotency-Key")
	if !requestKeyPattern.MatchString(key) {
		responseProblem(writer, http.StatusBadRequest, "invalid_idempotency_key")
		return
	}
	var input topicSelection
	if !decodeRequest(writer, request, &input) {
		return
	}
	if !projectID.MatchString(input.ProjectID) || !topicPattern.MatchString(input.Topic) || len(input.Members) > 64 || (input.Members != nil && len(input.Members) == 0) {
		responseProblem(writer, http.StatusBadRequest, "invalid_topic")
		return
	}
	sort.Slice(input.Members, func(first, second int) bool {
		return input.Members[first].RepositoryID < input.Members[second].RepositoryID
	})
	topic := TopicChangeSet{ProjectID: input.ProjectID, Topic: input.Topic, CreatorID: user.ID, CreatedAt: time.Now().UTC()}
	var previousRepoID int64
	for _, member := range input.Members {
		hasBranches := member.SourceBranch != "" || member.TargetBranch != ""
		if member.RepositoryID <= previousRepoID || (hasBranches && (!validBranch(member.SourceBranch) || !validBranch(member.TargetBranch) || member.SourceBranch == member.TargetBranch)) {
			responseProblem(writer, http.StatusBadRequest, "invalid_member")
			return
		}
		topic.Members = append(topic.Members, TopicMember{RepositoryID: member.RepositoryID, SourceBranch: member.SourceBranch, TargetBranch: member.TargetBranch})
		previousRepoID = member.RepositoryID
	}
	payload, err := json.Marshal(input)
	if err != nil {
		responseProblem(writer, http.StatusInternalServerError, "invalid_topic")
		return
	}
	digest := sha256.Sum256(payload)
	hash := hex.EncodeToString(digest[:])
	ctx := request.Context()
	store := server.config.TopicStore
	existing, existingHash, err := store.findRequest(ctx, user.ID, key)
	if err == nil {
		if _, err := server.authorizedTopicRepos(ctx, client, existing); err != nil {
			server.topicProblem(writer, request, err)
			return
		}
		if existingHash != hash {
			server.topicProblem(writer, request, errIdempotencyConflict)
			return
		}
		writeSavedTopic(writer, http.StatusOK, existing)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		responseProblem(writer, http.StatusInternalServerError, "topic_storage_failed")
		return
	}
	project, exists := server.projects[topic.ProjectID]
	if !exists {
		server.topicProblem(writer, request, ErrNotVisible)
		return
	}
	if input.Members == nil {
		for _, repo := range project.Repositories {
			topic.Members = append(topic.Members, TopicMember{RepositoryID: repo.ID})
		}
	}
	if _, err := server.authorizedTopicRepos(ctx, client, topic); err != nil {
		server.topicProblem(writer, request, err)
		return
	}
	topic.ID, err = randomID()
	if err != nil {
		responseProblem(writer, http.StatusInternalServerError, "topic_storage_failed")
		return
	}
	topic, created, err := store.create(ctx, topic, key, hash)
	if err != nil {
		if errors.Is(err, errIdempotencyConflict) {
			if _, checkErr := server.authorizedTopicRepos(ctx, client, topic); checkErr != nil {
				server.topicProblem(writer, request, checkErr)
				return
			}
			server.topicProblem(writer, request, err)
			return
		}
		responseProblem(writer, http.StatusInternalServerError, "topic_storage_failed")
		return
	}
	if !created {
		if _, err := server.authorizedTopicRepos(ctx, client, topic); err != nil {
			server.topicProblem(writer, request, err)
			return
		}
		writeSavedTopic(writer, http.StatusOK, topic)
		return
	}
	writeSavedTopic(writer, http.StatusCreated, topic)
}

func writeSavedTopic(writer http.ResponseWriter, status int, topic TopicChangeSet) {
	writer.Header().Set("Location", "/bridge/api/topics/"+topic.ID)
	responseJSON(writer, status, topic)
}

func (server *Server) getTopic(writer http.ResponseWriter, request *http.Request, client *Gitea, _ GiteaUser) {
	id := request.PathValue("id")
	if !topicIDPattern.MatchString(id) {
		server.topicProblem(writer, request, ErrNotVisible)
		return
	}
	ctx := request.Context()
	topic, err := server.config.TopicStore.get(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			server.topicProblem(writer, request, err)
			return
		}
		responseProblem(writer, http.StatusInternalServerError, "topic_storage_failed")
		return
	}
	repos, err := server.authorizedTopicRepos(ctx, client, topic)
	if err != nil {
		server.topicProblem(writer, request, err)
		return
	}
	results, err := Lookup(ctx, client, repos, topic.Topic, server.config.MaxPages)
	if err == nil {
		results, err = client.inspectMatches(ctx, results)
	}
	if err != nil {
		server.topicProblem(writer, request, err)
		return
	}
	if len(results) != len(repos) {
		server.topicProblem(writer, request, ErrNotVisible)
		return
	}
	responseJSON(writer, http.StatusOK, struct {
		TopicChangeSet
		Repositories []RepositoryResult `json:"repositories"`
		ReadAt       time.Time          `json:"read_at"`
	}{topic, results, time.Now().UTC()})
}
