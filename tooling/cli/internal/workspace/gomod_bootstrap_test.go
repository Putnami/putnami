package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// THE BOOTSTRAP CLOSURE CORPUS.
//
// The GRAPH half of the old/new equivalence suite that used to live beside this
// table is gone, and deliberately so: it fed the fixtures to `parseGoMod` and
// `deriveGoDependencies`, and slice C4b deleted both. The corpus itself did not
// disappear — it lives on as the Go extension's own conformance suite
// (go/extension/cmd/putnami-go/workspace_corpus_test.go), which is now the sole
// owner of "what does a go.mod say about this project".
//
// The EDIT half stays here, because core still performs one edit: the bootstrap
// repair of the workspace-replace closure for the extension modules whose
// runtime it has to prepare before any extension can run (see
// gomod_bootstrap.go). Core's copy and the extension's must stay byte-for-byte
// identical or a `projects sync` would oscillate between two spellings of the
// same file, so the fixtures and their fingerprint remain twinned.

// The epic asks for old/new equivalence over "normalized project graphs AND
// planned native-manifest edits" before any core implementation is deleted.
// The graph half is goCorpus above; this is the edit half. It is byte-exact on
// purpose: the codemod appends to files a human maintains, so "the right
// modules got replaced" is not enough — the surrounding bytes, the ordering and
// the trailing newline all have to survive, or every sync produces a diff.
type goClosureCase struct {
	Name  string              `json:"name"`
	Files map[string]string   `json:"files"`
	Added map[string][]string `json:"added"`
	Want  map[string]string   `json:"want"`
}

// goClosureFingerprint pins this table against its twin. It must equal the
// constant of the same name on the other side.
const goClosureFingerprint = "ce84ad46b9afcc748b55d169d6904a40c90ef93b2b743d42391b1a15e84b3bb2"

func goClosureCorpus() []goClosureCase {
	return []goClosureCase{
		{
			// The transitive chain app → config → protocol: app must gain TWO
			// replaces even though it never requires the protocol directly, and
			// the protocol target sits three levels up. Members whose closure is
			// already complete must come out byte-for-byte untouched.
			Name: "transitive-closure-across-depths",
			Files: map[string]string{
				"go.work": "go 1.26\n\nuse (\n\t./go/framework/app\n" +
					"\t./go/framework/config // a comment that must be stripped\n)\n\nuse ./protocols/config\n",
				"go/framework/app/go.mod": "module example.com/app\n\ngo 1.26\n\n" +
					"require example.com/config v0.0.1\n",
				"go/framework/config/go.mod": "module example.com/config\n\ngo 1.26\n\n" +
					"require (\n\texample.com/protocol/config v0.0.0 // indirect\n)\n\n" +
					"replace example.com/protocol/config => ../../../protocols/config\n",
				"protocols/config/go.mod": "module example.com/protocol/config\n\ngo 1.26\n",
			},
			Added: map[string][]string{
				"go/framework/app": {"example.com/config", "example.com/protocol/config"},
			},
			Want: map[string]string{
				"go/framework/app/go.mod": "module example.com/app\n\ngo 1.26\n\n" +
					"require example.com/config v0.0.1\n\n" +
					"replace example.com/config => ../config\n" +
					"replace example.com/protocol/config => ../../../protocols/config\n",
				"go/framework/config/go.mod": "module example.com/config\n\ngo 1.26\n\n" +
					"require (\n\texample.com/protocol/config v0.0.0 // indirect\n)\n\n" +
					"replace example.com/protocol/config => ../../../protocols/config\n",
				"protocols/config/go.mod": "module example.com/protocol/config\n\ngo 1.26\n",
			},
		},
		{
			// APPEND ONLY. A hand-maintained replace with no matching require is
			// preserved verbatim, and a replace already inside a `replace (...)`
			// block satisfies the closure so only the genuinely missing
			// directive is written.
			Name: "existing-directives-are-never-rewritten",
			Files: map[string]string{
				"go.work": "go 1.26\n\nuse (\n\t./a\n\t./b\n\t./c\n)\n",
				"a/go.mod": "module example.com/a\n\ngo 1.26\n\n" +
					"require (\n\texample.com/b v0.0.0\n\texample.com/c v0.0.0 // indirect\n)\n\n" +
					"replace (\n\texample.com/b => ../b\n\texample.com/schema => ../schema\n)\n",
				"b/go.mod": "module example.com/b\n\ngo 1.26\n",
				"c/go.mod": "module example.com/c\n\ngo 1.26\n",
			},
			Added: map[string][]string{"a": {"example.com/c"}},
			Want: map[string]string{
				"a/go.mod": "module example.com/a\n\ngo 1.26\n\n" +
					"require (\n\texample.com/b v0.0.0\n\texample.com/c v0.0.0 // indirect\n)\n\n" +
					"replace (\n\texample.com/b => ../b\n\texample.com/schema => ../schema\n)\n\n" +
					"replace example.com/c => ../c\n",
				"b/go.mod": "module example.com/b\n\ngo 1.26\n",
				"c/go.mod": "module example.com/c\n\ngo 1.26\n",
			},
		},
	}
}

func goClosureDigest(cases []goClosureCase) string {
	normalized := make([]goClosureCase, len(cases))
	copy(normalized, cases)
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Name < normalized[j].Name })
	encoded, err := json.Marshal(normalized)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// TestBootstrapClosureProducesTheCorpusEdits is core's side of the
// native-manifest-edit equivalence: the same fixtures through
// `RepairBootstrapGoModClosure`, asserting the same byte-exact output its twin
// (TestSyncGoWorkspace_ProducesTheCorpusEdits in the Go extension) asserts.
//
// Byte-exactness is the requirement, not a nicety. Two implementations write
// these files — core's, before an extension runtime can be built, and the Go
// extension's, on every `projects sync` — so any difference in the bytes they
// produce would make one undo the other on alternate runs, and the tree would
// never converge.
func TestBootstrapClosureProducesTheCorpusEdits(t *testing.T) {
	if got := goClosureDigest(goClosureCorpus()); got != goClosureFingerprint {
		t.Fatalf("closure corpus fingerprint = %q, want %q\n"+
			"apply the SAME edit to its twin in "+
			"go/extension/cmd/putnami-go/workspace_corpus_test.go and update "+
			"goClosureFingerprint in both files", got, goClosureFingerprint)
	}

	for _, c := range goClosureCorpus() {
		t.Run(c.Name, func(t *testing.T) {
			ws := t.TempDir()
			for rel, content := range c.Files {
				writeFile(t, filepath.Join(ws, filepath.FromSlash(rel)), content)
			}

			// Dry-run first: it must report exactly what the real run does and
			// leave every byte alone.
			dirs := corpusModuleDirs(c)
			planned, err := RepairBootstrapGoModClosure(ws, dirs, true)
			if err != nil {
				t.Fatalf("RepairBootstrapGoModClosure dry-run: %v", err)
			}
			for rel, content := range c.Files {
				data, readErr := os.ReadFile(filepath.Join(ws, filepath.FromSlash(rel)))
				if readErr != nil || string(data) != content {
					t.Fatalf("dry-run modified %s", rel)
				}
			}

			changes, err := RepairBootstrapGoModClosure(ws, dirs, false)
			if err != nil {
				t.Fatalf("RepairBootstrapGoModClosure: %v", err)
			}
			if len(planned) != len(changes) {
				t.Errorf("dry-run reported %d changes, the real run made %d", len(planned), len(changes))
			}
			added := make(map[string][]string, len(changes))
			for _, change := range changes {
				added[change.Dir] = change.Added
			}
			if len(added) != len(c.Added) {
				t.Fatalf("changed modules = %v, want %v", added, c.Added)
			}
			for dir, want := range c.Added {
				if !slices.Equal(added[dir], want) {
					t.Errorf("%s added = %v, want %v", dir, added[dir], want)
				}
			}

			for rel, want := range c.Want {
				data, readErr := os.ReadFile(filepath.Join(ws, filepath.FromSlash(rel)))
				if readErr != nil {
					t.Fatal(readErr)
				}
				if string(data) != want {
					t.Errorf("%s:\n%s\nwant:\n%s", rel, data, want)
				}
			}

			// IDEMPOTENCE: a converged tree writes nothing on the next run.
			again, err := RepairBootstrapGoModClosure(ws, dirs, false)
			if err != nil {
				t.Fatalf("second run: %v", err)
			}
			if len(again) != 0 {
				t.Errorf("second run reported %+v, want none (idempotent)", again)
			}
		})
	}
}

// corpusModuleDirs is every go.work member directory in a fixture. The
// bootstrap repair writes only what it is asked to, so the corpus — which
// asserts the full closure — has to ask for all of them.
func corpusModuleDirs(c goClosureCase) []string {
	dirs := make([]string, 0, len(c.Want))
	for rel := range c.Want {
		dirs = append(dirs, filepath.ToSlash(filepath.Dir(rel)))
	}
	sort.Strings(dirs)
	return dirs
}

// THE NARROWING IS THE POINT. Core keeps this repair only because an extension
// runtime cannot be compiled before its own module's closure is complete; every
// other module belongs to the Go extension's `workspace-sync` task. A core
// repair that wrote outside the directories it was handed would be a second
// closure maintainer, and two maintainers of one append-only codemod is how a
// file starts gaining directives nobody asked for.
func TestBootstrapClosureWritesOnlyTheNamedModules(t *testing.T) {
	ws := t.TempDir()
	files := map[string]string{
		"go.work":      "go 1.26\n\nuse (\n\t./ext\n\t./other\n\t./lib\n)\n",
		"ext/go.mod":   "module example.com/ext\n\ngo 1.26\n\nrequire example.com/lib v0.0.0\n",
		"other/go.mod": "module example.com/other\n\ngo 1.26\n\nrequire example.com/lib v0.0.0\n",
		"lib/go.mod":   "module example.com/lib\n\ngo 1.26\n",
	}
	for rel, content := range files {
		writeFile(t, filepath.Join(ws, filepath.FromSlash(rel)), content)
	}

	changes, err := RepairBootstrapGoModClosure(ws, []string{"ext"}, false)
	if err != nil {
		t.Fatalf("RepairBootstrapGoModClosure: %v", err)
	}
	if len(changes) != 1 || changes[0].Dir != "ext" {
		t.Fatalf("changes = %+v, want only ext", changes)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "other", "go.mod")); string(data) != files["other/go.mod"] {
		t.Errorf("other/go.mod was rewritten; the bootstrap repair must touch only the modules it is handed:\n%s", data)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "ext", "go.mod")); !slices.Contains(
		[]string{files["ext/go.mod"] + "\nreplace example.com/lib => ../lib\n"}, string(data)) {
		t.Errorf("ext/go.mod = %q, want the missing replace appended", data)
	}
}

// An empty selection is a no-op: a run that has no local extension to prepare
// must not walk go.work, and must certainly not write anything.
func TestBootstrapClosure_NoModulesNamedIsNoOp(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "go.work"), "go 1.26\n\nuse ./a\n")
	writeFile(t, filepath.Join(ws, "a", "go.mod"), "module example.com/a\n\ngo 1.26\n")

	changes, err := RepairBootstrapGoModClosure(ws, nil, false)
	if err != nil || len(changes) != 0 {
		t.Fatalf("changes = %+v err = %v, want none", changes, err)
	}
}

// A workspace without go.work is a no-op, not a failure: a repository with no
// Go modules must still be able to run `projects sync`.
func TestBootstrapClosure_NoGoWorkIsNoOp(t *testing.T) {
	changes, err := RepairBootstrapGoModClosure(t.TempDir(), []string{"a"}, false)
	if err != nil {
		t.Fatalf("RepairBootstrapGoModClosure without go.work: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none", changes)
	}
}
