package imageload

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/sdk/extension/jsonl"
)

func TestLoadNeedsATagAndALayout(t *testing.T) {
	if err := Load(t.TempDir(), nil, nil); err == nil {
		t.Error("no tag is an error")
	}
	if err := Load(filepath.Join(t.TempDir(), "missing"), []string{"demo:c-1"}, nil); err == nil || !strings.Contains(err.Error(), "docker tarball") {
		t.Errorf("a missing layout fails before docker runs: %v", err)
	}
}

func TestDockerRunsDockerLogsItsOutputAndReportsItsFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub docker is a shell script")
	}
	stubs := t.TempDir()
	log := filepath.Join(stubs, "docker.log")
	script := "#!/bin/sh\necho \"$*\" >> \"" + log + "\"\necho loaded\nprintf 'tag refused' >&2\n[ \"$1\" = tag ] && exit 3\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubs, "docker"), []byte(script), 0o755); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	var (
		mu    sync.Mutex
		lines []string
	)
	emit := jsonl.New()
	emit.Observe(func(message string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, message)
	})
	if err := docker(emit, "load", "-i", "image.tar"); err != nil {
		t.Errorf("load: %v", err)
	}
	if err := docker(nil, "tag", "demo:c-1", "demo:edge"); err == nil {
		t.Error("a failing docker tag is an error")
	}
	got, err := os.ReadFile(log) //nolint:gosec // the test's own file
	if err != nil || string(got) != "load -i image.tar\ntag demo:c-1 demo:edge\n" {
		t.Errorf("docker ran %q: %v", got, err)
	}
	if strings.Join(lines, "|") != "loaded|tag refused" && strings.Join(lines, "|") != "tag refused|loaded" {
		t.Errorf("logged %q", lines)
	}
}
