package agentctx

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/tooling/cli/internal/mcp"
)

// The contributor helpers reach tasks and proposals only through `putnami
// tasks|proposals <operation>`. Their shell suites therefore run against a real
// CLI built from this tree and the shipped local provider, compiled the way its
// bin/prepare compiles it: the collaboration route, the binding and the
// provider's store are the real ones.

var (
	collaborationHarnessOnce    sync.Once
	collaborationHarnessScratch *scratch.Dir
	collaborationHarnessDir     string
	collaborationHarnessErr     string
)

func TestMain(m *testing.M) {
	code := m.Run()
	_ = collaborationHarnessScratch.Remove()
	os.Exit(code)
}

// collaborationLauncher runs the harness CLI with no Putnami variable an
// enclosing run exported (a workspace root, a session, a selection), its own
// Putnami home, and telemetry and automatic installs off, so the suites'
// temporary workspaces are the only workspaces it sees.
const collaborationLauncher = `#!/usr/bin/env bash
for name in $(compgen -e); do
  case "$name" in
    PUTNAMI_*) unset "$name" ;;
  esac
done
export PUTNAMI_HOME=@HOME@
export PUTNAMI_TELEMETRY=off
export PUTNAMI_NO_AUTO_INSTALL=1
export DO_NOT_TRACK=1
exec @BINARY@ "$@"
`

// collaborationHarness builds the CLI and the local provider once per test
// process and returns the directory holding them.
func collaborationHarness(t *testing.T) string {
	t.Helper()
	collaborationHarnessOnce.Do(func() {
		collaborationHarnessErr = prepareCollaborationHarness()
	})
	if collaborationHarnessErr != "" {
		t.Fatalf("could not prepare the collaboration harness:\n%s", collaborationHarnessErr)
	}
	return collaborationHarnessDir
}

func prepareCollaborationHarness() string {
	// Scratch-owned: the harness holds two built binaries, and the next run
	// reclaims them when this one is killed before TestMain removes them.
	harness, err := scratch.New("putnami-contributor-harness-")
	if err != nil {
		return err.Error()
	}
	collaborationHarnessScratch = harness
	dir := harness.Path()
	collaborationHarnessDir = dir
	cliRoot := filepath.Join("..", "..", "..")
	providerRoot := filepath.Join(cliRoot, "..", "local-collaboration")
	binary := filepath.Join(dir, programName("putnami-binary"))
	provider := filepath.Join(dir, programName("putnami-local-collaboration"))
	builds := []struct{ dir, pkg, output string }{
		{cliRoot, "./cmd/putnami", binary},
		{providerRoot, "./cmd/putnami-local-collaboration", provider},
	}
	for _, build := range builds {
		cmd := exec.Command("go", "build", "-o", build.output, build.pkg)
		cmd.Dir = build.dir
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "go build " + build.pkg + ": " + string(out)
		}
	}
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err.Error()
	}
	launcher := strings.ReplaceAll(collaborationLauncher, "@HOME@", shellQuote(home))
	launcher = strings.ReplaceAll(launcher, "@BINARY@", shellQuote(binary))
	if err := os.WriteFile(filepath.Join(dir, "putnami"), []byte(launcher), 0o755); err != nil {
		return err.Error()
	}
	shipped, err := os.ReadFile(filepath.Join(providerRoot, "putnami.extension.json"))
	if err != nil {
		return err.Error()
	}
	resolved, err := resolveExtensionRuntime(shipped, provider)
	if err != nil {
		return err.Error()
	}
	extensionDir := filepath.Join(dir, "local-collaboration")
	if err := os.MkdirAll(extensionDir, 0o755); err != nil {
		return err.Error()
	}
	if err := os.WriteFile(filepath.Join(extensionDir, "putnami.extension.json"), resolved, 0o644); err != nil {
		return err.Error()
	}
	return ""
}

// resolveExtensionRuntime points every {extensionRuntime} in a shipped
// extension manifest at the built runtime. It rewrites decoded string values
// and encodes each level again, so a Windows path's backslashes are escaped
// JSON rather than invalid escapes spliced into the text.
func resolveExtensionRuntime(document json.RawMessage, runtimePath string) (json.RawMessage, error) {
	document = bytes.TrimSpace(document)
	if len(document) == 0 {
		return document, nil
	}
	switch document[0] {
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(document, &object); err != nil {
			return nil, err
		}
		for key, child := range object {
			resolved, err := resolveExtensionRuntime(child, runtimePath)
			if err != nil {
				return nil, err
			}
			object[key] = resolved
		}
		return json.Marshal(object)
	case '[':
		var array []json.RawMessage
		if err := json.Unmarshal(document, &array); err != nil {
			return nil, err
		}
		for index, child := range array {
			resolved, err := resolveExtensionRuntime(child, runtimePath)
			if err != nil {
				return nil, err
			}
			array[index] = resolved
		}
		return json.Marshal(array)
	case '"':
		var text string
		if err := json.Unmarshal(document, &text); err != nil {
			return nil, err
		}
		return json.Marshal(strings.ReplaceAll(text, "{extensionRuntime}", runtimePath))
	}
	return document, nil
}

// programName is the file name of a built program: Windows starts only a
// program whose name carries .exe, and the CLI advertises no extension tool
// whose command it cannot start.
func programName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// shellQuote quotes a path for a POSIX shell.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// collaborationScriptEnv is the environment a helper suite runs with: the
// caller's, plus the harness CLI launcher, the resolved local provider, and
// the reconcile hint the CLI writes on an unresolved proposals.upsert, which a
// suite that stands in for that answer relays. The paths are in slash form:
// the suites write them into JSON documents, where a Windows path's
// backslashes are escapes, and bash and the CLI read C:/… as the same path.
func collaborationScriptEnv(t *testing.T) []string {
	t.Helper()
	dir := collaborationHarness(t)
	return append(os.Environ(),
		"PUTNAMI_TEST_CLI="+filepath.ToSlash(filepath.Join(dir, "putnami")),
		"PUTNAMI_TEST_LOCAL_PROVIDER="+filepath.ToSlash(filepath.Join(dir, "local-collaboration")),
		"PUTNAMI_TEST_UPSERT_RECONCILE="+mcp.ReconcileHint("proposals", "upsert"),
	)
}

// A Windows runtime path reaches the harness manifest as escaped JSON: spliced
// into the text, C:\w\tmp\… is a run of invalid escapes, and every suite that
// binds the local provider failed before its first assertion.
func TestHarnessManifestCarriesAWindowsRuntimePath(t *testing.T) {
	runtimePath := `C:\w\tmp\putnami-contributor-harness-1\putnami-local-collaboration`
	resolved, err := resolveExtensionRuntime([]byte(`{"tools":{"a":{"command":"{extensionRuntime}","timeoutMs":30000}}}`), runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tools map[string]struct {
			Command   string `json:"command"`
			TimeoutMs int    `json:"timeoutMs"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resolved, &manifest); err != nil {
		t.Fatalf("the resolved manifest is not JSON: %v\n%s", err, resolved)
	}
	if tool := manifest.Tools["a"]; tool.Command != runtimePath || tool.TimeoutMs != 30000 {
		t.Fatalf("resolved tool = %+v, want command %s and timeoutMs 30000", tool, runtimePath)
	}
}
