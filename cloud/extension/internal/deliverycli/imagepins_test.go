package deliverycli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// decodeCommandJSON reads a command's machine surface out of the captured
// stdout lines, unwrapping the {status, exitCode, data} envelope when the CLI
// core wrote one.
func decodeCommandJSON(t *testing.T, out []string) map[string]any {
	t.Helper()
	var got map[string]any
	data := []byte(strings.Join(out, "\n"))
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err == nil {
		if _, hasStatus := probe["status"]; hasStatus {
			if _, hasExitCode := probe["exitCode"]; hasExitCode {
				payload := probe["data"]
				if len(payload) == 0 {
					payload = []byte("null")
				}
				if err := json.Unmarshal(payload, &got); err != nil {
					t.Fatalf("decode result data %q: %v", string(payload), err)
				}
				return got
			}
		}
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode output %q: %v", strings.Join(out, "\n"), err)
	}
	return got
}

// imagePinWorkspace materializes a workspace whose every pin source is present,
// as the real repository has them.
func imagePinWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("putnami.lock.json", `{"cli":{"version":"0.1.0-dedb6e331"},"extensions":{"@putnami/go":{"version":"0.1.0-goext"},"@putnami/typescript":{"version":"0.1.0-tsext"}}}`)
	write("go.work", "go 1.26.1\n\nuse (\n\t./libs/cli\n)\n")
	write(filepath.Join(".putnami", "bin", "extensions", "putnami-go", "tools", "versions.json"),
		`{"goVersion":"1.25.7","tools":{"golangci-lint":{"version":"v2.10.1"},"staticcheck":{"version":"v0.7.0"}}}`)
	write("package.json", `{"packageManager":"bun@1.3.14","engines":{"node":"24.14.1"}}`)
	return ws
}

// TestImageBuildWorkspacePinsDeriveEveryAxis is image invariant 2: every
// version the runner image bakes is READ from the workspace file that declares
// it, in one stable order, so no axis depends on a human copying a literal.
func TestImageBuildWorkspacePinsDeriveEveryAxis(t *testing.T) {
	pins, err := imageBuildWorkspacePins(imagePinWorkspace(t))
	if err != nil {
		t.Fatalf("derive pins: %v", err)
	}
	want := []string{
		"PUTNAMI_VERSION=0.1.0-dedb6e331",
		"GO_VERSION=1.26.1",
		"GOLANGCI_LINT_VERSION=2.10.1",
		"STATICCHECK_VERSION=0.7.0",
		"BUN_VERSION=1.3.14",
		"NODE_VERSION=24.14.1",
		"PUTNAMI_GO_EXTENSION_VERSION=0.1.0-goext",
		"PUTNAMI_TYPESCRIPT_EXTENSION_VERSION=0.1.0-tsext",
	}
	if strings.Join(pins, " ") != strings.Join(want, " ") {
		t.Fatalf("pins = %v, want %v", pins, want)
	}
}

// TestImageBuildWorkspacePinsPreferGoWorkToolchain: Go's own precedence. A
// `toolchain` line raises the requirement above the `go` line, and reading the
// weaker one would let the image bake a toolchain the workspace already
// declared insufficient.
func TestImageBuildWorkspacePinsPreferGoWorkToolchain(t *testing.T) {
	ws := imagePinWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "go.work"), []byte("go 1.26.1\n\ntoolchain go1.26.4\n"), 0o600); err != nil {
		t.Fatalf("write go.work: %v", err)
	}
	pins, err := imageBuildWorkspacePins(ws)
	if err != nil {
		t.Fatalf("derive pins: %v", err)
	}
	if !slices.Contains(pins, "GO_VERSION=1.26.4") {
		t.Fatalf("pins = %v, want the toolchain directive to win", pins)
	}
}

// TestImageBuildWorkspacePinsSkipUndeclaredSources: absence is not drift. A
// workspace on another package manager — or one whose engines.node is a RANGE
// rather than a pin — derives nothing for that axis. The consumer, not the
// table, decides whether a missing pin is fatal for it.
func TestImageBuildWorkspacePinsSkipUndeclaredSources(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "package.json"), []byte(`{"packageManager":"pnpm@9.0.0","engines":{"node":">=20"}}`), 0o600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	pins, err := imageBuildWorkspacePins(ws)
	if err != nil {
		t.Fatalf("derive pins: %v", err)
	}
	if len(pins) != 0 {
		t.Fatalf("pins = %v, want none from a workspace declaring none", pins)
	}
}

// TestImageBuildWorkspacePinsFailOnUnreadableGoVersion: the go directive selects
// the Go layer's toolchain tarball and becomes the runner's floor, which
// the runner library compares numerically. A go.work stating something else is drift, and
// must say so rather than degrade to "no pin".
func TestImageBuildWorkspacePinsFailOnUnreadableGoVersion(t *testing.T) {
	ws := imagePinWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "go.work"), []byte("go tip\n"), 0o600); err != nil {
		t.Fatalf("write go.work: %v", err)
	}
	if _, err := imageBuildWorkspacePins(ws); err == nil {
		t.Fatal("want an error for an unreadable go directive, got nil")
	} else if !strings.Contains(err.Error(), "go.work") {
		t.Fatalf("error %q does not name the file that failed", err)
	}
}

// TestImageBuildWorkspacePinsFailOnUnparseableSource: a source that EXISTS and
// cannot be read is drift, and must surface as an error instead of degrading to
// "no pin".
func TestImageBuildWorkspacePinsFailOnUnparseableSource(t *testing.T) {
	ws := imagePinWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "package.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if _, err := imageBuildWorkspacePins(ws); err == nil {
		t.Fatal("want an error for a malformed package.json, got nil")
	} else if !strings.Contains(err.Error(), "BUN_VERSION") {
		t.Fatalf("error %q does not name the axis that failed", err)
	}
}

// TestImageBuildLineWriterSplitsLines pins the streaming adapter: chunks are
// re-assembled into whole lines, a trailing \r is stripped, and an unterminated
// tail still reaches the sink on flush.
func TestImageBuildLineWriterSplitsLines(t *testing.T) {
	var lines []string
	w := &imageBuildLineWriter{emit: func(line string) { lines = append(lines, line) }}
	if _, err := w.Write([]byte("first\r\nsec")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := w.Write([]byte("ond\nthird")); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.flush()
	want := []string{"first", "second", "third"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
}

// TestImageProjectRootPrefersTheResolvedProject pins dispatch-cwd independence:
// the extension launcher does not preserve the caller's cwd, so the project the
// CLI resolved (params.app) is located by NAME under the workspace root.
func TestImageProjectRootPrefersTheResolvedProject(t *testing.T) {
	ws := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(ws); err == nil {
		ws = resolved
	}
	project := filepath.Join(ws, "images", "ci-runner")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	if err := os.WriteFile(filepath.Join(project, "putnami.json"),
		[]byte(`{"name":"images/ci-runner"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write putnami.json: %v", err)
	}
	got, err := imageProjectRoot(map[string]any{"app": "images/ci-runner"}, ws, "project")
	if err != nil {
		t.Fatalf("resolve project root: %v", err)
	}
	if got != project {
		t.Fatalf("project root = %q, want %q", got, project)
	}

	// An explicit directory param wins over the resolved project.
	explicit := t.TempDir()
	got, err = imageProjectRoot(map[string]any{"app": "images/ci-runner", "project": explicit}, ws, "project")
	if err != nil {
		t.Fatalf("resolve explicit project root: %v", err)
	}
	if want, _ := filepath.Abs(explicit); got != want {
		t.Fatalf("project root = %q, want the explicit %q", got, want)
	}
}

// TestImageProjectRootResolvesARelativeDirAgainstTheWorkspace: the extension
// launcher does not run in the caller's cwd, so the workspace-relative form the
// diagnostics tell an operator to type must resolve against the
// workspace root rather than against the extension's own directory.
func TestImageProjectRootResolvesARelativeDirAgainstTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(ws); err == nil {
		ws = resolved
	}
	project := filepath.Join(ws, "images", "ci-runner")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	got, err := imageProjectRoot(map[string]any{"project": "images/ci-runner"}, ws, "project")
	if err != nil {
		t.Fatalf("resolve project root: %v", err)
	}
	if got != project {
		t.Fatalf("project root = %q, want the workspace-relative %q", got, project)
	}

	// A relative path the workspace root does not answer still resolves against
	// the process cwd, so nothing that worked before now resolves elsewhere.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	got, err = imageProjectRoot(map[string]any{"project": "images/absent"}, ws, "project")
	if err != nil {
		t.Fatalf("resolve cwd project root: %v", err)
	}
	if want := filepath.Join(cwd, "images", "absent"); got != want {
		t.Fatalf("project root = %q, want the cwd-relative %q", got, want)
	}
}

// TestImageProjectRootFailsLoudlyOnUnresolvableAppName: swallowing the lookup
// and falling back to cwd would operate on the extension root, so the failure
// names the project and the workspace it was looked for in.
func TestImageProjectRootFailsLoudlyOnUnresolvableAppName(t *testing.T) {
	ws := t.TempDir()
	_, err := imageProjectRoot(map[string]any{"app": "images/missing"}, ws, "project")
	if err == nil {
		t.Fatal("want an error for an unresolvable app name, got nil")
	}
	if !strings.Contains(err.Error(), "images/missing") {
		t.Fatalf("error %q does not name the project that could not be located", err)
	}
}
