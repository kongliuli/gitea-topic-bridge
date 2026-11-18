package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCaddyIngressProbe(t *testing.T) {
	caddy := os.Getenv("BRIDGE_TEST_CADDY_BINARY")
	if caddy == "" {
		t.Skip("set BRIDGE_TEST_CADDY_BINARY for the client ingress acceptance test")
	}
	if !filepath.IsAbs(caddy) {
		t.Fatal("Caddy path must be absolute")
	}
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("ingress acceptance test requires PowerShell 7.4 or later")
	}
	gitea := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Errorf("public ingress probe unexpectedly forwarded %s %s to Gitea", request.Method, request.URL.Path)
		writer.WriteHeader(500)
	}))
	defer gitea.Close()
	app := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/bridge/":
			writer.WriteHeader(200)
		case "/bridge/api/projects":
			writer.WriteHeader(401)
		default:
			writer.WriteHeader(404)
		}
	}))
	defer app.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gateway := listener.Addr().String()
	listener.Close()
	ca, _ := startLabCaddy(t, caddy, gateway, gateway, gitea.URL, app.Listener.Addr().String())
	_, backendPort, err := net.SplitHostPort(app.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		name, access, backend string
		extra                 []string
		failCheck             string
		wantChecks            int
	}{
		{"trusted_https", "Allowed", "127.0.0.2", []string{"-TrustedCAFile", ca}, "", 4},
		{"missing_trust", "Allowed", "127.0.0.2", nil, "bridge_page", 4},
		{"exposed_backend", "Allowed", "127.0.0.1", []string{"-TrustedCAFile", ca}, "backend", 4},
		{"closed_ingress", "Denied", "127.0.0.2", []string{"-IngressAddress", "127.0.0.2"}, "", 2},
		{"exposed_ingress", "Denied", "127.0.0.2", []string{"-IngressAddress", "127.0.0.1"}, "ingress", 2},
	} {
		t.Run(sample.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			args := []string{"-NoProfile", "-NonInteractive", "-File", "examples/Test-BridgeIngress.ps1",
				"-BridgeUrl", "https://" + gateway + "/bridge/", "-ExpectedAccess", sample.access, "-SourceLabel", sample.name,
				"-BackendAddress", sample.backend, "-BackendPorts", backendPort, "-TimeoutSeconds", "2"}
			output, err := exec.CommandContext(ctx, pwsh, append(args, sample.extra...)...).CombinedOutput()
			wantPass := sample.failCheck == ""
			if wantPass && err != nil {
				t.Fatalf("ingress probe unexpectedly failed: %v\n%s", err, output)
			}
			if !wantPass {
				var exited *exec.ExitError
				if !errors.As(err, &exited) || exited.ExitCode() != 1 {
					t.Fatalf("ingress probe returned the wrong failure status: %v\n%s", err, output)
				}
			}
			var report struct {
				Passed bool
				Checks []struct {
					Check, Result string
					Passed        bool
				}
			}
			if err := json.Unmarshal(output, &report); err != nil || report.Passed != wantPass || len(report.Checks) != sample.wantChecks {
				t.Fatalf("unexpected ingress evidence: %v\n%s", err, output)
			}
			for _, check := range report.Checks {
				if check.Check == sample.failCheck && !check.Passed && check.Result != "probe_error" {
					return
				}
			}
			if !wantPass {
				t.Fatalf("missing failed check %q:\n%s", sample.failCheck, output)
			}
		})
	}
}
