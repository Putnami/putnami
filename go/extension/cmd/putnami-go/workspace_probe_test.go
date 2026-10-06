package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/releaseplan"
	"go.putnami.dev/go/extension/internal/toolchain"
	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"

	"go.putnami.dev/protocol/features/spectest"
)

func TestProbeSeparatesPublishedRequiresFromReplaceOnlyImpactEdges(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "app", "go.mod"), "module example.com/app\n\nrequire example.com/required v1.0.0\nreplace example.com/cache-only => ../cache-only\n")
	writeProbeFile(t, filepath.Join(root, "app", "putnami.json"), `{"publish":["go"]}`)
	writeProbeFile(t, filepath.Join(root, "required", "go.mod"), "module example.com/required\n")
	writeProbeFile(t, filepath.Join(root, "cache-only", "go.mod"), "module example.com/cache-only\n")

	result, err := probeGoWorkspace(root, wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: goExtensionName,
		Paths: []string{"app", "required", "cache-only"},
	})
	if err != nil {
		t.Fatal(err)
	}
	app := projectAt(t, result, "app")
	if !slices.Equal(app.Dependencies, []string{"cache-only", "required"}) {
		t.Fatalf("impact dependencies = %v", app.Dependencies)
	}
	metadata, found, err := releaseset.ProjectReleaseMetadata(
		map[string]json.RawMessage{goExtensionName: app.Metadata})
	if err != nil || !found || len(metadata.Ecosystems) != 1 {
		t.Fatalf("release metadata = %+v, %v, %v", metadata, found, err)
	}
	// The coordinate is the MODULE PATH: a release-set member is keyed by
	// (ecosystem, coordinate), and a Go consumer resolves the module path.
	member := metadata.Ecosystems[0]
	if member.Ecosystem != releaseplan.GoEcosystem || member.Coordinate != "example.com/app" || member.PackageStep != "go" || member.PublishStep != "go" {
		t.Fatalf("release member = %+v", member)
	}
	if !slices.Equal(member.Dependencies, []string{"example.com/required"}) {
		t.Fatalf("release dependencies = %v", member.Dependencies)
	}
}

// A sibling module that only `_test.go` files import is still a published
// requirement: `go mod tidy` requires it, the staged go.mod carries it, and
// the package job refuses any internal requirement the plan does not list. The
// probe therefore derives the member's dependencies from every internal require
// in go.mod — it does not, and cannot purely, read the sources to decide which
// requires are test-only. Such an import committed without its require line
// makes `package~go` refuse the required module at publish time.
func TestProbe_TestOnlyWorkspaceRequireIsAReleaseSetDependency(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "tidy-drift-visibility", "a-test-only-workspace-require-is-a-release-set-dependency")
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "openapi", "go.mod"),
		"module example.com/openapi\n\ngo 1.25\n\n"+
			"require (\n\texample.com/app v0.0.1\n\texample.com/security v0.0.1\n)\n\n"+
			"replace (\n\texample.com/app => ../app\n\texample.com/security => ../security\n)\n")
	writeProbeFile(t, filepath.Join(root, "openapi", "openapi.go"), "package openapi\n\nimport _ \"example.com/app\"\n")
	writeProbeFile(t, filepath.Join(root, "openapi", "openapi_test.go"), "package openapi\n\nimport _ \"example.com/security\"\n")
	writeProbeFile(t, filepath.Join(root, "openapi", "putnami.json"), `{"publish":["go"]}`)
	writeProbeFile(t, filepath.Join(root, "app", "go.mod"), "module example.com/app\n")
	writeProbeFile(t, filepath.Join(root, "security", "go.mod"), "module example.com/security\n")

	result := probeAll(t, root, "openapi", "app", "security")
	openapi := projectAt(t, result, "openapi")
	if !slices.Equal(openapi.Dependencies, []string{"app", "security"}) {
		t.Fatalf("impact dependencies = %v", openapi.Dependencies)
	}
	metadata, found, err := releaseset.ProjectReleaseMetadata(
		map[string]json.RawMessage{goExtensionName: openapi.Metadata})
	if err != nil || !found || len(metadata.Ecosystems) != 1 {
		t.Fatalf("release metadata = %+v, %v, %v", metadata, found, err)
	}
	if got := metadata.Ecosystems[0].Dependencies; !slices.Equal(got, []string{"example.com/app", "example.com/security"}) {
		t.Fatalf("release dependencies = %v, want the test-only require too", got)
	}
}

func writeProbeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// probeFixture is a workspace with two linked Go modules, a module the first
// reaches only through a local replace, a TypeScript-only directory, and a
// module nested three levels down.
func probeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "go.work"), "go 1.25\n\nuse (\n\t./apps/svc\n\t./libs/core\n)\n")
	writeProbeFile(t, filepath.Join(root, "apps", "svc", "go.mod"),
		"module example.com/svc\n\ngo 1.25\n\n"+
			"require (\n\texample.com/core v0.0.0\n\tgithub.com/google/uuid v1.6.0 // indirect\n)\n\n"+
			"replace example.com/core => ../../libs/core\n")
	writeProbeFile(t, filepath.Join(root, "libs", "core", "go.mod"), "module example.com/core\n\ngo 1.25\n")
	writeProbeFile(t, filepath.Join(root, "apps", "svc", "main.go"),
		"package main\n\nimport \"example.com/core\"\n\nfunc main() { _ = core.Name }\n")
	writeProbeFile(t, filepath.Join(root, "apps", "svc", "putnami.json"), `{"publish":["go"]}`)
	writeProbeFile(t, filepath.Join(root, "libs", "core", "putnami.json"), `{"publish":["go"]}`)
	writeProbeFile(t, filepath.Join(root, "web", "package.json"), `{"name":"@acme/web"}`)
	return root
}

func probeAll(t *testing.T, root string, paths ...string) wsproto.ProbeResult {
	t.Helper()
	result, err := probeGoWorkspace(root, wsproto.ProbeRequest{
		Version:   wsproto.ProbeProtocolVersion,
		Extension: goExtensionName,
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

func TestProbe_ReportsModuleIdentityAndMarker(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "probe-graph-fidelity", "a-project-is-identified-by-its-module-path")
	root := probeFixture(t)
	result := probeAll(t, root, ".", "apps/svc", "libs/core", "web")

	svc := projectAt(t, result, "apps/svc")
	if svc.SourceName != "example.com/svc" {
		t.Errorf("sourceName = %q, want the go.mod module path", svc.SourceName)
	}
	if svc.SourceFile != "apps/svc/go.mod" {
		t.Errorf("sourceFile = %q, want the repo-relative marker", svc.SourceFile)
	}
	if !slices.Contains(svc.WatchedFiles, "apps/svc/go.mod") {
		t.Errorf("watchedFiles = %v, want the project's own go.mod", svc.WatchedFiles)
	}
	for _, rootFile := range goWorkspaceRootFiles {
		if !slices.Contains(svc.WatchedFiles, rootFile) {
			t.Errorf("watchedFiles = %v, missing workspace-root invalidation input %q",
				svc.WatchedFiles, rootFile)
		}
	}
}

// A directory with no go.mod is not a Go project. Claiming it would make the
// merged view assert a module identity that does not exist — and, for a
// directory that a DIFFERENT provider owns, would turn a merge into a conflict.
func TestProbe_SkipsDirectoriesWithoutTheMarker(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "probe-graph-fidelity", "a-directory-without-the-marker-is-not-a-project")
	root := probeFixture(t)
	result := probeAll(t, root, ".", "apps/svc", "web", "does/not/exist")
	for _, project := range result.Projects {
		if project.Path == "web" || project.Path == "does/not/exist" || project.Path == wsproto.ProbeRootPath {
			t.Fatalf("probe claimed a directory with no go.mod: %+v", project)
		}
	}
}

// The contract carries dependency PATHS, not module paths: resolving the module
// graph is the part only this side can do, and keying the wire on paths is what
// lets core own canonical identity without learning Go's module rules.
func TestProbe_ResolvesModuleGraphToPaths(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "probe-graph-fidelity", "dependency-edges-are-repo-relative-paths-from-require-and-replace")
	root := probeFixture(t)
	svc := projectAt(t, probeAll(t, root, "apps/svc", "libs/core"), "apps/svc")

	if !slices.Equal(svc.Dependencies, []string{"libs/core"}) {
		t.Errorf("dependencies = %v, want [libs/core]", svc.Dependencies)
	}
	// An external module contributes no edge: inventing a path for
	// github.com/google/uuid would make core resolve an edge to a directory
	// that is not a project.
	for _, dep := range svc.Dependencies {
		if strings.Contains(dep, "uuid") {
			t.Errorf("external module leaked into the edges: %v", svc.Dependencies)
		}
	}
}

// An edge this provider reports is attributed to the module graph when a source
// file of the module IMPORTS the target. A consumer that must act on real
// imports — the visibility check, the declared-edge check — reads that
// attribution instead of guessing from which provider answered.
func TestProbe_AttributesEveryEdgeToTheModuleGraph(t *testing.T) {
	root := probeFixture(t)
	svc := projectAt(t, probeAll(t, root, "apps/svc", "libs/core"), "apps/svc")

	if len(svc.DependencySources) != len(svc.Dependencies) {
		t.Fatalf("dependencySources = %v, want one entry per reported edge %v",
			svc.DependencySources, svc.Dependencies)
	}
	for _, dependency := range svc.Dependencies {
		if got := svc.DependencySources[dependency]; got != wsproto.DependencySourceGoModule {
			t.Errorf("dependencySources[%q] = %q, want %q", dependency, got, wsproto.DependencySourceGoModule)
		}
	}
	core := projectAt(t, probeAll(t, root, "apps/svc", "libs/core"), "libs/core")
	if len(core.DependencySources) != 0 {
		t.Errorf("dependencySources = %v, want none for a project with no edge", core.DependencySources)
	}
}

// A require of a workspace module no source file imports is a DECLARATION, not
// an import: the requirement is available to the build and nothing reads it.
// The edge set is unchanged — the require is still an edge — and only its
// attribution moves, which is what makes the declared-edge check derivable.
func TestProbe_AttributesAnUnimportedRequireAsADeclaration(t *testing.T) {
	root := probeFixture(t)
	writeProbeFile(t, filepath.Join(root, "apps", "svc", "main.go"),
		"package main\n\nfunc main() {}\n")

	svc := projectAt(t, probeAll(t, root, "apps/svc", "libs/core"), "apps/svc")
	if !slices.Equal(svc.Dependencies, []string{"libs/core"}) {
		t.Fatalf("dependencies = %v, want the edge the require still states", svc.Dependencies)
	}
	if got := svc.DependencySources["libs/core"]; got != wsproto.DependencySourceDeclared {
		t.Errorf("dependencySources[libs/core] = %q, want %q", got, wsproto.DependencySourceDeclared)
	}
}

// A require of a workspace module whose sources import only a module nested
// inside it is a DECLARATION: the nested module provides those packages, so
// the parent the go.mod requires is not read by the build.
func TestProbe_AttributesARequireOfAParentOfAnImportedNestedModuleAsADeclaration(t *testing.T) {
	root := probeFixture(t)
	writeProbeFile(t, filepath.Join(root, "libs", "core", "client", "go.mod"), "module example.com/core/client\n\ngo 1.25\n")
	writeProbeFile(t, filepath.Join(root, "apps", "svc", "main.go"),
		"package main\n\nimport \"example.com/core/client\"\n\nfunc main() { _ = client.Hello }\n")

	svc := projectAt(t, probeAll(t, root, "apps/svc", "libs/core", "libs/core/client"), "apps/svc")
	if !slices.Equal(svc.Dependencies, []string{"libs/core"}) {
		t.Fatalf("dependencies = %v, want the edge the require still states", svc.Dependencies)
	}
	if got := svc.DependencySources["libs/core"]; got != wsproto.DependencySourceDeclared {
		t.Errorf("dependencySources[libs/core] = %q, want %q", got, wsproto.DependencySourceDeclared)
	}
}

// A test file counts exactly like a non-test file: a requirement a `_test.go`
// alone imports is one `go mod tidy` keeps, so it is an import here too.
func TestProbe_AttributesARequireATestFileAloneImports(t *testing.T) {
	root := probeFixture(t)
	writeProbeFile(t, filepath.Join(root, "apps", "svc", "main.go"),
		"package main\n\nfunc main() {}\n")
	writeProbeFile(t, filepath.Join(root, "apps", "svc", "main_test.go"),
		"package main\n\nimport \"example.com/core\"\n\nvar _ = core.Name\n")

	svc := projectAt(t, probeAll(t, root, "apps/svc", "libs/core"), "apps/svc")
	if got := svc.DependencySources["libs/core"]; got != wsproto.DependencySourceGoModule {
		t.Errorf("dependencySources[libs/core] = %q, want %q", got, wsproto.DependencySourceGoModule)
	}
}

// A go.mod with no module line cannot identify a project. It is REPORTED and
// skipped: guessing would invent identity, and dropping it silently would make
// a project vanish with no diagnostic.
func TestProbe_ModuleLessManifestWarnsAndSkips(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "probe-graph-fidelity", "a-manifest-with-no-module-warns-and-is-skipped")
	root := probeFixture(t)
	writeProbeFile(t, filepath.Join(root, "libs", "broken", "go.mod"), "go 1.25\n\nrequire example.com/core v0.0.0\n")

	result, err := probeGoWorkspace(root, wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: goExtensionName,
		Paths: []string{"apps/svc", "libs/broken"},
	})
	if err != nil {
		t.Fatalf("a malformed go.mod must not fail the whole probe: %v", err)
	}
	for _, project := range result.Projects {
		if project.Path == "libs/broken" {
			t.Fatal("a go.mod with no module line was reported as a project")
		}
	}
	if len(result.Diagnostics) == 0 {
		t.Fatal("a malformed go.mod produced no diagnostic")
	}
	if !strings.Contains(result.Diagnostics[0].Field, "libs/broken/go.mod") {
		t.Errorf("diagnostic = %+v, want it to name the file", result.Diagnostics[0])
	}
}

// The answer's digest keys core's snapshot, so a byte that varies between two
// runs over one tree is a cache-correctness bug. Authoring order of the
// candidate list must not move it either.
func TestProbe_IsDeterministicAcrossRunsAndRequestOrder(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "probe-purity", "the-answer-is-identical-across-repeated-runs-and-request-orderings")
	root := probeFixture(t)
	render := func(paths []string) string {
		result, err := probeGoWorkspace(root, wsproto.ProbeRequest{
			Version: wsproto.ProbeProtocolVersion, Extension: goExtensionName, Paths: paths,
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

	forward := render([]string{".", "apps/svc", "libs/core", "web"})
	reversed := render([]string{"web", "libs/core", "apps/svc", "."})
	if forward != reversed {
		t.Fatalf("candidate order changed the answer:\n%s\n%s", forward, reversed)
	}
	if again := render([]string{".", "apps/svc", "libs/core", "web"}); again != forward {
		t.Fatalf("two runs over one tree disagree:\n%s\n%s", forward, again)
	}

	first, _ := probeGoWorkspace(root, wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: goExtensionName,
		Paths: []string{"apps/svc", "libs/core"},
	})
	second, _ := probeGoWorkspace(root, wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: goExtensionName,
		Paths: []string{"libs/core", "apps/svc"},
	})
	if wsproto.ProbeResultDigest(first) != wsproto.ProbeResultDigest(second) {
		t.Fatal("probe result digest depends on the request order")
	}
}

// The Reason is advisory: a provider may use it to pick a cheaper strategy, but
// it must return the same facts for the same tree regardless, and it is never
// part of the digest.
func TestProbe_AnswersIdenticallyForEveryReason(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "probe-purity", "the-answer-is-identical-whatever-reason-it-was-asked-for")
	root := probeFixture(t)
	paths := []string{".", "apps/svc", "libs/core", "web"}

	var baseline string
	for _, reason := range []wsproto.ProbeReason{
		wsproto.ProbeReasonLoad, wsproto.ProbeReasonRefresh, wsproto.ProbeReasonPlan,
	} {
		result, err := probeGoWorkspace(root, wsproto.ProbeRequest{
			Version: wsproto.ProbeProtocolVersion, Extension: goExtensionName,
			Reason: reason, Paths: paths,
		})
		if err != nil {
			t.Fatalf("probe(%s): %v", reason, err)
		}
		digest := wsproto.ProbeResultDigest(result)
		if baseline == "" {
			baseline = digest
			continue
		}
		if digest != baseline {
			t.Fatalf("probe answer depends on Reason %q", reason)
		}
	}
}

// Duplicate module paths in one workspace are pathological, but the tiebreak
// must still be deterministic: candidates are walked in SORTED order, so the
// winner never depends on map iteration or on the order core happened to list
// the directories in.
func TestProbe_DuplicateModulePathsResolveDeterministically(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "probe-purity", "duplicate-module-paths-resolve-deterministically")
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "a", "go.mod"), "module example.com/dup\n\ngo 1.25\n")
	writeProbeFile(t, filepath.Join(root, "b", "go.mod"), "module example.com/dup\n\ngo 1.25\n")
	writeProbeFile(t, filepath.Join(root, "app", "go.mod"),
		"module example.com/app\n\ngo 1.25\n\nrequire example.com/dup v0.0.0\n")

	for _, order := range [][]string{{"a", "app", "b"}, {"b", "app", "a"}, {"app", "b", "a"}} {
		app := projectAt(t, probeAll(t, root, order...), "app")
		if !slices.Equal(app.Dependencies, []string{"a"}) {
			t.Fatalf("candidate order %v resolved the duplicate module to %v, want [a]", order, app.Dependencies)
		}
	}
}

// --- classification ---------------------------

// classificationFixture is a workspace holding the three shapes classification
// has to tell apart: a module that is only importable, a module that is only an
// entrypoint, and a module that is both.
func classificationFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	writeProbeFile(t, filepath.Join(root, "libs", "core", "go.mod"), "module example.com/core\n\ngo 1.25\n")
	writeProbeFile(t, filepath.Join(root, "libs", "core", "core.go"), "package core\n")
	writeProbeFile(t, filepath.Join(root, "libs", "core", "internal", "detail", "detail.go"), "package detail\n")

	writeProbeFile(t, filepath.Join(root, "apps", "cli", "go.mod"), "module example.com/cli\n\ngo 1.25\n")
	writeProbeFile(t, filepath.Join(root, "apps", "cli", "main.go"), "package main\n\nfunc main() {}\n")

	// Mixed: an importable API at the module root plus a command below it. This
	// is the layout most workloads in this repository actually use, and it is
	// the one a root-only scan gets wrong.
	writeProbeFile(t, filepath.Join(root, "apps", "svc", "go.mod"), "module example.com/svc\n\ngo 1.25\n")
	writeProbeFile(t, filepath.Join(root, "apps", "svc", "svc.go"), "package svc\n")
	writeProbeFile(t, filepath.Join(root, "apps", "svc", "cmd", "svc", "main.go"), "package main\n\nfunc main() {}\n")

	return root
}

// The classification rule: a module that contains a package main can produce an
// executable and is an application; a module that contains none can only be
// imported and is a library. Leaving Type empty was not neutral — core reads
// unclassified as "application", so every Go library planned serve and run it
// has no entrypoint for and joined infra aggregation it contributes nothing to.
func TestProbe_ClassifiesModulesByTheirMainPackage(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "probe-graph-fidelity", "a-project-is-classified-by-its-main-package")
	root := classificationFixture(t)
	result := probeAll(t, root, "libs/core", "apps/cli", "apps/svc")

	for path, want := range map[string]string{
		"libs/core": goTypeLibrary,
		"apps/cli":  goTypeApplication,
		"apps/svc":  goTypeApplication,
	} {
		if got := projectAt(t, result, path).Type; got != want {
			t.Errorf("%s type = %q, want %q", path, got, want)
		}
	}
	if diags := result.Diagnostics; len(diags) != 0 {
		t.Errorf("classification produced diagnostics over a well-formed tree: %v", diags)
	}
}

// A package main that the build never links belongs to somebody else, and each
// of these would turn a library into an application that plans serve, run, and
// a platform compile it can never satisfy.
func TestProbe_ClassificationIgnoresMainTheBuildNeverLinks(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "probe-graph-fidelity", "a-main-the-build-never-links-does-not-classify-the-project")
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "libs", "core", "go.mod"), "module example.com/core\n\ngo 1.25\n")
	writeProbeFile(t, filepath.Join(root, "libs", "core", "core.go"), "package core\n")
	// A generator script, guarded the conventional way.
	writeProbeFile(t, filepath.Join(root, "libs", "core", "gen.go"), "//go:build ignore\n\npackage main\n\nfunc main() {}\n")
	// A fixture tree the go tool never compiles.
	writeProbeFile(t, filepath.Join(root, "libs", "core", "testdata", "prog", "main.go"), "package main\n\nfunc main() {}\n")
	// A nested module: its binary is that module's, not this one's.
	writeProbeFile(t, filepath.Join(root, "libs", "core", "tool", "go.mod"), "module example.com/core/tool\n\ngo 1.25\n")
	writeProbeFile(t, filepath.Join(root, "libs", "core", "tool", "main.go"), "package main\n\nfunc main() {}\n")

	result := probeAll(t, root, "libs/core", "libs/core/tool")
	if got := projectAt(t, result, "libs/core").Type; got != goTypeLibrary {
		t.Errorf("libs/core type = %q, want %q: none of its main packages is linked by its own build", got, goTypeLibrary)
	}
	// The nested module owns that main and is classified on its own terms.
	if got := projectAt(t, result, "libs/core/tool").Type; got != goTypeApplication {
		t.Errorf("libs/core/tool type = %q, want %q", got, goTypeApplication)
	}
}

// Classification must not read GOOS: one tree has to classify identically on
// every host, because the answer's digest keys core's workspace snapshot.
// A windows-only command is still a command.
func TestProbe_ClassificationDoesNotDependOnTheProbingHost(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "probe-purity", "the-answer-does-not-depend-on-the-probing-host")
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "apps", "cli", "go.mod"), "module example.com/cli\n\ngo 1.25\n")
	writeProbeFile(t, filepath.Join(root, "apps", "cli", "main_windows.go"),
		"//go:build windows\n\npackage main\n\nfunc main() {}\n")

	if got := projectAt(t, probeAll(t, root, "apps/cli"), "apps/cli").Type; got != goTypeApplication {
		t.Errorf("apps/cli type = %q, want %q on every host", got, goTypeApplication)
	}
}

// The project root is not assumed to be the module root anywhere else in this
// file, and classification must hold the same line: the scan starts at the
// go.mod's own directory.
func TestProbe_ClassificationIsDeterministic(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "probe-purity", "classification-is-deterministic")
	root := classificationFixture(t)
	forward := probeAll(t, root, "apps/cli", "apps/svc", "libs/core")
	reversed := probeAll(t, root, "libs/core", "apps/svc", "apps/cli")
	if wsproto.ProbeResultDigest(forward) != wsproto.ProbeResultDigest(reversed) {
		t.Fatal("classification moved the digest with the request order")
	}
}

// The reserved control call is what core actually spawns; it must be handled
// before ordinary subcommand dispatch and must decline everything else.
func TestHandleWorkspaceProbe_ServesTheReservedInvocation(t *testing.T) {
	root := probeFixture(t)
	t.Chdir(root)

	var request, stdout bytes.Buffer
	if err := wsproto.EncodeProbeRequest(&request, wsproto.ProbeRequest{
		Extension: goExtensionName, Paths: []string{"apps/svc", "libs/core"},
	}); err != nil {
		t.Fatal(err)
	}

	handled, err := handleWorkspaceProbe(wsproto.ProbeControlArgs(), &request, &stdout)
	if !handled || err != nil {
		t.Fatalf("handleWorkspaceProbe = (%v, %v)", handled, err)
	}

	result, diags := wsproto.ParseAndValidateProbeResult(stdout.Bytes())
	if result == nil || diag.HasErrors(diags) {
		t.Fatalf("stdout does not carry a conformant result: %v", diags)
	}
	if result.Extension != goExtensionName || len(result.Projects) != 2 {
		t.Fatalf("result = %+v", result)
	}
}

func TestHandleWorkspaceProbe_DeclinesOrdinarySubcommands(t *testing.T) {
	handled, err := handleWorkspaceProbe([]string{"build"}, &bytes.Buffer{}, &bytes.Buffer{})
	if handled || err != nil {
		t.Fatalf("handleWorkspaceProbe(build) = (%v, %v), want (false, nil)", handled, err)
	}
}

// --- old/new equivalence ------------------------------------------------

// TestProbe_DerivesTheCorpusEdges runs the migrated workspace-graph corpus through the
// probe. Its twin, TestCoreDerivationMatchesTheCorpus in
// tooling/cli/internal/workspace/go_probe_equivalence_test.go, runs the SAME
// fixtures through core's `deriveGoDependencies`; between them they prove the
// implementation that was deleted and the one that replaces it answer the same
// facts.
func TestProbe_DerivesTheCorpusEdges(t *testing.T) {
	for _, c := range goCorpus() {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			writeGoCorpusCase(t, root, c)

			result := probeAll(t, root, c.Candidates...)
			reported := make(map[string][]string, len(result.Projects))
			for _, project := range result.Projects {
				reported[project.Path] = project.Dependencies
			}

			for path, want := range c.Expect {
				got := reported[path]
				if len(want) == 0 && len(got) == 0 {
					continue
				}
				if !slices.Equal(got, want) {
					t.Errorf("%s dependencies = %v, want %v", path, got, want)
				}
			}
			for path := range reported {
				if _, expected := c.Expect[path]; !expected {
					t.Errorf("probe reported an unexpected project at %q", path)
				}
			}
		})
	}
}

// TestProbe_RepositoryEdgesAreBackedByTheModuleGraph is the real-repository half
// of the equivalence: the probe is run over the module graph of THIS
// repository's actual go.work members, and every edge it reports must be
// justifiable from the go.mod bytes alone — a require or a replace naming
// another member's module or directory.
//
// It is the check that a fixture corpus cannot make: the corpus proves the
// algorithm on eight small trees, this proves it on the ~40 real modules whose
// cache keys the answer feeds, and it self-updates as the repository's module
// graph changes rather than freezing a snapshot that would rot.
//
// The probe runs over a mirror that holds only each member's go.mod, the
// declared test inputs the edges come from. Over the repository itself it
// would also read every member's Go sources and project documents, which
// classify a project and attribute an edge but never create or remove one.
func TestProbe_RepositoryEdgesAreBackedByTheModuleGraph(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "probe-graph-fidelity", "repository-edges-are-backed-by-the-module-graph")
	repoRoot, ok := repoRootFromTest()
	if !ok {
		t.Skip("repo root (go.work) not found walking up from the test dir; out-of-repo test run")
	}
	members, err := repoGoWorkMembers(repoRoot)
	if err != nil {
		t.Fatalf("parse repo go.work: %v", err)
	}
	if len(members) < 2 {
		t.Fatalf("repo go.work has %d members; the equivalence needs the real graph", len(members))
	}
	root := moduleGraphMirror(t, repoRoot, members)

	result := probeAll(t, root, members...)
	if len(result.Projects) != len(members) {
		t.Errorf("probe reported %d of %d go.work members", len(result.Projects), len(members))
	}

	byPath := make(map[string]wsproto.ProbeProject, len(result.Projects))
	modules := make(map[string]string, len(result.Projects))
	for _, project := range result.Projects {
		byPath[project.Path] = project
		modules[project.SourceName] = project.Path
	}

	for _, project := range result.Projects {
		justified := justifiedEdges(t, root, project.Path, modules, byPath)
		for _, dep := range project.Dependencies {
			if !justified[dep] {
				t.Errorf("%s: probe reports an edge to %q that no require or replace in its go.mod justifies",
					project.Path, dep)
			}
		}
		for dep := range justified {
			if !slices.Contains(project.Dependencies, dep) {
				t.Errorf("%s: go.mod links %q but the probe reported no edge; a dropped edge is exactly the "+
					"silent-edge-loss failure (the dependent stops keying on the dependency)", project.Path, dep)
			}
		}
	}
}

// justifiedEdges recomputes one module's expected edges straight from its
// go.mod text, independently of the probe's own index, so the assertion above
// is a real second opinion rather than a restatement.
func justifiedEdges(
	t *testing.T, root, projectPath string, modules map[string]string, byPath map[string]wsproto.ProbeProject,
) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(projectPath), "go.mod"))
	if err != nil {
		t.Fatalf("read %s/go.mod: %v", projectPath, err)
	}
	edges := map[string]bool{}
	add := func(path string) {
		if path != "" && path != projectPath {
			edges[path] = true
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "//", 2)[0])
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if target, ok := modules[fields[0]]; ok {
			add(target)
		}
		if arrow := slices.Index(fields, "=>"); arrow >= 0 && arrow+1 < len(fields) {
			right := fields[arrow+1]
			if strings.HasPrefix(right, ".") {
				resolved := filepath.ToSlash(filepath.Clean(filepath.Join(projectPath, right)))
				if _, ok := byPath[resolved]; ok {
					add(resolved)
				}
				continue
			}
			if target, ok := modules[right]; ok {
				add(target)
			}
		}
	}
	return edges
}

// moduleGraphMirror copies every member's go.mod from the repository at
// repoRoot into a scratch root with the same layout, and returns that root.
func moduleGraphMirror(t *testing.T, repoRoot string, members []string) string {
	t.Helper()
	mirror := t.TempDir()
	for _, member := range members {
		rel := filepath.Join(filepath.FromSlash(member), goWorkspaceMarker)
		data, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", filepath.ToSlash(rel), err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(mirror, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mirror, rel), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return mirror
}

// repoRootFromTest walks up from this test file to the directory carrying
// go.work.
func repoRootFromTest() (string, bool) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", false
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// repoGoWorkMembers returns this repository's go.work member directories,
// sorted.
func repoGoWorkMembers(root string) ([]string, error) {
	rels, err := toolchain.ParseGoWorkUses(filepath.Join(root, "go.work"))
	if err != nil {
		return nil, err
	}
	sort.Strings(rels)
	return rels, nil
}

// A Go project contributes exactly the members it is configured to publish.
// This is especially important for Docker-only services: their go.mod source
// identity need not be a publishable Go coordinate, and no Go publish job will
// emit a module member for the coordinator to reconcile.
//
// The oci coordinate is the repository path WITHOUT the registry host, exactly
// as the publisher computes it: the namespace declared in
// `registries.oci.publish`, then the image name derived from the project name.
func TestProbeDeclaresGoAndOCIMembers(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-ecosystem-profile", "probe-declares-go-and-oci-members")

	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"putnami","registries":{"oci":{"publish":"oci.putnami.dev/putnami"}}}`)

	writeProbeFile(t, filepath.Join(root, "server", "go.mod"), "module example.com/server\n")
	writeProbeFile(t, filepath.Join(root, "server", "putnami.json"),
		`{"name":"server","publish":["go"],"options":{"package":{"docker":true},"publish":{"docker":true}}}`)
	// A library publishes only its module.
	writeProbeFile(t, filepath.Join(root, "lib", "go.mod"), "module example.com/lib\n")
	writeProbeFile(t, filepath.Join(root, "lib", "putnami.json"), `{"publish":["go"]}`)
	// An authored image project contributes verified immutable OCI evidence.
	writeProbeFile(t, filepath.Join(root, "base", "go.mod"), "module example.com/base\n")
	writeProbeFile(t, filepath.Join(root, "base", "putnami.json"),
		`{"name":"base","type":"image","options":{"publish":{"docker":true}}}`)
	// A project entry REPLACES the workspace entry for its ecosystem.
	writeProbeFile(t, filepath.Join(root, "edge", "go.mod"), "module example.com/edge\n")
	writeProbeFile(t, filepath.Join(root, "edge", "putnami.json"),
		`{"name":"@acme/edge","publish":["go","docker"],"registries":{"oci":{"publish":"ghcr.io/acme/images"}}}`)
	// This is the production failure shape: a legal root-domain module identity
	// for a Docker-only service must never be validated as a Go publication.
	writeProbeFile(t, filepath.Join(root, "site", "go.mod"), "module telemetry.example.dev\n")
	writeProbeFile(t, filepath.Join(root, "site", "putnami.json"),
		`{"name":"telemetry.example.dev","options":{"package":{"docker":true},"publish":{"docker":true}}}`)
	// Publication intent stays visible even when package configuration is
	// missing, so planning can diagnose the missing route instead of silently
	// omitting the requested artifact.
	writeProbeFile(t, filepath.Join(root, "upload-only", "go.mod"), "module example.com/upload-only\n")
	writeProbeFile(t, filepath.Join(root, "upload-only", "putnami.json"),
		`{"name":"upload-only","options":{"publish":{"docker":true}}}`)
	writeProbeFile(t, filepath.Join(root, "worker", "go.mod"), "module example.com/worker\n")
	writeProbeFile(t, filepath.Join(root, "worker", "putnami.json"),
		`{"name":"worker","publish":["go","docker"],"options":{"@putnami/go:publish":{"docker-package-publisher":"@acme/cgo","docker-package-step":"cgo"}}}`)

	result, err := probeGoWorkspace(root, wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: goExtensionName,
		Paths: []string{"base", "edge", "lib", "server", "site", "upload-only", "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		path    string
		members []releaseset.MemberDeclaration
	}{
		{"server", []releaseset.MemberDeclaration{
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: "example.com/server", PackageStep: "go", PublishStep: "go"},
			{Ecosystem: releaseplan.OCIEcosystem, Coordinate: "putnami/server", PackageStep: "docker", PublishStep: "docker"},
		}},
		{"lib", []releaseset.MemberDeclaration{
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: "example.com/lib", PackageStep: "go", PublishStep: "go"},
		}},
		{"base", []releaseset.MemberDeclaration{
			{Ecosystem: releaseplan.OCIEcosystem, Coordinate: "putnami/base", PackageStep: "image", PublishStep: "docker"},
		}},
		{"edge", []releaseset.MemberDeclaration{
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: "example.com/edge", PackageStep: "go", PublishStep: "go"},
			{Ecosystem: releaseplan.OCIEcosystem, Coordinate: "acme/images/acme-edge", PackageStep: "docker", PublishStep: "docker"},
		}},
		{"site", []releaseset.MemberDeclaration{
			{Ecosystem: releaseplan.OCIEcosystem, Coordinate: "putnami/telemetry.example.dev", PackageStep: "docker", PublishStep: "docker"},
		}},
		{"upload-only", []releaseset.MemberDeclaration{
			{Ecosystem: releaseplan.OCIEcosystem, Coordinate: "putnami/upload-only", PackageStep: "docker", PublishStep: "docker"},
		}},
		{"worker", []releaseset.MemberDeclaration{
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: "example.com/worker", PackageStep: "go", PublishStep: "go"},
			{Ecosystem: releaseplan.OCIEcosystem, Coordinate: "putnami/worker", PackagePublisher: "@acme/cgo", PackageStep: "cgo", PublishStep: "docker"},
		}},
	} {
		project := projectAt(t, result, want.path)
		metadata, found, err := releaseset.ProjectReleaseMetadata(
			map[string]json.RawMessage{goExtensionName: project.Metadata})
		if err != nil || !found {
			t.Fatalf("%s release metadata = %+v, %v, %v", want.path, metadata, found, err)
		}
		if len(metadata.Ecosystems) != len(want.members) {
			t.Fatalf("%s members = %+v, want %+v", want.path, metadata.Ecosystems, want.members)
		}
		for i, member := range want.members {
			got := metadata.Ecosystems[i]
			if got.Ecosystem != member.Ecosystem || got.Coordinate != member.Coordinate || got.PackageStep != member.PackageStep || got.PublishStep != member.PublishStep {
				t.Fatalf("%s member %d = %+v, want %+v", want.path, i, got, member)
			}
		}
		// The project document decides both facts, so it must invalidate the
		// recorded answer when it changes.
		if !slices.Contains(project.WatchedFiles, filepath.ToSlash(filepath.Join(want.path, "putnami.json"))) {
			t.Fatalf("%s watched files = %v, want the project document", want.path, project.WatchedFiles)
		}
		if !slices.Contains(project.WatchedFiles, "putnami.workspace.json") {
			t.Fatalf("%s watched files = %v, want the workspace document", want.path, project.WatchedFiles)
		}
	}
}
