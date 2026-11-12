package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitLeavesALinkedFileAlone: a README.md, .gitignore or .gitattributes
// that is a symbolic link is the team's, even when its target does not
// resolve. vercel/ai links README.md to "packages/ai/README.md\n", target
// newline included: init must not write through the link and create that
// file.
func TestInitLeavesALinkedFileAlone(t *testing.T) {
	for _, name := range []string{"README.md", ".gitignore", ".gitattributes"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			// The directory exists, as in vercel/ai, so a write through the
			// link would succeed.
			target := "packages/ai/" + name + "\n"
			if err := os.MkdirAll(filepath.Join(dir, "packages", "ai"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			stubInitFrom(t, true, "")
			out, err := runInit(t)
			if err != nil {
				t.Fatalf("WorkspaceInit: %v\n%s", err, out)
			}
			if got, err := os.Readlink(filepath.Join(dir, name)); err != nil || got != target {
				t.Errorf("%s = %q, %v; want the link to %q kept", name, got, err, target)
			}
			if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(target))); !os.IsNotExist(err) {
				t.Errorf("init wrote through the link to %q (lstat error: %v)", target, err)
			}
			if strings.Contains(out, name+" created") {
				t.Errorf("init reports %s created:\n%s", name, out)
			}
		})
	}
}
