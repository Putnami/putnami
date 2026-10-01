package pkg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

func stageManifest(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const conformingManifest = `{
	"name": "@putnami/testext",
	"version": "1.2.3",
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

// TestGateAndStampManifestContract_StampsEarnedContract pins the happy path:
// a conforming staged manifest earns the cliContract stamp, and the result is
// byte-deterministic (staged manifests feed content-addressed packages).
func TestGateAndStampManifestContract_StampsEarnedContract(t *testing.T) {
	dir := stageManifest(t, conformingManifest)
	if err := gateAndStampManifestContract(dir); err != nil {
		t.Fatalf("gate should pass a conforming manifest: %v", err)
	}

	first, err := os.ReadFile(filepath.Join(dir, "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		CLIContract int    `json:"cliContract"`
		Version     string `json:"version"`
	}
	if err := json.Unmarshal(first, &m); err != nil {
		t.Fatal(err)
	}
	if m.CLIContract != protocolcli.CurrentContract {
		t.Errorf("cliContract = %d, want %d", m.CLIContract, protocolcli.CurrentContract)
	}
	if m.Version != "1.2.3" {
		t.Errorf("version = %q — stamping must not disturb other fields", m.Version)
	}

	// Determinism: gating and stamping the same input again yields identical
	// bytes (the stamp is a constant; re-stamping is idempotent).
	if err := gateAndStampManifestContract(dir); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("staged manifest is not byte-deterministic:\n%s\n---\n%s", first, second)
	}
}

// TestGateAndStampManifestContract_StampsToolOnlyManifest ensures the MCP
// surface participates in the same package-time lifecycle as commands. This
// prevents an older CLI from silently loading a tool it cannot understand.
func TestGateAndStampManifestContract_StampsToolOnlyManifest(t *testing.T) {
	dir := stageManifest(t, conformingToolOnlyManifest)
	if err := gateAndStampManifestContract(dir); err != nil {
		t.Fatalf("gate should pass a conforming tool-only manifest: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "putnami.extension.json"))
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
		t.Errorf("cliContract = %d, want %d", staged.CLIContract, protocolcli.CurrentContract)
	}
}

func TestValidateStagedRuntimeExecutable(t *testing.T) {
	const manifest = `{
		"runtime": {"executable": "compiled/runtime"}
	}`
	dir := stageManifest(t, manifest)
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

	directoryStage := stageManifest(t, manifest)
	if err := os.MkdirAll(filepath.Join(directoryStage, "compiled", "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := validateStagedRuntimeExecutable(directoryStage, "linux"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory runtime error = %v, want not a regular file", err)
	}

	if got, err := validateStagedRuntimeExecutable(stageManifest(t, `{}`), "windows"); err != nil || got != "" {
		t.Fatalf("manifest without runtime = %q, %v; want no executable", got, err)
	}

	symlinkStage := stageManifest(t, manifest)
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

// TestGateAndStampManifestContract_BlocksReservedShadow pins the gate: a
// manifest shadowing a reserved global flag fails packaging instead of being
// stamped — strictness moved from load time to package time.
func TestGateAndStampManifestContract_BlocksReservedShadow(t *testing.T) {
	dir := stageManifest(t, `{
		"name": "@putnami/testext",
		"commands": {
			"deploy": {
				"flags": {"json": {"type": "boolean"}},
				"run": [{"id": "d", "task": "t"}]
			}
		},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`)

	err := gateAndStampManifestContract(dir)
	if err == nil {
		t.Fatal("gate must block a shadow-carrying manifest from packaging")
	}
	if !strings.Contains(err.Error(), "cannot be published") || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("gate error should explain the reserved-flag violation, got %v", err)
	}

	// The staged manifest must not have been stamped.
	data, _ := os.ReadFile(filepath.Join(dir, "putnami.extension.json"))
	if strings.Contains(string(data), "cliContract") {
		t.Error("a rejected manifest must not carry the earned cliContract stamp")
	}
}

// TestGateAndStampManifestContract_SkipsHookOnlyManifest pins the scoping: a
// hook-only manifest (empty commands — e.g. a framework package's preBuild
// hook) has no CLI surface for the contract to govern and intentionally fails
// full strict validation, so the gate leaves it untouched and unstamped.
func TestGateAndStampManifestContract_SkipsHookOnlyManifest(t *testing.T) {
	manifest := `{
	"commands": {},
	"hooks": {"preBuild": {"kind": "command", "command": "echo"}}
}`
	dir := stageManifest(t, manifest)
	if err := gateAndStampManifestContract(dir); err != nil {
		t.Fatalf("hook-only manifests must not be gated: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "putnami.extension.json"))
	if string(data) != manifest {
		t.Errorf("hook-only manifest must be left byte-identical, got:\n%s", data)
	}
}

// TestGateAndStampManifestContract_UnparseableStagedManifest pins that a
// broken staged manifest fails the package job loudly.
func TestGateAndStampManifestContract_UnparseableStagedManifest(t *testing.T) {
	dir := stageManifest(t, "{not json")
	if err := gateAndStampManifestContract(dir); err == nil {
		t.Fatal("an unparseable staged manifest must fail packaging")
	}
}

// TestGateAndStampManifestContract_GatesMalformedContractSurface pins that a
// commands, commandGroups, or tools value of the wrong JSON type is NOT
// mistaken for a hook-only manifest: it is gated and fails strict parsing,
// rather than publishing an unloadable manifest unstamped that every consumer
// would fail to unmarshal and skip.
func TestGateAndStampManifestContract_GatesMalformedContractSurface(t *testing.T) {
	for _, m := range []string{`{"commands": []}`, `{"commandGroups": "nope"}`, `{"tools": []}`} {
		dir := stageManifest(t, m)
		if err := gateAndStampManifestContract(dir); err == nil {
			t.Fatalf("a wrong-typed contract surface must fail the gate, not bypass it: %s", m)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "putnami.extension.json"))
		if strings.Contains(string(data), "cliContract") {
			t.Errorf("a gated-and-rejected manifest must not carry the cliContract stamp: %s", m)
		}
	}
}
