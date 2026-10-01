package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
)

func writeProbeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// probeFixture is a workspace with a root manifest, two packages linked by a
// `workspace:` dependency, a Go-only directory, and a package that declares its
// metadata through the `putnami` section.
func probeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "package.json"),
		`{"name":"repo","private":true,"workspaces":["apps/web","libs/core"]}`)
	writeProbeFile(t, filepath.Join(root, "apps", "web", "package.json"), `{
	  "name": "@acme/web",
	  "main": "src/index.ts",
	  "bin": {"web": "./bin/web.js"},
	  "dependencies": {"@acme/core": "workspace:*", "react": "^19.0.0"},
	  "devDependencies": {"@putnami/typescript": "workspace:*", "typescript": "^5.9.3"}
	}`)
	writeProbeFile(t, filepath.Join(root, "apps", "web", "src", "index.ts"),
		"import { name } from '@acme/core';\n\nexport const app = name;\n")
	writeProbeFile(t, filepath.Join(root, "apps", "web", "putnami.json"),
		`{"options":{"package":{"npm":true},"publish":{"npm":true}}}`)
	writeProbeFile(t, filepath.Join(root, "libs", "core", "package.json"), `{
	  "name": "@acme/core",
	  "putnami": {"type": "library", "tags": ["ts", "lib"], "publish": ["npm"], "runsWith": ["db"]}
	}`)
	writeProbeFile(t, filepath.Join(root, "svc", "go.mod"), "module acme/svc\n\ngo 1.25\n")
	return root
}

func probeAll(t *testing.T, root string, paths ...string) wsproto.ProbeResult {
	t.Helper()
	result, err := probeTypeScriptWorkspace(root, wsproto.ProbeRequest{
		Version:   wsproto.ProbeProtocolVersion,
		Extension: tsExtensionName,
		Reason:    wsproto.ProbeReasonLoad,
		Paths:     paths,
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if diags := wsproto.ValidateProbeResult(&result); diag.HasErrors(diags) {
		t.Fatalf("probe answer does not conform: %v", diags)
	}
	return result
}

func projectAt(t *testing.T, result wsproto.ProbeResult, path string) wsproto.ProbeProject {
	t.Helper()
	for _, project := range result.Projects {
		if project.Path == path {
			return project
		}
	}
	t.Fatalf("no project reported at %q; got %+v", path, result.Projects)
	return wsproto.ProbeProject{}
}

func TestProbe_ReportsSourceIdentityAndMarker(t *testing.T) {
	root := probeFixture(t)
	result := probeAll(t, root, ".", "apps/web", "libs/core", "svc")

	web := projectAt(t, result, "apps/web")
	if web.SourceName != "@acme/web" {
		t.Errorf("sourceName = %q, want @acme/web", web.SourceName)
	}
	if web.SourceFile != "apps/web/package.json" {
		t.Errorf("sourceFile = %q, want the repo-relative marker", web.SourceFile)
	}
	if !slices.Contains(web.WatchedFiles, "apps/web/package.json") {
		t.Errorf("watchedFiles = %v, want the project's own manifest", web.WatchedFiles)
	}
	for _, rootFile := range typeScriptWorkspaceRootFiles {
		if !slices.Contains(web.WatchedFiles, rootFile) {
			t.Errorf("watchedFiles = %v, missing workspace-root invalidation input %q",
				web.WatchedFiles, rootFile)
		}
	}
}

// A directory with no package.json is not a TypeScript project. Claiming it
// would make the merged view assert an npm identity that does not exist.
func TestProbe_SkipsDirectoriesWithoutTheMarker(t *testing.T) {
	root := probeFixture(t)
	result := probeAll(t, root, "apps/web", "svc", "does/not/exist")
	for _, project := range result.Projects {
		if project.Path == "svc" || project.Path == "does/not/exist" {
			t.Fatalf("probe claimed a directory with no package.json: %+v", project)
		}
	}
}

// An npm workspace ROOT is not a member of its own workspaces array.
func TestProbe_SkipsTheWorkspaceRootManifest(t *testing.T) {
	root := probeFixture(t)
	result := probeAll(t, root, ".", "apps/web")
	for _, project := range result.Projects {
		if project.Path == wsproto.ProbeRootPath {
			t.Fatalf("probe claimed the workspace root as a project: %+v", project)
		}
	}
}

// REGRESSION: request paths are NORMALIZED before anything reads them, exactly
// as the Go and Python probes do. The request validator only tests that a path
// normalizes and throws the canonical value away, so a non-canonical spelling
// reaches the probe verbatim.
//
// "./" is the spelling that hurt: it joins to the ROOT package.json, but the
// workspaces guard compares against ProbeRootPath ("."), so the raw spelling
// walked past it — and both ServeProbe and core's ParseAndValidateProbeResult
// accept the result, so the workspace root was silently claimed as a TypeScript
// project carrying the root package's npm name.
func TestProbe_NormalizesTheRootPathSpelling(t *testing.T) {
	root := probeFixture(t)
	result := probeAll(t, root, "./", "apps/web")

	for _, project := range result.Projects {
		if project.Path == wsproto.ProbeRootPath || project.SourceName == "repo" {
			t.Fatalf("a non-canonical root spelling claimed the workspace root: %+v", project)
		}
	}
	if len(result.Projects) != 1 {
		t.Fatalf("projects = %+v, want only apps/web", result.Projects)
	}
}

// A padded spelling must resolve to the same project, not to nothing: request
// validation accepts it (NormalizeProbePath trims), so a probe that reads it raw
// makes a real project vanish with no diagnostic.
func TestProbe_NormalizesPaddedPaths(t *testing.T) {
	root := probeFixture(t)
	result := probeAll(t, root, " apps/web ", "./libs/core")

	web := projectAt(t, result, "apps/web")
	if web.SourceName != "@acme/web" {
		t.Errorf("sourceName = %q, want @acme/web", web.SourceName)
	}
	if web.SourceFile != "apps/web/package.json" {
		t.Errorf("sourceFile = %q, want the canonical marker path", web.SourceFile)
	}
	// The edge resolves too: dependency paths are keyed on the candidate paths,
	// so a spelling that never made it into the index would drop the edge.
	if !slices.Equal(web.Dependencies, []string{"libs/core"}) {
		t.Errorf("dependencies = %v, want [libs/core]", web.Dependencies)
	}
}

// Two spellings of one path are ONE candidate. ValidateProbeResult hard-fails
// the whole probe with duplicate-project when a result carries two entries at
// the same path, so a probe that answered per-spelling could take down the
// entire workspace load over a cosmetic difference in the request.
func TestProbe_DedupesCandidatesThatNormalizeToOnePath(t *testing.T) {
	root := t.TempDir()
	// No `workspaces` member, so the root manifest IS a project — which is what
	// makes both spellings answer and the duplicate reachable.
	writeProbeFile(t, filepath.Join(root, "package.json"), `{"name":"@acme/root"}`)

	result, err := probeTypeScriptWorkspace(root, wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: tsExtensionName,
		Paths: []string{".", "./"},
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if diags := wsproto.ValidateProbeResult(&result); diag.HasErrors(diags) {
		t.Fatalf("two spellings of one path failed the whole probe: %v", diags)
	}
	if len(result.Projects) != 1 {
		t.Fatalf("projects = %+v, want exactly one entry for the single path", result.Projects)
	}
}

// The contract carries dependency PATHS, not names: resolving `workspace:`
// specifiers is the part only this side can do, and keying the wire on paths is
// what lets core own canonical identity without learning npm's naming rules.
func TestProbe_ResolvesWorkspaceDependenciesToPaths(t *testing.T) {
	root := probeFixture(t)
	result := probeAll(t, root, "apps/web", "libs/core")

	web := projectAt(t, result, "apps/web")
	if !slices.Equal(web.Dependencies, []string{"libs/core"}) {
		t.Errorf("dependencies = %v, want [libs/core]", web.Dependencies)
	}
	// An external package contributes no edge: inventing a path for `react`
	// would make core resolve an edge to a directory that is not a project.
	for _, dep := range web.Dependencies {
		if strings.Contains(dep, "react") {
			t.Errorf("external dependency leaked into the edges: %v", web.Dependencies)
		}
	}
	if !slices.Equal(web.Extensions, []string{"@putnami/typescript"}) {
		t.Errorf("extensions = %v, want the workspace devDependencies", web.Extensions)
	}
}

// The answer attributes each reported edge: a `workspace:` dependency of the
// package is an import, and an edge a `putnami` block declares is not. Nothing
// else can tell them apart — both arrive on the same list — and a boundary
// enforced on a declaration is a boundary the build never respects.
func TestProbe_SeparatesImportedEdgesFromDeclaredOnes(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "package.json"),
		`{"name":"repo","private":true,"workspaces":["app","imported","declared"]}`)
	writeProbeFile(t, filepath.Join(root, "app", "package.json"), `{
	  "name": "@acme/app",
	  "dependencies": {"@acme/imported": "workspace:*"},
	  "putnami": {"dependencies": ["@acme/imported", "@acme/declared"]}
	}`)
	writeProbeFile(t, filepath.Join(root, "app", "src", "index.ts"),
		"import '@acme/imported';\n")
	writeProbeFile(t, filepath.Join(root, "imported", "package.json"), `{"name":"@acme/imported"}`)
	writeProbeFile(t, filepath.Join(root, "declared", "package.json"), `{"name":"@acme/declared"}`)

	app := projectAt(t, probeAll(t, root, "app", "imported", "declared"), "app")
	// The reported edge SET is unchanged: the putnami block still decides it.
	if !slices.Equal(app.Dependencies, []string{"declared", "imported"}) {
		t.Fatalf("dependencies = %v, want both edges the putnami block names", app.Dependencies)
	}
	if got := app.DependencySources["imported"]; got != wsproto.DependencySourcePackageJSON {
		t.Errorf("dependencySources[imported] = %q, want %q", got, wsproto.DependencySourcePackageJSON)
	}
	if got := app.DependencySources["declared"]; got != wsproto.DependencySourceDeclared {
		t.Errorf("dependencySources[declared] = %q, want %q", got, wsproto.DependencySourceDeclared)
	}
}

// Without a `putnami` block the reported edges ARE the package's imports.
func TestProbe_AttributesWorkspaceDependenciesToThePackageManifest(t *testing.T) {
	root := probeFixture(t)
	web := projectAt(t, probeAll(t, root, "apps/web", "libs/core"), "apps/web")

	if got := web.DependencySources["libs/core"]; got != wsproto.DependencySourcePackageJSON {
		t.Errorf("dependencySources[libs/core] = %q, want %q", got, wsproto.DependencySourcePackageJSON)
	}
	if len(web.DependencySources) != len(web.Dependencies) {
		t.Errorf("dependencySources = %v, want one entry per reported edge %v",
			web.DependencySources, web.Dependencies)
	}
}

// A `workspace:` dependency no source file imports is a DECLARATION, not an
// import: the package is linked and nothing reads it. The reported edge set is
// unchanged and only the attribution moves, which is what makes the
// declared-edge check derivable.
func TestProbe_AttributesAnUnimportedWorkspaceDependencyAsADeclaration(t *testing.T) {
	root := probeFixture(t)
	writeProbeFile(t, filepath.Join(root, "apps", "web", "src", "index.ts"),
		"export const app = 1;\n")

	web := projectAt(t, probeAll(t, root, "apps/web", "libs/core"), "apps/web")
	if !slices.Contains(web.Dependencies, "libs/core") {
		t.Fatalf("dependencies = %v, want the edge the manifest still states", web.Dependencies)
	}
	if got := web.DependencySources["libs/core"]; got != wsproto.DependencySourceDeclared {
		t.Errorf("dependencySources[libs/core] = %q, want %q", got, wsproto.DependencySourceDeclared)
	}
}

// A project that depends on @putnami/application gets a generated serve entry
// (.gen/src/serve.bundled.ts) that imports @putnami/application and
// @putnami/runtime. The scan skips .gen, so the probe credits those imports
// from the manifest: calling the runtime edge unused would advise a prune that
// breaks the generated entry under an isolated linker. A dependency nothing
// imports, in committed or generated sources, stays a declaration.
func TestProbe_CreditsTheImportsOfTheGeneratedServeEntry(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "package.json"),
		`{"name":"repo","private":true,"workspaces":["app","tool","bare","lib","application","runtime","ui"]}`)
	// Shaped like samples/01-hello-world, plus a dependency nothing imports.
	writeProbeFile(t, filepath.Join(root, "app", "package.json"), `{
	  "name": "@acme/app",
	  "main": "src/main.ts",
	  "dependencies": {"@putnami/application": "workspace:*", "@putnami/runtime": "workspace:*", "@putnami/ui": "workspace:*"}
	}`)
	writeProbeFile(t, filepath.Join(root, "app", "src", "main.ts"),
		"import { application } from '@putnami/application';\nexport const app = () => application();\n")
	// The hook runs for a devDependency too.
	writeProbeFile(t, filepath.Join(root, "tool", "package.json"), `{
	  "name": "@acme/tool",
	  "dependencies": {"@putnami/runtime": "workspace:*"},
	  "devDependencies": {"@putnami/application": "workspace:*"}
	}`)
	// No committed source at all: the generated entry still imports both.
	writeProbeFile(t, filepath.Join(root, "bare", "package.json"), `{
	  "name": "@acme/bare",
	  "dependencies": {"@putnami/application": "workspace:*", "@putnami/runtime": "workspace:*"}
	}`)
	// Without @putnami/application the build writes no serve entry.
	writeProbeFile(t, filepath.Join(root, "lib", "package.json"), `{
	  "name": "@acme/lib",
	  "dependencies": {"@putnami/runtime": "workspace:*"}
	}`)
	writeProbeFile(t, filepath.Join(root, "lib", "src", "index.ts"), "export const lib = 1;\n")
	writeProbeFile(t, filepath.Join(root, "application", "package.json"), `{"name":"@putnami/application"}`)
	writeProbeFile(t, filepath.Join(root, "runtime", "package.json"), `{"name":"@putnami/runtime"}`)
	writeProbeFile(t, filepath.Join(root, "ui", "package.json"), `{"name":"@putnami/ui"}`)

	result := probeAll(t, root, "app", "tool", "bare", "lib", "application", "runtime", "ui")
	for _, tc := range []struct {
		project, dependency string
		want                wsproto.DependencySource
	}{
		{"app", "application", wsproto.DependencySourcePackageJSON},
		{"app", "runtime", wsproto.DependencySourcePackageJSON},
		{"app", "ui", wsproto.DependencySourceDeclared},
		{"tool", "runtime", wsproto.DependencySourcePackageJSON},
		{"bare", "application", wsproto.DependencySourcePackageJSON},
		{"bare", "runtime", wsproto.DependencySourcePackageJSON},
		{"lib", "runtime", wsproto.DependencySourceDeclared},
	} {
		project := projectAt(t, result, tc.project)
		if !slices.Contains(project.Dependencies, tc.dependency) {
			t.Errorf("%s: dependencies = %v, want the edge to %s the manifest states", tc.project, project.Dependencies, tc.dependency)
			continue
		}
		if got := project.DependencySources[tc.dependency]; got != tc.want {
			t.Errorf("%s: dependencySources[%s] = %q, want %q", tc.project, tc.dependency, got, tc.want)
		}
	}
}

func TestProbe_CarriesThePutnamiSection(t *testing.T) {
	root := probeFixture(t)
	core := projectAt(t, probeAll(t, root, "libs/core"), "libs/core")

	if core.Type != "library" {
		t.Errorf("type = %q", core.Type)
	}
	if !slices.Equal(core.Tags, []string{"lib", "ts"}) {
		t.Errorf("tags = %v, want normalized (sorted, deduped)", core.Tags)
	}
	if !slices.Equal(core.Publish, []string{"npm"}) || !slices.Equal(core.RunsWith, []string{"db"}) {
		t.Errorf("publish=%v runsWith=%v", core.Publish, core.RunsWith)
	}
}

// The TypeScript-shaped project fields land under the provider's own metadata
// bucket, namespaced so no other provider can collide with them.
func TestProbe_CarriesTypeScriptMetadata(t *testing.T) {
	root := probeFixture(t)
	result := probeAll(t, root, "apps/web", "libs/core")
	web := projectAt(t, result, "apps/web")

	if len(web.Metadata) == 0 {
		t.Fatal("no provider metadata reported")
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(web.Metadata, &metadata); err != nil {
		t.Fatalf("metadata is not an object: %v", err)
	}
	if string(metadata["main"]) != `"src/index.ts"` {
		t.Errorf("metadata.main = %s", metadata["main"])
	}
	if _, ok := metadata["bin"]; !ok {
		t.Errorf("metadata = %v, want bin carried", metadata)
	}
	member, dependencies := npmMemberDeclaration(t, web.Metadata)
	if member.Coordinate != "@acme/web" || member.PackageStep != "npm" || member.PublishStep != "npm" || !slices.Equal(dependencies, []string{"@acme/core"}) {
		t.Fatalf("release member = %+v, dependencies = %v", member, dependencies)
	}
	// A package with no internal dependency still carries an authoritative empty
	// release-set projection, distinct from an old provider that did not answer.
	core := projectAt(t, result, "libs/core")
	member, dependencies = npmMemberDeclaration(t, core.Metadata)
	if member.Coordinate != "@acme/core" || member.PackageStep != "npm" || member.PublishStep != "npm" || len(dependencies) != 0 {
		t.Fatalf("empty release member = %+v, dependencies = %v", member, dependencies)
	}
}

// npmMemberDeclaration reads the one npm member a probed project declares. The
// probe now answers with a LIST of members — the release set keys a member by
// (ecosystem, coordinate), not by project — so the coordinate is part of what
// the probe has to get right.
func npmMemberDeclaration(t *testing.T, raw json.RawMessage) (releaseset.MemberDeclaration, []string) {
	t.Helper()
	metadata, found, err := releaseset.ProjectReleaseMetadata(map[string]json.RawMessage{tsExtensionName: raw})
	if err != nil || !found {
		t.Fatalf("release metadata = %+v, %v, %v", metadata, found, err)
	}
	for _, declaration := range metadata.Ecosystems {
		if declaration.Ecosystem == "npm" {
			return declaration, declaration.Dependencies
		}
	}
	t.Fatalf("no npm member in %+v", metadata)
	return releaseset.MemberDeclaration{}, nil
}

// A project produces as many release-set members as it produces artifacts: the
// npm package it publishes, and — when its putnami.json declares an image
// publication — the oci repository the packager writes. The oci profile belongs
// to the SDK; this extension declares `uses: ["oci"]` and contributes members
// to it.
func TestProbe_DeclaresNpmAndOciMembers(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "npm-ecosystem-profile", "probe-declares-npm-and-oci-members")

	root := probeFixture(t)
	writeProbeFile(t, filepath.Join(root, "apps", "web", "putnami.json"),
		`{"name":"@acme/web","options":{"package":{"npm":true,"docker":true},"publish":{"npm":true,"docker":true}}}`)

	result := probeAll(t, root, "apps/web", "libs/core")

	web := memberDeclarations(t, projectAt(t, result, "apps/web").Metadata)
	if len(web) != 2 {
		t.Fatalf("image project members = %+v, want one npm and one oci", web)
	}
	if web[0].Ecosystem != "npm" || web[0].Coordinate != "@acme/web" || web[0].PackageStep != "npm" || web[0].PublishStep != "npm" {
		t.Fatalf("npm member = %+v", web[0])
	}
	if web[1].Ecosystem != "oci" || web[1].Coordinate != "acme-web" || web[1].PackageStep != "docker" || web[1].PublishStep != "docker" {
		t.Fatalf("oci member = %+v", web[1])
	}

	// A project that publishes no image declares no oci member.
	core := memberDeclarations(t, projectAt(t, result, "libs/core").Metadata)
	if len(core) != 1 || core[0].Ecosystem != "npm" || core[0].PackageStep != "npm" || core[0].PublishStep != "npm" {
		t.Fatalf("library members = %+v, want the npm member alone", core)
	}
}

// A Docker-only project declares only its image. The image repository is the
// flattened project name, and an explicit putnami.json name outranks the
// package name exactly as it does for core.
func TestProbe_OciCoordinateFollowsTheProjectName(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "package.json"), `{"private":true,"workspaces":["site"]}`)
	writeProbeFile(t, filepath.Join(root, "site", "package.json"), `{"name":"@acme/site-web"}`)
	writeProbeFile(t, filepath.Join(root, "site", "putnami.json"),
		`{"name":"putnami.dev","options":{"package":{"docker":true},"publish":{"docker":true}}}`)

	members := memberDeclarations(t, projectAt(t, probeAll(t, root, "site"), "site").Metadata)
	if len(members) != 1 || members[0].Ecosystem != "oci" || members[0].Coordinate != "putnami.dev" || members[0].PackageStep != "docker" || members[0].PublishStep != "docker" {
		t.Fatalf("members = %+v, want only the configured oci coordinate", members)
	}
}

func TestProbe_OciCoordinateRetainsRegistryNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, override, want string
	}{
		{name: "workspace namespace", want: "team/acme-app"},
		{name: "project namespace replaces workspace", override: `,"registries":{"oci":{"publish":"ghcr.io/acme/images"}}`, want: "acme/images/acme-app"},
		{name: "project bare host replaces workspace", override: `,"registries":{"oci":{"publish":"ghcr.io"}}`, want: "acme-app"},
		{name: "other ecosystem preserves workspace", override: `,"registries":{"npm":{"publish":"https://npm.example.test"}}`, want: "team/acme-app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeProbeFile(t, filepath.Join(root, "putnami.workspace.json"), `{"registries":{"oci":{"publish":"oci.example.test/team"}}}`)
			writeProbeFile(t, filepath.Join(root, "app", "package.json"), `{"name":"@acme/app"}`)
			writeProbeFile(t, filepath.Join(root, "app", "putnami.json"), `{"publish":["docker"]`+tc.override+`}`)
			project := projectAt(t, probeAll(t, root, "app"), "app")
			members := memberDeclarations(t, project.Metadata)
			if len(members) != 1 || members[0].Ecosystem != "oci" || members[0].Coordinate != tc.want {
				t.Fatalf("members = %+v, want OCI coordinate %q", members, tc.want)
			}
			if !slices.Contains(project.WatchedFiles, "putnami.workspace.json") {
				t.Fatalf("watched files = %v, missing registry configuration", project.WatchedFiles)
			}
		})
	}
}

func TestProbe_PublishOptionsWithoutPackageOptionsKeepPublicationIntent(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "package.json"), `{"private":true,"workspaces":["app"]}`)
	writeProbeFile(t, filepath.Join(root, "app", "package.json"), `{"name":"@acme/app"}`)
	writeProbeFile(t, filepath.Join(root, "app", "putnami.json"),
		`{"options":{"publish":{"npm":true,"docker":true}}}`)

	members := memberDeclarations(t, projectAt(t, probeAll(t, root, "app"), "app").Metadata)
	if len(members) != 2 || members[0].Ecosystem != "npm" || members[1].Ecosystem != "oci" {
		t.Fatalf("members = %+v, want requested npm and oci artifacts for planning validation", members)
	}
}

func TestProbe_LegacyManifestDockerPublicationDeclaresOciMember(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "package.json"), `{"private":true,"workspaces":["app"]}`)
	writeProbeFile(t, filepath.Join(root, "app", "package.json"), `{
	  "name":"@acme/app",
	  "putnami":{"publish":["docker"]}
	}`)

	members := memberDeclarations(t, projectAt(t, probeAll(t, root, "app"), "app").Metadata)
	if len(members) != 1 || members[0].Ecosystem != "oci" || members[0].Coordinate != "acme-app" || members[0].PackageStep != "docker" || members[0].PublishStep != "docker" {
		t.Fatalf("members = %+v, want the legacy Docker publication's oci member", members)
	}
}

func TestProbe_NamedPackageWithoutPublicationDeclaresNoMember(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "package.json"), `{"private":true,"workspaces":["internal"]}`)
	writeProbeFile(t, filepath.Join(root, "internal", "package.json"), `{"name":"@acme/internal"}`)

	members := memberDeclarations(t, projectAt(t, probeAll(t, root, "internal"), "internal").Metadata)
	if len(members) != 0 {
		t.Fatalf("members = %+v, want no publication for a merely named package", members)
	}
}

// The oci member is a function of putnami.json, so a change to that file has to
// re-probe the project. Declaring it is what makes the answer honest about its
// own inputs.
func TestProbe_WatchesTheProjectDocument(t *testing.T) {
	root := probeFixture(t)
	web := projectAt(t, probeAll(t, root, "apps/web"), "apps/web")
	if !slices.Contains(web.WatchedFiles, "apps/web/putnami.json") {
		t.Fatalf("watched files = %v, want the project document declared", web.WatchedFiles)
	}
}

// memberDeclarations reads every member a probed project declares, in the order
// the probe wrote them.
func memberDeclarations(t *testing.T, raw json.RawMessage) []releaseset.MemberDeclaration {
	t.Helper()
	metadata, found, err := releaseset.ProjectReleaseMetadata(map[string]json.RawMessage{tsExtensionName: raw})
	if err != nil || !found {
		t.Fatalf("release metadata = %+v, %v, %v", metadata, found, err)
	}
	return metadata.Ecosystems
}

func TestProbeReleaseSetMetadataIncludesEveryPublishedInternalDependencyMap(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "package.json"), `{"private":true,"workspaces":["app","dep-a","dep-b","dep-c"]}`)
	writeProbeFile(t, filepath.Join(root, "app", "package.json"), `{
	  "name":"app",
	  "dependencies":{"dep-a":"1.0.0","external":"^1"},
	  "peerDependencies":{"dep-b":"workspace:^"},
	  "optionalDependencies":{"dep-c":"catalog:"},
	  "devDependencies":{"dev-only":"workspace:*"},
	  "putnami":{"dependencies":["dep-a"],"publish":["npm"]}
	}`)
	for _, name := range []string{"dep-a", "dep-b", "dep-c", "dev-only"} {
		writeProbeFile(t, filepath.Join(root, name, "package.json"), `{"name":"`+name+`"}`)
	}
	result := probeAll(t, root, "app", "dep-a", "dep-b", "dep-c", "dev-only")
	app := projectAt(t, result, "app")
	_, dependencies := npmMemberDeclaration(t, app.Metadata)
	if !slices.Equal(dependencies, []string{"dep-a", "dep-b", "dep-c"}) {
		t.Fatalf("release dependencies = %v", dependencies)
	}
	if !slices.Equal(app.Dependencies, []string{"dep-a"}) {
		t.Fatalf("build dependencies = %v, want authored putnami override only", app.Dependencies)
	}
}

// A malformed manifest is REPORTED and skipped. Guessing would invent identity;
// dropping it silently is the silent-project-loss failure where a project vanishes with no
// diagnostic.
func TestProbe_MalformedManifestWarnsAndSkips(t *testing.T) {
	root := probeFixture(t)
	writeProbeFile(t, filepath.Join(root, "libs", "broken", "package.json"), `{"name": `)

	result, err := probeTypeScriptWorkspace(root, wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: tsExtensionName,
		Paths: []string{"apps/web", "libs/broken"},
	})
	if err != nil {
		t.Fatalf("a malformed manifest must not fail the whole probe: %v", err)
	}
	for _, project := range result.Projects {
		if project.Path == "libs/broken" {
			t.Fatal("a manifest that could not be parsed was reported as a project")
		}
	}
	if len(result.Diagnostics) == 0 {
		t.Fatal("a malformed manifest produced no diagnostic")
	}
	if !strings.Contains(result.Diagnostics[0].Field, "libs/broken/package.json") {
		t.Errorf("diagnostic = %+v, want it to name the file", result.Diagnostics[0])
	}
}

// The answer's digest keys core's snapshot, so a byte that varies between two
// runs over one tree is a cache-correctness bug. Authoring order of the
// candidate list must not move it either.
func TestProbe_IsDeterministicAcrossRunsAndRequestOrder(t *testing.T) {
	root := probeFixture(t)
	render := func(paths []string) string {
		result, err := probeTypeScriptWorkspace(root, wsproto.ProbeRequest{
			Version: wsproto.ProbeProtocolVersion, Extension: tsExtensionName, Paths: paths,
		})
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}

	forward := render([]string{".", "apps/web", "libs/core", "svc"})
	reversed := render([]string{"svc", "libs/core", "apps/web", "."})
	if forward != reversed {
		t.Fatalf("candidate order changed the answer:\n%s\n%s", forward, reversed)
	}
	if again := render([]string{".", "apps/web", "libs/core", "svc"}); again != forward {
		t.Fatalf("two runs over one tree disagree:\n%s\n%s", forward, again)
	}

	// The digest must agree too — that is what actually reaches the snapshot.
	first, _ := probeTypeScriptWorkspace(root, wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: tsExtensionName, Paths: []string{"apps/web", "libs/core"},
	})
	second, _ := probeTypeScriptWorkspace(root, wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: tsExtensionName, Paths: []string{"libs/core", "apps/web"},
	})
	if wsproto.ProbeResultDigest(first) != wsproto.ProbeResultDigest(second) {
		t.Fatal("probe result digest depends on the request order")
	}
}

// The reserved control call is what core actually spawns; it must be handled
// before ordinary subcommand dispatch and must decline everything else.
func TestHandleWorkspaceProbe_ServesTheReservedInvocation(t *testing.T) {
	root := probeFixture(t)
	t.Chdir(root)

	var request bytes.Buffer
	if err := wsproto.EncodeProbeRequest(&request, wsproto.ProbeRequest{
		Extension: tsExtensionName, Paths: []string{"apps/web", "libs/core"},
	}); err != nil {
		t.Fatal(err)
	}
	stdin, stdout := redirectStd(t, request.Bytes())

	handled, err := handleWorkspaceProbe(wsproto.ProbeControlArgs())
	if !handled || err != nil {
		t.Fatalf("handleWorkspaceProbe = (%v, %v)", handled, err)
	}
	_ = stdin

	result, diags := wsproto.ParseAndValidateProbeResult(readAll(t, stdout))
	if result == nil || diag.HasErrors(diags) {
		t.Fatalf("stdout does not carry a conformant result: %v", diags)
	}
	if result.Extension != tsExtensionName || len(result.Projects) != 2 {
		t.Fatalf("result = %+v", result)
	}
}

func TestHandleWorkspaceProbe_DeclinesOrdinarySubcommands(t *testing.T) {
	handled, err := handleWorkspaceProbe([]string{"build"})
	if handled || err != nil {
		t.Fatalf("handleWorkspaceProbe(build) = (%v, %v), want (false, nil)", handled, err)
	}
}

// redirectStd points os.Stdin at the given payload and os.Stdout at a temp
// file, restoring both when the test ends.
func redirectStd(t *testing.T, stdin []byte) (*os.File, *os.File) {
	t.Helper()
	dir := t.TempDir()

	inPath := filepath.Join(dir, "stdin")
	if err := os.WriteFile(inPath, stdin, 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(inPath) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}

	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = in, out
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldIn, oldOut
		in.Close()  //nolint:errcheck // test cleanup
		out.Close() //nolint:errcheck // test cleanup
	})
	return in, out
}

func readAll(t *testing.T, f *os.File) []byte {
	t.Helper()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return data
}
