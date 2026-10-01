package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/exec"
)

// writeInstalledTypeScript lays out node_modules/typescript/package.json under
// dir with the given version, the file package resolution reads.
func writeInstalledTypeScript(t *testing.T, dir, version string) {
	t.Helper()
	pkgDir := filepath.Join(dir, "node_modules", "typescript")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"name":"typescript","version":"` + version + `"}`
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// RunTypes passes the --ignoreDeprecations value the TypeScript that runs
// accepts. A fixed "6.0" made every build~types of a fresh workspace fail with
// TS5103 on the TypeScript 5.9 that workspace-install added.
func TestRunTypesPassesTheIgnoreDeprecationsTheInstalledTypeScriptAccepts(t *testing.T) {
	cases := []struct {
		name        string
		rootVersion string // TypeScript installed at the workspace root, "" for none
		projVersion string // TypeScript installed in the project, "" for none
		want        string // expected --ignoreDeprecations value, "" for no flag
	}{
		{name: "typescript 5.9 at the root", rootVersion: "5.9.3", want: "5.0"},
		{name: "typescript 6.0 at the root", rootVersion: "6.0.2", want: "6.0"},
		{name: "the project's own typescript wins", rootVersion: "6.0.2", projVersion: "5.9.3", want: "5.0"},
		{name: "unknown major", rootVersion: "7.0.2", want: ""},
		{name: "unparsable version", rootVersion: "next", want: ""},
		{name: "no typescript installed", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			projectPath := filepath.Join(root, "apps", "web")
			if err := os.MkdirAll(filepath.Join(projectPath, "src"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("export const a = 1;\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if c.rootVersion != "" {
				writeInstalledTypeScript(t, root, c.rootVersion)
			}
			if c.projVersion != "" {
				writeInstalledTypeScript(t, projectPath, c.projVersion)
			}

			var got []string
			withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
				got = args
				return &exec.Result{Success: true}, nil
			})
			if _, err := RunTypes("bun", projectPath, filepath.Join(t.TempDir(), "types"), ""); err != nil {
				t.Fatalf("RunTypes: %v", err)
			}
			if got == nil {
				t.Fatal("tsc was not invoked")
			}
			if c.want == "" {
				if argsHaveFlag(got, "--ignoreDeprecations") {
					t.Fatalf("args carry --ignoreDeprecations %q, want no flag: %v", argValueOf(got, "--ignoreDeprecations"), got)
				}
				return
			}
			if v := argValueOf(got, "--ignoreDeprecations"); v != c.want {
				t.Fatalf("--ignoreDeprecations = %q, want %q: %v", v, c.want, got)
			}
		})
	}
}

// The TypeScript range workspace-install adds to a workspace must resolve to a
// compiler whose --ignoreDeprecations value is known, so a fresh workspace's
// build~types passes a value its compiler accepts. Bumping the range to a new
// major without teaching ignoreDeprecationsByMajor that major fails here.
func TestWorkspaceInstallTypeScriptRangeHasAKnownIgnoreDeprecations(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest struct {
		WorkspaceDevDependencies map[string]string `json:"workspaceDevDependencies"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse extension manifest: %v", err)
	}
	rangeSpec, ok := manifest.WorkspaceDevDependencies["typescript"]
	if !ok {
		t.Fatal("workspaceDevDependencies has no typescript entry")
	}
	// Only a caret range keeps the major fixed; any other form could install
	// a major this table does not know.
	floor, ok := strings.CutPrefix(rangeSpec, "^")
	if !ok {
		t.Fatalf("workspaceDevDependencies.typescript = %q, want a caret range that pins the major", rangeSpec)
	}
	head, _, _ := strings.Cut(floor, ".")
	major, err := strconv.Atoi(head)
	if err != nil || major == 0 {
		t.Fatalf("workspaceDevDependencies.typescript = %q: no major version", rangeSpec)
	}
	if ignoreDeprecationsByMajor[major] == "" {
		t.Fatalf("workspaceDevDependencies.typescript = %q installs TypeScript %d, which has no --ignoreDeprecations value", rangeSpec, major)
	}
}
