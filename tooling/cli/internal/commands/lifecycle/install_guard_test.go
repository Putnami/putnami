package lifecycle

import (
	"strings"
	"testing"
)

// The rewrite is a warning naming every file, on every host: the hosted
// runner rewrites bun.lock around the install by design, so the CLI cannot
// fail on a rewrite it cannot attribute. A clean install says nothing.
func TestInstallRewriteGuard_WarnsAndNeverFails(t *testing.T) {
	rewritten := []string{"putnami.lock.json", "tooling/cli/go.sum"}

	var out strings.Builder
	installRewriteGuard(&out, rewritten)
	for _, want := range []string{"warning:", "2 committed file(s)", "putnami.lock.json, tooling/cli/go.sum", "--impacted"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("warning %q lacks %q", out.String(), want)
		}
	}

	var clean strings.Builder
	installRewriteGuard(&clean, nil)
	if clean.Len() != 0 {
		t.Errorf("a clean install reported %q", clean.String())
	}
}
