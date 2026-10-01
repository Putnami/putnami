package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// THE OLD/NEW EQUIVALENCE CORPUS.
//
// This table is the migrated workspace-graph guard corpus — the cases that pinned the
// CLI's `deriveGoDependencies` when Go workspace edges stopped being derivable
// from hand-declared putnami.json dependencies and started being derived from
// the module graph. It exists TWICE on purpose, once here and once in
// tooling/cli/internal/workspace/go_probe_equivalence_test.go, because an
// equivalence suite needs two independent implementations reading ONE set of
// facts:
//
//   - here it is fed to the workspace probe, which answers in repo-relative
//     PATHS;
//   - there it is fed to core's discovery + `deriveGoDependencies`, which
//     answers in project NAMES.
//
// The two copies were pinned together by goCorpusFingerprint while both existed.
// Deleting the core parser removed its copy of the graph corpus with it, so
// this is now the SOLE conformance suite for "what does a go.mod say about this
// project"; the fingerprint stays as the pin for the CLOSURE corpus, whose twin
// (tooling/cli/internal/workspace/gomod_bootstrap_test.go) survives for core's
// bootstrap residue.
//
// Every case is a directory tree of real go.mod files rather than a struct
// literal. The old core tests built `*Project` values with a pre-parsed GoMod
// field, which meant the parser and the derivation were never exercised
// together; reading the bytes is what makes this corpus able to catch a parser
// change that only shows up as a missing edge — the silent-edge-loss failure shape, where
// an undeclared workspace dependency silently stops keying the dependent's
// build.

// goCorpusCase is one equivalence fixture.
type goCorpusCase struct {
	// Name identifies the case.
	Name string `json:"name"`
	// Files are repo-relative files written into the fixture tree.
	Files map[string]string `json:"files"`
	// Candidates are the repo-relative directories core knows about — the probe
	// request's Paths, and core's config includes.
	Candidates []string `json:"candidates"`
	// Explicit are the putnami.json dependencies authored per project. The probe
	// never reports them (core owns explicit identity and unions it in); core's
	// derivation unions them into Project.Dependencies.
	Explicit map[string][]string `json:"explicit,omitempty"`
	// Expect maps a project path to the dependency PATHS the module graph
	// derives for it, sorted. Explicit dependencies are deliberately absent.
	Expect map[string][]string `json:"expect"`
}

// goCorpusFingerprint is the canonical digest of goCorpus().
//
// It pinned both sides of the old/new equivalence while both existed. That migration
// deleted core's parser and its copy of this corpus, so what the pin protects now
// is the corpus ITSELF: this table is the sole conformance suite for "what does a
// go.mod say about this project", and a case silently edited or dropped from it
// would leave a passing suite proving less than it did yesterday.
const goCorpusFingerprint = "c6ec4213da5cf00212eac5b5a15b046276af5002ec1223b6fc1ca4ec0a671a9e"

// goCorpus is the migrated workspace-graph corpus.
func goCorpus() []goCorpusCase {
	return []goCorpusCase{
		{
			// The original end-to-end case: a Go project requires two workspace
			// modules — one of them `// indirect` — and replaces one of them
			// with a local path pointing at a THIRD workspace project. All three
			// edges must resolve, sorted and deduped, with no self-edge.
			Name: "module-index-and-local-replace",
			Files: map[string]string{
				"app/go.mod": "module example.com/app\n\ngo 1.25\n\n" +
					"require (\n\texample.com/liba v0.0.0\n\texample.com/libb v0.0.0 // indirect\n)\n\n" +
					"replace example.com/liba => ../libc\n",
				"liba/go.mod": "module example.com/liba\n\ngo 1.25\n",
				"libb/go.mod": "module example.com/libb\n\ngo 1.25\n",
				"libc/go.mod": "module example.com/libc\n\ngo 1.25\n",
			},
			Candidates: []string{"app", "liba", "libb", "libc"},
			Expect: map[string][]string{
				"app":  {"liba", "libb", "libc"},
				"liba": {},
				"libb": {},
				"libc": {},
			},
		},
		{
			// UNION, NEVER REPLACE. Explicit putnami.json dependencies survive
			// derivation: one of them also appears in the module graph (must
			// dedupe) and one is an ID-form edge with no module relationship at
			// all (must be kept). The probe reports only the derived half —
			// core owns explicit identity and unions it in the merge.
			Name: "explicit-dependencies-are-unioned",
			Files: map[string]string{
				"app/go.mod": "module example.com/app\n\ngo 1.25\n\n" +
					"require (\n\texample.com/liba v0.0.0\n\texample.com/libb v0.0.0\n)\n",
				"liba/go.mod":        "module example.com/liba\n\ngo 1.25\n",
				"libb/go.mod":        "module example.com/libb\n\ngo 1.25\n",
				"infra/putnami.json": "{}\n",
			},
			Candidates: []string{"app", "infra", "liba", "libb"},
			Explicit:   map[string][]string{"app": {"example.com/liba", "/infra"}},
			Expect: map[string][]string{
				"app":   {"liba", "libb"},
				"infra": {},
				"liba":  {},
				"libb":  {},
			},
		},
		{
			// A project never depends on itself, external modules are ignored,
			// and a local replace that escapes the workspace root resolves to
			// nothing rather than to a path outside the repository.
			Name: "self-and-external-are-excluded",
			Files: map[string]string{
				"app/go.mod": "module example.com/app\n\ngo 1.25\n\n" +
					"require (\n\texample.com/app v0.0.0\n\tgithub.com/stretchr/testify v1.9.0\n" +
					"\texample.com/lib v0.0.0\n)\n\n" +
					"replace example.com/escapes => ../../../outside\n",
				"lib/go.mod": "module example.com/lib\n\ngo 1.25\n",
			},
			Candidates: []string{"app", "lib"},
			Expect: map[string][]string{
				"app": {"lib"},
				"lib": {},
			},
		},
		{
			// Go map iteration is random and this list feeds the build cache
			// key, so the derived edges must come out sorted whatever order the
			// requires were authored in.
			Name: "derived-edges-are-sorted",
			Files: map[string]string{
				"app/go.mod": "module example.com/app\n\ngo 1.25\n\n" +
					"require (\n\texample.com/libz v0.0.0\n\texample.com/liba v0.0.0\n" +
					"\texample.com/libm v0.0.0\n)\n",
				"liba/go.mod": "module example.com/liba\n\ngo 1.25\n",
				"libm/go.mod": "module example.com/libm\n\ngo 1.25\n",
				"libz/go.mod": "module example.com/libz\n\ngo 1.25\n",
			},
			Candidates: []string{"app", "liba", "libm", "libz"},
			Expect: map[string][]string{
				"app":  {"liba", "libm", "libz"},
				"liba": {},
				"libm": {},
				"libz": {},
			},
		},
		{
			// A directory with no go.mod is not a Go project: the probe claims
			// nothing there and core's derivation leaves its declared
			// dependencies exactly as authored.
			Name: "non-go-projects-are-untouched",
			Files: map[string]string{
				"ui/putnami.json":  "{}\n",
				"web/putnami.json": "{}\n",
			},
			Candidates: []string{"ui", "web"},
			Explicit:   map[string][]string{"web": {"@acme/ui"}},
			Expect:     map[string][]string{"ui": {}, "web": {}},
		},
		{
			// DERIVATION IS NOT TRANSITIVE, and depth does not matter. app
			// requires config, config requires (indirectly) the protocol and
			// reaches it through a three-level-up local replace. app gains ONE
			// edge, not two — the transitive closure is the replace codemod's
			// job, not the graph's.
			Name: "nested-depth-and-no-transitive-edge",
			Files: map[string]string{
				"go/framework/app/go.mod": "module example.com/app\n\ngo 1.25\n\n" +
					"require example.com/config v0.0.1\n",
				"go/framework/config/go.mod": "module example.com/config\n\ngo 1.25\n\n" +
					"require (\n\texample.com/protocol/config v0.0.0 // indirect\n)\n\n" +
					"replace example.com/protocol/config => ../../../protocols/config\n",
				"protocols/config/go.mod": "module example.com/protocol/config\n\ngo 1.25\n",
			},
			Candidates: []string{"go/framework/app", "go/framework/config", "protocols/config"},
			Expect: map[string][]string{
				"go/framework/app":    {"go/framework/config"},
				"go/framework/config": {"protocols/config"},
				"protocols/config":    {},
			},
		},
		{
			// A module→module replace retargets a require onto another
			// workspace module. The require itself names a module no project
			// answers for, so the ONLY signal is the replace's right-hand side.
			Name: "module-target-replace-resolves-through-the-module-index",
			Files: map[string]string{
				"app/go.mod": "module example.com/app\n\ngo 1.25\n\n" +
					"require example.com/fake v0.0.0\n\n" +
					"replace example.com/fake => example.com/real v1.0.0\n",
				"real/go.mod": "module example.com/real\n\ngo 1.25\n",
			},
			Candidates: []string{"app", "real"},
			Expect: map[string][]string{
				"app":  {"real"},
				"real": {},
			},
		},
		{
			// NEGATIVE STALE-KEY CASE. A local replace can point at a directory
			// that is a workspace project but carries no go.mod of its own (a
			// schema or fixture directory a module vendors by path). Core
			// indexes EVERY project by directory, so it produces this edge; a
			// probe that indexed only Go directories would silently drop it and
			// the dependent's cache key would stop observing the target — the
			// silent-edge-loss failure, reintroduced by the migration itself.
			Name: "local-replace-to-a-non-go-candidate-still-links",
			Files: map[string]string{
				"app/go.mod": "module example.com/app\n\ngo 1.25\n\n" +
					"replace example.com/schema => ../schema\n",
				"schema/putnami.json": "{}\n",
			},
			Candidates: []string{"app", "schema"},
			Expect: map[string][]string{
				"app":    {"schema"},
				"schema": {},
			},
		},
	}
}

// writeGoCorpusCase materializes one case's tree under root and returns root.
func writeGoCorpusCase(t *testing.T, root string, c goCorpusCase) {
	t.Helper()
	for rel, content := range c.Files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// goCorpusDigest canonically digests the corpus. Maps are encoded by
// encoding/json in sorted key order and every list is sorted first, so the
// digest is a function of the FACTS rather than of the authoring order — the
// same rule the probe result digest follows.
func goCorpusDigest(cases []goCorpusCase) string {
	normalized := make([]goCorpusCase, len(cases))
	copy(normalized, cases)
	for i := range normalized {
		normalized[i].Candidates = sortedCopy(normalized[i].Candidates)
		normalized[i].Explicit = sortedLists(normalized[i].Explicit)
		normalized[i].Expect = sortedLists(normalized[i].Expect)
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Name < normalized[j].Name })
	encoded, err := json.Marshal(normalized)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func sortedLists(in map[string][]string) map[string][]string {
	if in == nil {
		return nil
	}
	out := make(map[string][]string, len(in))
	for key, value := range in {
		out[key] = sortedCopy(value)
	}
	return out
}

// TestGoCorpusFingerprint pins the corpus itself.
//
// Its twin in core (tooling/cli/internal/workspace/go_probe_equivalence_test.go)
// was deleted with the parser it checked; this table is what is
// left, and it is now the only place these cases are asserted. A case
// weakened or dropped from it costs nothing at the time and everything the next
// time an edge stops being derived, so the digest has to move ON PURPOSE.
func TestGoCorpusFingerprint(t *testing.T) {
	if got := goCorpusDigest(goCorpus()); got != goCorpusFingerprint {
		t.Fatalf("corpus fingerprint = %q, want %q\n"+
			"the workspace-graph conformance corpus changed; if the edit is intended, update "+
			"goCorpusFingerprint in this file and say in the commit which case moved "+
			"and why the suite still covers what it covered before", got, goCorpusFingerprint)
	}
}

// --- planned native-manifest edits --------------------------------------

// goClosureCase is one replace-closure fixture: a tree, and the go.mod bytes
// the codemod must produce from it.
//
// The migration owes old/new equivalence over "normalized project graphs AND
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
