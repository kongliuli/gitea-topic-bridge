package bridge

import (
	"context"
	"encoding/json"
	"errors"
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
	mux := http.NewServeMux()
	serveBrowserTestPage(t, mux, "/bridge/{$}", "index.html", `
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
`)
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

func TestPartialMergeEdge(t *testing.T) {
	edge := edgeForTest(t)
	id := strings.Repeat("A", 43)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bridge/{$}", serveAsset("index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /bridge/app.js", serveAsset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /bridge/style.css", serveAsset("style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /bridge/api/projects", func(writer http.ResponseWriter, _ *http.Request) {
		responseJSON(writer, 200, json.RawMessage(`{"user":"fixture","projects":[{"id":"lab","name":"Lab"}],"saved_topics":true,"merge_enabled":true,"revert_enabled":true}`))
	})
	mux.HandleFunc("GET /bridge/api/topics/"+id, func(writer http.ResponseWriter, _ *http.Request) {
		at, sha := time.Now(), strings.Repeat("a", 40)
		members := make([]TopicMember, 0, 3)
		repositories := make([]RepositoryResult, 0, 3)
		for index, name := range []string{"first", "second", "conflict"} {
			repo := Repository{ID: int64(index + 1), Owner: "lab", Name: name}
			pull := PullRequest{Number: 1, Title: "Fixture", Lifecycle: "merged", HeadSHA: sha, URL: "https://git.example/lab/" + name + "/pulls/1", ReadAt: at}
			member := TopicMember{ID: repo.ID, RepositoryID: repo.ID, SourceBranch: "feature", TargetBranch: "main", PullNumber: 1,
				Merge: &MergeState{State: "merged", MergeSHA: sha}, State: "linked"}
			switch name {
			case "first":
				member.Revert = &RevertState{State: "opened", PullNumber: 2}
			case "conflict":
				pull.Lifecycle, pull.Conflict = "open", "not_mergeable_reason_unknown"
				member.Merge = &MergeState{State: "blocked", ResultCode: "pull_not_ready"}
			}
			pulls := []PullRequest{pull}
			if member.Revert != nil {
				pulls = append(pulls, PullRequest{Number: 2, Title: "Reverse fixture", Lifecycle: "open", ReadAt: at, URL: "https://git.example/lab/first/pulls/2"})
			}
			members = append(members, member)
			repositories = append(repositories, RepositoryResult{Repository: repo, Status: "complete", PullRequests: pulls})
		}
		responseJSON(writer, 200, struct {
			TopicChangeSet
			ReadAt       time.Time          `json:"read_at"`
			Repositories []RepositoryResult `json:"repositories"`
		}{TopicChangeSet{ID: id, ProjectID: "lab", Topic: "CAD-PARTIAL", Members: members}, at, repositories})
	})
	rendered := dumpBrowserPage(t, edge, mux, "/bridge/?topic_id="+id)
	for _, value := range []string{"已核验合并 2/3 仓，受阻 1 仓", `data-merge="blocked"`, "合并受阻",
		`href="https://git.example/lab/first/pulls/2">反向 PR #2（到 Gitea 审批）`} {
		if !strings.Contains(rendered, value) {
			t.Errorf("batch page did not render %q", value)
		}
	}
}

func TestTopicSelectionInvalidatesOldResultsEdge(t *testing.T) {
	edge := edgeForTest(t)
	mux := http.NewServeMux()
	serveBrowserTestPage(t, mux, "/bridge/{$}", "index.html", `
const form = document.querySelector('#lookup');
const project = form.elements.namedItem('project_id');
const topic = form.elements.namedItem('topic');
const results = document.querySelector('#results');
const save = document.querySelector('#save-topic');
let stage = 0;
const observer = new MutationObserver(step);
observer.observe(results, { childList: true });
step();
function step() {
  if (stage === 0 && results.textContent.includes('lab/first') && !save.hidden) {
    stage++;
    project.value = 'second';
    project.dispatchEvent(new Event('change'));
    if (results.children.length || !save.hidden) {
      document.body.dataset.result = 'stale-project';
      observer.disconnect();
      return;
    }
    form.requestSubmit();
  } else if (stage === 1 && results.textContent.includes('lab/second') && !save.hidden) {
    observer.disconnect();
    topic.value = 'CAD-NEW';
    topic.dispatchEvent(new Event('input'));
    const oldQuery = new URLSearchParams(location.search).has('topic');
    document.body.dataset.result = results.children.length || !save.hidden || oldQuery ? 'stale-topic' : 'fresh-selection';
  }
}
`)
	mux.HandleFunc("GET /bridge/app.js", serveAsset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /bridge/style.css", serveAsset("style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /bridge/api/projects", func(writer http.ResponseWriter, _ *http.Request) {
		responseJSON(writer, 200, json.RawMessage(`{"user":"fixture","projects":[{"id":"first","name":"First"},{"id":"second","name":"Second"}],"saved_topics":true}`))
	})
	mux.HandleFunc("GET /bridge/api/topics/lookup", func(writer http.ResponseWriter, request *http.Request) {
		project := request.URL.Query().Get("project_id")
		if (project != "first" && project != "second") || request.URL.Query().Get("topic") != "CAD-01" {
			t.Error("unexpected query from the project selector")
			writer.WriteHeader(400)
			return
		}
		responseJSON(writer, 200, map[string]any{
			"repositories": []RepositoryResult{{Repository: Repository{ID: 1, Owner: "lab", Name: project},
				Status: "complete", PullRequests: []PullRequest{}}},
			"complete": true, "found": false,
		})
	})
	if rendered := dumpBrowserPage(t, edge, mux, "/bridge/?project_id=first&topic=CAD-01"); !strings.Contains(rendered, `data-result="fresh-selection"`) {
		t.Fatal("changing a project or topic left a previous result or action in the page")
	}
}

func TestReceiptIssueEdge(t *testing.T) {
	edge := edgeForTest(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("the receipt navigation test requires Node.js with built-in WebSocket")
	}
	id, topicID := strings.Repeat("A", 43), strings.Repeat("B", 43)
	const sha = "ab1234567890cdefab1234567890cdefab123456"
	mux := http.NewServeMux()
	serveBrowserTestPage(t, mux, "/bridge/receipts/{$}", "receipts.html", `
const issue = document.querySelector('#issue');
const observer = new MutationObserver(submitReceipt);
observer.observe(issue, { attributes: true });
submitReceipt();
function submitReceipt() {
  if (issue.hidden || new URLSearchParams(location.search).has('id')) return;
  observer.disconnect();
  window.confirm = () => true;
  const [artifact, release, commit] = document.querySelectorAll('#members input');
  artifact.value = 'dist/bundle.zip';
  release.value = '7';
  commit.value = '`+sha+`';
  issue.querySelector('button').click();
}
`)
	mux.HandleFunc("GET /bridge/receipts.js", serveAsset("receipts.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /bridge/style.css", serveAsset("style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /bridge/api/receipts", func(writer http.ResponseWriter, _ *http.Request) {
		responseJSON(writer, 200, json.RawMessage(`{"topic":"CAD-01","can_issue":true,"repositories":[{"id":3,"owner":"lab","name":"fixture"}],"versions":[]}`))
	})
	mux.HandleFunc("POST /bridge/api/receipts", func(writer http.ResponseWriter, request *http.Request) {
		var input receiptRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.TopicID != topicID || input.PreviousID != "" ||
			request.Header.Get("Idempotency-Key") == "" || len(input.Artifacts) != 1 || len(input.Sources) != 1 {
			t.Error("unexpected browser receipt issue request")
			writer.WriteHeader(400)
			return
		}
		if a, s := input.Artifacts[0], input.Sources[0]; a.RepositoryID != 3 || a.Path != "dist/bundle.zip" ||
			s.RepositoryID != 3 || s.ReleaseID != 7 || s.ExpectedSHA != sha {
			t.Error("browser did not send the expected artifact and release evidence")
			writer.WriteHeader(400)
			return
		}
		responseJSON(writer, 201, json.RawMessage(`{"snapshot":{"id":"`+id+`"}}`))
	})
	mux.HandleFunc("GET /bridge/api/receipts/"+id, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Bridge-Anchor-Status", "pending")
		responseJSON(writer, 200, json.RawMessage(`{"snapshot":{"id":"`+id+`","topic_id":"`+topicID+`","topic":"CAD-01","version":1,"issued_at":"2026-11-19T00:00:00Z","issuer_id":2,"members":[],"artifacts":[]}}`))
	})
	browser := browserFixtureServer(t, mux)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "--input-type=module", "-e", receiptBrowserNavigation,
		edge, t.TempDir(), browser.URL+"/bridge/receipts/?topic_id="+topicID, id)
	command.WaitDelay = 2 * time.Second
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("browser did not navigate to the signed receipt and show its anchor status: %v\n%s", err, output)
	}
}

// Browser CLI DOM snapshots stop at the first document. DevTools lets this
// check wait for the new document after submitting the form.
const edgeDevTools = `
import { spawn } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

async function withEdge(edge, profile, url, flags, check) {
  const browser = spawn(edge, ['--headless=new', '--disable-gpu', '--no-first-run',
    '--disable-background-networking', '--remote-debugging-port=0', '--no-proxy-server',
    '--user-data-dir=' + profile, ...flags, url], { windowsHide: true, stdio: 'ignore' });
  browser.on('error', error => console.error(error.message));
  let socket;
  try {
    let target;
    for (let i = 0; i < 120 && !target; i++) {
      const portFile = await readFile(join(profile, 'DevToolsActivePort'), 'utf8').catch(() => '');
      const port = portFile.split('\n')[0].trim();
      if (/^[0-9]+$/.test(port)) {
        const pages = await fetch('http://127.0.0.1:' + port + '/json/list', {
          signal: AbortSignal.timeout(1000)
        }).then(res => res.json()).catch(() => []);
        target = pages.find(page => page.type === 'page');
      }
      if (!target) await delay(100);
    }
    if (!target) throw new Error('no headless Edge page');
    socket = new WebSocket(target.webSocketDebuggerUrl);
    await new Promise((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error('DevTools connection timed out')), 5000);
      socket.addEventListener('open', () => { clearTimeout(timeout); resolve(); }, { once: true });
      socket.addEventListener('error', () => { clearTimeout(timeout); reject(new Error('DevTools connection failed')); }, { once: true });
    });

    let nextID = 0;
    function send(method, params = {}) {
      const id = ++nextID;
      return new Promise((resolve, reject) => {
        const timeout = setTimeout(() => {
          socket.removeEventListener('message', receive);
          reject(new Error('DevTools did not answer ' + method));
        }, 5000);
        function receive(event) {
          const reply = JSON.parse(event.data);
          if (reply.id !== id) return;
          clearTimeout(timeout);
          socket.removeEventListener('message', receive);
          resolve(reply);
        }
        socket.addEventListener('message', receive);
        socket.send(JSON.stringify({ id, method, params }));
      });
    }
    async function evaluate(fn, ...args) {
      const expression = '(' + fn.toString() + ')(...' + JSON.stringify(args) + ')';
      const reply = await send('Runtime.evaluate', { expression, returnByValue: true });
      if (reply.result?.exceptionDetails) throw new Error('page script failed');
      // Runtime.evaluate can briefly lose its execution context during a page transition.
      return reply.result?.result?.value;
    }
    await check(evaluate, send);
  } finally {
    if (socket?.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify({ id: 99999, method: 'Browser.close' }));
      socket.close();
      await delay(200);
    }
    if (browser.exitCode === null) browser.kill();
  }
}

async function waitForPage(label, check) {
  for (let i = 0; i < 100; i++) {
    const match = await check();
    if (match) return match;
    await delay(100);
  }
  throw new Error(label + ' was not observed in Edge');
}
`

const receiptBrowserNavigation = edgeDevTools + `
const [edge, profile, url, receiptID] = process.argv.slice(1);
await withEdge(edge, profile, url, [], async (evaluate) => {
  await waitForPage('receipt detail after submission', () => evaluate(id =>
    new URLSearchParams(location.search).get('id') === id &&
      document.querySelector('#receipt')?.innerText.includes('独立存证待确认'), receiptID));
});
`

func serveBrowserTestPage(t *testing.T, mux *http.ServeMux, route, name, script string) {
	t.Helper()
	page, err := fs.ReadFile(site, "web/"+name)
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("GET "+route, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Write([]byte(strings.Replace(string(page), "</head>", `<script type="module" src="/bridge/test-actions.js"></script></head>`, 1)))
	})
	mux.HandleFunc("GET /bridge/test-actions.js", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/javascript")
		writer.Write([]byte(script))
	})
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
	lab := browserFixtureServer(t, handler)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, edge, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-background-networking", "--user-data-dir="+t.TempDir(), "--virtual-time-budget=5000", "--dump-dom", lab.URL+path)
	output, err := os.CreateTemp("", "bridge-edge-page-*.html")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// A Windows Edge child can release the inherited handle shortly after exit.
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			err := os.Remove(output.Name())
			if err == nil || errors.Is(err, os.ErrNotExist) {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("remove Edge output: %v", err)
				return
			}
		}
	})
	// Edge child processes can inherit an output pipe and keep it open after the main process exits.
	command.Stdout = output
	err = command.Run()
	output.Close()
	if err != nil {
		t.Fatalf("headless Edge could not render %s: %v", path, err)
	}
	rendered, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(rendered)
}

func browserFixtureServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	lab := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(lab.Close)
	return lab
}
