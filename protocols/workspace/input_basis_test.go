package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// writeBasisTree writes files, each path to its content, below root.
func writeBasisTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// symlinkOrSkip creates a link at rel below root, or skips the test on a host
// that cannot create one.
func symlinkOrSkip(t *testing.T, root, target, rel string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, abs); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

func basisDigest(t *testing.T, root string, basis InputBasis) string {
	t.Helper()
	digest, err := InputBasisDigest(root, basis)
	if err != nil {
		t.Fatalf("InputBasisDigest: %v", err)
	}
	return digest
}

// goLikeBasis reads the Go sources and go.mod as content and an embed tree as
// a listing.
var goLikeBasis = InputBasis{
	Content: [][]string{{"**/*.go", "go.mod", "!vendor/**"}},
	Listing: [][]string{{"assets/**"}},
}

func goLikeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeBasisTree(t, root, map[string]string{
		"go.mod":            "module example.com/web\n",
		"main.go":           "package main\n",
		"internal/a.go":     "package internal\n",
		"assets/app.css":    "body{}\n",
		"assets/img/a.png":  "png",
		"vendor/x/y.go":     "package x\n",
		"README.md":         "# web\n",
		"docs/guide.md":     "guide\n",
		"other/target.css":  "target\n",
		"other/target2.css": "target2\n",
	})
	return root
}

func TestInputBasisDigestBindsWhatTheBasisSelects(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-input-basis-digest-is-host-independent", "the-digest-binds-what-the-basis-selects")
	cases := []struct {
		name    string
		mutate  func(t *testing.T, root string)
		changes bool
	}{
		{"content bytes", func(t *testing.T, root string) {
			writeBasisTree(t, root, map[string]string{"internal/a.go": "package internal // edited\n"})
		}, true},
		{"content file appears", func(t *testing.T, root string) {
			writeBasisTree(t, root, map[string]string{"internal/b.go": "package internal\n"})
		}, true},
		{"content file disappears", func(t *testing.T, root string) {
			mustRemove(t, root, "internal/a.go")
		}, true},
		{"content file becomes a directory", func(t *testing.T, root string) {
			mustRemove(t, root, "main.go")
			mustMkdir(t, root, "main.go")
		}, true},
		{"content file becomes a link", func(t *testing.T, root string) {
			mustRemove(t, root, "main.go")
			symlinkOrSkip(t, root, "internal/a.go", "main.go")
		}, true},
		{"content link changes target", func(t *testing.T, root string) {
			symlinkOrSkip(t, root, "internal/a.go", "link.go")
			before := basisDigest(t, root, goLikeBasis)
			mustRemove(t, root, "link.go")
			symlinkOrSkip(t, root, "main.go", "link.go")
			if basisDigest(t, root, goLikeBasis) == before {
				t.Fatal("retargeting a content link left the digest unchanged")
			}
		}, true},
		{"listing path appears", func(t *testing.T, root string) {
			writeBasisTree(t, root, map[string]string{"assets/new.css": ""})
		}, true},
		{"listing directory appears", func(t *testing.T, root string) {
			mustMkdir(t, root, "assets/fonts")
		}, true},
		{"listing path disappears", func(t *testing.T, root string) {
			mustRemove(t, root, "assets/img/a.png")
		}, true},
		{"listing file becomes a directory", func(t *testing.T, root string) {
			mustRemove(t, root, "assets/app.css")
			mustMkdir(t, root, "assets/app.css")
		}, true},
		{"listing link changes target", func(t *testing.T, root string) {
			symlinkOrSkip(t, root, "../other/target.css", "assets/link.css")
			before := basisDigest(t, root, goLikeBasis)
			mustRemove(t, root, "assets/link.css")
			symlinkOrSkip(t, root, "../other/target2.css", "assets/link.css")
			if basisDigest(t, root, goLikeBasis) == before {
				t.Fatal("retargeting a listing link left the digest unchanged")
			}
		}, true},
		{"listing file bytes", func(t *testing.T, root string) {
			writeBasisTree(t, root, map[string]string{"assets/app.css": "body{color:red}\n"})
		}, false},
		{"listing link referent bytes", func(t *testing.T, root string) {
			symlinkOrSkip(t, root, "../other/target.css", "assets/link.css")
			before := basisDigest(t, root, goLikeBasis)
			writeBasisTree(t, root, map[string]string{"other/target.css": "edited\n"})
			if basisDigest(t, root, goLikeBasis) != before {
				t.Fatal("editing a listing link's referent changed the digest")
			}
		}, true},
		{"unselected bytes", func(t *testing.T, root string) {
			writeBasisTree(t, root, map[string]string{"README.md": "# edited\n"})
		}, false},
		{"unselected path appears", func(t *testing.T, root string) {
			writeBasisTree(t, root, map[string]string{"docs/new.md": "new\n"})
		}, false},
		{"unselected path disappears", func(t *testing.T, root string) {
			mustRemove(t, root, "README.md")
		}, false},
		{"excluded content bytes", func(t *testing.T, root string) {
			writeBasisTree(t, root, map[string]string{"vendor/x/y.go": "package x // edited\n"})
		}, false},
		{"content file's modification time", func(t *testing.T, root string) {
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(root, "main.go"), later, later); err != nil {
				t.Fatal(err)
			}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := goLikeTree(t)
			before := basisDigest(t, root, goLikeBasis)
			tc.mutate(t, root)
			after := basisDigest(t, root, goLikeBasis)
			if changed := after != before; changed != tc.changes {
				t.Fatalf("digest changed = %v, want %v", changed, tc.changes)
			}
		})
	}
}

// A path both kinds select gets both records, so the basis that selects it
// twice differs from the one that selects it once.
func TestInputBasisDigestRecordsBothKindsOfAPath(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-input-basis-digest-is-host-independent", "the-digest-binds-what-the-basis-selects")
	root := goLikeTree(t)
	set := []string{"assets/**"}
	both := basisDigest(t, root, InputBasis{Content: [][]string{set}, Listing: [][]string{set}})
	content := basisDigest(t, root, InputBasis{Content: [][]string{set}})
	listing := basisDigest(t, root, InputBasis{Listing: [][]string{set}})
	if both == content || both == listing || content == listing {
		t.Fatalf("digests do not tell the kinds apart: both %s, content %s, listing %s", both, content, listing)
	}
	writeBasisTree(t, root, map[string]string{"assets/app.css": "edited\n"})
	if basisDigest(t, root, InputBasis{Content: [][]string{set}, Listing: [][]string{set}}) == both {
		t.Fatal("a content edit to a path both kinds select left the digest unchanged")
	}
}

// The walk names a link by its target text and never reads through it.
func TestInputBasisDigestNeverFollowsALink(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-input-basis-digest-is-host-independent", "the-digest-binds-what-the-basis-selects")
	outside := t.TempDir()
	writeBasisTree(t, outside, map[string]string{"pkg/x.go": "package x\n", "y.go": "package y\n"})
	root := t.TempDir()
	writeBasisTree(t, root, map[string]string{"main.go": "package main\n"})
	symlinkOrSkip(t, root, filepath.Join(outside, "pkg"), "pkg")
	symlinkOrSkip(t, root, filepath.Join(outside, "y.go"), "y.go")
	basis := InputBasis{Content: [][]string{{"**"}}, Listing: [][]string{{"**"}}}
	before := basisDigest(t, root, basis)
	writeBasisTree(t, outside, map[string]string{"pkg/x.go": "package x // edited\n", "pkg/new.go": "", "y.go": "package y // edited\n"})
	if basisDigest(t, root, basis) != before {
		t.Fatal("an edit behind a link changed the digest; the walk followed the link")
	}
}

// basisFixtureTree holds the shapes pruning must get right: nested selected
// files, excluded trees, a directory whose name an include matches, links,
// and a path a character class matches across a slash.
func basisFixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeBasisTree(t, root, map[string]string{
		"go.mod":                  "module m\n",
		"main.go":                 "package main\n",
		"main_test.go":            "package main\n",
		"internal/a.go":           "package internal\n",
		"internal/sub/b.go":       "package sub\n",
		"internal/sub/data.txt":   "data\n",
		"internal/sub/deep/c.go":  "package deep\n",
		"assets/app.css":          "body{}\n",
		"assets/img/logo.png":     "png",
		"assets/.hidden":          "hidden\n",
		"vendor/x/y.go":           "package x\n",
		"vendor/x/z.txt":          "z\n",
		"vendor/keep/k.go":        "package keep\n",
		"node_modules/p/index.js": "module.exports = 1\n",
		"templates/index.html":    "<html></html>\n",
		"templates/x.go/inner.go": "package inner\n",
		"testdata/a.golden":       "golden\n",
		"x/y":                     "class\n",
		"aXXb/c":                  "star\n",
	})
	mustMkdir(t, root, "empty/dir")
	if runtime.GOOS != "windows" {
		symlinkOrSkip(t, root, "app.css", "assets/link.css")
		symlinkOrSkip(t, root, "internal", "linkdir")
		symlinkOrSkip(t, root, "missing.go", "broken.go")
	}
	return root
}

var pruningBases = map[string]InputBasis{
	"go-like": {
		Content: [][]string{{"**/*.go", "go.mod", "!vendor/**", "!**/*_test.go"}},
		Listing: [][]string{{"assets/**"}, {"templates/*.html"}},
	},
	"deep literal prefix":            {Content: [][]string{{"internal/sub/*"}}},
	"class across a slash":           {Listing: [][]string{{"x[^a]y"}}},
	"excluded subtree":               {Content: [][]string{{"vendor/**", "!vendor/x/**"}}},
	"wildcard first segment":         {Listing: [][]string{{"*/x/*"}, {"*/sub/deep/*"}}},
	"everything":                     {Content: [][]string{{"**"}}, Listing: [][]string{{"**"}}},
	"nothing matches":                {Content: [][]string{{"nomatch/**"}}},
	"star pair inside a segment":     {Listing: [][]string{{"a**b/*"}}},
	"an exclusion of another prefix": {Content: [][]string{{"**/sub/**", "!internal/**"}}},
	"a directory named like a file":  {Content: [][]string{{"templates/*.go"}}},
	"empty directories":              {Listing: [][]string{{"empty/**"}}},
	"no set":                         {},
}

// Pruning skips only directories no set can select, so the pruned walk
// records exactly what the full walk records.
func TestInputBasisDigestPruningKeepsTheRecords(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-input-basis-digest-is-host-independent", "the-digest-binds-what-the-basis-selects")
	root := basisFixtureTree(t)
	for name, basis := range pruningBases {
		t.Run(name, func(t *testing.T) {
			pruned, prunedVisits, err := walkInputBasis(root, basis, true)
			if err != nil {
				t.Fatal(err)
			}
			full, fullVisits, err := walkInputBasis(root, basis, false)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := hashInputBasisRecords(pruned), hashInputBasisRecords(full); got != want {
				t.Fatalf("pruned walk records\n%s\nfull walk records\n%s", strings.Join(pruned, ""), strings.Join(full, ""))
			}
			if prunedVisits > fullVisits {
				t.Fatalf("pruned walk visited %d paths, the full walk %d", prunedVisits, fullVisits)
			}
			if digest := basisDigest(t, root, basis); digest != hashInputBasisRecords(full) {
				t.Fatalf("InputBasisDigest = %s, the full walk hashes to %s", digest, hashInputBasisRecords(full))
			}
		})
	}
	_, prunedVisits, _ := walkInputBasis(root, pruningBases["nothing matches"], true)
	_, fullVisits, _ := walkInputBasis(root, pruningBases["nothing matches"], false)
	if prunedVisits >= fullVisits {
		t.Fatalf("a basis that selects nothing visited %d paths, as many as the full walk's %d", prunedVisits, fullVisits)
	}
}

// The class case is the one where a whole-path match crosses a slash: the
// pattern x[^a]y selects x/y, so the directory x must be walked.
func TestInputBasisDigestWalksADirectoryAClassReaches(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-input-basis-digest-is-host-independent", "the-digest-binds-what-the-basis-selects")
	root := basisFixtureTree(t)
	records, _, err := walkInputBasis(root, pruningBases["class across a slash"], true)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !strings.Contains(records[0], `"x/y"`) {
		t.Fatalf("records = %q, want x/y alone", records)
	}
}

func TestInputBasisDigestIsDeterministic(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-input-basis-digest-is-host-independent", "the-digest-is-deterministic-and-unambiguous")
	first := goLikeTree(t)
	second := t.TempDir()
	// The same tree, written in another order below another root.
	for _, rel := range []string{"other/target2.css", "docs/guide.md", "README.md", "vendor/x/y.go", "assets/img/a.png", "assets/app.css", "internal/a.go", "main.go", "go.mod", "other/target.css"} {
		data, err := os.ReadFile(filepath.Join(first, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		writeBasisTree(t, second, map[string]string{rel: string(data)})
	}
	want := basisDigest(t, first, goLikeBasis)
	if got := basisDigest(t, first, goLikeBasis); got != want {
		t.Fatalf("two digests of one tree: %s and %s", want, got)
	}
	if got := basisDigest(t, second, goLikeBasis); got != want {
		t.Fatalf("the same tree below another root digests to %s, want %s", got, want)
	}
	if len(want) != 64 || strings.Trim(want, "0123456789abcdef") != "" {
		t.Fatalf("digest %q is not a sha256 hex digest", want)
	}

	records, _, err := walkInputBasis(first, goLikeBasis, true)
	if err != nil {
		t.Fatal(err)
	}
	reversed := slices.Clone(records)
	slices.Reverse(reversed)
	if hashInputBasisRecords(reversed) != want {
		t.Fatal("the digest depends on the order the walk yields its records")
	}
}

// A record quotes its path and payload, so a name holding the separators
// cannot spell another record.
func TestInputBasisRecordIsUnambiguous(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-input-basis-digest-is-host-independent", "the-digest-is-deterministic-and-unambiguous")
	a := inputBasisRecord("l", "a\" symlink \"b", inputBasisKindSymlink, "c")
	b := inputBasisRecord("l", "a", inputBasisKindSymlink, "b\" symlink \"c")
	if a == b {
		t.Fatalf("two records share one framing: %q", a)
	}
	if got := inputBasisRecord("c", "a\nb", inputBasisKindFile, ""); strings.Count(got, "\n") != 1 {
		t.Fatalf("a newline in a path is not escaped: %q", got)
	}
}

func TestInputBasisDigestRefusesWhatItCannotDigest(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "the-input-basis-digest-is-host-independent", "what-cannot-be-digested-is-refused")
	root := goLikeTree(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil && runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	cases := map[string]struct {
		root  string
		basis InputBasis
		want  string
	}{
		"missing root":      {filepath.Join(root, "missing"), goLikeBasis, "input basis root"},
		"relative root":     {"web", goLikeBasis, "not an absolute path"},
		"file root":         {filepath.Join(root, "main.go"), goLikeBasis, "not a directory"},
		"link root":         {link, goLikeBasis, "not a directory"},
		"git pattern":       {root, InputBasis{Content: [][]string{{"git:**"}}}, "input basis rejected"},
		"escaping pattern":  {root, InputBasis{Listing: [][]string{{"../**"}}}, "input basis rejected"},
		"exclusions alone":  {root, InputBasis{Listing: [][]string{{"!vendor/**"}}}, "input basis rejected"},
		"empty pattern set": {root, InputBasis{Content: [][]string{{}}}, "input basis rejected"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "link root" && runtime.GOOS == "windows" {
				t.Skip("symlinks unavailable")
			}
			digest, err := InputBasisDigest(tc.root, tc.basis)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("InputBasisDigest = (%q, %v), want an error naming %q", digest, err, tc.want)
			}
		})
	}
}

// A directory the basis may select must be read, so an unreadable one is an
// error; one no set can select is skipped before it is read.
func TestInputBasisDigestReadsOnlyWhatTheBasisMaySelect(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind this user")
	}
	root := goLikeTree(t)
	locked := filepath.Join(root, "docs")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	skipsDocs := InputBasis{
		Content: [][]string{{"**/*.go", "go.mod", "!docs/**"}},
		Listing: [][]string{{"assets/**"}},
	}
	if _, err := InputBasisDigest(root, skipsDocs); err != nil {
		t.Fatalf("an unreadable directory no set can select failed the digest: %v", err)
	}
	if _, err := InputBasisDigest(root, InputBasis{Listing: [][]string{{"docs/*.md"}}}); err == nil {
		t.Fatal("an unreadable directory the basis may select did not fail the digest")
	}
}

func mustRemove(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
}
