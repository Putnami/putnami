package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The wire-only requirement moved here with the parity recording (decision 8
// of the spec rollout): the extension binary must not import this CLI's
// internal packages, must not replace into tooling/cli, and must not exec
// putnami at run time. The CLI is the side that can observe the boundary —
// the extension cannot assert what it does not depend on without creating
// exactly the dependency the rule forbids.
func TestSDDExtensionSpeaksOnlyTheWire(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..", "sdd-extension")

	spectest.Proves(t, "cli/sdd-extraction", "wire-only-workspace-knowledge", "the-extension-module-never-replaces-into-tooling-cli")
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read sdd-extension go.mod: %v", err)
	}
	for _, forbidden := range []string{"tooling/cli", "go.putnami.dev/cli-model"} {
		if strings.Contains(string(gomod), forbidden) {
			t.Errorf("sdd-extension go.mod references %q; the extension knows the workspace only through the wire", forbidden)
		}
	}

	spectest.Proves(t, "cli/sdd-extraction", "wire-only-workspace-knowledge", "the-extension-source-never-imports-cli-internals-or-execs-putnami")
	var checked int
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "compiled" || name == "doc" || name == ".gen" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(body)
		checked++
		if strings.Contains(src, `"go.putnami.dev/tooling/cli/`) || strings.Contains(src, `go.putnami.dev/cli/internal`) {
			t.Errorf("%s imports a CLI internal package", path)
		}
		// exec.Command("putnami", …) in any spelling: the binary re-entering the
		// CLI would re-derive workspace facts the wire deliberately owns.
		if strings.Contains(src, `exec.Command("putnami"`) || strings.Contains(src, `exec.CommandContext(ctx, "putnami"`) {
			t.Errorf("%s execs putnami at run time", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk sdd-extension: %v", err)
	}
	if checked < 50 {
		t.Fatalf("only %d Go files inspected; the sweep looks vacuous", checked)
	}
}
