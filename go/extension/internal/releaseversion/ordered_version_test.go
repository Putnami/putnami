package releaseversion

import (
	"testing"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// TestOrderedPrereleaseIsValidAndSortsByCommitTime pins the version shape the
// CLI stamps (ADR 0018) against the Go module rules: it is a valid module
// version, a newer commit sorts after an older one whatever its SHA, and the
// prior <base>-<sha> shape stays valid.
func TestOrderedPrereleaseIsValidAndSortsByCommitTime(t *testing.T) {
	older := "v0.2.0-20260902173000-fd5edb751"
	newer := "v0.2.0-20260902180000-0abc1234"
	dirty := "v0.2.0-20260902180000-0abc1234-a1b2c3d"
	prior := "v0.2.0-8d5edb751"
	for _, version := range []string{older, newer, dirty, prior} {
		if !semver.IsValid(version) {
			t.Fatalf("%s is not a valid semver", version)
		}
		if err := module.Check("go.putnami.dev/app", version); err != nil {
			t.Fatalf("%s is not a valid module version: %v", version, err)
		}
	}
	if semver.Compare(older, newer) >= 0 {
		t.Fatalf("commit-time ordering broken: %s !< %s", older, newer)
	}
	if semver.Compare(newer, dirty) >= 0 {
		t.Fatalf("a dirty build of a commit must sort after the clean one: %s !< %s", newer, dirty)
	}
	if semver.Compare(prior, "v0.2.0-2") < 0 {
		t.Fatalf("prior shape %s unexpectedly sorts below the new range", prior)
	}
}
