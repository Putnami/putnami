package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// The Python probe's contract, as tests.
//
// The probe is the LAST of the three that had to exist before core's own
// manifest parsers could be deleted, and it is the successor to
// `readPyProjectName`. Two properties carry the weight here: the answer must be
// a pure function of the tree (its digest keys the workspace snapshot and every
// project's metadata digest), and a manifest it cannot understand must produce a
// DIAGNOSTIC rather than a silently missing project.

func writeProbeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func probeWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "pyproject.toml"),
		"[project]\nname = \"putnami-workspace\"\nversion = \"0.0.1\"\n\n[tool.uv.workspace]\nmembers = [\"app\"]\n")
	writeProbeFile(t, filepath.Join(root, "app", "pyproject.toml"),
		"[project]\nname = \"py_example_application\"\nversion = \"0.1.0\"\n")
	writeProbeFile(t, filepath.Join(root, "lib", "pyproject.toml"),
		"[tool.poetry]\nname = \"not-the-project-name\"\n\n[project]\nname = \"py_example_library\"\n")
	writeProbeFile(t, filepath.Join(root, "web", "package.json"), `{"name":"web"}`)
	return root
}

func probeAll(t *testing.T, root string, paths ...string) wsproto.ProbeResult {
	t.Helper()
	result, err := probePythonWorkspace(root, wsproto.ProbeRequest{
		Version:   wsproto.ProbeProtocolVersion,
		Extension: pythonExtensionName,
		Paths:     paths,
	})
	if err != nil {
		t.Fatalf("probePythonWorkspace: %v", err)
	}
	return result
}

func projectAt(result wsproto.ProbeResult, path string) (wsproto.ProbeProject, bool) {
	for _, project := range result.Projects {
		if project.Path == path {
			return project, true
		}
	}
	return wsproto.ProbeProject{}, false
}

// A Python project's identity is its `[project] name`, read from its own
// pyproject.toml — and the manifest path is reported so core's watch and
// snapshot machinery never has to know what this provider reads.
func TestProbe_ReportsPyprojectIdentity(t *testing.T) {
	root := probeWorkspace(t)
	result := probeAll(t, root, ".", "app", "lib", "web")

	app, ok := projectAt(result, "app")
	if !ok {
		t.Fatalf("app missing from %+v", result.Projects)
	}
	if app.SourceName != "py_example_application" {
		t.Errorf("app sourceName = %q, want py_example_application", app.SourceName)
	}
	if app.SourceFile != "app/pyproject.toml" {
		t.Errorf("app sourceFile = %q, want app/pyproject.toml", app.SourceFile)
	}
	if !slices.Contains(app.WatchedFiles, "app/pyproject.toml") {
		t.Errorf("app watchedFiles = %v, want the project's own pyproject.toml", app.WatchedFiles)
	}
	for _, rootFile := range pythonWorkspaceRootFiles {
		if !slices.Contains(app.WatchedFiles, rootFile) {
			t.Errorf("app watchedFiles = %v, missing workspace-root invalidation input %q",
				app.WatchedFiles, rootFile)
		}
	}
	if _, ok := projectAt(result, "web"); ok {
		t.Error("a directory with no pyproject.toml must not be claimed as a Python project")
	}
}

// Section gating is identity-critical: a `name` under `[tool.poetry]` is a
// different key, and reading it would rename the distribution.
func TestProbe_OnlyTheProjectTableNamesTheDistribution(t *testing.T) {
	root := probeWorkspace(t)
	lib, ok := projectAt(probeAll(t, root, "lib"), "lib")
	if !ok {
		t.Fatal("lib missing from the answer")
	}
	if lib.SourceName != "py_example_library" {
		t.Errorf("lib sourceName = %q, want py_example_library (the [project] table, not [tool.poetry])", lib.SourceName)
	}
}

// The uv workspace ROOT's manifest is the workspace's own, not a member's.
// Claiming it would give the workspace root a distribution identity, and core
// would then resolve a project named after the repository.
func TestProbe_SkipsTheUVWorkspaceRootManifest(t *testing.T) {
	root := probeWorkspace(t)
	if _, ok := projectAt(probeAll(t, root, ".", "app"), wsproto.ProbeRootPath); ok {
		t.Error("the root pyproject.toml declares [tool.uv.workspace]; it must not be reported as a project")
	}
}

// A root manifest with NO uv workspace table is an ordinary Python project that
// happens to live at the root, and it is reported.
func TestProbe_ReportsARootManifestThatIsNotAWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "pyproject.toml"), "[project]\nname = \"solo\"\n")
	project, ok := projectAt(probeAll(t, root, "."), wsproto.ProbeRootPath)
	if !ok {
		t.Fatal("the root project is missing from the answer")
	}
	if project.SourceName != "solo" || project.SourceFile != "pyproject.toml" {
		t.Errorf("root project = %+v, want solo/pyproject.toml", project)
	}
}

// A manifest that exists but cannot be understood must be REPORTED. A
// silently dropped project is a project that vanishes from the graph with no
// diagnostic — the failure this repository already paid for once.
func TestProbe_MalformedManifestIsReportedNotDropped(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "broken", "pyproject.toml"), "[build-system]\nrequires = []\n")

	result := probeAll(t, root, "broken")
	if _, ok := projectAt(result, "broken"); ok {
		t.Error("a pyproject.toml with no [project] name must not produce a project with an invented identity")
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want exactly one", result.Diagnostics)
	}
	if got := result.Diagnostics[0].Field; got != "broken/pyproject.toml" {
		t.Errorf("diagnostic field = %q, want broken/pyproject.toml", got)
	}
	if !strings.Contains(result.Diagnostics[0].Message, "[project] name") {
		t.Errorf("diagnostic message = %q, want it to name the missing key", result.Diagnostics[0].Message)
	}
}

// PURE: the answer's digest keys core's snapshot and every project's metadata
// digest, so two runs over one tree — in any request order — must produce one
// byte-identical answer.
func TestProbe_IsDeterministicRegardlessOfRequestOrder(t *testing.T) {
	root := probeWorkspace(t)
	first := probeAll(t, root, "app", "lib", ".", "web")
	second := probeAll(t, root, "web", ".", "lib", "app")

	if wsproto.ProbeResultDigest(first) != wsproto.ProbeResultDigest(second) {
		t.Fatalf("probe digest depends on request order:\n%+v\n%+v", first, second)
	}
	encodedFirst, _ := json.Marshal(first)
	encodedSecond, _ := json.Marshal(second)
	if !bytes.Equal(encodedFirst, encodedSecond) {
		t.Errorf("probe answer is not byte-identical across orders:\n%s\n%s", encodedFirst, encodedSecond)
	}
}

// The wire refuses paths that would make a digest depend on where the repository
// is checked out; an unrepresentable candidate is skipped rather than carried.
func TestProbe_RefusesPathsOutsideTheWorkspace(t *testing.T) {
	root := probeWorkspace(t)
	result := probeAll(t, root, "../escape", "/absolute", "app")
	if len(result.Projects) != 1 || result.Projects[0].Path != "app" {
		t.Errorf("projects = %+v, want only app", result.Projects)
	}
}

// The reserved control call must answer on stdout and claim only its own argv.
func TestHandleWorkspaceProbe_ServesTheControlCall(t *testing.T) {
	root := probeWorkspace(t)
	t.Chdir(root)

	request, err := json.Marshal(wsproto.ProbeRequest{
		Version:   wsproto.ProbeProtocolVersion,
		Extension: pythonExtensionName,
		Paths:     []string{"app"},
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}

	var stdout bytes.Buffer
	handled, err := handleWorkspaceProbe([]string{"__putnami", "workspace-probe"}, bytes.NewReader(request), &stdout)
	if !handled || err != nil {
		t.Fatalf("handleWorkspaceProbe handled=%t err=%v", handled, err)
	}
	var result wsproto.ProbeResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("decode answer %q: %v", stdout.String(), err)
	}
	if result.Extension != pythonExtensionName || len(result.Projects) != 1 {
		t.Fatalf("answer = %+v, want one project attributed to %s", result, pythonExtensionName)
	}

	stdout.Reset()
	if handled, _ := handleWorkspaceProbe([]string{"test"}, bytes.NewReader(nil), &stdout); handled {
		t.Error("the probe claimed an ordinary subcommand's argv")
	}
}
