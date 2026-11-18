package bridge

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestArtifactRejectsWindowsDirectoryJunction(t *testing.T) {
	root, external := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "fixture.txt"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(root, "18")
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"New-Item -ItemType Junction -Path $env:BRIDGE_TEST_LINK -Target $env:BRIDGE_TEST_LINK_TARGET -ErrorAction Stop | Out-Null")
	command.Env = append(os.Environ(), "BRIDGE_TEST_LINK="+junction, "BRIDGE_TEST_LINK_TARGET="+external)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("could not create temporary junction: %v %s", err, output)
	}
	if _, err := openArtifact(root, []string{"18", "fixture.txt"}); !errors.Is(err, errUnsafeArtifact) {
		t.Fatalf("junction was not rejected: %v", err)
	}
	if _, err := openArtifact(junction, []string{"fixture.txt"}); !errors.Is(err, errUnsafeArtifact) {
		t.Fatalf("junction as root was not rejected: %v", err)
	}
}
