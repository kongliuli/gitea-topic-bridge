package bridge

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReceiptCenterEdge(t *testing.T) {
	edge := edgeForTest(t)
	id, topicID := strings.Repeat("A", 43), strings.Repeat("B", 43)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bridge/receipts/", serveAsset("receipts.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /bridge/receipts.js", serveAsset("receipts.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /bridge/style.css", serveAsset("style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /bridge/api/receipts/"+id, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Bridge-Anchor-Status", "verified")
		responseJSON(writer, 200, receiptEnvelope{Digest: strings.Repeat("f", 64), Snapshot: json.RawMessage(`{"id":"` + id + `","topic_id":"` + topicID + `","topic":"CAD-01","version":2,"issued_at":"2026-11-19T00:00:00Z","issuer_id":3,"members":[{"repository":{"owner":"lab","name":"fixture"},"status":"unverified"}],"artifacts":[]}`)})
	})
	mux.HandleFunc("GET /bridge/api/receipts", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("topic_id") != topicID {
			t.Error("wrong topic loaded")
		}
		responseJSON(writer, 200, map[string]any{"topic": "CAD-01", "can_issue": true, "repositories": []Repository{{ID: 3, Owner: "lab", Name: "fixture"}},
			"versions": []receiptVersion{{ID: id, Version: 2, IssuedAt: time.Now()}}})
	})
	rendered := dumpBrowserPage(t, edge, mux, "/bridge/receipts/?id="+id)
	for _, expected := range []string{"CAD-01 · v2", "独立存证已核验", "未核验源提交", "CAD-01 · 版本历史", "更正 v2（旧版保留）", "?format=proof", "?format=markdown"} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("receipt center did not render %q", expected)
		}
	}
}

func TestProjectOverviewEdge(t *testing.T) {
	edge := edgeForTest(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bridge/{$}", serveAsset("index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /bridge/app.js", serveAsset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /bridge/style.css", serveAsset("style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /bridge/api/projects", func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Query().Get("overview") {
		case "":
			responseJSON(writer, 200, json.RawMessage(`{"user":"ordinary","projects":[{"id":"lab","name":"联调组"}]}`))
		case "lab":
			responseJSON(writer, 200, json.RawMessage(`{"name":"联调组","read_at":"2026-11-19T00:00:00Z","repositories":[
				{"repository":{"owner":"lab","name":"fixture"},"url":"https://git.example/lab/fixture","default_branch":"main",
				"latest_commit":{"id":"0123456789abcdef","url":"https://git.example/lab/fixture/commit/0123456789abcdef"},"protection":{"status":"partial","required_approvals":1,"user_can_push":false}},
				{"repository":{"owner":"lab","name":"empty"},"url":"https://git.example/lab/empty","protection":{"status":"unknown"}}]}`))
		default:
			writer.WriteHeader(404)
		}
	})
	rendered := dumpBrowserPage(t, edge, mux, "/bridge/?overview=lab")
	for _, expected := range []string{"lab/fixture", "规则仅部分可见", "需要 1 人批准",
		"href=\"https://git.example/lab/fixture/commit/0123456789abcdef\">0123456789ab", "分支保护：未知"} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("project overview did not render %q", expected)
		}
	}
}

func TestForkSelectionEdge(t *testing.T) {
	edge := edgeForTest(t)
	page, err := fs.ReadFile(site, "web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bridge/{$}", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Write([]byte(strings.Replace(string(page), "</head>", `<script type="module" src="/bridge/fork-test.js"></script></head>`, 1)))
	})
	mux.HandleFunc("GET /bridge/fork-test.js", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/javascript")
		writer.Write([]byte(`
const results = document.querySelector('#results');
const observer = new MutationObserver(selectFork);
observer.observe(results, { childList: true });
selectFork();
function selectFork() {
  const source = results.querySelector('fieldset select');
  if (!source) return;
  observer.disconnect();
  source.value = '5';
  for (const branch of results.querySelectorAll('fieldset input[type=text]')) branch.value = 'main';
  document.querySelector('#save-topic').click();
}
`))
	})
	mux.HandleFunc("GET /bridge/app.js", serveAsset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /bridge/style.css", serveAsset("style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /bridge/api/projects", func(writer http.ResponseWriter, _ *http.Request) {
		responseJSON(writer, 200, json.RawMessage(`{"user":"fixture","projects":[{"id":"lab","name":"Lab"}],"pull_creation":true,"saved_topics":true}`))
	})
	mux.HandleFunc("GET /bridge/api/topics/lookup", func(writer http.ResponseWriter, _ *http.Request) {
		responseJSON(writer, 200, json.RawMessage(`{"repositories":[{"repository":{"id":3,"owner":"base","name":"target"},"status":"complete","pull_requests":[]}],"complete":true,"found":false}`))
	})
	mux.HandleFunc("GET /bridge/api/projects/lab/forks", func(writer http.ResponseWriter, _ *http.Request) {
		responseJSON(writer, 200, []ProjectFork{{TargetID: 3, Source: Repository{ID: 5, Owner: "dev", Name: "fork"}}})
	})
	mux.HandleFunc("POST /bridge/api/topics", func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			Members []TopicMember
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil || len(input.Members) != 1 {
			t.Errorf("unexpected topic request: %v", err)
			writer.WriteHeader(400)
			return
		}
		member := input.Members[0]
		if member.RepositoryID != 3 || member.SourceRepositoryID != 5 || member.SourceBranch != "main" || member.TargetBranch != "main" ||
			request.Header.Get("Idempotency-Key") == "" {
			t.Errorf("fork selection did not submit the selected source and both branch names: %+v", member)
			writer.WriteHeader(400)
			return
		}
		responseJSON(writer, 201, TopicChangeSet{ID: strings.Repeat("A", 43), Topic: "CAD-FORK", Version: 1})
	})
	if rendered := dumpBrowserPage(t, edge, mux, "/bridge/?project_id=lab&topic=CAD-FORK"); !strings.Contains(rendered, "打开联调单 CAD-FORK · v1") {
		t.Fatal("selecting a fork and saving did not render a link to the new batch")
	}
}

func edgeForTest(t *testing.T) string {
	t.Helper()
	edge := os.Getenv("BRIDGE_TEST_EDGE_BINARY")
	if edge == "" {
		t.Skip("set BRIDGE_TEST_EDGE_BINARY for the isolated headless Edge check")
	}
	if !filepath.IsAbs(edge) {
		t.Fatal("browser path must be absolute")
	}
	return edge
}

func dumpBrowserPage(t *testing.T, edge string, handler http.Handler, path string) string {
	t.Helper()
	lab := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		handler.ServeHTTP(writer, request)
	}))
	defer lab.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, edge, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-background-networking", "--user-data-dir="+t.TempDir(), "--virtual-time-budget=5000", "--dump-dom", lab.URL+path)
	rendered, err := command.Output()
	if err != nil {
		t.Fatalf("headless Edge could not render %s: %v", path, err)
	}
	return string(rendered)
}
