package bridge

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func labGit(t *testing.T, dir, origin, user, password string, args ...string) (string, error) {
	t.Helper()
	endpoint, err := url.Parse(origin)
	if err != nil || endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" || endpoint.User != nil {
		t.Fatal("Git fixture must use an isolated loopback Gitea")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			command.Env = append(command.Env, entry)
		}
	}
	basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=http."+origin+"/.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+basic,
		"GIT_CONFIG_KEY_1=http.followRedirects", "GIT_CONFIG_VALUE_1=false", "GIT_CONFIG_KEY_2=http.proxy", "GIT_CONFIG_VALUE_2=")
	output, err := command.CombinedOutput()
	return strings.ReplaceAll(string(output), basic, "[redacted]"), err
}

func verifyProtectedGitPush(t *testing.T, origin, user, password string) {
	t.Helper()
	dir := t.TempDir()
	remote := origin + "/labowner/no-match.git"
	git := func(args ...string) string {
		t.Helper()
		result, err := labGit(t, dir, origin, user, password, args...)
		if err != nil {
			t.Fatalf("Git %s: %v\n%s", args[0], err, result)
		}
		return result
	}
	git("clone", "--branch", "main", remote, ".")
	before := strings.TrimSpace(git("ls-remote", "origin", "refs/heads/main"))
	if before == "" {
		t.Fatal("could not read protected main")
	}
	git("-c", "user.name=Lab Review", "-c", "user.email=lab@example.invalid", "commit", "--allow-empty", "-m", "Protected push fixture")
	git("push", "origin", "HEAD:refs/heads/lab-push-proof")
	head := strings.TrimSpace(git("rev-parse", "HEAD"))
	if got := strings.TrimSpace(git("ls-remote", "origin", "refs/heads/lab-push-proof")); got != fmt.Sprintf("%s\trefs/heads/lab-push-proof", head) {
		t.Fatal("positive control push did not reach the authorized branch")
	}
	result, err := labGit(t, dir, origin, user, password, "push", "origin", "HEAD:refs/heads/main")
	if err == nil || !strings.Contains(strings.ToLower(result), "protected branch") {
		t.Fatalf("Gitea did not reject the push for branch protection: %v\n%s", err, result)
	}
	if after := strings.TrimSpace(git("ls-remote", "origin", "refs/heads/main")); after != before {
		t.Fatal("Gitea changed the protected branch despite rejecting the push")
	}
}
