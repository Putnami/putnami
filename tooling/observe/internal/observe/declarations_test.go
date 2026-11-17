package observe

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/gitbase"
)

// observeDeclaringChange observes a repository that holds
// putnami.workspace.json and whose change edits a README only, against base.
func observeDeclaringChange(t *testing.T, base func(first string) string) Observation {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	writeDemoFile(t, root, "putnami.workspace.json", "{}\n")
	writeDemoFile(t, root, "README.md", "one\n")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "base")
	first := git(t, root, "rev-parse", "HEAD")
	writeDemoFile(t, root, "README.md", "two\n")
	git(t, root, "commit", "-q", "-am", "change")
	env := demos["go"].env
	opts := Options{
		Dir:      root,
		Env:      func(key string) string { return env[key] },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:  func(p string) string { return filepath.Join(root, p) },
		Producer: Producer{Name: "@putnami/observe", Version: "1.0.0"},
	}
	opts.Overrides.Base = base(first)
	return Run(context.Background(), opts)
}

func TestADeclaringChangeThatTouchesNoDeclarationCarriesAnEmptyList(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "declared-repository-evidence", "a-workspace-change-that-touches-no-declaration-carries-an-empty-list")
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

	read := Evidence(observeDeclaringChange(t, func(first string) string { return first }), now)
	if read.Declarations == nil || len(read.Declarations) != 0 {
		t.Errorf("a change read whole that touches no declaration: declarations = %#v, want an empty list", read.Declarations)
	}

	unread := Evidence(observeDeclaringChange(t, func(string) string { return "0000000000000000000000000000000000000000" }), now)
	if unread.Declarations != nil {
		t.Errorf("a change whose base cannot be resolved: declarations = %#v, want none", unread.Declarations)
	}
}

func TestOnlyAWholeDiffCountsAsRead(t *testing.T) {
	cases := map[string]struct {
		resolved gitbase.Result
		want     bool
	}{
		"not read":  {gitbase.Result{}, false},
		"empty":     {gitbase.Result{ChangedFiles: []gitbase.ChangedFile{}}, true},
		"one file":  {gitbase.Result{ChangedFiles: make([]gitbase.ChangedFile, 1)}, true},
		"at bound":  {gitbase.Result{ChangedFiles: make([]gitbase.ChangedFile, gitbase.MaxChangedFiles)}, true},
		"truncated": {gitbase.Result{ChangedFiles: make([]gitbase.ChangedFile, gitbase.MaxChangedFiles), Truncated: true}, false},
	}
	for name, tc := range cases {
		if got := diffRead(tc.resolved); got != tc.want {
			t.Errorf("%s: diffRead = %v, want %v", name, got, tc.want)
		}
	}
}
