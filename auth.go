package bridge

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	stateCookie     = "bridge_oauth_state"
	sessionCookie   = "bridge_session"
	loginLifetime   = 5 * time.Minute
	sessionLifetime = time.Hour
)

type expiringEntry[Value any] struct {
	value   Value
	expires time.Time
}

type expiringStore[Value any] struct {
	mu      sync.Mutex
	entries map[string]expiringEntry[Value]
}

func (store *expiringStore[Value]) put(key string, value Value, lifetime time.Duration) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := time.Now()
	if store.entries == nil {
		store.entries = make(map[string]expiringEntry[Value])
	}
	if len(store.entries) >= 4096 {
		for existing, entry := range store.entries {
			if !now.Before(entry.expires) {
				delete(store.entries, existing)
			}
		}
		if len(store.entries) >= 4096 {
			return false
		}
	}
	store.entries[key] = expiringEntry[Value]{value, now.Add(lifetime)}
	return true
}

func (store *expiringStore[Value]) get(key string, consume bool) (Value, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, exists := store.entries[key]
	valid := exists && time.Now().Before(entry.expires)
	if consume || !valid {
		delete(store.entries, key)
	}
	if !valid {
		var zero Value
		return zero, false
	}
	return entry.value, true
}

type loginAttempt struct {
	verifier    string
	returnQuery string
	returnPath  string
}

func randomID() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}

func cookieValue(request *http.Request, name string) string {
	cookie, err := request.Cookie(name)
	if err != nil || len(cookie.Value) != 43 {
		return ""
	}
	return cookie.Value
}

func (server *Server) setCookie(writer http.ResponseWriter, name, value string, maxAge int) {
	path := "/bridge/"
	if name == stateCookie {
		path = "/bridge/auth/"
	}
	http.SetCookie(writer, &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: true, Secure: server.public.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

func (server *Server) startLogin(writer http.ResponseWriter, request *http.Request) {
	state, stateErr := randomID()
	verifier, verifierErr := randomID()
	returnQuery := url.Values{}
	query := request.URL.Query()
	returnPath := "/bridge/"
	if query.Get("page") == "receipts" && server.receipts != nil {
		returnPath = "/bridge/receipts/"
		if topicIDPattern.MatchString(query.Get("topic_id")) {
			returnQuery.Set("topic_id", query.Get("topic_id"))
		}
		if topicIDPattern.MatchString(query.Get("id")) {
			returnQuery.Set("id", query.Get("id"))
		}
	} else if topicIDPattern.MatchString(query.Get("topic_id")) {
		returnQuery.Set("topic_id", query.Get("topic_id"))
	} else if projectID.MatchString(query.Get("overview")) {
		returnQuery.Set("overview", query.Get("overview"))
	} else if projectID.MatchString(query.Get("project_id")) && topicPattern.MatchString(query.Get("topic")) {
		returnQuery.Set("project_id", query.Get("project_id"))
		returnQuery.Set("topic", query.Get("topic"))
	}
	if previous := cookieValue(request, stateCookie); previous != "" {
		server.pending.get(previous, true)
	}
	if stateErr != nil || verifierErr != nil || !server.pending.put(state, loginAttempt{verifier, returnQuery.Encode(), returnPath}, loginLifetime) {
		http.Error(writer, "暂时无法开始登录，请稍后重试。", http.StatusServiceUnavailable)
		return
	}
	server.setCookie(writer, stateCookie, state, int(loginLifetime.Seconds()))
	sum := sha256.Sum256([]byte(verifier))
	scope := "read:user read:repository"
	if server.config.EnablePullCreation || server.config.EnableMerge || server.config.EnableRevert {
		scope = "read:user write:repository"
	}
	authorize := server.giteaPublic.JoinPath("login", "oauth", "authorize")
	authorize.RawQuery = url.Values{
		"client_id": {server.config.ClientID}, "response_type": {"code"}, "redirect_uri": {server.callbackURL()},
		"scope": {scope}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}.Encode()
	http.Redirect(writer, request, authorize.String(), http.StatusSeeOther)
}

func (server *Server) callbackURL() string {
	return server.public.JoinPath("auth", "callback").String()
}

func (server *Server) finishLogin(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	state := query.Get("state")
	if len(state) != 43 || subtle.ConstantTimeCompare([]byte(state), []byte(cookieValue(request, stateCookie))) != 1 {
		http.Error(writer, "登录请求无效，请从看板重新登录。", http.StatusBadRequest)
		return
	}
	server.setCookie(writer, stateCookie, "", -1)
	attempt, ok := server.pending.get(state, true)
	code := query.Get("code")
	if !ok || query.Get("error") != "" || code == "" || len(code) > 4096 {
		http.Error(writer, "登录未完成，请从看板重新登录。", http.StatusBadRequest)
		return
	}
	form := url.Values{
		"client_id": {server.config.ClientID}, "client_secret": {server.config.ClientSecret}, "grant_type": {"authorization_code"},
		"redirect_uri": {server.callbackURL()}, "code": {code}, "code_verifier": {attempt.verifier},
	}
	tokenEndpoint := server.giteaAPI.JoinPath("login", "oauth", "access_token")
	upstream, err := http.NewRequestWithContext(request.Context(), http.MethodPost, tokenEndpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		http.Error(writer, "无法完成 Gitea 登录。", http.StatusBadGateway)
		return
	}
	upstream.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	upstream.Header.Set("Accept", "application/json")
	response, err := server.http.Do(upstream)
	if err != nil {
		http.Error(writer, "无法完成 Gitea 登录。", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	const maxTokenResponse = 64 << 10
	data, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponse+1))
	if response.StatusCode != http.StatusOK || err != nil || len(data) > maxTokenResponse || json.Unmarshal(data, &token) != nil || token.AccessToken == "" || !strings.EqualFold(token.TokenType, "bearer") || token.ExpiresIn < 1 {
		http.Error(writer, "无法完成 Gitea 登录。", http.StatusBadGateway)
		return
	}
	lifetime := sessionLifetime
	if token.ExpiresIn < int64(lifetime/time.Second) {
		lifetime = time.Duration(token.ExpiresIn) * time.Second
	}
	if previous := cookieValue(request, sessionCookie); previous != "" {
		server.sessions.get(previous, true)
	}
	session, err := randomID()
	if err != nil || !server.sessions.put(session, token.AccessToken, lifetime) {
		http.Error(writer, "暂时无法建立会话，请稍后重试。", http.StatusServiceUnavailable)
		return
	}
	server.setCookie(writer, sessionCookie, session, int(lifetime.Seconds()))
	location := attempt.returnPath
	if attempt.returnQuery != "" {
		location += "?" + attempt.returnQuery
	}
	http.Redirect(writer, request, location, http.StatusSeeOther)
}

func (server *Server) checkOrigin(writer http.ResponseWriter, request *http.Request) bool {
	if request.Header.Get("Origin") != server.public.Scheme+"://"+server.public.Host {
		responseProblem(writer, http.StatusForbidden, "invalid_origin")
		return false
	}
	return true
}

func (server *Server) logout(writer http.ResponseWriter, request *http.Request) {
	if !server.checkOrigin(writer, request) {
		return
	}
	server.sessions.get(cookieValue(request, sessionCookie), true)
	server.setCookie(writer, sessionCookie, "", -1)
	writer.WriteHeader(http.StatusNoContent)
}
