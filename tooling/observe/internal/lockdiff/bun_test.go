package lockdiff

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

// The bun.lock fixtures are this repository's own, before and after the
// commit that moved @biomejs/biome from 2.5.3 to 2.5.15.
func readBunFixture(t *testing.T, side string) Packages {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "bun", side, BunLock))
	if err != nil {
		t.Fatal(err)
	}
	return read(t, BunLock, string(data))
}

func TestABunLockListsItsPackages(t *testing.T) {
	spectest.Proves(t, feature, "bun-lockfiles", "a-bun-lock-lists-its-packages")
	kind, ok := KindOf("apps/web/bun.lock")
	if !ok || kind.Ecosystem != EcosystemNPM {
		t.Fatalf("bun.lock: kind %+v %v", kind, ok)
	}
	if _, ok := KindOf("bun.lockb"); ok {
		t.Error("bun.lockb has a reader")
	}
	base, head := readBunFixture(t, "base"), readBunFixture(t, "head")
	if got := head["@biomejs/biome"]; len(got) != 1 || got[0] != "2.5.15" {
		t.Errorf("@biomejs/biome = %v", got)
	}
	// A key that is an install path names the package its first member names.
	if got := head["@putnami/cloud"]; len(got) != 1 || got[0] != "0.0.0-20260929210146-c834161ac" {
		t.Errorf("@putnami/cloud = %v", got)
	}
	// The workspace's own packages are left out.
	if got, ok := head["@putnami/runtime"]; ok {
		t.Errorf("the workspace package @putnami/runtime reads as %v", got)
	}
	changes := Diff(base, head)
	upgraded := map[string]Change{}
	for _, c := range changes {
		if c.Change != evidence.DependencyUpgraded {
			t.Errorf("change %+v, want only upgrades", c)
		}
		upgraded[c.Name] = c
	}
	if c := upgraded["@biomejs/biome"]; c.From != "2.5.3" || c.To != "2.5.15" {
		t.Errorf("@biomejs/biome change %+v in %+v", c, changes)
	}
	if c := upgraded["@biomejs/cli-linux-x64"]; c.From != "2.5.3" || c.To != "2.5.15" {
		t.Errorf("@biomejs/cli-linux-x64 change %+v", c)
	}

	expect(t, "bun.lock with comments", read(t, BunLock, `{
  // Bun writes no comment, a hand edit may.
  "lockfileVersion": 1,
  "workspaces": { "": { "name": "app", "dependencies": { "zod": "^3", "lib": "workspace:*", }, }, },
  /* local packages */
  "packages": {
    "lib": ["lib@workspace:packages/lib"],
    "linked": ["linked@link:../linked", {}],
    "local": ["local@file:../local", {}],
    "app": ["app@root:", {}],
    "zod": ["zod@3.24.1", "", {}, "sha512-x//y"],
    "alias": ["real-name@1.0.0", "", {}, "sha512-z"],
    "a/zod": ["zod@3.23.8", "", {}, "sha512-w"],
    "tarball": ["tarball@https://example.com/t.tgz", {}],
    "@scope/git": ["@scope/git@git+ssh://git@github.com/acme/git.git#4f2a", {}],
  },
}`), map[string][]string{"zod": {"3.23.8", "3.24.1"}, "real-name": {"1.0.0"}, "tarball": {"https://example.com/t.tgz"}, "@scope/git": {"git+ssh://git@github.com/acme/git.git#4f2a"}})
	for name, content := range map[string]string{
		"not JSON":          `{"packages": {`,
		"unterminated":      `{"packages": {"a": ["a@1"]}} /* open`,
		"a non-string head": `{"packages": {"a": [1, ""]}}`,
	} {
		if _, err := ReadBunLock([]byte(content)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
