package toolchain

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// scaffoldModule writes a minimal go.mod declaring modulePath at
// workspaceRoot/rel/go.mod so WorkspaceGoModules can resolve the member.
func scaffoldModule(t *testing.T, root, rel, modulePath string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := "module " + modulePath + "\n\ngo 1.24\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
}

func TestWorkspaceGoModules(t *testing.T) {
	tests := []struct {
		name    string
		goWork  string
		members map[string]string // rel dir -> declared module path
		want    []string
	}{
		{
			name:    "block form",
			goWork:  "go 1.24\n\nuse (\n\t./a\n\t./b\n)\n",
			members: map[string]string{"a": "example.com/a", "b": "example.com/b"},
			want:    []string{"example.com/a", "example.com/b"},
		},
		{
			// Regression for the single-line `use ./dir` form. A prior
			// block-only parser silently ignored it, so
			// updateDependencyVersions no-oped and the published module kept
			// stale intra-workspace version pins.
			name:    "single-line form",
			goWork:  "go 1.24\n\nuse ./svc\n",
			members: map[string]string{"svc": "example.com/svc"},
			want:    []string{"example.com/svc"},
		},
		{
			name:    "mixed single-line and block",
			goWork:  "go 1.24\n\nuse ./root\n\nuse (\n\t./a\n\t./b\n)\n",
			members: map[string]string{"root": "example.com/root", "a": "example.com/a", "b": "example.com/b"},
			want:    []string{"example.com/root", "example.com/a", "example.com/b"},
		},
		{
			name:    "trailing comment stripped",
			goWork:  "go 1.24\n\nuse ./svc // primary service\n",
			members: map[string]string{"svc": "example.com/svc"},
			want:    []string{"example.com/svc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "go.work"), []byte(tt.goWork), 0o644); err != nil {
				t.Fatalf("write go.work: %v", err)
			}
			for rel, mod := range tt.members {
				scaffoldModule(t, root, rel, mod)
			}
			got := WorkspaceGoModules(root)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("WorkspaceGoModules() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWorkspaceGoModules_MissingGoWork(t *testing.T) {
	if got := WorkspaceGoModules(t.TempDir()); got != nil {
		t.Fatalf("WorkspaceGoModules() = %v, want nil for missing go.work", got)
	}
}
