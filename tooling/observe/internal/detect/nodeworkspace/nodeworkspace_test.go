package nodeworkspace

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

const feature = "tooling/observe-node-workspaces"

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

func pkg(name, version string, deps map[string]string) string {
	m := map[string]any{"name": name}
	if version != "" {
		m["version"] = version
	}
	if deps != nil {
		m["dependencies"] = deps
	}
	data, _ := json.Marshal(m)
	return string(data)
}

func read(t *testing.T, root string) Workspace {
	t.Helper()
	ws, ok, diags := Read(root)
	if !ok {
		t.Fatalf("no workspace detected: %v", diags)
	}
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	return ws
}

// detect returns the projects of a detected workspace by path.
func detect(t *testing.T, root string) (map[string]wsproto.ProbeProject, wsproto.ProbeResult) {
	t.Helper()
	result, ok := Detect(root)
	if !ok {
		t.Fatalf("no workspace detected: %v", result.Diagnostics)
	}
	if problems := wsproto.ValidateProbeResult(&result); diag.HasErrors(problems) {
		t.Fatalf("the probe result does not validate: %v", problems)
	}
	projects := map[string]wsproto.ProbeProject{}
	for _, p := range result.Projects {
		projects[p.Path] = p
	}
	return projects, result
}

func dependencies(t *testing.T, root string) map[string][]string {
	t.Helper()
	projects, _ := detect(t, root)
	out := map[string][]string{}
	for p, project := range projects {
		out[p] = project.Dependencies
	}
	return out
}

func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestNPMAndYarnWorkspacesNameTheMembers(t *testing.T) {
	spectest.Proves(t, feature, "workspace-members", "npm-and-yarn-workspaces-name-the-members")
	npm := tree(t, map[string]string{
		"package.json":                `{"name":"root","workspaces":["packages/*","./tools/cli/"]}`,
		"package-lock.json":           `{}`,
		"packages/a/package.json":     pkg("a", "1.0.0", nil),
		"packages/b/package.json":     pkg("b", "1.0.0", nil),
		"packages/docs/readme.md":     "no manifest",
		"tools/cli/package.json":      pkg("cli", "0.1.0", nil),
		"tools/other/package.json":    pkg("other", "0.1.0", nil),
		"packages/a/nested/x.json":    "{}",
		"packages/a/sub/package.json": pkg("sub", "1.0.0", nil),
	})
	ws := read(t, npm)
	if want := []string{"packages/a", "packages/b", "tools/cli"}; !reflect.DeepEqual(ws.Members, want) {
		t.Fatalf("members %v, want %v", ws.Members, want)
	}
	if ws.Manager != ManagerNPM || ws.Source != PackageJSON || ws.Lockfile != "package-lock.json" {
		t.Fatalf("workspace %+v", ws)
	}

	yarn := tree(t, map[string]string{
		"package.json":              `{"name":"root","packageManager":"yarn@4.5.0","workspaces":{"packages":["libs/**"],"nohoist":["**/x"]}}`,
		"yarn.lock":                 "",
		"libs/a/package.json":       pkg("@acme/a", "2.0.0", nil),
		"libs/group/b/package.json": pkg("@acme/b", "2.0.0", nil),
	})
	ws = read(t, yarn)
	if want := []string{"libs/a", "libs/group/b"}; !reflect.DeepEqual(ws.Members, want) {
		t.Fatalf("members %v, want %v", ws.Members, want)
	}
	if ws.Manager != ManagerYarn || ws.Lockfile != "yarn.lock" {
		t.Fatalf("workspace %+v", ws)
	}
	projects, _ := detect(t, yarn)
	if p := projects["libs/group/b"]; p.SourceName != "@acme/b" || p.Version != "2.0.0" || p.SourceFile != "libs/group/b/package.json" {
		t.Fatalf("project %+v", p)
	}
	if _, root := projects["."]; root {
		t.Fatal("the workspace root is reported as a member")
	}
}

func TestAPNPMWorkspaceNamesTheMembers(t *testing.T) {
	spectest.Proves(t, feature, "workspace-members", "a-pnpm-workspace-names-the-members")
	block := tree(t, map[string]string{
		"package.json":             `{"name":"root","workspaces":["ignored/*"]}`,
		"pnpm-workspace.yaml":      "# members\npackages:\n  - 'apps/*' # the apps\n  - \"packages/*\"\n\n  - tools\ncatalog:\n  zod: 3.24.1\n",
		"apps/web/package.json":    pkg("web", "1.0.0", nil),
		"packages/ui/package.json": pkg("ui", "1.0.0", nil),
		"tools/package.json":       pkg("tools", "1.0.0", nil),
		"ignored/x/package.json":   pkg("x", "1.0.0", nil),
	})
	ws := read(t, block)
	if want := []string{"apps/web", "packages/ui", "tools"}; !reflect.DeepEqual(ws.Members, want) {
		t.Fatalf("members %v, want %v", ws.Members, want)
	}
	if ws.Source != PNPMWorkspace || ws.Manager != ManagerPNPM {
		t.Fatalf("workspace %+v", ws)
	}

	flow := tree(t, map[string]string{
		"pnpm-workspace.yaml":    "packages: ['apps/*', libs/core]\n",
		"apps/web/package.json":  pkg("web", "1.0.0", nil),
		"libs/core/package.json": pkg("core", "1.0.0", nil),
	})
	if ws := read(t, flow); !reflect.DeepEqual(ws.Members, []string{"apps/web", "libs/core"}) {
		t.Fatalf("members %v", ws.Members)
	}

	// A pnpm-workspace.yaml that holds only settings declares no member.
	settings := tree(t, map[string]string{
		"package.json":        `{"workspaces":["libs/*"]}`,
		"pnpm-workspace.yaml": "onlyBuiltDependencies:\n  - esbuild\n",
		"libs/a/package.json": pkg("a", "1.0.0", nil),
	})
	if ws := read(t, settings); ws.Source != PackageJSON || !reflect.DeepEqual(ws.Members, []string{"libs/a"}) {
		t.Fatalf("workspace %+v", ws)
	}
}

func TestABracePatternNamesEachAlternative(t *testing.T) {
	spectest.Proves(t, feature, "workspace-members", "a-brace-pattern-names-each-alternative")
	root := tree(t, map[string]string{
		"package.json":                `{"workspaces":["{apps,tools/{lint,config}}/*","!apps/{old,legacy}","packages/{a","b}"]}`,
		"apps/web/package.json":       pkg("web", "1.0.0", nil),
		"apps/old/package.json":       pkg("old", "1.0.0", nil),
		"tools/lint/x/package.json":   pkg("x", "1.0.0", nil),
		"tools/config/y/package.json": pkg("y", "1.0.0", nil),
		"tools/other/z/package.json":  pkg("z", "1.0.0", nil),
	})
	ws, ok, diags := Read(root)
	if !ok || !reflect.DeepEqual(ws.Members, []string{"apps/web", "tools/config/y", "tools/lint/x"}) {
		t.Fatalf("members %v, diagnostics %v", ws.Members, diags)
	}
	if len(diags) != 2 || !hasCode(diags, CodePatternInvalid) {
		t.Fatalf("unbalanced braces are not diagnostics: %v", diags)
	}
	if _, ok := expandBraces(strings.Repeat("{a,b,c,d}", 5)); ok {
		t.Fatal("a brace pattern past the bound is expanded")
	}
}

func TestANegatedPatternExcludesAMember(t *testing.T) {
	spectest.Proves(t, feature, "workspace-members", "a-negated-pattern-excludes-a-member")
	root := tree(t, map[string]string{
		"pnpm-workspace.yaml":                      "packages:\n  - 'packages/**'\n  - '!**/test/**'\n  - '!packages/legacy'\n",
		"packages/a/package.json":                  pkg("a", "1.0.0", nil),
		"packages/group/b/package.json":            pkg("b", "1.0.0", nil),
		"packages/legacy/package.json":             pkg("legacy", "1.0.0", nil),
		"packages/a/test/fixture/package.json":     pkg("fixture", "1.0.0", nil),
		"packages/a/node_modules/dep/package.json": pkg("dep", "1.0.0", nil),
		"packages/.cache/c/package.json":           pkg("cached", "1.0.0", nil),
		"packages/.hidden/package.json":            pkg("hidden", "1.0.0", nil),
	})
	if ws := read(t, root); !reflect.DeepEqual(ws.Members, []string{"packages/a", "packages/group/b"}) {
		t.Fatalf("members %v", ws.Members)
	}
}

func TestAPatternNamesADotDirectory(t *testing.T) {
	root := tree(t, map[string]string{
		"package.json":                           `{"workspaces":["packages/**/.internal"]}`,
		"packages/a/.internal/package.json":      pkg("a-internal", "1.0.0", nil),
		"packages/.cache/.internal/package.json": pkg("cached", "1.0.0", nil),
		"packages/b/package.json":                pkg("b", "1.0.0", nil),
	})
	if ws := read(t, root); !reflect.DeepEqual(ws.Members, []string{"packages/a/.internal"}) {
		t.Fatalf("members %v", ws.Members)
	}
}

func TestARepositoryWithoutAWorkspaceIsNotDetected(t *testing.T) {
	spectest.Proves(t, feature, "workspace-members", "a-repository-without-a-workspace-is-not-detected")
	for name, files := range map[string]map[string]string{
		"single package":     {"package.json": pkg("app", "1.0.0", nil), "package-lock.json": "{}"},
		"no package":         {"go.mod": "module x\n"},
		"pnpm settings only": {"package.json": pkg("app", "1.0.0", nil), "pnpm-workspace.yaml": "onlyBuiltDependencies:\n  - esbuild\n"},
	} {
		root := tree(t, files)
		if _, ok, diags := Read(root); ok || len(diags) != 0 {
			t.Fatalf("%s: detected %v, diagnostics %v", name, ok, diags)
		}
		result, ok := Detect(root)
		if ok || len(result.Projects) != 0 {
			t.Fatalf("%s: detected %+v", name, result)
		}
	}
}

func TestWorkspaceProtocolDependenciesAreEdges(t *testing.T) {
	spectest.Proves(t, feature, "internal-edges", "workspace-protocol-dependencies-are-edges")
	root := tree(t, map[string]string{
		"pnpm-workspace.yaml":  "packages: ['p/*']\n",
		"p/core/package.json":  pkg("core", "1.0.0", nil),
		"p/util/package.json":  pkg("util", "1.0.0", nil),
		"p/ui/package.json":    pkg("ui", "1.0.0", nil),
		"p/local/package.json": pkg("local", "1.0.0", nil),
		"p/app/package.json": `{"name":"app","version":"1.0.0",
			"dependencies":{"core":"workspace:*","react":"^19.0.0","app":"workspace:*","renamed":"workspace:util@^1.0.0","missing":"workspace:*"},
			"devDependencies":{"ui":"workspace:^"},
			"peerDependencies":{"local":"link:../local"},
			"optionalDependencies":{"elsewhere":"file:../../outside"}}`,
		"p/web/package.json": `{"name":"web","dependencies":{"local":"file:../local/","core":"workspace:../core"}}`,
	})
	deps := dependencies(t, root)
	if want := []string{"p/core", "p/local", "p/ui", "p/util"}; !reflect.DeepEqual(deps["p/app"], want) {
		t.Fatalf("app depends on %v, want %v", deps["p/app"], want)
	}
	if want := []string{"p/core", "p/local"}; !reflect.DeepEqual(deps["p/web"], want) {
		t.Fatalf("web depends on %v, want %v", deps["p/web"], want)
	}
	if len(deps["p/core"]) != 0 {
		t.Fatalf("core depends on %v", deps["p/core"])
	}
}

func TestASatisfiedVersionRangeIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "internal-edges", "a-satisfied-version-range-is-an-edge")
	root := tree(t, map[string]string{
		"package.json":        `{"workspaces":["p/*"]}`,
		"p/core/package.json": pkg("core", "1.4.2", nil),
		"p/util/package.json": pkg("util", "0.3.0", nil),
		"p/old/package.json":  pkg("old", "3.0.0", nil),
		"p/app/package.json":  pkg("app", "1.0.0", map[string]string{"core": "^1.2.0", "util": "~0.3", "old": "^2.0.0"}),
	})
	if want := []string{"p/core", "p/util"}; !reflect.DeepEqual(dependencies(t, root)["p/app"], want) {
		t.Fatalf("app depends on %v, want %v", dependencies(t, root)["p/app"], want)
	}

	for _, c := range []struct {
		version, rng string
		want         bool
	}{
		{"1.2.3", "1.2.3", true},
		{"1.2.3", "=v1.2.3", true},
		{"1.2.4", "1.2.3", false},
		{"1.2.3", "*", true},
		{"1.2.3", "", true},
		{"1.2.3", "1.x", true},
		{"1.2.3", "1.2", true},
		{"1.3.0", "1.2", false},
		{"1.9.0", "^1.2.3", true},
		{"2.0.0", "^1.2.3", false},
		{"0.2.9", "^0.2.3", true},
		{"0.3.0", "^0.2.3", false},
		{"0.0.3", "^0.0.3", true},
		{"0.0.4", "^0.0.3", false},
		{"0.9.0", "^0", true},
		{"1.2.9", "~1.2.3", true},
		{"1.3.0", "~1.2.3", false},
		{"1.9.0", "~1", true},
		{"1.2.3", ">=1.2.3 <2", true},
		{"2.0.0", ">=1.2.3 <2", false},
		{"1.2.3", ">= 1.0.0", true},
		{"1.2.3", ">1.2", false},
		{"1.3.0", ">1.2", true},
		{"1.2.4", ">1.2.3", true},
		{"1.2.9", "<=1.2", true},
		{"1.3.0", "<=1.2", false},
		{"1.2.3", "<=1.2.3", true},
		{"1.2.3", "<1.2.3", false},
		{"1.5.0", "1.2.3 - 1.6", true},
		{"1.7.0", "1.2.3 - 1.6", false},
		{"3.0.0", "^1.0.0 || ^3.0.0", true},
		{"1.0.0-beta.1", "1.0.0-beta.1", true},
		{"1.0.0", ">1.0.0-beta.1", true},
		{"1.2.3", "latest", false},
		{"1.2.3", "npm:other@1.2.3", false},
		{"", "^1.0.0", false},
		{"1.2.3", "1.2.3.4", false},
	} {
		if got := Satisfies(c.version, c.rng); got != c.want {
			t.Errorf("Satisfies(%q, %q) = %v, want %v", c.version, c.rng, got, c.want)
		}
	}
}

func TestPNPMLinksARangeOnlyWhenConfigured(t *testing.T) {
	spectest.Proves(t, feature, "internal-edges", "pnpm-links-a-range-only-when-configured")
	files := func(yaml string) map[string]string {
		return map[string]string{
			"pnpm-workspace.yaml": yaml,
			"p/core/package.json": pkg("core", "1.0.0", nil),
			"p/app/package.json":  pkg("app", "1.0.0", map[string]string{"core": "^1.0.0"}),
		}
	}
	if deps := dependencies(t, tree(t, files("packages: ['p/*']\n")))["p/app"]; len(deps) != 0 {
		t.Fatalf("pnpm links a range by default: %v", deps)
	}
	if deps := dependencies(t, tree(t, files("packages: ['p/*']\nlinkWorkspacePackages: true\n")))["p/app"]; !reflect.DeepEqual(deps, []string{"p/core"}) {
		t.Fatalf("linkWorkspacePackages: %v", deps)
	}
	withNPMRC := files("packages: ['p/*']\n")
	withNPMRC[".npmrc"] = "auto-install-peers=true\nlink-workspace-packages = true\n"
	if deps := dependencies(t, tree(t, withNPMRC))["p/app"]; !reflect.DeepEqual(deps, []string{"p/core"}) {
		t.Fatalf(".npmrc link-workspace-packages: %v", deps)
	}
}

func TestTheAnswerValidatesOnTheProbeWire(t *testing.T) {
	spectest.Proves(t, feature, "probe-answer", "the-answer-validates-on-the-probe-wire")
	root := tree(t, map[string]string{
		"package.json":         `{"workspaces":["p/*"]}`,
		"yarn.lock":            "",
		"p/core/package.json":  pkg("core", "1.0.0", nil),
		"p/app/package.json":   pkg("app", "1.0.0", map[string]string{"core": "workspace:*"}),
		"p/extra/package.json": pkg("extra", "1.0.0", nil),
	})
	serve := func(request wsproto.ProbeRequest) *wsproto.ProbeResult {
		t.Helper()
		var stdin, stdout bytes.Buffer
		if err := wsproto.EncodeProbeRequest(&stdin, request); err != nil {
			t.Fatal(err)
		}
		handled, err := wsproto.ServeProbe(wsproto.ProbeControlArgs(), &stdin, &stdout, func(r wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
			return Probe(root, r)
		})
		if !handled || err != nil {
			t.Fatalf("handled %v, err %v", handled, err)
		}
		result, problems := wsproto.ParseAndValidateProbeResult(stdout.Bytes())
		if diag.HasErrors(problems) {
			t.Fatalf("the answer does not validate: %v\n%s", problems, stdout.String())
		}
		return result
	}

	result := serve(wsproto.ProbeRequest{Extension: Extension, Paths: []string{"p/app", "p/core", "elsewhere"}, DependencySources: true})
	if len(result.Projects) != 2 {
		t.Fatalf("projects %+v, want p/app and p/core", result.Projects)
	}
	app := result.Projects[0]
	if app.Path != "p/app" || !reflect.DeepEqual(app.Dependencies, []string{"p/core"}) ||
		app.DependencySources["p/core"] != wsproto.DependencySourcePackageJSON {
		t.Fatalf("app %+v", app)
	}
	if want := []string{".npmrc", "npm-shrinkwrap.json", "package-lock.json", "package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "yarn.lock"}; !reflect.DeepEqual(result.WatchedFiles, want) {
		t.Fatalf("watched %v, want %v", result.WatchedFiles, want)
	}
	if result.Extension != Extension {
		t.Fatalf("extension %q", result.Extension)
	}

	unattributed := serve(wsproto.ProbeRequest{Extension: Extension, Paths: []string{"p/app"}})
	if len(unattributed.Projects) != 1 || unattributed.Projects[0].DependencySources != nil {
		t.Fatalf("an unasked attribution is answered: %+v", unattributed.Projects)
	}
	if none := serve(wsproto.ProbeRequest{Extension: Extension}); len(none.Projects) != 0 {
		t.Fatalf("a request without paths is answered with %+v", none.Projects)
	}
}

// TestTheTurborepoDemoLayout reads the layout of Putnami/demo-observe-turbo:
// pnpm and Turborepo, an api app that depends on a pricing package, and
// Vitest reports under each package's reports directory.
func TestTheTurborepoDemoLayout(t *testing.T) {
	spectest.Proves(t, feature, "probe-answer", "the-turborepo-demo-layout")
	root := tree(t, map[string]string{
		"package.json":                       `{"name":"demo-observe-turbo","private":true,"packageManager":"pnpm@10.26.1","devDependencies":{"turbo":"^2.5.0","vitest":"^3.2.0"}}`,
		"pnpm-workspace.yaml":                "packages:\n  - apps/*\n  - packages/*\nonlyBuiltDependencies:\n  - esbuild\n",
		"pnpm-lock.yaml":                     "lockfileVersion: '9.0'\n",
		"turbo.json":                         `{"tasks":{"test":{"outputs":["reports/**"]}}}`,
		"apps/api/package.json":              `{"name":"@demo/api","version":"1.0.0","private":true,"dependencies":{"@demo/pricing":"workspace:*"},"devDependencies":{"@types/node":"^22.20.5"}}`,
		"apps/api/src/quote.ts":              "",
		"packages/pricing/package.json":      `{"name":"@demo/pricing","version":"1.0.0","dependencies":{"zod":"3.24.1"}}`,
		"packages/pricing/src/index.ts":      "",
		"packages/pricing/reports/junit.xml": "<testsuites/>",
	})
	projects, result := detect(t, root)
	if len(projects) != 2 {
		t.Fatalf("projects %v", projects)
	}
	api, pricing := projects["apps/api"], projects["packages/pricing"]
	if api.SourceName != "@demo/api" || !reflect.DeepEqual(api.Dependencies, []string{"packages/pricing"}) {
		t.Fatalf("api %+v", api)
	}
	if pricing.SourceName != "@demo/pricing" || len(pricing.Dependencies) != 0 {
		t.Fatalf("pricing %+v", pricing)
	}
	meta, err := decodeMetadata(pricing.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Ecosystem != Ecosystem || meta.Manager != ManagerPNPM || meta.Lockfile != "pnpm-lock.yaml" {
		t.Fatalf("metadata %+v", meta)
	}
	if !contains(meta.Reports.JUnit, "reports/junit.xml") || !contains(meta.Reports.Coverage, "reports/coverage/lcov.info") {
		t.Fatalf("the demo's report locations are not named: %+v", meta.Reports)
	}
	if want := []string{".npmrc", "npm-shrinkwrap.json", "package-lock.json", "package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "yarn.lock"}; !reflect.DeepEqual(result.WatchedFiles, want) {
		t.Fatalf("watched %v, want %v", result.WatchedFiles, want)
	}
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func TestTheManagerAndTheLockfileAreNamed(t *testing.T) {
	spectest.Proves(t, feature, "lockfile-and-reports", "the-manager-and-the-lockfile-are-named")
	member := pkg("a", "1.0.0", nil)
	for _, c := range []struct {
		name          string
		files         map[string]string
		manager, lock string
	}{
		{"packageManager wins", map[string]string{"package.json": `{"packageManager":"yarn@1.22.22","workspaces":["a"]}`, "package-lock.json": "{}"}, ManagerYarn, "package-lock.json"},
		{"pnpm-workspace.yaml", map[string]string{"pnpm-workspace.yaml": "packages: [a]\n"}, ManagerPNPM, ""},
		{"yarn lockfile", map[string]string{"package.json": `{"workspaces":["a"]}`, "yarn.lock": ""}, ManagerYarn, "yarn.lock"},
		{"pnpm lockfile", map[string]string{"package.json": `{"workspaces":["a"]}`, "pnpm-lock.yaml": ""}, ManagerPNPM, "pnpm-lock.yaml"},
		{"shrinkwrap", map[string]string{"package.json": `{"workspaces":["a"]}`, "npm-shrinkwrap.json": "{}"}, ManagerNPM, "npm-shrinkwrap.json"},
		{"the manager's own lockfile", map[string]string{"package.json": `{"packageManager":"npm@10.9.0","workspaces":["a"]}`, "yarn.lock": "", "package-lock.json": "{}"}, ManagerNPM, "package-lock.json"},
		{"another lockfile", map[string]string{"package.json": `{"packageManager":"npm@10.9.0","workspaces":["a"]}`, "yarn.lock": ""}, ManagerNPM, "yarn.lock"},
		{"nothing else", map[string]string{"package.json": `{"packageManager":"bun@1.2.0","workspaces":["a"]}`}, ManagerNPM, ""},
	} {
		c.files["a/package.json"] = member
		ws := read(t, tree(t, c.files))
		if ws.Manager != c.manager || ws.Lockfile != c.lock {
			t.Errorf("%s: manager %q lockfile %q, want %q %q", c.name, ws.Manager, ws.Lockfile, c.manager, c.lock)
		}
	}
}

func TestStandardReportLocationsAreNamed(t *testing.T) {
	spectest.Proves(t, feature, "lockfile-and-reports", "standard-report-locations-are-named")
	projects, _ := detect(t, tree(t, map[string]string{
		"package.json":   `{"workspaces":["a"]}`,
		"a/package.json": pkg("a", "1.0.0", nil),
	}))
	meta, err := decodeMetadata(projects["a"].Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Lockfile != "" {
		t.Fatalf("a lockfile is named where there is none: %q", meta.Lockfile)
	}
	for _, want := range []string{"junit.xml", "test-results/*.xml"} {
		if !contains(meta.Reports.JUnit, want) {
			t.Errorf("JUnit location %q is not named", want)
		}
	}
	for _, want := range []string{"coverage/lcov.info", "coverage/coverage-final.json", "coverage/cobertura-coverage.xml", "coverage/clover.xml"} {
		if !contains(meta.Reports.Coverage, want) {
			t.Errorf("coverage location %q is not named", want)
		}
	}
	for _, pattern := range append(append([]string{}, meta.Reports.JUnit...), meta.Reports.Coverage...) {
		if _, ok := patternSegments(pattern); !ok || strings.HasPrefix(pattern, "/") {
			t.Errorf("report pattern %q is not relative to the member", pattern)
		}
	}
}

func TestAMalformedManifestIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "malformed-manifests", "a-malformed-manifest-is-a-diagnostic")

	// A root package.json that is not JSON: no workspace, one diagnostic.
	_, ok, diags := Read(tree(t, map[string]string{"package.json": "{", "a/package.json": pkg("a", "1.0.0", nil)}))
	if ok || !hasCode(diags, CodeManifestInvalid) {
		t.Fatalf("detected %v, diagnostics %v", ok, diags)
	}
	// workspaces of another shape.
	_, ok, diags = Read(tree(t, map[string]string{"package.json": `{"workspaces":"a"}`}))
	if ok || !hasCode(diags, CodeManifestInvalid) {
		t.Fatalf("detected %v, diagnostics %v", ok, diags)
	}
	// A pnpm-workspace.yaml whose packages is not a list.
	for _, yaml := range []string{"packages: apps/*\n", "packages:\n  apps: '*'\n", "packages:\n  - 'apps/*\n", "packages: ['a', ]\n", "packages:\n  - *alias\n"} {
		ws, ok, diags := Read(tree(t, map[string]string{"pnpm-workspace.yaml": yaml, "a/package.json": pkg("a", "1.0.0", nil)}))
		if !ok || len(ws.Members) != 0 || !hasCode(diags, CodeManifestInvalid) {
			t.Fatalf("%q: detected %v, members %v, diagnostics %v", yaml, ok, ws.Members, diags)
		}
	}
	// Invalid patterns are left out, the valid one still reads.
	ws, ok, diags := Read(tree(t, map[string]string{
		"package.json":   `{"workspaces":["../outside","/abs","a\\b","[","","a"]}`,
		"a/package.json": pkg("a", "1.0.0", nil),
	}))
	if !ok || !reflect.DeepEqual(ws.Members, []string{"a"}) || len(diags) != 5 || !hasCode(diags, CodePatternInvalid) {
		t.Fatalf("members %v, diagnostics %v", ws.Members, diags)
	}
	// A member whose package.json is not JSON is left out, and the probe still answers.
	root := tree(t, map[string]string{
		"package.json":       `{"workspaces":["p/*"]}`,
		"p/bad/package.json": "{",
		"p/ok/package.json":  pkg("ok", "1.0.0", map[string]string{"bad": "*"}),
	})
	result, err := Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: Extension, Paths: []string{"p/bad", "p/ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 || result.Projects[0].Path != "p/ok" || len(result.Projects[0].Dependencies) != 0 || !hasCode(result.Diagnostics, CodeManifestInvalid) {
		t.Fatalf("result %+v", result)
	}
	// Two members with one name: the first directory keeps it, with a diagnostic.
	projects, result := func() (map[string]wsproto.ProbeProject, wsproto.ProbeResult) {
		return detect(t, tree(t, map[string]string{
			"package.json":       `{"workspaces":["p/*"]}`,
			"p/a/package.json":   pkg("shared", "1.0.0", nil),
			"p/b/package.json":   pkg("shared", "1.0.0", nil),
			"p/app/package.json": pkg("app", "1.0.0", map[string]string{"shared": "workspace:*"}),
		}))
	}()
	if !reflect.DeepEqual(projects["p/app"].Dependencies, []string{"p/a"}) || !hasCode(result.Diagnostics, CodeManifestInvalid) {
		t.Fatalf("app depends on %v, diagnostics %v", projects["p/app"].Dependencies, result.Diagnostics)
	}
	// Outside a workspace the probe answers nothing, with no error.
	result, err = Probe(t.TempDir(), wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: Extension, Paths: []string{"a"}})
	if err != nil || len(result.Projects) != 0 {
		t.Fatalf("result %+v, err %v", result, err)
	}
}

func TestABoundedWalkIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "malformed-manifests", "a-bounded-walk-is-a-diagnostic")
	saved := maxWalkEntries
	t.Cleanup(func() { maxWalkEntries = saved })
	maxWalkEntries = 3
	ws, ok, diags := Read(tree(t, map[string]string{
		"package.json":     `{"workspaces":["p/**"]}`,
		"p/a/package.json": pkg("a", "1.0.0", nil),
		"p/b/package.json": pkg("b", "1.0.0", nil),
		"p/c/package.json": pkg("c", "1.0.0", nil),
		"p/d/package.json": pkg("d", "1.0.0", nil),
	}))
	if !ok || len(ws.Members) >= 4 || !hasCode(diags, CodeWalkBounded) {
		t.Fatalf("members %v, diagnostics %v", ws.Members, diags)
	}
}

func decodeMetadata(raw json.RawMessage) (Metadata, error) {
	var m Metadata
	err := json.Unmarshal(raw, &m)
	return m, err
}
