package build

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"go.putnami.dev/go/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/protocol/features/spectest"
)

// --- proxy-dependent tidy ---
//
// `go mod tidy` runs in module mode. When the version go.mod requires lacks a
// package the code imports, tidy queries the module proxy for the latest
// version that provides it. That answer changes when a release is published,
// so the same tree tidies differently over time, and a cached entry would hide
// the incomplete committed go.mod. These tests pin that such a tidy is reported
// and stays out of the cache, and that a go.mod that pins a providing version
// tidies without a lookup.

const proxyLib = "example.com/lib"

// writeProxyModule publishes one version of a module to a file:// proxy at
// proxyDir. Each entry of files maps a module-relative path to its content.
func writeProxyModule(t *testing.T, proxyDir, path, version string, files map[string]string) {
	t.Helper()
	src := t.TempDir()
	goMod := "module " + path + "\n\ngo 1.24\n"
	files["go.mod"] = goMod
	for name, content := range files {
		full := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTidyFile(t, filepath.Dir(full), filepath.Base(full), content)
	}
	dir := filepath.Join(proxyDir, filepath.FromSlash(path), "@v")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTidyFile(t, dir, version+".info", `{"Version":"`+version+`"}`)
	writeTidyFile(t, dir, version+".mod", goMod)
	zipFile, err := os.Create(filepath.Join(dir, version+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	if err := modzip.CreateFromDir(zipFile, module.Version{Path: path, Version: version}, src); err != nil {
		t.Fatalf("zip %s@%s: %v", path, version, err)
	}
	if err := zipFile.Close(); err != nil {
		t.Fatal(err)
	}
	list, _ := os.ReadFile(filepath.Join(dir, "list"))
	writeTidyFile(t, dir, "list", string(list)+version+"\n")
}

// proxyTidyFixture publishes example.com/lib at v0.1.0, without package sub,
// and at v0.2.0, with it, then writes an app that imports example.com/lib/sub
// and requires lib at the given version. Module resolution is confined to that
// proxy and to a private module cache, so nothing reaches the network.
func proxyTidyFixture(t *testing.T, required string) (root, appDir string) {
	t.Helper()
	if _, err := toolchain.ResolveGo(); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	proxyDir := t.TempDir()
	writeProxyModule(t, proxyDir, proxyLib, "v0.1.0", map[string]string{
		"lib.go": "package lib\n",
	})
	writeProxyModule(t, proxyDir, proxyLib, "v0.2.0", map[string]string{
		"lib.go":     "package lib\n",
		"sub/sub.go": "package sub\n",
	})

	root = t.TempDir()
	appDir = filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTidyFile(t, appDir, "go.mod", "module example.com/app\n\ngo 1.24\n\nrequire "+proxyLib+" "+required+"\n")
	writeTidyFile(t, appDir, "main.go", "package main\n\nimport _ \""+proxyLib+"/sub\"\n\nfunc main() {}\n")

	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "file://"+filepath.ToSlash(proxyDir))
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GONOSUMDB", "")
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOMODCACHE", t.TempDir())
	// The module cache is read-only by default, which t.TempDir cannot clean.
	t.Setenv("GOFLAGS", "-modcacherw")
	return root, appDir
}

func runTidyPhase(t *testing.T, root, appDir string) (string, []map[string]any) {
	t.Helper()
	var status string
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		var err error
		status, _, err = Run(&pctx.Context{
			WorkspaceRoot: root,
			Project:       pctx.Project{Name: "app", FullPath: appDir},
		}, emit, []string{"--phase", "tidy"})
		if err != nil {
			t.Errorf("Run tidy: %v", err)
		}
	})
	return status, events
}

func diagnostics(events []map[string]any) []string {
	var messages []string
	for _, ev := range events {
		if ev["type"] != "diagnostic" {
			continue
		}
		message, _ := ev["message"].(string)
		messages = append(messages, message)
	}
	return messages
}

// The reproduction of the issue: go.mod requires a version that lacks an
// imported package. Every run looks it up, reports it, and ends as the SKIP the
// orchestrator never caches for build-tidy, which declares no
// `cache.deterministic` (IsCacheableResult,
// tooling/cli/internal/jobs/cachekey/executor.go).
func TestRunTidy_APackageLookupIsReportedAndStaysASkip(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "proxy-dependent-tidy-is-reported-and-uncached",
		"a-package-lookup-is-reported-and-stays-a-skip")
	root, appDir := proxyTidyFixture(t, "v0.1.0")
	committed := readTidyFile(t, appDir, "go.mod")

	for run := 1; run <= 2; run++ {
		writeTidyFile(t, appDir, "go.mod", committed)
		os.Remove(filepath.Join(appDir, "go.sum"))

		status, events := runTidyPhase(t, root, appDir)
		if status != "SKIP" {
			t.Fatalf("run %d: status = %q, want SKIP: a proxy-dependent tidy must not be cached", run, status)
		}
		messages := diagnostics(events)
		if len(messages) != 1 {
			t.Fatalf("run %d: diagnostics = %q, want one", run, messages)
		}
		for _, want := range []string{"example.com/app", proxyLib + "/sub", proxyLib + " v0.2.0", "module proxy", "commit go.mod"} {
			if !strings.Contains(messages[0], want) {
				t.Errorf("run %d: diagnostic %q does not mention %q", run, messages[0], want)
			}
		}
		// The tidied go.mod is kept: it is what the diagnostic says to commit.
		if got := readTidyFile(t, appDir, "go.mod"); !strings.Contains(got, proxyLib+" v0.2.0") {
			t.Errorf("run %d: go.mod = %q, want the tidied requirement on v0.2.0", run, got)
		}
	}
}

// Once go.mod pins a version that provides every imported package, tidy looks
// nothing up: the run is a cacheable OK with no diagnostic, and a second run
// leaves byte-identical files, so a cached and an uncached run agree.
func TestRunTidy_AGoModThatProvidesEveryPackageTidiesWithoutALookup(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "proxy-dependent-tidy-is-reported-and-uncached",
		"a-go-mod-that-provides-every-package-tidies-without-a-lookup")
	root, appDir := proxyTidyFixture(t, "v0.2.0")

	status, events := runTidyPhase(t, root, appDir)
	if status != "OK" {
		t.Fatalf("status = %q, want OK; diagnostics %q", status, diagnostics(events))
	}
	if messages := diagnostics(events); len(messages) != 0 {
		t.Fatalf("diagnostics = %q, want none", messages)
	}
	goMod, goSum := readTidyFile(t, appDir, "go.mod"), readTidyFile(t, appDir, "go.sum")

	status, events = runTidyPhase(t, root, appDir)
	if status != "OK" || len(diagnostics(events)) != 0 {
		t.Fatalf("second run: status %q, diagnostics %q; want OK and none", status, diagnostics(events))
	}
	if got := readTidyFile(t, appDir, "go.mod"); got != goMod {
		t.Errorf("go.mod moved between runs:\n%s\nthen\n%s", goMod, got)
	}
	if got := readTidyFile(t, appDir, "go.sum"); got != goSum {
		t.Errorf("go.sum moved between runs:\n%s\nthen\n%s", goSum, got)
	}
}

func TestTidyRaisedRequirements_ReportsAnExistingRequirementMovedUp(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "proxy-dependent-tidy-is-reported-and-uncached",
		"a-raised-requirement-is-reported")
	dir := t.TempDir()
	writeTidyFile(t, dir, "go.mod",
		"module example.com/app\n\ngo 1.25\n\nrequire (\n\texample.com/lib v0.1.0\n\texample.com/other v1.0.0\n)\n")
	snap := snapshotModFiles(dir)

	// Tidy raised lib, kept other, and added a module go.mod did not name: only
	// the raise is reported here; an addition is a lookup tidy prints.
	writeTidyFile(t, dir, "go.mod",
		"module example.com/app\n\ngo 1.25\n\nrequire (\n\texample.com/lib v0.2.0\n\texample.com/new v1.0.0\n\texample.com/other v1.0.0\n)\n")

	raised := tidyRaisedRequirements(snap, dir)
	want := []tidyRaise{{Module: "example.com/lib", From: "v0.1.0", To: "v0.2.0"}}
	if !slices.Equal(raised, want) {
		t.Fatalf("raised = %+v, want %+v", raised, want)
	}
	message := tidyUnpinnedResolutionMessage(dir, nil, raised)
	for _, want := range []string{"example.com/app", "raised example.com/lib from v0.1.0 to v0.2.0", "does not pin", "commit go.mod"} {
		if !strings.Contains(message, want) {
			t.Errorf("diagnostic %q does not mention %q", message, want)
		}
	}

	if strings.Contains(message, "module proxy") {
		t.Errorf("diagnostic %q blames the module proxy for a raise no lookup made", message)
	}

	// A go.mod tidy left alone raises nothing.
	writeTidyFile(t, dir, "go.mod",
		"module example.com/app\n\ngo 1.25\n\nrequire (\n\texample.com/lib v0.1.0\n\texample.com/other v1.0.0\n)\n")
	if raised := tidyRaisedRequirements(snap, dir); raised != nil {
		t.Errorf("raised = %+v for an unchanged go.mod, want nil", raised)
	}
}

func TestTidyPackageLookups_ReadsTheModuleAndVersionTidyPicked(t *testing.T) {
	output := "go: finding module for package example.com/lib/sub\n" +
		"go: finding module for package example.com/other/pkg\n" +
		"go: downloading example.com/lib v0.2.0\n" +
		"go: found example.com/lib/sub in example.com/lib v0.2.0\n" +
		"go: found example.com/sibling in example.com/sibling v0.0.0-00010101000000-000000000000\n"
	got := tidyPackageLookups(output)
	want := []tidyLookup{
		{Package: "example.com/lib/sub", Module: "example.com/lib", Version: "v0.2.0"},
		{Package: "example.com/other/pkg"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("lookups = %+v, want %+v", got, want)
	}
	if got := tidyPackageLookups("go: downloading example.com/lib v0.2.0\n"); got != nil {
		t.Errorf("lookups = %+v for a tidy that looked nothing up, want nil", got)
	}
}

// Go prints `go: found` for an import a directory replace resolves, too, though
// it queries no proxy. Such a tidy is the ordinary workspace case: it reports
// no proxy lookup and stays a cacheable OK.
func TestRunTidy_APackageResolvedThroughAReplaceIsNoLookup(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "proxy-dependent-tidy-is-reported-and-uncached",
		"a-package-resolved-through-a-replace-is-no-lookup")
	root, appDir := proxyTidyFixture(t, "v0.2.0")
	libDir := filepath.Join(root, "lib")
	if err := os.MkdirAll(filepath.Join(libDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTidyFile(t, libDir, "go.mod", "module "+proxyLib+"\n\ngo 1.24\n")
	writeTidyFile(t, filepath.Join(libDir, "sub"), "sub.go", "package sub\n")
	// No requirement, only the replace: tidy adds the requirement and prints
	// `go: found` for the package, without a `finding` query.
	writeTidyFile(t, appDir, "go.mod", "module example.com/app\n\ngo 1.24\n\nreplace "+proxyLib+" => ../lib\n")

	status, events := runTidyPhase(t, root, appDir)
	if status != "OK" {
		t.Fatalf("status = %q, want OK; diagnostics %q", status, diagnostics(events))
	}
	if messages := diagnostics(events); len(messages) != 0 {
		t.Fatalf("diagnostics = %q, want none for a package a replace resolves", messages)
	}
	if got := readTidyFile(t, appDir, "go.mod"); !strings.Contains(got, "require "+proxyLib+" v0.0.0-") {
		t.Errorf("go.mod = %q, want tidy to have added the replaced requirement", got)
	}
}
