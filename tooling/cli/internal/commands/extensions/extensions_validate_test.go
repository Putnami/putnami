package extensions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestExtensionsValidate_MissingPath(t *testing.T) {
	err := ExtensionsValidate([]string{filepath.Join(t.TempDir(), "missing")})
	if err == nil || !strings.Contains(err.Error(), "path not found") {
		t.Fatalf("err = %v, want path not found", err)
	}
}
