package inputresolve

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/goembed"
)

// closure says when a mutation's fresh answer is an error that the core's
// own selection does not raise: a read the basis digest cannot see.
type closure int

const (
	// neverClosed: the fresh answer is an error exactly when the core's
	// selection is.
	neverClosed closure = iota
	// alwaysClosed: the fresh answer is an error, and the core's selection
	// succeeds.
	alwaysClosed
	// closedIfCaseInsensitive: alwaysClosed on a case-insensitive file
	// system, neverClosed on another.
	closedIfCaseInsensitive
)

// closes reports whether a mutation of closure c makes the fresh answer an
// error the core's selection does not raise, on a file system that matches
// names without letter case when insensitive is true.
func (c closure) closes(insensitive bool) bool {
	switch c {
	case neverClosed:
		return false
	case alwaysClosed:
		return true
	case closedIfCaseInsensitive:
		return insensitive
	}
	panic(fmt.Sprintf("closure %d is not declared", c))
}

// mutation is one write to a fixture tree.
type mutation struct {
	name   string
	apply  func(t *testing.T, root string)
	closed closure
	// keeps names the selectors whose basis digest the mutation must leave
	// unchanged: a write the selection does not read.
	keeps []string
}

var bothSelectors = []string{goembed.BuildSelector, goembed.TestSelector}

// propertyCorpus is a fixture, the queries asked of it, and the mutations
// applied to it, one fresh tree per mutation.
type propertyCorpus struct {
	name      string
	fixture   func(t *testing.T, root string)
	queries   []wsproto.InputResolveQuery
	mutations []mutation
}

var buildPatterns = []string{"**/*.go", "!**/*_test.go", "!**/.gen/**"}
var testPatterns = []string{"**/*.go", "!**/.gen/**"}

func appCorpus() propertyCorpus {
	return propertyCorpus{
		name:    "app",
		fixture: appFixture,
		queries: []wsproto.InputResolveQuery{
			{Project: "app", Selector: goembed.BuildSelector, Patterns: buildPatterns},
			{Project: "app", Selector: goembed.TestSelector, Patterns: testPatterns},
			{Project: "app", Selector: goembed.BuildSelector},
		},
		mutations: []mutation{
			{name: "edit a directive", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/main.go", goSource("main", "static/a.txt", "assets", "all:web", "_private/data.txt"))
			}},
			{name: "add a go:build ignore header", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/lib/lib.go", ignoredSource("lib", "tmpl/*.tmpl"))
			}},
			{name: "remove a go:build ignore header", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/ignored/ign.go", goSource("ignored", "missing.txt"))
			}},
			{name: "add a Go source", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/extra.go", goSource("main"))
			}},
			{name: "remove a Go source", apply: func(t *testing.T, root string) {
				remove(t, root, "app/cmd/tool/main.go")
			}},
			{name: "rename a Go source to an underscore name", apply: func(t *testing.T, root string) {
				rename(t, root, "app/cmd/tool/main.go", "app/cmd/tool/_main.go")
			}},
			{name: "move a Go source into an underscore directory", apply: func(t *testing.T, root string) {
				rename(t, root, "app/cmd/tool/main.go", "app/cmd/_tool/main.go")
			}},
			{name: "move a Go source into a dot directory", apply: func(t *testing.T, root string) {
				rename(t, root, "app/lib/lib.go", "app/lib/.hidden/lib.go")
			}},
			{name: "move a Go source into testdata", apply: func(t *testing.T, root string) {
				rename(t, root, "app/cmd/tool/main.go", "app/testdata/main.go")
			}},
			{name: "move a Go source into vendor", apply: func(t *testing.T, root string) {
				rename(t, root, "app/cmd/tool/main.go", "app/vendor/tool.go")
			}},
			{name: "move a Go source out of an underscore directory", apply: func(t *testing.T, root string) {
				rename(t, root, "app/_gen/g.go", "app/gen/g.go")
			}},
			{name: "move a Go link out of testdata", apply: func(t *testing.T, root string) {
				remove(t, root, "app/testdata/td.go")
				symlink(t, root, "app/lib/td.go", "../_shared/real.go")
			}},
			{name: "add go.mod in a package directory", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/lib/go.mod", "module example.com/lib\n")
			}},
			{name: "add GO.MOD in a package directory", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/lib/GO.MOD", "module example.com/lib\n")
			}},
			{name: "remove a nested module's go.mod", apply: func(t *testing.T, root string) {
				remove(t, root, "app/nested/go.mod")
			}},
			{name: "add go.mod in an ignored directory an embedded file sits in", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/_private/go.mod", "module example.com/private\n")
			}},
			{name: "add go.mod in an embedded directory", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/assets/sub/go.mod", "module example.com/sub\n")
			}},
			{name: "create a file an embed glob matches", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/static/c.txt", "c")
			}},
			{name: "create a dot file an embed glob matches", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/static/.c", "c")
			}},
			{name: "create a directory an embed glob matches", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/static/dir/f.txt", "f")
			}},
			{name: "add a file under an embedded directory", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/assets/sub/new.txt", "new")
			}},
			{name: "add a dot file under an embedded directory", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/assets/sub/.new", "new")
			}},
			{name: "add a file under a dot directory of an all: directory", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/web/.well-known/new.txt", "new")
			}},
			{name: "change an embedded file's bytes", keeps: bothSelectors, apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/static/a.txt", "changed")
			}},
			{name: "remove one of an embed glob's files", apply: func(t *testing.T, root string) {
				remove(t, root, "app/static/b.txt")
			}},
			{name: "remove the only file a directive names", apply: func(t *testing.T, root string) {
				remove(t, root, "app/_private/data.txt")
			}},
			{name: "replace an embedded file with a link", apply: func(t *testing.T, root string) {
				remove(t, root, "app/static/a.txt")
				symlink(t, root, "app/static/a.txt", "b.txt")
			}},
			{name: "retarget a Go source link", apply: func(t *testing.T, root string) {
				remove(t, root, "app/lib/link.go")
				symlink(t, root, "app/lib/link.go", "../_shared/other.go")
			}},
			{name: "retarget a selected Go link", apply: func(t *testing.T, root string) {
				remove(t, root, "app/testdata/td.go")
				symlink(t, root, "app/testdata/td.go", "../_shared/other.go")
			}},
			{name: "change a link referent's bytes", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/_shared/real.go", goSource("lib", "fixture.txt"))
			}},
			{name: "replace a directory a directive names with a link", apply: func(t *testing.T, root string) {
				replaceWithLink(t, root, "app/_private", "app/_privcopy", "_privcopy")
			}},
			{name: "replace an embedded directory's subdirectory with a link", apply: func(t *testing.T, root string) {
				replaceWithLink(t, root, "app/assets/sub", "app/_subcopy", "../_subcopy")
			}},
			{name: "replace a link referent's directory with a link", apply: func(t *testing.T, root string) {
				replaceWithLink(t, root, "app/_shared", "app/_sharedcopy", "_sharedcopy")
			}},
			{name: "replace a package directory with a link", apply: func(t *testing.T, root string) {
				replaceWithLink(t, root, "app/cmd", "app/_cmdcopy", "_cmdcopy")
			}},
			{name: "make a scanned go.mod a link", closed: alwaysClosed, apply: func(t *testing.T, root string) {
				remove(t, root, "app/nested/go.mod")
				symlink(t, root, "app/nested/go.mod", "../go.mod")
			}},
			{name: "add a dangling go.mod link in a package directory", closed: alwaysClosed, apply: func(t *testing.T, root string) {
				symlink(t, root, "app/lib/go.mod", "missing.mod")
			}},
			{name: "add a dangling go.mod link in an ignored directory an embedded file sits in", closed: alwaysClosed, apply: func(t *testing.T, root string) {
				symlink(t, root, "app/_private/go.mod", "missing.mod")
			}},
			{name: "add a go.mod link in an ignored directory an embedded file sits in", apply: func(t *testing.T, root string) {
				symlink(t, root, "app/_private/go.mod", "../go.mod")
			}},
			{name: "add a Go source link that climbs out of a named directory", closed: alwaysClosed, apply: func(t *testing.T, root string) {
				symlink(t, root, "app/lib/climb.go", "../lib/../_shared/real.go")
			}},
			{name: "add a _test.go source with a directive", keeps: []string{goembed.BuildSelector}, apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/lib/new_test.go", goSource("lib", "tmpl/a.tmpl"))
			}},
			{name: "add a broken Go link in testdata", apply: func(t *testing.T, root string) {
				symlink(t, root, "app/testdata/broken.go", "../nowhere.go")
			}},
			{name: "add a Go link in vendor", apply: func(t *testing.T, root string) {
				symlink(t, root, "app/vendor/l.go", "../_shared/real.go")
			}},
			{name: "add a Go source in a nested module", apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/nested/m.go", goSource("nested", "missing.txt"))
			}},
			{name: "add a Go source in an excluded dot directory", keeps: bothSelectors, apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/.gen/y.go", goSource("gen", "missing.txt"))
			}},
			{name: "add a file nothing reads", keeps: bothSelectors, apply: func(t *testing.T, root string) {
				writeFile(t, root, "app/docs/notes.md", "notes")
			}},
			{name: "add a directory named like a Go source", apply: func(t *testing.T, root string) {
				mkdir(t, root, "app/lib/dir.go")
			}},
			{name: "replace a Go source with a link to a directory", apply: func(t *testing.T, root string) {
				remove(t, root, "app/lib/lib.go")
				symlink(t, root, "app/lib/lib.go", "tmpl")
			}},
			{name: "rename a package directory", apply: func(t *testing.T, root string) {
				rename(t, root, "app/lib", "app/lib2")
			}},
			{name: "respell a directory a directive names", closed: closedIfCaseInsensitive, apply: func(t *testing.T, root string) {
				rename(t, root, "app/static", "app/Static.tmp")
				rename(t, root, "app/Static.tmp", "app/Static")
			}},
		},
	}
}

func rootCorpus() propertyCorpus {
	patterns := []string{"cmd/**/*.go", "internal/*/*.go", "main.go", "tools/**/*.go", "!internal/skip/*.go"}
	return propertyCorpus{
		name:    "root",
		fixture: rootFixture,
		queries: []wsproto.InputResolveQuery{
			{Project: ".", Selector: goembed.BuildSelector, Patterns: patterns},
			{Project: ".", Selector: goembed.TestSelector, Patterns: patterns},
		},
		mutations: []mutation{
			{name: "replace the literal prefix of a ** include with a link", closed: alwaysClosed, apply: func(t *testing.T, root string) {
				replaceWithLink(t, root, "cmd", "_cmd", "_cmd")
			}},
			{name: "replace a directory a glob include's prefix matches with a link", closed: alwaysClosed, apply: func(t *testing.T, root string) {
				remove(t, root, "internal/a")
				symlink(t, root, "internal/a", "../_alt")
			}},
			{name: "replace a glob include's literal directory with a link", closed: alwaysClosed, apply: func(t *testing.T, root string) {
				remove(t, root, "internal")
				symlink(t, root, "internal", "_internal")
			}},
			{name: "replace a directory an embed glob's prefix matches with a link", closed: alwaysClosed, apply: func(t *testing.T, root string) {
				symlink(t, root, "site/de", "../_de")
			}},
			{name: "add a page an embed glob's prefix reaches", apply: func(t *testing.T, root string) {
				writeFile(t, root, "site/es/page.html", "es")
			}},
			{name: "break a selected Go link", apply: func(t *testing.T, root string) {
				remove(t, root, "main.go")
				symlink(t, root, "main.go", "src/nowhere.txt")
			}},
			{name: "retarget the root Go link", apply: func(t *testing.T, root string) {
				remove(t, root, "main.go")
				symlink(t, root, "main.go", "src/other.txt")
			}},
			{name: "add a Go link a glob include selects", apply: func(t *testing.T, root string) {
				symlink(t, root, "internal/b/b.go", "../../_impl/a.go")
			}},
			{name: "add a Go link the input's exclusion removes", apply: func(t *testing.T, root string) {
				symlink(t, root, "internal/skip/t.go", "../../_impl/a.go")
			}},
			{name: "create the literal prefix of a ** include", apply: func(t *testing.T, root string) {
				symlink(t, root, "tools/x/x.go", "../../_impl/a.go")
			}},
			{name: "create the literal prefix of a ** include in another letter case", closed: closedIfCaseInsensitive, apply: func(t *testing.T, root string) {
				symlink(t, root, "TOOLS/y.go", "../_impl/a.go")
			}},
			{name: "respell the literal prefix of a ** include", closed: closedIfCaseInsensitive, apply: func(t *testing.T, root string) {
				rename(t, root, "cmd", "CMD.tmp")
				rename(t, root, "CMD.tmp", "CMD")
			}},
			{name: "add a directive behind a source link", apply: func(t *testing.T, root string) {
				writeFile(t, root, "internal/a/a.txt", "a")
				writeFile(t, root, "_impl/a.go", goSource("a", "a.txt"))
			}},
			{name: "add an embedded page's sibling", keeps: bothSelectors, apply: func(t *testing.T, root string) {
				writeFile(t, root, "site/en/other.html", "other")
			}},
		},
	}
}

// For every mutation of every fixture tree, an answer whose basis digest the
// mutation leaves unchanged equals the fresh answer: the basis selects every
// path whose bytes, existence, kind or link target the answer read. Each
// answer's files equal the core's own selection, and the answer is an error
// exactly when the core's selection is, except where the mutation makes a
// read the digest cannot see.
func TestAnUnchangedBasisDigestKeepsTheAnswerExact(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-answers-its-input-selectors",
		"an-unchanged-basis-digest-keeps-the-answer-exact")
	insensitive := caseInsensitive(t, t.TempDir())
	checked, changed := 0, 0
	for _, corpus := range []propertyCorpus{appCorpus(), rootCorpus()} {
		for _, m := range corpus.mutations {
			t.Run(corpus.name+"/"+m.name, func(t *testing.T) {
				root := t.TempDir()
				corpus.fixture(t, root)
				type before struct {
					answer wsproto.InputResolveAnswer
					digest string
				}
				old := make([]before, len(corpus.queries))
				for i, query := range corpus.queries {
					answer := Answer(root, query)
					if answer.Error != "" {
						t.Fatalf("%s %v: the fixture answer is an error: %s", query.Selector, query.Patterns, answer.Error)
					}
					assertCoreSelection(t, root, query, answer)
					digest, err := wsproto.InputBasisDigest(filepath.Join(root, query.Project), answer.Basis)
					if err != nil {
						t.Fatal(err)
					}
					old[i] = before{answer: answer, digest: digest}
				}

				m.apply(t, root)

				for i, query := range corpus.queries {
					fresh := Answer(root, query)
					assertAgreesWithCore(t, root, query, fresh, m.closed.closes(insensitive))

					digest, err := wsproto.InputBasisDigest(filepath.Join(root, query.Project), old[i].answer.Basis)
					kept := err == nil && digest == old[i].digest
					checked++
					if !kept {
						changed++
					}
					if kept && !reflect.DeepEqual(fresh, old[i].answer) {
						t.Errorf("%s %v: the basis digest is unchanged, but the fresh answer differs:\nold:   %+v\nfresh: %+v",
							query.Selector, query.Patterns, old[i].answer, fresh)
					}
					for _, selector := range m.keeps {
						if selector == query.Selector && !kept {
							t.Errorf("%s %v: the mutation writes nothing the selection reads, but the basis digest changed (err %v)",
								query.Selector, query.Patterns, err)
						}
					}
				}
			})
		}
	}
	t.Logf("%d fresh answers checked against their old basis; %d basis digests changed, %d held", checked, changed, checked-changed)
}

// assertAgreesWithCore holds a fresh answer to the core's own selection: the
// same files when the answer succeeds; an error when the core's selection
// fails; and, when closed, an error while the core's selection succeeds.
func assertAgreesWithCore(t *testing.T, root string, query wsproto.InputResolveQuery, answer wsproto.InputResolveAnswer, closed bool) {
	t.Helper()
	dir := filepath.Join(root, query.Project)
	core, coreErr := coreRawEmbedFiles(dir, query.Patterns, query.Selector)
	switch {
	case closed:
		if answer.Error == "" || coreErr != nil {
			t.Errorf("%s %v: want an error answer over a selection the core accepts; answer error %q, core error %v",
				query.Selector, query.Patterns, answer.Error, coreErr)
		}
	case coreErr != nil:
		if answer.Error == "" {
			t.Errorf("%s %v: the core refuses the selection (%v), but the answer lists %v",
				query.Selector, query.Patterns, coreErr, answer.Files)
		}
	case answer.Error != "":
		t.Errorf("%s %v: the core accepts the selection, but the answer is an error: %s",
			query.Selector, query.Patterns, answer.Error)
	default:
		assertSameFiles(t, dir, query, answer, core)
	}
	assertValidAnswer(t, query, answer)
}

// assertCoreSelection holds a successful answer's files to the core's
// selection.
func assertCoreSelection(t *testing.T, root string, query wsproto.InputResolveQuery, answer wsproto.InputResolveAnswer) {
	t.Helper()
	dir := filepath.Join(root, query.Project)
	core, err := coreRawEmbedFiles(dir, query.Patterns, query.Selector)
	if err != nil {
		t.Fatalf("%s %v: the core refuses the fixture: %v", query.Selector, query.Patterns, err)
	}
	assertSameFiles(t, dir, query, answer, core)
	assertValidAnswer(t, query, answer)
}

func assertSameFiles(t *testing.T, dir string, query wsproto.InputResolveQuery, answer wsproto.InputResolveAnswer, core []string) {
	t.Helper()
	absolute := make([]string, len(answer.Files))
	for i, file := range answer.Files {
		absolute[i] = filepath.Join(dir, filepath.FromSlash(file))
	}
	if !reflect.DeepEqual(absolute, core) && (len(absolute) != 0 || len(core) != 0) {
		t.Errorf("%s %v: files differ from the core's selection:\nanswer: %v\ncore:   %v",
			query.Selector, query.Patterns, absolute, core)
	}
}

// assertValidAnswer holds an answer to both protocol checks, as
// ServeInputResolve does before it writes a byte.
func assertValidAnswer(t *testing.T, query wsproto.InputResolveQuery, answer wsproto.InputResolveAnswer) {
	t.Helper()
	request := wsproto.InputResolveRequest{
		Version: wsproto.InputResolveProtocolVersion, Extension: "@putnami/go",
		Inputs: []wsproto.InputResolveQuery{query},
	}
	result := wsproto.InputResolveResult{
		Version: wsproto.InputResolveProtocolVersion, Extension: "@putnami/go",
		Inputs: []wsproto.InputResolveAnswer{answer},
	}
	diags := append(wsproto.ValidateInputResolveResult(&result), wsproto.ValidateInputResolveAnswer(&request, &result)...)
	if diag.HasErrors(diags) {
		t.Errorf("%s %v: the answer breaks the protocol:\n%s", query.Selector, query.Patterns, diag.ErrorText(diags))
	}
}
