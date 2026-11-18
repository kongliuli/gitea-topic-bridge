package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGiteaLookupHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer reader-token" {
			t.Errorf("unexpected Gitea request: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(401)
			return
		}
		switch request.URL.Path {
		case "/git/api/v1/repos/team/visible":
			fmt.Fprint(writer, `{"id":17}`)
		case "/git/api/v1/repos/team/hidden":
			writer.WriteHeader(404)
		case "/git/api/v1/repos/team/recreated":
			fmt.Fprint(writer, `{"id":999}`)
		case "/git/api/v1/repos/team/visible/pulls":
			query := request.URL.Query()
			if query.Get("state") != "all" || query.Get("limit") != "1" || query.Get("sort") != "oldest" {
				t.Errorf("unexpected query: %s", request.URL.RawQuery)
			}
			writer.Header().Set("X-Total-Count", "2")
			if query.Get("page") == "1" {
				writer.Header().Set("Link", `<https://unexpected.invalid/>; rel="next"`)
				fmt.Fprint(writer, `[{"number":1,"title":"[Topic: A-1] fix","state":"closed","merged":true,"base":{"repo_id":17}}]`)
			} else {
				fmt.Fprint(writer, `[{"number":2,"title":"fix","body":"[Topic: A-1]","state":"open","base":{"repo_id":17},"head":{"sha":"abc"}}]`)
			}
		default:
			t.Errorf("requested an unapproved endpoint: %s", request.URL.Path)
			writer.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	client, err := NewGitea(upstream.URL+"/git", "https://git.example.com", "reader-token", 1)
	if err != nil {
		t.Fatal(err)
	}
	repos := []Repository{{ID: 17, Owner: "team", Name: "visible"}, {ID: 18, Owner: "team", Name: "hidden"}, {ID: 19, Owner: "team", Name: "recreated"}}
	results, err := Lookup(context.Background(), client, repos, "A-1", 2)
	if err != nil || len(results) != 1 || results[0].Status != "complete" || len(results[0].PullRequests) != 2 {
		t.Fatalf("got %#v, %v", results, err)
	}
	pulls := results[0].PullRequests
	if pulls[0].Lifecycle != "merged" || pulls[1].Lifecycle != "open" || pulls[1].URL != "https://git.example.com/team/visible/pulls/2" || pulls[1].HeadSHA != "abc" {
		t.Fatalf("unexpected PR mapping: %#v", pulls)
	}
}

func TestGiteaLookupRevocationHTTP(t *testing.T) {
	for _, scenario := range []struct{ status, resultCount int }{{404, 0}, {401, 0}, {500, 1}} {
		t.Run(fmt.Sprint(scenario.status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/api/v1/repos/team/visible" {
					fmt.Fprint(writer, `{"id":17}`)
				} else if request.URL.Query().Get("limit") == "1" {
					writer.Header().Set("X-Total-Count", "1")
					fmt.Fprint(writer, `[{"number":1,"title":"[Topic: A-1]","base":{"repo_id":17}}]`)
				} else {
					writer.WriteHeader(scenario.status)
				}
			}))
			defer upstream.Close()
			client, err := NewGitea(upstream.URL, upstream.URL, "reader-token", 2)
			if err != nil {
				t.Fatal(err)
			}
			results, err := Lookup(context.Background(), client, []Repository{{ID: 17, Owner: "team", Name: "visible"}}, "A-1", 1)
			if (scenario.status == 401 && err != ErrUnauthorized) || (scenario.status != 401 && err != nil) || len(results) != scenario.resultCount {
				t.Fatalf("got %#v, %v", results, err)
			}
			if scenario.resultCount == 1 && results[0].Status != "failed" {
				t.Fatalf("got %#v", results)
			}
		})
	}
}
