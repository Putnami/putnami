//go:build unix

package workspacejob_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/protocol/features/spectest"
)

// writeProgram writes an executable go at dir that records it ran in marker.
func writeProgram(t *testing.T, dir, marker string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "go")
	script := "#!/bin/sh\necho ran >> '" + marker + "'\necho 'go version go1.99.0 linux/amd64'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // G306: a test program must be executable
		t.Fatal(err)
	}
	return path
}

// TestOnlyProgramsOutsideTheWorkspace proves workspace-fetch neither finds
// nor runs a program the repository commits into its own tree: not on PATH,
// not through a PATH entry that links into the tree, not as a managed install,
// and not when another lookup hands the job its path. A go outside the tree is
// still found.
func TestOnlyProgramsOutsideTheWorkspace(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"workspace-fetch-runs-no-program-from-the-workspace")
	t.Parallel()
	workspace := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	committed := writeProgram(t, filepath.Join(workspace, "bin"), marker)
	writeProgram(t, filepath.Join(workspace, ".putnami", "extensions", "@putnami-go", "bin"), marker)
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(filepath.Join(workspace, "bin"), linked); err != nil {
		t.Fatal(err)
	}

	newJob := func(path string) *workspacejob.Job {
		return workspacejob.New(context.Background(), &jobtest.Recorder{}, []string{"PATH=" + path}, workspace, "")
	}
	inside := strings.Join([]string{filepath.Join(workspace, "bin"), linked}, string(filepath.ListSeparator))

	if got := newJob(inside).LookPath("go"); got != committed {
		t.Fatalf("without the restriction LookPath(go) = %q, want %q", got, committed)
	}
	j := newJob(linked + string(filepath.ListSeparator) + inside)
	j.OnlyProgramsOutsideTheWorkspace()
	if got := j.LookPath("go"); got != "" {
		t.Errorf("LookPath(go) = %q, want nothing inside the workspace", got)
	}
	if j.FindGoBinary() {
		t.Errorf("FindGoBinary selected %q inside the workspace", j.GoBinary)
	}
	// A program found another way, such as a managed tool candidate, is refused
	// when the job starts it.
	for _, program := range []string{committed, linked + "/go"} {
		if out, err := j.Combined(nil, program, "version"); err == nil || !strings.Contains(err.Error(), "runs no program inside the workspace") {
			t.Errorf("Combined(%s) = %q, %v; want the refusal", program, out, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("a program inside the workspace ran: %v", err)
	}

	outside := writeProgram(t, t.TempDir(), marker)
	j = newJob(inside + string(filepath.ListSeparator) + filepath.Dir(outside))
	j.OnlyProgramsOutsideTheWorkspace()
	if got := j.LookPath("go"); got != outside {
		t.Errorf("LookPath(go) = %q, want the go outside the workspace %q", got, outside)
	}
}
