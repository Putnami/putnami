package extensions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
)

// validExtensionManifest is a minimal manifest that passes strict parsing,
// full structural validation, and contract validation.
const validExtensionManifest = `{
  "name": "@putnami/test-ext",
  "version": "1.0.0",
  "cliContract": 4,
  "commands": {
    "build": {
      "run": [{"id": "build", "task": "build-exec"}]
    }
  },
  "tasks": {
    "build-exec": {
      "kind": "command",
      "command": "echo",
      "cache": false
    }
  }
}`

func TestResolveExtensionManifestPath(t *testing.T) {
	t.Run("explicit file", func(t *testing.T) {
		dir := t.TempDir()
		manifest := filepath.Join(dir, "putnami.extension.json")
		if err := os.WriteFile(manifest, []byte(`{"name":"x"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := resolveExtensionManifestPath([]string{manifest})
		if err != nil {
			t.Fatalf("resolveExtensionManifestPath: %v", err)
		}
		if got != manifest {
			t.Errorf("path = %q, want %q", got, manifest)
		}
	})

	t.Run("directory resolves manifest", func(t *testing.T) {
		dir := t.TempDir()
		manifest := filepath.Join(dir, "putnami.extension.json")
		if err := os.WriteFile(manifest, []byte(`{"name":"x"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := resolveExtensionManifestPath([]string{dir})
		if err != nil {
			t.Fatalf("resolveExtensionManifestPath: %v", err)
		}
		if got != manifest {
			t.Errorf("path = %q, want %q", got, manifest)
		}
	})

	t.Run("missing path", func(t *testing.T) {
		_, err := resolveExtensionManifestPath([]string{filepath.Join(t.TempDir(), "nope")})
		if err == nil || !strings.Contains(err.Error(), "path not found") {
			t.Fatalf("err = %v, want path not found", err)
		}
	})

	t.Run("dir without manifest", func(t *testing.T) {
		_, err := resolveExtensionManifestPath([]string{t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "no putnami.extension.json") {
			t.Fatalf("err = %v, want missing manifest", err)
		}
	})

	t.Run("cwd no manifest", func(t *testing.T) {
		withWorkingDir(t, t.TempDir())
		_, err := resolveExtensionManifestPath(nil)
		if err == nil || !strings.Contains(err.Error(), "no putnami.extension.json in current directory") {
			t.Fatalf("err = %v, want missing manifest in cwd", err)
		}
	})
}

func TestExtensionsValidate_Success(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(validExtensionManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsValidate([]string{dir}) })
	if err != nil {
		t.Fatalf("ExtensionsValidate: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Manifest parsed successfully") {
		t.Errorf("output = %q, want parse success", out)
	}
	if !strings.Contains(out, "Structural validation passed") {
		t.Errorf("output = %q, want structural success", out)
	}
	if !strings.Contains(out, "Extension is valid.") {
		t.Errorf("output = %q, want valid message", out)
	}
}

func TestExtensionsValidate_SchemaFailure(t *testing.T) {
	dir := t.TempDir()
	// Unknown top-level field is rejected by strict parsing.
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(`{"name":"x","bogusField":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsValidate([]string{dir}) })
	if err == nil {
		t.Fatal("expected schema validation error")
	}
	if !strings.Contains(out, "Schema validation failed") {
		t.Errorf("output = %q, want schema failure", out)
	}
}

func TestExtensionsValidate_ReservedGlobalFlagFailure(t *testing.T) {
	dir := t.TempDir()
	manifest := `{
  "name": "@putnami/test-ext",
  "cliContract": 4,
  "commands": {
    "test": {
      "flags": {
        "json": { "type": "boolean" },
        "details": { "type": "boolean", "short": "-v" }
      },
      "run": [{"id": "test", "task": "test-exec"}]
    }
  },
  "tasks": {
    "test-exec": { "kind": "command", "command": "echo" }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsValidate([]string{dir}) })
	if err == nil {
		t.Fatal("expected reserved global flag validation error")
	}
	for _, want := range []string{
		"reserved-global-flag",
		"commands.test.flags.details.short",
		"commands.test.flags.json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// `putnami extensions validate` refuses a command kind, a workload or a batch
// phase outside its closed vocabulary, and names each offending field, so a
// manifest that uses one never earns a contract 11 stamp.
func TestExtensionsValidate_AnUnknownKindWorkloadOrPhaseIsRefused(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "command-kinds-and-traits", "an-unknown-kind-workload-or-phase-is-refused")
	manifest := `{
  "name": "@putnami/test-ext",
  "cliContract": 11,
  "commands": {
    "test": {"traits": {"kind": "report"}, "run": [{"id": "test", "task": "test-exec"}]},
    "serve": {"traits": {"workload": "daemon"}, "run": [{"id": "serve", "task": "test-exec"}]}
  },
  "tasks": {
    "test-exec": {"kind": "command", "command": "echo", "timeoutMs": -1, "batchable": {"tool": "echo", "kind": "files", "phase": "Test"}}
  }
}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsValidate([]string{dir}) })
	if err == nil {
		t.Fatalf("an unknown kind, workload or phase validated:\n%s", out)
	}
	for _, want := range []string{
		"commands.test.traits.kind", "commands.serve.traits.workload",
		"tasks.test-exec.batchable.kind", "tasks.test-exec.batchable.phase",
		"invalid-enum", "invalid-value",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	valid := strings.NewReplacer(`"report"`, `"verification"`, `"daemon"`, `"serve"`, `"files"`, `"diagnostics"`, `"Test"`, `"test"`).Replace(manifest)
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsValidate([]string{dir}) }); err != nil {
		t.Fatalf("the same manifest with known values must validate: %v\n%s", err, out)
	}
}

// `putnami extensions validate` refuses a manifest that names no extension, as
// a required-field error on name, and accepts the same manifest once it has one.
func TestExtensionsValidate_AManifestWithoutANameIsRefused(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-extension-manifest-names-its-extension", "a-manifest-without-a-name-is-refused-by-validation")
	manifest := `{
  "commands": {"build": {"run": [{"id": "build", "task": "build-exec"}]}},
  "tasks": {"build-exec": {"kind": "command", "command": "echo"}}
}`
	dir := t.TempDir()
	path := filepath.Join(dir, "putnami.extension.json")
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsValidate([]string{dir}) })
	if err == nil {
		t.Fatalf("a manifest without a name validated:\n%s", out)
	}
	for _, want := range []string{"name: extension name is required", "required-field"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	named := strings.Replace(manifest, "{\n", "{\n  \"name\": \"@putnami/test-ext\",\n", 1)
	if err := os.WriteFile(path, []byte(named), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsValidate([]string{dir}) }); err != nil {
		t.Fatalf("the same manifest with a name must validate: %v\n%s", err, out)
	}
}

// `putnami extensions validate` refuses a publish channel list that is empty,
// names a channel twice, or sits on a task that does not publish to a
// registry, and names each offending task.
func TestExtensionsValidate_AnInvalidPublishChannelDeclarationIsRefused(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "publish-tasks-declare-their-channels", "an-invalid-publish-channel-declaration-is-refused")
	manifest := `{
  "name": "@putnami/test-ext",
  "cliContract": 12,
  "commands": {
    "publish": {"run": [{"id": "twice", "task": "twice"}, {"id": "none", "task": "none"}, {"id": "local", "task": "local"}]}
  },
  "tasks": {
    "twice": {"kind": "command", "command": "echo", "timeoutMs": -1, "cache": false, "declares": {"effects": ["network", "registry"], "publishChannels": ["npm", "docker", "npm"]}},
    "none": {"kind": "command", "command": "echo", "timeoutMs": -1, "cache": false, "declares": {"effects": ["network", "registry"], "publishChannels": []}},
    "local": {"kind": "command", "command": "echo", "timeoutMs": -1, "cache": false, "declares": {"effects": ["network"], "publishChannels": ["npm"]}}
  }
}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsValidate([]string{dir}) })
	if err == nil {
		t.Fatalf("an invalid publish channel declaration validated:\n%s", out)
	}
	for _, want := range []string{
		"tasks.twice.declares.publishChannels[2]", "duplicate-publish-channel",
		"tasks.none.declares.publishChannels", "invalid-value",
		"tasks.local.declares.publishChannels", "effect-conflict",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	valid := strings.NewReplacer(`["npm", "docker", "npm"]`, `["npm", "docker"]`, `"publishChannels": []`, `"publishChannels": ["docker"]`,
		`"effects": ["network"]`, `"effects": ["network", "registry"]`).Replace(manifest)
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsValidate([]string{dir}) }); err != nil {
		t.Fatalf("the same manifest with valid channel lists must validate: %v\n%s", err, out)
	}
}

// `putnami extensions validate` refuses readByDependents on an output outside
// the project tree: under the workspace root, under the command-output root, or
// in the invocation scratch. It names each offending output, and the same
// outputs under the project root validate.
func TestExtensionsValidate_AReadByDependentsOutputOutsideTheProjectIsRefused(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "dependents-wait-for-an-output-they-read",
		"a-read-by-dependents-output-outside-the-project-is-refused")
	manifest := `{
  "name": "@putnami/test-ext",
  "cliContract": 4,
  "commands": {
    "build": {"run": [{"id": "generate", "task": "generate"}, {"id": "scratch", "task": "scratch"}]}
  },
  "tasks": {
    "generate": {"kind": "command", "command": "echo", "timeoutMs": -1, "cache": false, "declares": {"outputs": {
      "shared": {"kind": "directory", "root": "workspace", "path": "dist/clients", "readByDependents": true},
      "bin": {"kind": "directory", "root": "command-output", "path": "bin", "readByDependents": true}
    }}},
    "scratch": {"kind": "command", "command": "echo", "timeoutMs": -1, "cache": false, "declares": {"outputs": {
      "scratch": {"kind": "directory", "scope": "invocation", "path": "scratch", "readByDependents": true}
    }}}
  }
}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsValidate([]string{dir}) })
	if err == nil {
		t.Fatalf("readByDependents outside the project tree validated:\n%s", out)
	}
	for _, want := range []string{
		"invalid-output-read-by-dependents",
		"tasks.generate.declares.outputs.shared.readByDependents",
		"tasks.generate.declares.outputs.bin.readByDependents",
		"tasks.scratch.declares.outputs.scratch.readByDependents",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	valid := strings.NewReplacer(`"root": "workspace"`, `"root": "project"`, `"root": "command-output"`, `"root": "project"`,
		`"scope": "invocation", `, ``).Replace(manifest)
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsValidate([]string{dir}) }); err != nil {
		t.Fatalf("the same outputs under the project root must validate: %v\n%s", err, out)
	}
}

// `putnami extensions validate` refuses a provisioned candidate that names a
// path, a toolchain that lists two, and one whose lock identity the manifest
// does not answer for, and names each offending candidate.
func TestExtensionsValidate_AnInvalidProvisionedCandidateIsRefused(t *testing.T) {
	manifest := `{
  "name": "@putnami/test-ext",
  "cliContract": 13,
  "commands": {"build": {"run": [{"id": "build", "task": "build"}]}},
  "tasks": {"build": {"kind": "command", "command": "{extensionRuntime}", "toolchains": ["compiler", "linter", "runner"]}},
  "runtime": {
    "executable": "bin/test-ext",
    "resolves": ["compiler", "linter"],
    "toolchains": {
      "compiler": {"lock": "compiler", "candidates": [{"from": "provisioned", "path": "bin/compiler"}], "probe": {"expect": "{version}"}},
      "linter": {"lock": "linter", "candidates": [{"from": "provisioned"}, {"from": "provisioned"}], "probe": {"expect": "{version}"}},
      "runner": {"lock": "runner", "candidates": [{"from": "provisioned"}], "probe": {"expect": "{version}"}}
    }
  }
}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsValidate([]string{dir}) })
	if err == nil {
		t.Fatalf("an invalid provisioned candidate validated:\n%s", out)
	}
	for _, want := range []string{
		"runtime.toolchains.compiler.candidates[0].path", "invalid-value",
		"runtime.toolchains.linter.candidates[1]", "duplicate-value",
		"runtime.toolchains.runner.candidates[0]", "unknown-reference",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	valid := strings.NewReplacer(`{"from": "provisioned", "path": "bin/compiler"}`, `{"from": "provisioned"}`,
		`[{"from": "provisioned"}, {"from": "provisioned"}]`, `[{"from": "provisioned"}]`,
		`"resolves": ["compiler", "linter"]`, `"resolves": ["compiler", "linter", "runner"]`).Replace(manifest)
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsValidate([]string{dir}) }); err != nil {
		t.Fatalf("the same manifest with valid provisioned candidates must validate: %v\n%s", err, out)
	}
}

func TestExtensionsValidate_MissingPath(t *testing.T) {
	err := ExtensionsValidate([]string{filepath.Join(t.TempDir(), "missing")})
	if err == nil || !strings.Contains(err.Error(), "path not found") {
		t.Fatalf("err = %v, want path not found", err)
	}
}
