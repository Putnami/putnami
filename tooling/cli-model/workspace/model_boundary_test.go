package workspace

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The module's stated guarantee (README): the model requires ONLY
// go.putnami.dev/protocol/* modules, and cannot import the CLI that drives it.
// These tests turn that guarantee from prose into a gate: a require line
// outside the protocol namespace, or one source import of tooling/cli, fails
// here before the build ever gets to prove it the hard way.

// moduleRoot walks up from this package to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the workspace package")
		}
		dir = parent
	}
}

func TestModuleRequiresOnlyProtocolModules(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "model-purity", "the-module-requires-only-protocol-modules")

	data, err := os.ReadFile(filepath.Join(moduleRoot(t), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}

	requires := 0
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		var spec string
		switch {
		case strings.HasPrefix(trimmed, "require ("):
			inBlock = true
			continue
		case inBlock && trimmed == ")":
			inBlock = false
			continue
		case inBlock && trimmed != "" && !strings.HasPrefix(trimmed, "//"):
			spec = trimmed
		case strings.HasPrefix(trimmed, "require "):
			spec = strings.TrimPrefix(trimmed, "require ")
		default:
			continue
		}
		module := strings.Fields(spec)[0]
		if !strings.HasPrefix(module, "go.putnami.dev/protocol/") {
			t.Errorf("go.mod requires %q — the model may require only go.putnami.dev/protocol/* modules", module)
		}
		requires++
	}
	if requires == 0 {
		t.Fatal("no require line was checked; the guarantee check is vacuous")
	}
}

func TestNoSourceImportsTheCLI(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "model-purity", "no-source-imports-the-cli")

	root := moduleRoot(t)
	checked := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if name := entry.Name(); name == ".git" || name == "doc" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		//nolint:gosec // the walk only reaches this module's committed sources
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		// The needle is split so this file's own literal never trips the walk.
		forbidden := `"go.putnami.dev/` + `tooling/cli`
		if strings.Contains(string(data), forbidden) {
			t.Errorf("%s imports the CLI — the model must never reach back into the engine that drives it", path)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The module carries three packages of real sources; a low count means the
	// walk missed them and the boundary check proved nothing.
	if checked < 50 {
		t.Fatalf("only %d Go files were checked; the boundary walk is not reaching the module's sources", checked)
	}
}
