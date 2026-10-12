package proposals

import (
	"strings"
	"testing"
)

func TestNativeGitReadBoundaryRejectsWritingVerbs(t *testing.T) {
	t.Parallel()
	g := NativeGit{Root: t.TempDir()}
	for _, verb := range []string{"commit", "push", "config"} {
		if _, err := g.run(verb); err == nil || !strings.Contains(err.Error(), "unsupported read-only git operation") {
			t.Fatalf("read boundary accepted %q: %v", verb, err)
		}
	}
	if _, err := g.runWrite("status"); err == nil || !strings.Contains(err.Error(), "unsupported repository-writing git operation") {
		t.Fatalf("write boundary accepted status: %v", err)
	}
}
