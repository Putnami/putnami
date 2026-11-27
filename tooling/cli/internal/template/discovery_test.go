package template

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A lock that cannot be read is noted once per workspace and reason in a
// process, and once per run in a machine worker, which forgets the notices it
// printed before each run (ForgetProcessMemos, CLI ADR 0116 §4).
func TestAnUnreadableLockIsNotedOncePerRun(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "putnami.lock.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	for run := 1; run <= 2; run++ {
		ForgetProcessMemos()
		for range 2 {
			if _, err := DiscoverTemplates(root, nil); err != nil {
				t.Fatal(err)
			}
		}
		if got := strings.Count(buf.String(), "lock file could not be read"); got != run {
			t.Fatalf("after run %d the notice was printed %d times, want once per run:\n%s", run, got, buf.String())
		}
	}
}
