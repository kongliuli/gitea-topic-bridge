package bridge

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGiteaRunnerLab(t *testing.T) {
	gitea := os.Getenv("BRIDGE_TEST_GITEA_BINARY")
	runner := os.Getenv("BRIDGE_TEST_RUNNER_BINARY")
	if gitea == "" || runner == "" {
		t.Skip("set BRIDGE_TEST_GITEA_BINARY and BRIDGE_TEST_RUNNER_BINARY for the isolated Actions check")
	}
	if !filepath.IsAbs(gitea) || !filepath.IsAbs(runner) {
		t.Fatal("test binaries must use absolute paths")
	}
	base, command := startLocalGitea(t, gitea, "", "[actions]\nENABLED = true\n")
	admin, _ := createLabUser(t, command, "labowner", "all", "--admin")
	reader, _ := createLabUser(t, command, "labreader", "read:repository,read:user")
	const path = "/repos/labowner/ci-fixture"
	var repo struct{ ID int64 }
	labAPI(t, base, admin, "POST", "/user/repos", map[string]any{
		"name": "ci-fixture", "private": true, "auto_init": true, "default_branch": "main",
	}, &repo, 201)
	labAPI(t, base, admin, "PATCH", path, map[string]bool{"has_actions": true}, nil, 200)
	labAPI(t, base, admin, "PUT", path+"/collaborators/labreader", map[string]string{"permission": "read"}, nil, 204)
	var registration struct{ Token string }
	labAPI(t, base, admin, "POST", path+"/actions/runners/registration-token", nil, &registration, 200)
	log := startLabRunner(t, runner, base, registration.Token)

	const workflow = `name: Bridge CI
on:
  push:
    branches: [passing, failing]
jobs:
  check:
    runs-on: bridge-lab
    steps:
      - shell: pwsh
        run: |
          if ($env:GITHUB_REF_NAME -eq 'failing') { exit 1 }
          if ($env:GITHUB_REF_NAME -ne 'passing') { exit 2 }
          Write-Output 'Bridge CI fixture passed'
`
	addFile := func(file, branch, content string) {
		t.Helper()
		labAPI(t, base, admin, "POST", path+"/contents/"+file, map[string]string{
			"branch": branch, "message": "Bridge CI fixture", "content": base64.StdEncoding.EncodeToString([]byte(content)),
		}, nil, 201)
	}
	addFile(".gitea/workflows/check.yml", "main", workflow)
	expected := map[int64]string{}
	for _, sample := range []struct{ branch, state string }{{"passing", "success"}, {"failing", "failure"}} {
		labAPI(t, base, admin, "POST", path+"/branches", map[string]string{"old_branch_name": "main", "new_branch_name": sample.branch}, nil, 201)
		addFile(sample.branch+".txt", sample.branch, "synthetic change\n")
		var pull struct{ Number int64 }
		labAPI(t, base, admin, "POST", path+"/pulls", map[string]string{
			"title": "[Topic: CAD-CI] " + sample.branch, "base": "main", "head": sample.branch,
		}, &pull, 201)
		expected[pull.Number] = sample.state
	}
	client, err := NewGitea(base, base, reader, 10)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	repos := []Repository{{ID: repo.ID, Owner: "labowner", Name: "ci-fixture"}}
	var observed []RepositoryResult
	for ctx.Err() == nil {
		observed, err = Lookup(ctx, client, repos, "CAD-CI", 2)
		if err != nil {
			break
		}
		observed, err = client.inspectMatches(ctx, observed)
		if err != nil {
			break
		}
		if len(observed) == 1 && len(observed[0].PullRequests) == len(expected) {
			match := true
			for _, pull := range observed[0].PullRequests {
				match = match && commitSHA.MatchString(pull.HeadSHA) && pull.CI != "" && pull.CI == expected[pull.Number]
			}
			if match {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("Bridge did not observe the two actual runner results: %+v, %v. Runner log: %s", observed, err, log())
}

func startLabRunner(t *testing.T, binary, instance, token string) func() string {
	t.Helper()
	if token == "" || !strings.HasPrefix(instance, "http://127.0.0.1:") {
		t.Fatal("runner requires a registration token from the loopback lab")
	}
	dir := t.TempDir()
	workspace := filepath.Join(dir, "jobs")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config.yaml")
	settings := "runner:\n  file: .runner\n  capacity: 1\n  fetch_interval: 500ms\n" +
		"  fetch_interval_max: 2s\ncache:\n  enabled: false\nhost:\n  workdir_parent: '" + filepath.ToSlash(workspace) + "'\n"
	if err := os.WriteFile(config, []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	command := func(ctx context.Context, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binary, append(args, "--config", config)...)
		cmd.Dir = dir
		for _, variable := range os.Environ() {
			name, _, _ := strings.Cut(variable, "=")
			switch strings.ToUpper(name) {
			case "PATH", "PATHEXT", "COMSPEC", "SYSTEMROOT", "WINDIR", "SYSTEMDRIVE", "PROGRAMFILES",
				"PROGRAMFILES(X86)", "PROGRAMDATA", "OS", "PROCESSOR_ARCHITECTURE":
				cmd.Env = append(cmd.Env, variable)
			}
		}
		cmd.Env = append(cmd.Env, "TEMP="+dir, "TMP="+dir)
		return cmd
	}
	regCtx, cancelRegister := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelRegister()
	register := command(regCtx, "register", "--no-interactive", "--instance", instance, "--labels", "bridge-lab:host", "--name", "bridge-fixture")
	register.Env = append(register.Env, "GITEA_RUNNER_REGISTRATION_TOKEN="+token)
	if output, err := register.CombinedOutput(); err != nil {
		t.Fatalf("register loopback runner: %v\n%s", err, strings.ReplaceAll(string(output), token, "[redacted]"))
	}
	path := filepath.Join(dir, "runner.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	daemon := command(ctx, "daemon")
	daemon.Stdout, daemon.Stderr = file, file
	err = daemon.Start()
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); daemon.Wait() })
	return func() string {
		data, _ := os.ReadFile(path)
		return strings.ReplaceAll(string(data), token, "[redacted]")
	}
}
