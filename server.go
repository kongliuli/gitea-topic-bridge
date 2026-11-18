package bridge

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Project struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Repositories []Repository `json:"repositories"`
}

type ServerConfig struct {
	PublicBaseURL      string      `json:"public_base_url"`
	GiteaPublicURL     string      `json:"gitea_public_url"`
	GiteaAPIURL        string      `json:"gitea_api_url"`
	ClientID           string      `json:"client_id"`
	ClientSecret       string      `json:"-"`
	DatabasePath       string      `json:"database_path"`
	TopicStore         *TopicStore `json:"-"`
	AllowLoopbackHTTP  bool        `json:"allow_loopback_http"`
	EnablePullCreation bool        `json:"enable_pull_creation"`
	PageSize           int         `json:"page_size"`
	MaxPages           int         `json:"max_pages"`
	Projects           []Project   `json:"projects"`
}

type Server struct {
	config                        ServerConfig
	public, giteaPublic, giteaAPI *url.URL
	http                          *http.Client
	projects                      map[string]Project
	gate                          chan struct{}
	pending                       expiringStore[loginAttempt]
	sessions                      expiringStore[string]
}

var projectID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

//go:embed web/index.html web/app.js web/style.css
var site embed.FS

func NewServer(config ServerConfig) (http.Handler, error) {
	if config.EnablePullCreation && config.TopicStore == nil {
		return nil, errors.New("enable_pull_creation requires a Bridge database")
	}
	public, publicErr := baseURL(config.PublicBaseURL)
	giteaPublic, giteaPublicErr := baseURL(config.GiteaPublicURL)
	giteaAPI, giteaAPIErr := baseURL(config.GiteaAPIURL)
	if publicErr != nil || giteaPublicErr != nil || giteaAPIErr != nil || (public.Path != "/bridge" && public.Path != "/bridge/") || public.RawPath != "" {
		return nil, errors.New("invalid URLs; public_base_url must end in /bridge/")
	}
	if !config.AllowLoopbackHTTP && (public.Scheme != "https" || giteaPublic.Scheme != "https") {
		return nil, errors.New("public URLs require HTTPS (except explicit loopback testing)")
	}
	if strings.TrimSpace(config.ClientID) == "" || strings.TrimSpace(config.ClientSecret) == "" || config.PageSize < 1 || config.PageSize > 100 || config.MaxPages < 1 || config.MaxPages > 100 || len(config.Projects) < 1 || len(config.Projects) > 64 {
		return nil, errors.New("OAuth credentials, pagination settings or project count are invalid")
	}
	server := &Server{config: config, public: public, giteaPublic: giteaPublic, giteaAPI: giteaAPI, http: newUpstreamClient(), projects: make(map[string]Project), gate: make(chan struct{}, 16)}
	repoVerifier := Gitea{apiBase: giteaAPI}
	for _, project := range config.Projects {
		if !projectID.MatchString(project.ID) || strings.TrimSpace(project.Name) == "" || len(project.Name) > 160 || len(project.Repositories) < 1 || len(project.Repositories) > 64 || server.projects[project.ID].ID != "" {
			return nil, errors.New("invalid or duplicate project")
		}
		seen := make(map[int64]bool)
		for _, repo := range project.Repositories {
			if _, err := repoVerifier.repoURL(repo); err != nil || seen[repo.ID] {
				return nil, errors.New("invalid or duplicate repository in project")
			}
			seen[repo.ID] = true
		}
		server.projects[project.ID] = project
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bridge/{$}", serveAsset("index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /bridge/app.js", serveAsset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /bridge/style.css", serveAsset("style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /bridge/auth/login", server.startLogin)
	mux.HandleFunc("GET /bridge/auth/callback", server.finishLogin)
	mux.HandleFunc("POST /bridge/auth/logout", server.logout)
	mux.HandleFunc("GET /bridge/api/projects", server.withUser(server.listProjects))
	mux.HandleFunc("GET /bridge/api/topics/lookup", server.withUser(server.lookupTopic))
	if config.TopicStore != nil {
		mux.HandleFunc("POST /bridge/api/topics", server.withUser(server.createTopic))
		mux.HandleFunc("GET /bridge/api/topics/{id}", server.withUser(server.getTopic))
	}
	if config.EnablePullCreation {
		mux.HandleFunc("POST /bridge/api/topics/{id}/pulls", server.withUser(server.createTopicPulls))
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		mux.ServeHTTP(writer, request)
	}), nil
}

func serveAsset(name, contentType string) http.HandlerFunc {
	data, err := fs.ReadFile(site, "web/"+name)
	if err != nil {
		panic(err)
	}
	return func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", contentType)
		writer.Write(data)
	}
}

func responseJSON(writer http.ResponseWriter, status int, data any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	json.NewEncoder(writer).Encode(data)
}

func responseProblem(writer http.ResponseWriter, status int, code string) {
	responseJSON(writer, status, map[string]string{"error": code})
}

func (server *Server) withUser(handler func(http.ResponseWriter, *http.Request, *Gitea, GiteaUser)) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		sessionID := cookieValue(request, sessionCookie)
		token, ok := server.sessions.get(sessionID, false)
		if !ok {
			responseProblem(writer, http.StatusUnauthorized, "login_required")
			return
		}
		select {
		case server.gate <- struct{}{}:
			defer func() { <-server.gate }()
		default:
			responseProblem(writer, http.StatusServiceUnavailable, "busy")
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), 25*time.Second)
		defer cancel()
		client := &Gitea{apiBase: server.giteaAPI, publicBase: server.giteaPublic, token: token, pageSize: server.config.PageSize, http: server.http}
		user, err := client.CurrentUser(ctx)
		if err != nil {
			server.upstreamProblem(writer, request, err)
			return
		}
		handler(writer, request.WithContext(ctx), client, user)
	}
}

func (server *Server) upstreamProblem(writer http.ResponseWriter, request *http.Request, err error) {
	if errors.Is(err, ErrUnauthorized) {
		server.sessions.get(cookieValue(request, sessionCookie), true)
		server.setCookie(writer, sessionCookie, "", -1)
		responseProblem(writer, http.StatusUnauthorized, "login_required")
		return
	}
	responseProblem(writer, http.StatusBadGateway, "upstream_unavailable")
}

func (server *Server) listProjects(writer http.ResponseWriter, request *http.Request, client *Gitea, user GiteaUser) {
	type projectSummary struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	projects := make([]projectSummary, 0)
	visibility := make(map[Repository]bool)
	for _, project := range server.config.Projects {
		for _, repo := range project.Repositories {
			visible, checked := visibility[repo]
			if !checked {
				var err error
				visible, err = client.CanReadPullRequests(request.Context(), repo)
				if err != nil {
					server.upstreamProblem(writer, request, err)
					return
				}
				visibility[repo] = visible
			}
			if visible {
				projects = append(projects, projectSummary{ID: project.ID, Name: project.Name})
				break
			}
		}
	}
	responseJSON(writer, http.StatusOK, struct {
		User         string           `json:"user"`
		Projects     []projectSummary `json:"projects"`
		SavedTopics  bool             `json:"saved_topics"`
		PullCreation bool             `json:"pull_creation"`
	}{user.Login, projects, server.config.TopicStore != nil, server.config.EnablePullCreation})
}

func (server *Server) lookupTopic(writer http.ResponseWriter, request *http.Request, client *Gitea, _ GiteaUser) {
	query := request.URL.Query()
	topic := query.Get("topic")
	if !projectID.MatchString(query.Get("project_id")) || !topicPattern.MatchString(topic) {
		responseProblem(writer, http.StatusBadRequest, "invalid_query")
		return
	}
	project, exists := server.projects[query.Get("project_id")]
	if !exists {
		responseProblem(writer, http.StatusNotFound, "project_not_found")
		return
	}
	results, err := Lookup(request.Context(), client, project.Repositories, topic, server.config.MaxPages)
	if err != nil {
		server.upstreamProblem(writer, request, err)
		return
	}
	results, err = client.inspectMatches(request.Context(), results)
	if err != nil {
		server.upstreamProblem(writer, request, err)
		return
	}
	if len(results) == 0 {
		responseProblem(writer, http.StatusNotFound, "project_not_found")
		return
	}
	complete, found := true, false
	for _, result := range results {
		complete = complete && result.Status == "complete"
		found = found || len(result.PullRequests) != 0
	}
	responseJSON(writer, http.StatusOK, struct {
		Repositories []RepositoryResult `json:"repositories"`
		Complete     bool               `json:"complete"`
		Found        bool               `json:"found"`
		ReadAt       time.Time          `json:"read_at"`
	}{results, complete, found, time.Now().UTC()})
}
