package stacks

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/jsonl"
)

func TestRunForegroundAnswersTheExitCode(t *testing.T) {
	dir := t.TempDir()
	root := RootStep{Project: dir}
	if code, stopped, err := RunForeground(root, "sh", os.Environ(), nil, "-c", "exit 3"); code != 3 || stopped || err != nil {
		t.Errorf("exit 3: %d %v %v", code, stopped, err)
	}
	out := captureStdout(t, func() {
		if code, _, err := RunForeground(root, "sh", os.Environ(), jsonl.New(), "-c", "echo hello; echo oops >&2"); code != 0 || err != nil {
			t.Errorf("echo: %d %v", code, err)
		}
	})
	if !strings.Contains(out, "hello") || !strings.Contains(out, "oops") {
		t.Errorf("the output becomes log events: %s", out)
	}
	if _, _, err := RunForeground(root, filepath.Join(dir, "missing"), os.Environ(), nil); err == nil {
		t.Error("a program that does not start is an error")
	}
}

func TestAnnounceReadyWaitsForTheServer(t *testing.T) {
	port, err := FreePort()
	if err != nil || port == 0 {
		t.Fatalf("FreePort: %d %v", port, err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	out := captureStdout(t, func() { AnnounceReady(context.Background(), jsonl.New(), port) })
	if strings.Count(out, `"ready"`) != 2 {
		t.Errorf("a server and a workload ready event: %s", out)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	closed, _ := FreePort()
	if out := captureStdout(t, func() { AnnounceReady(ended, jsonl.New(), closed) }); out != "" {
		t.Errorf("no event once serving ends: %s", out)
	}
}

// captureStdout answers what fn writes to the standard output, where a job's
// emitter writes its events.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = file
	fn()
	os.Stdout = saved
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestProgramInFindsAToolOnTheTaskPath(t *testing.T) {
	tools := t.TempDir()
	writeFile(t, tools, "tojunit", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(tools, "tojunit"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, tools, "readme", "not a program")
	tc := mustProfile(t).Toolchain
	env := []string{"PATH=" + tools}
	if got := (Selection{}).ProgramIn(tc, "tojunit", env); got != filepath.Join(tools, "tojunit") {
		t.Errorf("a tool on the task's PATH = %s", got)
	}
	for _, name := range []string{"readme", "absent", "./tojunit"} {
		if got := (Selection{}).ProgramIn(tc, name, env); got != name {
			t.Errorf("%s = %s, want the name itself", name, got)
		}
	}
}

func TestProgramInPrefersAProgramBesideTheToolchain(t *testing.T) {
	sdk, other := t.TempDir(), t.TempDir()
	for _, f := range []string{filepath.Join(sdk, "flutter"), filepath.Join(sdk, "dart"), filepath.Join(other, "dart")} {
		writeFile(t, filepath.Dir(f), filepath.Base(f), "#!/bin/sh\n")
		if err := os.Chmod(f, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tc := Toolchain{Command: []string{"flutter", "--version"}}
	env := []string{"PATH=" + other + string(os.PathListSeparator) + sdk}
	if got := (Selection{}).ProgramIn(tc, "dart", env); got != filepath.Join(sdk, "dart") {
		t.Errorf("the dart beside flutter wins over the first on PATH: %s", got)
	}
	if got := (Selection{}).Program(tc, "flutter"); got != "flutter" {
		t.Errorf("Program keeps the bare name of a toolchain on PATH: %s", got)
	}
	if _, ok := LookPathIn([]string{"PATH=relative" + string(os.PathListSeparator) + other}, "dart"); !ok {
		t.Error("an absolute entry is searched")
	}
	if _, ok := LookPathIn([]string{"PATH=."}, "dart"); ok {
		t.Error("a relative entry is not searched")
	}
}
