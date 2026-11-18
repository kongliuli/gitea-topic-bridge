package bridge

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBrowserOAuthAndLookup(t *testing.T) {
	const base = "https://git.example.com/bridge/"
	const token = "fake-upstream-token"
	var challenge atomic.Value
	var exchangeCount atomic.Int32
	var revoked atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/login/oauth/access_token" {
			exchangeCount.Add(1)
			if err := request.ParseForm(); err != nil {
				t.Error(err)
				writer.WriteHeader(400)
				return
			}
			verifier := sha256.Sum256([]byte(request.Form.Get("code_verifier")))
			if request.Method != "POST" || request.Form.Get("redirect_uri") != base+"auth/callback" || request.Form.Get("client_secret") != "fake-secret" ||
				request.Form.Get("code") != "issued-code" || base64.RawURLEncoding.EncodeToString(verifier[:]) != challenge.Load() {
				t.Error("invalid authorization code exchange")
				writer.WriteHeader(400)
				return
			}
			fmt.Fprintf(writer, `{"access_token":%q,"token_type":"bearer","expires_in":3600}`, token)
			return
		}
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Error("lookup did not use browser user's server-side token")
		}
		switch request.URL.Path {
		case "/api/v1/user":
			if revoked.Load() {
				writer.WriteHeader(401)
				return
			}
			fmt.Fprint(writer, `{"id":7,"login":"labreader"}`)
		case "/api/v1/repos/team/open":
			fmt.Fprint(writer, `{"id":41}`)
		case "/api/v1/repos/team/open/pulls":
			writer.Header().Set("X-Total-Count", "2")
			fmt.Fprint(writer, `[{"number":1,"title":"<b>fix</b>","body":"[Topic: CAD-01] private body","state":"open","base":{"repo_id":41},"head":{"repo_id":41}}]`)
		case "/api/v1/repos/team/hidden":
			writer.WriteHeader(404)
		default:
			t.Errorf("unexpected route: %s", request.URL.Path)
			writer.WriteHeader(500)
		}
	}))
	defer upstream.Close()
	visible := Repository{ID: 41, Owner: "team", Name: "open"}
	hidden := Repository{ID: 42, Owner: "team", Name: "hidden"}
	handler, err := NewServer(ServerConfig{PublicBaseURL: base, GiteaPublicURL: "https://git.example.com", GiteaAPIURL: upstream.URL, ClientID: "client", ClientSecret: "fake-secret", PageSize: 1, MaxPages: 1,
		Projects: []Project{{ID: "team", Name: "Team", Repositories: []Repository{visible, hidden}}, {ID: "secret", Name: "Secret", Repositories: []Repository{hidden}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	page := func(method, endpoint string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, endpoint, nil)
		request.Host = "ignored.test"
		request.Header.Set("Origin", origin)
		if cookie != nil {
			request.AddCookie(cookie)
		}
		record := httptest.NewRecorder()
		handler.ServeHTTP(record, request)
		return record
	}
	const projects = "/bridge/api/projects"
	if got := page("GET", projects, nil, ""); got.Code != 401 {
		t.Fatalf("unsigned project request returned %d", got.Code)
	}
	if got := page("GET", "/bridge/", nil, ""); got.Code != 200 || !strings.Contains(got.Body.String(), "/bridge/app.js") || got.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("dashboard HTML or its security headers were missing")
	}
	begin := page("GET", "/bridge/auth/login?project_id=team&topic=CAD-01&return_to=https://elsewhere.test", nil, "")
	location, err := url.Parse(begin.Header().Get("Location"))
	if err != nil || begin.Code != 303 || location.Host != "git.example.com" || location.Path != "/login/oauth/authorize" ||
		location.Query().Get("redirect_uri") != base+"auth/callback" || location.Query().Get("scope") != "read:user read:repository" || location.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("wrong OAuth start: %d %v", begin.Code, location)
	}
	challenge.Store(location.Query().Get("code_challenge"))
	state := requireCookie(t, begin, stateCookie)
	callback := "/bridge/auth/callback?code=issued-code&state=" + location.Query().Get("state")
	if got := page("GET", callback, nil, ""); got.Code != 400 || exchangeCount.Load() != 0 {
		t.Fatal("callback was accepted without the initiating cookie")
	}
	finish := page("GET", callback, state, "")
	if finish.Code != 303 || finish.Header().Get("Location") != "/bridge/?project_id=team&topic=CAD-01" || strings.Contains(finish.Body.String(), token) {
		t.Fatalf("login callback returned %d", finish.Code)
	}
	session := requireCookie(t, finish, sessionCookie)
	if session.Value == token || session.Path != "/bridge/" || session.Value == state.Value {
		t.Fatal("session did not use a separate opaque cookie")
	}
	if got := page("GET", callback, state, ""); got.Code != 400 || exchangeCount.Load() != 1 {
		t.Fatal("callback was replayable")
	}
	if got := page("GET", projects, session, ""); got.Code != 200 || got.Header().Get("Cache-Control") != "no-store" || !strings.Contains(got.Body.String(), `"user":"labreader"`) || strings.Contains(got.Body.String(), "Secret") {
		t.Fatalf("unexpected project listing: %d %s", got.Code, got.Body)
	}
	lookup := "/bridge/api/topics/lookup?topic=CAD-01&project_id="
	if got := page("GET", lookup+"team", session, ""); got.Code != 200 || !strings.Contains(got.Body.String(), `"complete":false,"found":true`) || strings.Contains(got.Body.String(), "hidden") || strings.Contains(got.Body.String(), "private body") {
		t.Fatalf("unexpected lookup result: %d %s", got.Code, got.Body)
	}
	unknown := page("GET", lookup+"unknown", session, "")
	forbidden := page("GET", lookup+"secret", session, "")
	if forbidden.Code != 404 || unknown.Code != forbidden.Code || unknown.Body.String() != forbidden.Body.String() {
		t.Fatal("hidden project was distinguishable from an unknown project")
	}
	if got := page("POST", "/bridge/auth/logout", session, "https://elsewhere.test"); got.Code != 403 {
		t.Fatal("logout accepted a cross-origin request")
	}
	if got := page("GET", projects, session, ""); got.Code != 200 {
		t.Fatal("cross-origin logout ended the session")
	}
	revoked.Store(true)
	if got := page("GET", projects, session, ""); got.Code != 401 {
		t.Fatal("invalid upstream token kept the Bridge session active")
	}
	revoked.Store(false)
	if got := page("GET", projects, session, ""); got.Code != 401 {
		t.Fatal("revoked Bridge session was reused")
	}
}

func requireCookie(t *testing.T, recorder *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == name {
			if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge <= 0 {
				t.Fatalf("cookie %s is missing protections", name)
			}
			return cookie
		}
	}
	t.Fatalf("cookie %s is missing", name)
	return nil
}
