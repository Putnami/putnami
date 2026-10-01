package pkg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// ---- package-time contract gate + cliContract stamp ----

const conformingCLIManifest = `{
	"name": "@putnami/testext",
	"commands": {
		"deploy": {
			"flags": {"env": {"type": "string"}},
			"run": [{"id": "d", "task": "t"}]
		}
	},
	"tasks": {"t": {"kind": "command", "command": "echo"}}
}`

const conformingToolOnlyManifest = `{
	"name": "@putnami/testext",
	"tools": {
		"putnami.search": {
			"description": "Search the extension index.",
			"inputSchema": {"type": "object", "properties": {}},
			"annotations": {
				"readOnlyHint": true,
				"destructiveHint": false,
				"idempotentHint": true,
				"openWorldHint": true
			},
			"_meta": {"putnami.dev/contract": {"access": "read", "readOnly": true, "supportsDryRun": false}},
			"command": "search"
		}
	}
}`

// TestCopyHookFiles_StampsEarnedCLIContract pins the earn-the-stamp flow: a
// conforming CLI-surface manifest is validated strictly and the staged copy
// carries cliContract = CurrentContract; the source manifest is untouched.
func TestCopyHookFiles_StampsEarnedCLIContract(t *testing.T) {
	projDir := t.TempDir()
	outDir := t.TempDir()
	srcPath := filepath.Join(projDir, "putnami.extension.json")
	os.WriteFile(srcPath, []byte(conformingCLIManifest), 0o644)

	if err := copyHookFiles(projDir, outDir, "1.0.0"); err != nil {
		t.Fatalf("copyHookFiles should pass a conforming manifest: %v", err)
	}

	first, err := os.ReadFile(filepath.Join(outDir, "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read staged manifest: %v", err)
	}
	var staged struct {
		CLIContract int    `json:"cliContract"`
		Version     string `json:"version"`
	}
	if err := json.Unmarshal(first, &staged); err != nil {
		t.Fatal(err)
	}
	if staged.CLIContract != protocolcli.CurrentContract {
		t.Errorf("staged cliContract = %d, want %d", staged.CLIContract, protocolcli.CurrentContract)
	}
	if staged.Version != "1.0.0" {
		t.Errorf("staged version = %q, want 1.0.0", staged.Version)
	}

	// The source manifest on disk is untouched.
	src, _ := os.ReadFile(srcPath)
	if string(src) != conformingCLIManifest {
		t.Error("source manifest must not be modified by packaging")
	}

	// Determinism: packaging the same input again yields identical bytes.
	outDir2 := t.TempDir()
	if err := copyHookFiles(projDir, outDir2, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(outDir2, "putnami.extension.json"))
	if string(first) != string(second) {
		t.Errorf("staged manifest is not byte-deterministic:\n%s\n---\n%s", first, second)
	}
}

// TestCopyHookFiles_StampsToolOnlyManifest ensures the MCP surface earns the
// same package-time contract stamp as commands, so older CLIs reject it rather
// than silently ignoring agent-facing capabilities.
func TestCopyHookFiles_StampsToolOnlyManifest(t *testing.T) {
	projDir := t.TempDir()
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projDir, "putnami.extension.json"), []byte(conformingToolOnlyManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyHookFiles(projDir, outDir, "1.0.0"); err != nil {
		t.Fatalf("copyHookFiles should pass a conforming tool-only manifest: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var staged struct {
		CLIContract int `json:"cliContract"`
	}
	if err := json.Unmarshal(data, &staged); err != nil {
		t.Fatal(err)
	}
	if staged.CLIContract != protocolcli.CurrentContract {
		t.Errorf("staged cliContract = %d, want %d", staged.CLIContract, protocolcli.CurrentContract)
	}
}

func TestValidateStagedRuntimeExecutable(t *testing.T) {
	const manifest = `{
		"runtime": {"executable": "compiled/runtime"}
	}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := validateStagedRuntimeExecutable(dir, "linux"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing runtime error = %v, want unavailable", err)
	}

	runtimePath := filepath.Join(dir, "compiled", "runtime")
	if err := os.MkdirAll(filepath.Dir(runtimePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePath, []byte("runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := validateStagedRuntimeExecutable(dir, "linux"); err != nil || got != "compiled/runtime" {
		t.Fatalf("staged executable = %q, %v; want compiled/runtime", got, err)
	}

	// The archive marks the declared runtime executable, so the file's own
	// mode is not checked: a Windows host's disk records no execute bit.
	if err := os.Chmod(runtimePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := validateStagedRuntimeExecutable(dir, "darwin"); err != nil || got != "compiled/runtime" {
		t.Fatalf("runtime without an execute bit = %q, %v; want compiled/runtime", got, err)
	}

	// A Windows archive carries the declared runtime as compiled/runtime.exe.
	if _, err := validateStagedRuntimeExecutable(dir, "windows"); err == nil || !strings.Contains(err.Error(), `"compiled/runtime.exe" is unavailable`) {
		t.Fatalf("windows runtime without .exe error = %v, want compiled/runtime.exe unavailable", err)
	}
	if err := os.WriteFile(runtimePath+".exe", []byte("runtime"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := validateStagedRuntimeExecutable(dir, "windows"); err != nil || got != "compiled/runtime.exe" {
		t.Fatalf("windows runtime = %q, %v; want compiled/runtime.exe", got, err)
	}

	directoryStage := t.TempDir()
	if err := os.WriteFile(filepath.Join(directoryStage, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directoryStage, "compiled", "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := validateStagedRuntimeExecutable(directoryStage, "linux"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory runtime error = %v, want not a regular file", err)
	}

	symlinkStage := t.TempDir()
	if err := os.WriteFile(filepath.Join(symlinkStage, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	externalDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(externalDir, "runtime"), []byte("runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalDir, filepath.Join(symlinkStage, "compiled")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := validateStagedRuntimeExecutable(symlinkStage, "linux"); err == nil ||
		!strings.Contains(err.Error(), `ancestor "compiled"`) ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked ancestor error = %v, want explicit ancestor rejection", err)
	}
}

// TestCopyHookFiles_GateBlocksReservedShadow pins the gate: a CLI-surface
// manifest shadowing a reserved global flag fails the package job — the
// hard reject moved from load time to package time.
func TestCopyHookFiles_GateBlocksReservedShadow(t *testing.T) {
	projDir := t.TempDir()
	outDir := t.TempDir()
	os.WriteFile(filepath.Join(projDir, "putnami.extension.json"), []byte(`{
		"name": "@putnami/testext",
		"commands": {
			"deploy": {
				"flags": {"json": {"type": "boolean"}},
				"run": [{"id": "d", "task": "t"}]
			}
		},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`), 0o644)

	err := copyHookFiles(projDir, outDir, "1.0.0")
	if err == nil {
		t.Fatal("a shadow-carrying manifest must fail packaging")
	}
	if !strings.Contains(err.Error(), "cannot be published") {
		t.Errorf("gate error should say the manifest cannot be published, got %v", err)
	}

	// A rejected manifest never earns the stamp.
	if data, readErr := os.ReadFile(filepath.Join(outDir, "putnami.extension.json")); readErr == nil {
		if strings.Contains(string(data), "cliContract") {
			t.Error("a rejected manifest must not carry the earned cliContract stamp")
		}
	}
}

// TestCopyHookFiles_HookOnlyManifestNotGated pins the scoping: hook-only
// manifests (empty commands — the framework-package shape) have no CLI flag
// surface, keep the historical best-effort staging, and earn no stamp.
func TestCopyHookFiles_HookOnlyManifestNotGated(t *testing.T) {
	projDir := t.TempDir()
	outDir := t.TempDir()
	os.WriteFile(filepath.Join(projDir, "putnami.extension.json"), []byte(`{
		"commands": {},
		"hooks": {"preBuild": {"kind": "command", "command": "bun"}}
	}`), 0o644)

	if err := copyHookFiles(projDir, outDir, "1.0.0"); err != nil {
		t.Fatalf("hook-only manifests must not be gated: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "putnami.extension.json"))
	if err != nil {
		t.Fatalf("hook-only manifest should still be staged: %v", err)
	}
	if strings.Contains(string(data), "cliContract") {
		t.Error("hook-only manifests must not carry a cliContract stamp")
	}
	var staged map[string]any
	if err := json.Unmarshal(data, &staged); err != nil {
		t.Fatal(err)
	}
	if staged["version"] != "1.0.0" {
		t.Errorf("version stamping must be preserved, got %v", staged["version"])
	}
}

// TestCopyHookFiles_GatesMalformedContractSurface pins that a commands,
// commandGroups, or tools value of the wrong JSON type is not mistaken for a
// hook-only manifest: it is gated and fails the package job, rather than
// publishing an unloadable manifest unstamped.
func TestCopyHookFiles_GatesMalformedContractSurface(t *testing.T) {
	for _, m := range []string{`{"commands": []}`, `{"commandGroups": "nope"}`, `{"tools": []}`} {
		projDir := t.TempDir()
		outDir := t.TempDir()
		os.WriteFile(filepath.Join(projDir, "putnami.extension.json"), []byte(m), 0o644)

		if err := copyHookFiles(projDir, outDir, "1.0.0"); err == nil {
			t.Fatalf("a wrong-typed contract surface must fail the gate, not bypass it: %s", m)
		}
		data, _ := os.ReadFile(filepath.Join(outDir, "putnami.extension.json"))
		if strings.Contains(string(data), "cliContract") {
			t.Errorf("a gated-and-rejected manifest must not carry the cliContract stamp: %s", m)
		}
	}
}
