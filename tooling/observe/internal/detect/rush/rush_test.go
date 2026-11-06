package rush

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/lockdiff"
)

const feature = "tooling/observe-rush"

// tree writes files under a new temporary directory and returns it.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// pkg renders a package.json with its dependencies, devDependencies and
// peerDependencies.
func pkg(name, version string, deps, dev, peer map[string]string) string {
	m := map[string]any{"name": name, "version": version}
	for key, value := range map[string]map[string]string{"dependencies": deps, "devDependencies": dev, "peerDependencies": peer} {
		if value != nil {
			m[key] = value
		}
	}
	data, _ := json.Marshal(m)
	return string(data)
}

const commentedConfig = `/**
 * This is the main configuration file for Rush.
 * For full documentation, please see https://rushjs.io
 */
{
  "$schema": "https://developer.microsoft.com/json-schemas/rush/v5/rush.schema.json",
  "rushVersion": "5.140.0",
  // The pnpm version "//" comments do not end strings.
  "pnpmVersion": "9.15.0",
  "nodeSupportedVersionRange": ">=18.20.3 <19.0.0 || >=20.14.0 <21.0.0",
  "projects": [
    {
      "packageName": "@demo/api",
      "projectFolder": "apps/api" // the API
    },
    /* the pricing library */
    {
      "packageName": "@demo/pricing",
      "projectFolder": "libs/pricing/",
    },
    {
      "packageName": "@demo/web",
      "projectFolder": "apps/web"
    },
  ]
}
`

func detect(t *testing.T, root string) Detection {
	t.Helper()
	d := Detect(root)
	if len(d.Diagnostics) != 0 {
		t.Fatalf("diagnostics %v", d.Diagnostics)
	}
	return d
}

func edges(d Detection) map[string][]string {
	out := map[string][]string{}
	for _, p := range d.Projects {
		out[p.Folder] = p.DependsOn
	}
	return out
}

func TestRushProjectsAreComponents(t *testing.T) {
	spectest.Proves(t, feature, "rush-projects-are-components", "rush-projects-are-orchestrator-components")
	spectest.Proves(t, feature, "rush-projects-are-components", "a-rush-json-with-comments-is-read")
	root := tree(t, map[string]string{
		Config:                      commentedConfig,
		"apps/api/package.json":     pkg("@demo/api", "1.0.0", nil, nil, nil),
		"libs/pricing/package.json": pkg("@demo/pricing", "2.1.0", nil, nil, nil),
		"apps/web/package.json":     pkg("@demo/web", "0.1.0", nil, nil, nil),
	})
	d := detect(t, root)
	want := []Project{
		{Folder: "apps/api", Name: "@demo/api", Version: "1.0.0"},
		{Folder: "apps/web", Name: "@demo/web", Version: "0.1.0"},
		{Folder: "libs/pricing", Name: "@demo/pricing", Version: "2.1.0"},
	}
	if !reflect.DeepEqual(d.Projects, want) {
		t.Errorf("projects %+v, want %+v", d.Projects, want)
	}
	if d.Manager != "pnpm" || d.Lockfile != PNPMLock {
		t.Errorf("manager %q, lockfile %q", d.Manager, d.Lockfile)
	}
}

func TestStripCommentsKeepsStrings(t *testing.T) {
	spectest.Proves(t, feature, "rush-projects-are-components", "a-rush-json-with-comments-is-read")
	in := `{"a": "x // y", "b": "/* z */", "c": "q\"//", /* c */ "d": [1, 2,], // e
"f": {"g": 1,}}`
	var got map[string]any
	if err := json.Unmarshal(StripComments([]byte(in)), &got); err != nil {
		t.Fatalf("%v: %s", err, StripComments([]byte(in)))
	}
	if got["a"] != "x // y" || got["b"] != "/* z */" || got["c"] != `q"//` || len(got["d"].([]any)) != 2 {
		t.Errorf("stripped %v", got)
	}
}

func TestAMalformedRushJSONIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "rush-projects-are-components", "a-malformed-rush-json-is-a-diagnostic")
	d := Detect(tree(t, map[string]string{Config: `{"projects": [ {"packageName": "a", "projectFolder": "a"} `}))
	if len(d.Projects) != 0 || len(d.Diagnostics) != 1 || d.Diagnostics[0].Code != CodeConfigInvalid || d.Diagnostics[0].Field != Config {
		t.Errorf("detection %+v", d)
	}
	if d := Detect(t.TempDir()); len(d.Projects) != 0 || len(d.Diagnostics) != 0 {
		t.Errorf("a repository without rush.json: %+v", d)
	}
}

func TestAnInvalidProjectEntryIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "rush-projects-are-components", "an-invalid-project-entry-is-a-diagnostic")
	root := tree(t, map[string]string{
		Config: `{"pnpmVersion": "9.0.0", "projects": [
			{"packageName": "a", "projectFolder": "libs/a"},
			{"packageName": "", "projectFolder": "libs/nameless"},
			{"packageName": "out", "projectFolder": "../outside"},
			{"packageName": "abs", "projectFolder": "/abs"},
			{"packageName": "a", "projectFolder": "libs/again"},
			{"packageName": "b", "projectFolder": "libs/a/"},
			{"packageName": "c", "projectFolder": "libs/c"}
		]}`,
		"libs/a/package.json": pkg("a", "1.0.0", nil, nil, nil),
		"libs/c/package.json": `{"name": `,
	})
	d := Detect(root)
	var folders []string
	for _, p := range d.Projects {
		folders = append(folders, p.Folder)
	}
	if strings.Join(folders, ",") != "libs/a,libs/c" {
		t.Errorf("projects %v", folders)
	}
	var locations []string
	for _, diagnostic := range d.Diagnostics {
		locations = append(locations, diagnostic.Code+" "+diagnostic.Field)
	}
	want := []string{
		CodeConfigInvalid + " rush.json#projects[1]",
		CodeConfigInvalid + " rush.json#projects[2]",
		CodeConfigInvalid + " rush.json#projects[3]",
		CodeConfigInvalid + " rush.json#projects[4]",
		CodeConfigInvalid + " rush.json#projects[5]",
		CodeManifestInvalid + " libs/c/package.json",
	}
	if !reflect.DeepEqual(locations, want) {
		t.Errorf("diagnostics %v, want %v", locations, want)
	}
}

func TestAWorkspaceRangeIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "rush-project-edges", "a-workspace-range-is-an-edge")
	root := tree(t, map[string]string{
		Config: `{"pnpmVersion": "9.0.0", "projects": [
			{"packageName": "api", "projectFolder": "apps/api"},
			{"packageName": "web", "projectFolder": "apps/web"},
			{"packageName": "pricing", "projectFolder": "libs/pricing"},
			{"packageName": "ui", "projectFolder": "libs/ui"},
			{"packageName": "tools", "projectFolder": "tools"}
		]}`,
		"apps/api/package.json":     pkg("api", "1.0.0", map[string]string{"pricing": "workspace:*", "react": "^18.0.0"}, nil, nil),
		"apps/web/package.json":     pkg("web", "1.0.0", nil, map[string]string{"my-ui": "workspace:ui@*"}, map[string]string{"pricing": "workspace:^"}),
		"libs/pricing/package.json": pkg("pricing", "2.0.0", map[string]string{"pricing": "workspace:*"}, nil, nil),
		"libs/ui/package.json":      pkg("ui", "1.0.0", nil, nil, nil),
		"tools/package.json":        pkg("tools", "1.0.0", nil, nil, nil),
	})
	got := edges(detect(t, root))
	want := map[string][]string{"apps/api": {"libs/pricing"}, "apps/web": {"libs/pricing", "libs/ui"}, "libs/pricing": nil, "libs/ui": nil, "tools": nil}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("edges %v, want %v", got, want)
	}
}

func TestASatisfiedLocalVersionIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "rush-project-edges", "a-satisfied-local-version-is-an-edge")
	root := tree(t, map[string]string{
		Config: `{"pnpmVersion": "9.0.0", "projects": [
			{"packageName": "api", "projectFolder": "apps/api"},
			{"packageName": "old", "projectFolder": "apps/old"},
			{"packageName": "opt", "projectFolder": "apps/opt"},
			{"packageName": "alias", "projectFolder": "apps/alias"},
			{"packageName": "pricing", "projectFolder": "libs/pricing"}
		]}`,
		"apps/api/package.json":     pkg("api", "1.0.0", nil, map[string]string{"pricing": "^2.1.0"}, nil),
		"apps/old/package.json":     pkg("old", "1.0.0", map[string]string{"pricing": "^1.0.0", "price": "npm:pricing@^1.0.0"}, nil, nil),
		"apps/opt/package.json":     `{"name": "opt", "version": "1.0.0", "optionalDependencies": {"pricing": "2.3.0"}}`,
		"apps/alias/package.json":   pkg("alias", "1.0.0", map[string]string{"price": "npm:pricing@^2.0.0"}, nil, nil),
		"libs/pricing/package.json": pkg("pricing", "2.3.0", nil, nil, nil),
	})
	got := edges(detect(t, root))
	for _, folder := range []string{"apps/api", "apps/opt", "apps/alias"} {
		if !reflect.DeepEqual(got[folder], []string{"libs/pricing"}) {
			t.Errorf("%s edges %v", folder, got[folder])
		}
	}
	if got["apps/old"] != nil {
		t.Errorf("apps/old edges %v", got["apps/old"])
	}
}

func TestADecoupledDependencyIsNoEdge(t *testing.T) {
	spectest.Proves(t, feature, "rush-project-edges", "a-decoupled-dependency-is-no-edge")
	root := tree(t, map[string]string{
		Config: `{"pnpmVersion": "9.0.0", "projects": [
			{"packageName": "lint", "projectFolder": "tools/lint", "decoupledLocalDependencies": ["core"]},
			{"packageName": "build", "projectFolder": "tools/build", "cyclicDependencyProjects": ["core"]},
			{"packageName": "docs", "projectFolder": "tools/docs", "decoupledLocalDependencies": ["kernel"]},
			{"packageName": "core", "projectFolder": "libs/core"}
		]}`,
		"tools/lint/package.json":  pkg("lint", "1.0.0", nil, map[string]string{"core": "1.0.0"}, nil),
		"tools/build/package.json": pkg("build", "1.0.0", nil, map[string]string{"core": "^1.0.0"}, nil),
		"tools/docs/package.json":  pkg("docs", "1.0.0", nil, map[string]string{"kernel": "npm:core@^1.0.0"}, nil),
		"libs/core/package.json":   pkg("core", "1.0.0", nil, nil, nil),
	})
	for folder, deps := range edges(detect(t, root)) {
		if deps != nil {
			t.Errorf("%s depends on %v", folder, deps)
		}
	}
}

func TestThePackageManagerSelectsTheLockfile(t *testing.T) {
	spectest.Proves(t, feature, "rush-lockfiles", "the-package-manager-selects-the-lockfile")
	for _, c := range []struct{ field, manager, lockfile, content, other string }{
		{"pnpmVersion", "pnpm", PNPMLock, "lockfileVersion: '9.0'\n\npackages:\n\n  zod@3.24.1:\n    resolution: {integrity: x}\n", YarnLock},
		{"npmVersion", "npm", NPMShrinkwrap, `{"lockfileVersion": 3, "packages": {"node_modules/zod": {"version": "3.24.1"}}}`, PNPMLock},
		{"yarnVersion", "yarn", YarnLock, "zod@3.24.1:\n  version \"3.24.1\"\n", NPMShrinkwrap},
	} {
		d := detect(t, tree(t, map[string]string{Config: `{"` + c.field + `": "1.0.0", "projects": []}`}))
		if d.Manager != c.manager || d.Lockfile != c.lockfile {
			t.Errorf("%s: manager %q, lockfile %q", c.field, d.Manager, d.Lockfile)
		}
		read, ok := d.LockReader(c.lockfile)
		if !ok {
			t.Fatalf("%s: %s has no reader", c.field, c.lockfile)
		}
		packages, err := read([]byte(c.content))
		if err != nil || !reflect.DeepEqual(packages, lockdiff.Packages{"zod": {"3.24.1"}}) {
			t.Errorf("%s: %v, %v", c.field, packages, err)
		}
		if _, ok := d.LockReader(c.other); ok {
			t.Errorf("%s: %s has a reader", c.field, c.other)
		}
	}
	if _, ok := detect(t, tree(t, map[string]string{Config: `{"projects": []}`})).LockReader(PNPMLock); ok {
		t.Error("a rush.json without a package manager reads a lockfile")
	}
}

func TestASubspaceLockfileIsRead(t *testing.T) {
	spectest.Proves(t, feature, "rush-lockfiles", "a-subspace-lockfile-is-read")
	pnpm := detect(t, tree(t, map[string]string{Config: `{"pnpmVersion": "9.0.0", "projects": []}`}))
	read, ok := pnpm.LockReader("common/config/subspaces/legacy/pnpm-lock.yaml")
	if !ok {
		t.Fatal("a subspace lockfile has no reader")
	}
	packages, err := read([]byte("lockfileVersion: '9.0'\n\npackages:\n\n  zod@3.24.1:\n    resolution: {integrity: x}\n  '@rush-temp/api@1.0.0':\n    resolution: {integrity: x}\n"))
	if err != nil || !reflect.DeepEqual(packages, lockdiff.Packages{"zod": {"3.24.1"}}) {
		t.Errorf("a subspace lockfile reads %v, %v", packages, err)
	}
	for _, file := range []string{"common/config/subspaces/pnpm-lock.yaml", "common/config/subspaces/a/b/pnpm-lock.yaml", "apps/api/pnpm-lock.yaml"} {
		if _, ok := pnpm.LockReader(file); ok {
			t.Errorf("%s has a reader", file)
		}
	}
	yarn := detect(t, tree(t, map[string]string{Config: `{"yarnVersion": "1.22.0", "projects": []}`}))
	if _, ok := yarn.LockReader("common/config/subspaces/legacy/pnpm-lock.yaml"); ok {
		t.Error("a yarn monorepo reads a subspace pnpm lockfile")
	}
}

func TestRushTemporaryProjectsAreNoDependency(t *testing.T) {
	spectest.Proves(t, feature, "rush-lockfiles", "rush-temporary-projects-are-no-dependency")
	npm := detect(t, tree(t, map[string]string{Config: `{"npmVersion": "6.14.0", "projects": []}`}))
	read, _ := npm.LockReader(NPMShrinkwrap)
	packages, err := read([]byte(`{"lockfileVersion": 2, "packages": {
		"node_modules/@rush-temp/api": {"version": "0.0.0", "resolved": "file:projects/api.tgz"},
		"node_modules/zod": {"version": "3.24.1"}}}`))
	if err != nil || !reflect.DeepEqual(packages, lockdiff.Packages{"zod": {"3.24.1"}}) {
		t.Errorf("npm-shrinkwrap.json: %v, %v", packages, err)
	}
	pnpm := detect(t, tree(t, map[string]string{Config: `{"pnpmVersion": "9.0.0", "projects": []}`}))
	read, _ = pnpm.LockReader(PNPMLock)
	packages, err = read([]byte("lockfileVersion: 5.4\n\npackages:\n\n  file:projects/api.tgz:\n    name: '@rush-temp/api'\n  /zod/3.24.1:\n    resolution: {integrity: x}\n"))
	if err != nil || !reflect.DeepEqual(packages, lockdiff.Packages{"zod": {"3.24.1"}}) {
		t.Errorf("pnpm-lock.yaml: %v, %v", packages, err)
	}
	if _, err := read([]byte("lockfileVersion: '9.0'\npackages:\n" + strings.Repeat("x", 17<<20))); err == nil {
		t.Error("an unreadable lockfile reads")
	}
}
