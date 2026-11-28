package emit

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// cededGenFiles writes, under a member root, each file the generate step
// cedes to other tasks, a file beneath each directory it cedes, and the build
// stamp: files other tasks write, in no order build-emit waits for.
func cededGenFiles(t *testing.T, root, content string) {
	t.Helper()
	for _, ceded := range CededGenPaths {
		target := filepath.Join(root, filepath.FromSlash(ceded))
		if path.Ext(ceded) == "" {
			target = filepath.Join(target, "nested.json")
		}
		writeFile(t, target, content)
	}
	writeFile(t, filepath.Join(root, ".gen", "version.json"), `{"buildTime":"`+content+`"}`)
}

// removeCededGenFiles removes what cededGenFiles wrote.
func removeCededGenFiles(t *testing.T, root string) {
	t.Helper()
	for _, ceded := range CededGenPaths {
		if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(ceded))); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(root, ".gen", "version.json")); err != nil {
		t.Fatal(err)
	}
}

// A manifest holds the .gen files a member's generate step writes, and none
// of those another task writes there or the build stamp, whether the cut lists
// the member's files or the disk walk does, which lists .gen too. Their
// presence and bytes depend on which tasks ran before this one, in this run or
// an earlier one, so holding them would move the test run's key between two
// runs of one tree.
func TestAManifestLeavesOutTheGenFilesOtherTasksWrite(t *testing.T) {
	listings := map[string]func(root string) ([]string, error){
		"the candidate cut": func(root string) ([]string, error) {
			files, err := WalkFiles(root)
			var cut []string
			for _, file := range files {
				if file != ".gen" && !strings.HasPrefix(file, ".gen/") {
					cut = append(cut, file)
				}
			}
			return cut, err
		},
		"the disk walk": WalkFiles,
	}
	for name, list := range listings {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			root := filepath.Join(ws, "app")
			writeFile(t, filepath.Join(root, "package.json"), `{"name":"app"}`)
			writeFile(t, filepath.Join(root, "src", "index.ts"), "export const a = 1\n")
			writeFile(t, filepath.Join(root, ".gen", "generate-result.json"), `{}`)
			writeFile(t, filepath.Join(root, ".gen", "infra", "contract.json"), `{}`)
			members := []Member{{Path: "app", Root: root}}
			manifest := func() string {
				m, err := Build(Options{WorkspaceRoot: ws, Members: members, ListFiles: list, Transpile: sameEmit, Runner: RunnerBun})
				if err != nil {
					t.Fatal(err)
				}
				return encoded(t, m)
			}

			bare := manifest()
			for _, owned := range []string{".gen/generate-result.json", ".gen/infra/contract.json"} {
				if !strings.Contains(bare, `"path": "`+owned+`"`) {
					t.Fatalf("the manifest holds no %s, which generate writes:\n%s", owned, bare)
				}
			}
			cededGenFiles(t, root, "one run")
			if got := manifest(); got != bare {
				t.Fatalf("the files other tasks write under .gen moved the manifest:\n%s\nwant\n%s", got, bare)
			}
			cededGenFiles(t, root, "another run")
			if got := manifest(); got != bare {
				t.Fatalf("new bytes in the files other tasks write under .gen moved the manifest:\n%s\nwant\n%s", got, bare)
			}
			removeCededGenFiles(t, root)
			if got := manifest(); got != bare {
				t.Fatalf("removing the files other tasks write under .gen moved the manifest:\n%s\nwant\n%s", got, bare)
			}
		})
	}
}

// Only a ceded path and what lies beneath it is left out: a sibling whose
// name extends a ceded path's is a file generate writes.
func TestUnownedGenPathMatchesWholeSegments(t *testing.T) {
	for rel, want := range map[string]bool{
		".gen/version.json":             true,
		".gen/requirements.json":        true,
		".gen/layers/base.tar":          true,
		".gen/site-content/index.html":  true,
		".gen/infra/runtime.json":       true,
		".gen/infra/runtime.json.bak":   false,
		".gen/layers-index.json":        false,
		".gen/infra/contract.json":      false,
		".gen/generate-result.json":     false,
		"src/.gen/requirements.json":    false,
		"requirements.json":             false,
		".gen/schema/version.json":      false,
		".gen/config-schema.jsonschema": false,
	} {
		if got := unownedGenPath(rel); got != want {
			t.Errorf("unownedGenPath(%q) = %v, want %v", rel, got, want)
		}
	}
}
