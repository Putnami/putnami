package lockdiff

import (
	"slices"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-analyses"

func read(t *testing.T, name, content string) Packages {
	t.Helper()
	kind, ok := KindOf("sub/" + name)
	if !ok {
		t.Fatalf("%s is not a lockfile", name)
	}
	packages, err := kind.Read([]byte(content))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return packages
}

func expect(t *testing.T, label string, got Packages, want map[string][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %v, want %v", label, got, want)
	}
	for name, versions := range want {
		have := slices.Clone(got[name])
		slices.Sort(have)
		if !slices.Equal(have, versions) {
			t.Errorf("%s: %s = %v, want %v", label, name, got[name], versions)
		}
	}
}

func TestEachLockfileFormatGivesItsPackageChanges(t *testing.T) {
	spectest.Proves(t, feature, "dependency-changes", "each-lockfile-format-gives-its-package-changes")
	expect(t, "package-lock v3", read(t, PackageLock, `{"lockfileVersion":3,"packages":{
		"":{"name":"root"},
		"packages/pricing":{"name":"@demo/pricing","version":"1.0.0"},
		"node_modules/@demo/pricing":{"resolved":"packages/pricing","link":true},
		"node_modules/decimal.js":{"version":"10.5.0"},
		"node_modules/@scope/a":{"version":"1.0.0"},
		"node_modules/b/node_modules/@scope/a":{"version":"2.0.0"},
		"node_modules/alias":{"name":"real","version":"3.0.0"}}}`),
		map[string][]string{"decimal.js": {"10.5.0"}, "@scope/a": {"1.0.0", "2.0.0"}, "real": {"3.0.0"}})
	expect(t, "package-lock v1", read(t, PackageLock, `{"lockfileVersion":1,"dependencies":{
		"a":{"version":"1.0.0","dependencies":{"b":{"version":"2.0.0"}}},
		"local":{"version":"file:../local"}}}`),
		map[string][]string{"a": {"1.0.0"}, "b": {"2.0.0"}})
	expect(t, "pnpm v9", read(t, PNPMLock, `lockfileVersion: '9.0'

importers:

  packages/pricing:
    dependencies:
      zod:
        specifier: 3.24.1
        version: 3.24.1

packages:

  '@vitest/coverage-v8@3.2.7':
    resolution: {integrity: sha512-x}

  zod@3.24.1:
    resolution: {integrity: sha512-y}

snapshots:

  zod@3.24.1: {}
  other@1.0.0: {}
`), map[string][]string{"@vitest/coverage-v8": {"3.2.7"}, "zod": {"3.24.1"}})
	expect(t, "pnpm v6", read(t, PNPMLock, "lockfileVersion: '6.0'\n\npackages:\n\n  /react-dom@18.2.0(react@18.2.0):\n    resolution: {integrity: x}\n"),
		map[string][]string{"react-dom": {"18.2.0"}})
	expect(t, "pnpm v5", read(t, PNPMLock, "lockfileVersion: 5.4\n\npackages:\n\n  /@babel/core/7.22.0_abc:\n    resolution: {integrity: x}\n"),
		map[string][]string{"@babel/core": {"7.22.0"}})
	expect(t, "yarn classic", read(t, YarnLock, `# yarn lockfile v1

"@babel/core@^7.0.0", "@babel/core@^7.1.0":
  version "7.2.0"
  dependencies:
    debug "^4.1.0"

debug@^4.1.0:
  version "4.3.4"
`), map[string][]string{"@babel/core": {"7.2.0"}, "debug": {"4.3.4"}})
	expect(t, "yarn berry", read(t, YarnLock, `__metadata:
  version: 8

"@demo/pricing@workspace:packages/pricing":
  version: 0.0.0-use.local

"zod@npm:3.24.1":
  version: 3.24.1
  resolution: "zod@npm:3.24.1"
`), map[string][]string{"zod": {"3.24.1"}})
	expect(t, "composer", read(t, ComposerLock, `{"packages":[
		{"name":"brick/math","version":"0.13.1","dist":{"type":"zip"}},
		{"name":"demo/pricing","version":"dev-main","dist":{"type":"path"}}],
		"packages-dev":[{"name":"phpunit/phpunit","version":"12.0.0"}]}`),
		map[string][]string{"brick/math": {"0.13.1"}, "phpunit/phpunit": {"12.0.0"}})
	if _, ok := KindOf("go.sum"); ok {
		t.Error("go.sum is read as a lockfile")
	}
	for _, name := range []string{PackageLock, ComposerLock} {
		kind, _ := KindOf(name)
		if _, err := kind.Read([]byte("{")); err == nil {
			t.Errorf("a malformed %s reads", name)
		}
	}

	changes := Diff(
		Packages{"same": {"1.0.0"}, "up": {"1.2.3"}, "down": {"2.0.0"}, "gone": {"1.0.0"}, "git": {"abc"}, "multi": {"1.0.0"}, "pre": {"1.0.0-beta.2"}},
		Packages{"same": {"1.0.0"}, "up": {"1.10.0"}, "down": {"1.9.9"}, "new": {"0.1.0"}, "git": {"def"}, "multi": {"1.0.0", "2.0.0"}, "pre": {"1.0.0"}},
	)
	want := []Change{
		{Name: "down", Change: evidence.DependencyDowngraded, From: "2.0.0", To: "1.9.9"},
		{Name: "git", Change: evidence.DependencyChanged, From: "abc", To: "def"},
		{Name: "gone", Change: evidence.DependencyRemoved, From: "1.0.0"},
		{Name: "multi", Change: evidence.DependencyChanged, From: "1.0.0", To: "1.0.0, 2.0.0"},
		{Name: "new", Change: evidence.DependencyAdded, To: "0.1.0"},
		{Name: "pre", Change: evidence.DependencyUpgraded, From: "1.0.0-beta.2", To: "1.0.0"},
		{Name: "up", Change: evidence.DependencyUpgraded, From: "1.2.3", To: "1.10.0"},
	}
	if !slices.Equal(changes, want) {
		t.Errorf("diff\n got %+v\nwant %+v", changes, want)
	}
	for _, c := range []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v0.20.0", "v0.21.0", -1, true},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1, true},
		{"1.0.0-alpha.10", "1.0.0-alpha.9", 1, true},
		{"1.0.0-alpha.beta", "1.0.0-alpha.1", 1, true},
		{"1.0.0-rc.1", "1.0.0-beta", 1, true},
		{"1.2", "1.2.0+build", 0, true},
		{"dev-main", "1.0.0", 0, false},
	} {
		if got, ok := Compare(c.a, c.b); ok != c.ok || (ok && (got < 0) != (c.want < 0)) || (ok && (got > 0) != (c.want > 0)) {
			t.Errorf("Compare(%s, %s) = %d %v, want %d %v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}
