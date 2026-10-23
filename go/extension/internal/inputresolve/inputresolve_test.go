package inputresolve

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/goembed"
)

// answerFor answers one go-embed:build input of project app under root.
func answerFor(root string, patterns ...string) wsproto.InputResolveAnswer {
	return Answer(root, wsproto.InputResolveQuery{Project: "app", Selector: goembed.BuildSelector, Patterns: patterns})
}

// wantError fails the test unless answer is an error that mentions want,
// carries no files and no basis, and passes the protocol's checks.
func wantError(t *testing.T, answer wsproto.InputResolveAnswer, want string) {
	t.Helper()
	if answer.Error == "" || !strings.Contains(answer.Error, want) {
		t.Fatalf("answer error = %q, want one that mentions %q (files %v)", answer.Error, want, answer.Files)
	}
	if len(answer.Files) != 0 || len(answer.Basis.Content) != 0 || len(answer.Basis.Listing) != 0 {
		t.Fatalf("an error answer carries files or a basis: %+v", answer)
	}
	assertValidAnswer(t, wsproto.InputResolveQuery{Project: answer.Project, Selector: answer.Selector}, answer)
}

// smallProject writes a project at app/ whose main.go embeds static/a.txt.
func smallProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "app/go.mod", "module example.com/app\n")
	writeFile(t, root, "app/main.go", goSource("main", "static/a.txt"))
	writeFile(t, root, "app/static/a.txt", "a")
	return root
}

// The answer lists the files and states the basis the documentation
// describes, for both selectors.
func TestAnswerListsTheSelectionAndItsBasis(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-answers-its-input-selectors",
		"the-files-are-the-cores-embed-selection")
	root := t.TempDir()
	appFixture(t, root)

	build := Answer(root, wsproto.InputResolveQuery{Project: "app", Selector: goembed.BuildSelector, Patterns: buildPatterns})
	wantBuild := []string{
		"_private/data.txt", "_shared/real.go", "assets/sub/y.txt", "assets/x.txt",
		"cmd/tool/main.go", "cmd/tool/version.txt", "ignored/ign.go", "lib/lib.go", "lib/link.go",
		"lib/tmpl/a.tmpl", "main.go", "static/a.txt", "static/b.txt", "testdata/td.go",
		"web/.well-known/k.txt", "web/index.html",
	}
	if build.Error != "" || !reflect.DeepEqual(build.Files, wantBuild) {
		t.Fatalf("build answer = %q %v\nwant %v", build.Error, build.Files, wantBuild)
	}
	wantBasis := wsproto.InputBasis{
		Content: [][]string{
			{"**/*.go", "!**/testdata/**", "!**/vendor/**", "!**/.*/**", "!**/_*/**", "!**/*_test.go"},
			{"_shared/real.go"},
		},
		Listing: [][]string{
			{"**/[gG][oO].[mM][oO][dD]", "!**/testdata/**", "!**/vendor/**", "!**/.*/**", "!**/_*/**"},
			{"**/*.go", "!**/*_test.go", "!**/.gen/**"},
			{
				"_private", "_private/[gG][oO].[mM][oO][dD]", "_private/data.txt/**",
				"_shared", "_shared/[gG][oO].[mM][oO][dD]",
				"assets/**", "cmd/tool/version.txt/**", "lib/tmpl", "lib/tmpl/*.tmpl/**",
				"static", "static/*/**", "testdata/[gG][oO].[mM][oO][dD]",
				"web/**", "web/.well-known/[gG][oO].[mM][oO][dD]",
			},
		},
	}
	if !reflect.DeepEqual(build.Basis, wantBasis) {
		t.Fatalf("build basis =\n%#v\nwant\n%#v", build.Basis, wantBasis)
	}

	test := Answer(root, wsproto.InputResolveQuery{Project: "app", Selector: goembed.TestSelector, Patterns: testPatterns})
	wantTest := append([]string{}, wantBuild...)
	wantTest = append(wantTest, "lib/fixture.txt", "lib/lib_test.go")
	wantTest = sortedUnique(wantTest)
	if test.Error != "" || !reflect.DeepEqual(test.Files, wantTest) {
		t.Fatalf("test answer = %q %v\nwant %v", test.Error, test.Files, wantTest)
	}
	if got := test.Basis.Content[0]; len(got) != 5 || got[len(got)-1] == "!**/*_test.go" {
		t.Fatalf("test sources set = %v, want the build set without the _test.go exclusion", got)
	}
	for _, answer := range []wsproto.InputResolveAnswer{build, test} {
		dir := filepath.Join(root, "app")
		query := wsproto.InputResolveQuery{Project: "app", Selector: answer.Selector, Patterns: buildPatterns}
		if answer.Selector == goembed.TestSelector {
			query.Patterns = testPatterns
		}
		assertCoreSelection(t, root, query, answer)
		if _, err := wsproto.InputBasisDigest(dir, answer.Basis); err != nil {
			t.Fatalf("%s basis digest: %v", answer.Selector, err)
		}
	}
}

// Resolve answers every input in request order and names the request's
// extension.
func TestResolveAnswersInRequestOrder(t *testing.T) {
	root := smallProject(t)
	request := wsproto.InputResolveRequest{
		Version: wsproto.InputResolveProtocolVersion, Extension: "@putnami/go",
		Inputs: []wsproto.InputResolveQuery{
			{Project: "app", Selector: goembed.TestSelector},
			{Project: "app", Selector: "go-embed:other"},
			{Project: "app", Selector: goembed.BuildSelector, Patterns: []string{"**/*.go"}},
		},
	}
	result := Resolve(root, request)
	if diags := append(wsproto.ValidateInputResolveResult(&result), wsproto.ValidateInputResolveAnswer(&request, &result)...); len(diags) != 0 {
		t.Fatalf("result breaks the protocol: %v", diags)
	}
	if result.Inputs[0].Error != "" || result.Inputs[1].Error == "" || result.Inputs[2].Error != "" {
		t.Fatalf("answers = %+v", result.Inputs)
	}
	if want := []string{"main.go", "static/a.txt"}; !reflect.DeepEqual(result.Inputs[2].Files, want) {
		t.Fatalf("files = %v, want %v", result.Inputs[2].Files, want)
	}
}

// A selection the core refuses, a selector this extension does not expand,
// and a pattern the basis cannot bind are error answers.
func TestAnInvalidSelectionIsAnError(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-answers-its-input-selectors",
		"an-invalid-selection-is-an-error-answer")
	root := smallProject(t)

	wantError(t, Answer(root, wsproto.InputResolveQuery{Project: "app", Selector: "go-embed:other"}), `not "go-embed:other"`)
	wantError(t, Answer(root, wsproto.InputResolveQuery{Project: "app", Selector: "sql:migrations"}), `not "sql:migrations"`)
	for _, pattern := range []string{"git:**/*.go", "!git:vendor/**"} {
		wantError(t, answerFor(root, "**/*.go", pattern), "Git's candidate list")
	}
	for _, pattern := range []string{"../shared/**/*.go", "!../x", "a/../b/*.go", "a/.."} {
		wantError(t, answerFor(root, "**/*.go", pattern), "climbs out")
	}
	for _, pattern := range []string{"./**/*.go", "a//b.go", "a/./b.go", "a/", `a\b.go`, "/abs/*.go"} {
		wantError(t, answerFor(root, pattern), "not a clean relative slash path")
	}
	wantError(t, Answer(root, wsproto.InputResolveQuery{Project: "missing", Selector: goembed.BuildSelector}), "project directory")

	writeFile(t, root, "app/main.go", goSource("main", "static/missing.txt"))
	wantError(t, answerFor(root), "matched no files")
}

// A project directory that is a link or a file is an error: the basis
// digest walks the project without following a link.
func TestAProjectDirectoryMustBeADirectory(t *testing.T) {
	root := smallProject(t)
	symlink(t, root, "linked", "app")
	writeFile(t, root, "file", "")
	wantError(t, Answer(root, wsproto.InputResolveQuery{Project: "linked", Selector: goembed.BuildSelector}), "symbolic link")
	wantError(t, Answer(root, wsproto.InputResolveQuery{Project: "file", Selector: goembed.BuildSelector}), "not a directory")
}

// A path the exchange cannot carry is an error answer.
func TestAPathTheProtocolCannotCarryIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a backslash separates paths on Windows")
	}
	root := smallProject(t)
	writeFile(t, root, `app/static/b\c.txt`, "b")
	writeFile(t, root, "app/main.go", goSource("main", "static/*"))
	wantError(t, answerFor(root), "contains a backslash")

	if err := checkCarriedPath("a/\xff.txt"); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("checkCarriedPath(invalid UTF-8) = %v", err)
	}
	if err := checkCarriedPath("../x"); err == nil {
		t.Fatal("checkCarriedPath(../x) accepted a path outside the project")
	}
}

// Every read the basis digest cannot see makes the input an error, while
// the core's own selection accepts the same tree.
func TestAReadTheDigestCannotSeeIsAnError(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-answers-its-input-selectors",
		"a-read-the-digest-cannot-see-is-an-error-answer")
	cases := []struct {
		name     string
		setup    func(t *testing.T, root string)
		patterns []string
		want     string
	}{
		{
			name: "a go.mod the scan stats is a link",
			setup: func(t *testing.T, root string) {
				writeFile(t, root, "app/sub/go.mod.real", "module example.com/sub\n")
				symlink(t, root, "app/sub/go.mod", "go.mod.real")
			},
			want: "sub/go.mod is a symbolic link",
		},
		{
			name: "a go.mod between the project and an embedded file is a dangling link",
			setup: func(t *testing.T, root string) {
				writeFile(t, root, "app/main.go", goSource("main", "_data/a.txt"))
				writeFile(t, root, "app/_data/a.txt", "a")
				symlink(t, root, "app/_data/go.mod", "missing.mod")
			},
			want: "_data/go.mod is a symbolic link",
		},
		{
			name: "a path an embed directive's glob prefix matches is a link",
			setup: func(t *testing.T, root string) {
				writeFile(t, root, "app/main.go", goSource("main", "*/page.html"))
				writeFile(t, root, "app/en/page.html", "en")
				writeFile(t, root, "app/_other/x.html", "x")
				symlink(t, root, "app/de", "_other")
			},
			want: "resolved through symbolic link de",
		},
		{
			name: "a directory on the literal prefix of a ** include is a link",
			setup: func(t *testing.T, root string) {
				writeFile(t, root, "app/_real/x.go", goSource("x"))
				symlink(t, root, "app/cmd", "_real")
			},
			patterns: []string{"cmd/**/*.go"},
			want:     "walked through symbolic link cmd",
		},
		{
			name: "a path a glob include's proper prefix matches is a link",
			setup: func(t *testing.T, root string) {
				writeFile(t, root, "app/_real/x.go", goSource("x"))
				mkdir(t, root, "app/internal")
				symlink(t, root, "app/internal/a", "../_real")
			},
			patterns: []string{"internal/*/*.go"},
			want:     "expanded through symbolic link internal/a",
		},
		{
			name: "a Go source link names a directory and climbs out of it",
			setup: func(t *testing.T, root string) {
				writeFile(t, root, "app/_impl/x.go", goSource("main"))
				mkdir(t, root, "app/dir")
				symlink(t, root, "app/x.go", "dir/../_impl/x.go")
			},
			want: "names a directory and climbs out of it",
		},
		{
			name: "a query include opens a non-ASCII literal directory",
			setup: func(t *testing.T, root string) {
				mkdir(t, root, "app/café")
			},
			patterns: []string{"café/**/*.go"},
			want:     "Unicode spellings",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := smallProject(t)
			tc.setup(t, root)
			dir := filepath.Join(root, "app")
			if _, err := coreRawEmbedFiles(dir, tc.patterns, goembed.BuildSelector); err != nil {
				t.Fatalf("the core refuses the tree: %v", err)
			}
			wantError(t, answerFor(root, tc.patterns...), tc.want)
		})
	}
}

// A glob metacharacter in a directory filepath.Glob resolves under is read
// as a pattern, so a pattern that globs under such a directory is an error,
// while the core's selection accepts the tree.
func TestAGlobMetacharacterInAResolvedDirectoryIsAnError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws[1]")
	writeFile(t, root, "app/go.mod", "module example.com/app\n")
	writeFile(t, root, "app/main.go", goSource("main"))
	if _, err := coreRawEmbedFiles(filepath.Join(root, "app"), []string{"*.go"}, goembed.BuildSelector); err != nil {
		t.Fatalf("the core refuses the tree: %v", err)
	}
	if answer := answerFor(root, "**/*.go"); answer.Error != "" {
		t.Fatalf("a ** include walks without a glob, but the answer is an error: %s", answer.Error)
	}
	wantError(t, answerFor(root, "*.go"), "glob metacharacters")

	plain := t.TempDir()
	writeFile(t, plain, "app/go.mod", "module example.com/app\n")
	writeFile(t, plain, "app/p*q/p.go", goSource("p", "a.txt"))
	writeFile(t, plain, "app/p*q/a.txt", "a")
	if _, err := coreRawEmbedFiles(filepath.Join(plain, "app"), nil, goembed.BuildSelector); err != nil {
		t.Fatalf("the core refuses the tree: %v", err)
	}
	wantError(t, answerFor(plain), "holds a glob metacharacter")
}

// On a case-insensitive file system, a name opened in another letter case
// is an error, because the basis matches names exactly.
func TestAPathOpenedUnderAnotherSpellingIsAnError(t *testing.T) {
	root := smallProject(t)
	if !caseInsensitive(t, root) {
		t.Skip("the file system matches names exactly")
	}
	writeFile(t, root, "app/main.go", goSource("main", "STATIC/a.txt"))
	wantError(t, answerFor(root), "under another spelling")

	writeFile(t, root, "app/main.go", goSource("main"))
	writeFile(t, root, "app/_impl/x.go", goSource("main"))
	symlink(t, root, "app/x.go", "_IMPL/x.go")
	wantError(t, answerFor(root), "under another spelling")
}

// A basis pattern built from a path matches that path alone, whatever glob
// metacharacters its names hold, and a folded one matches every ASCII letter
// case of it.
func TestLiteralPatternsMatchTheirPath(t *testing.T) {
	for _, rel := range []string{"plain/file.go", "!bang/x", "git:x/y", "a*b/c?d/[e]", "x]y/-^", "dir/!inner"} {
		pattern := literalPattern(rel, false)
		if strings.HasPrefix(pattern, "!") || strings.HasPrefix(pattern, "git:") {
			t.Fatalf("literalPattern(%q) = %q starts with a marker a basis reads", rel, pattern)
		}
		if diags := wsproto.ValidateInputResolveResult(&wsproto.InputResolveResult{
			Version: wsproto.InputResolveProtocolVersion, Extension: "@putnami/go",
			Inputs: []wsproto.InputResolveAnswer{{
				Project: "app", Selector: goembed.BuildSelector,
				Files: []string{rel}, Basis: wsproto.InputBasis{Content: [][]string{{pattern}}},
			}},
		}); diag.HasErrors(diags) {
			t.Fatalf("literalPattern(%q) = %q breaks the protocol:\n%s", rel, pattern, diag.ErrorText(diags))
		}
		if !wsproto.SelectsPath(rel, []string{pattern}) {
			t.Fatalf("literalPattern(%q) = %q does not select its path", rel, pattern)
		}
		for _, other := range []string{rel + "x", "x" + rel, strings.ReplaceAll(rel, "/", "/z")} {
			if wsproto.SelectsPath(other, []string{pattern}) {
				t.Fatalf("literalPattern(%q) = %q selects %q", rel, pattern, other)
			}
		}
	}
	folded := literalPattern("Tools/x", true)
	for _, rel := range []string{"tools/x", "TOOLS/X", "Tools/x"} {
		if !wsproto.SelectsPath(rel, []string{folded}) {
			t.Fatalf("literalPattern(Tools/x, fold) = %q does not select %q", folded, rel)
		}
	}
	if wsproto.SelectsPath("tool/x", []string{folded}) {
		t.Fatalf("literalPattern(Tools/x, fold) = %q selects tool/x", folded)
	}
}

// An error answer is valid UTF-8, never blank, and at most 4096 bytes, cut
// at a character boundary.
func TestErrorTextIsBounded(t *testing.T) {
	long := errors.New(strings.Repeat("é", 3000) + "\xff")
	text := errorText(long)
	if len(text) > maxErrorBytes || !utf8.ValidString(text) || len(text) < maxErrorBytes-utf8.UTFMax {
		t.Fatalf("errorText of a long message is %d bytes, valid UTF-8 %v", len(text), utf8.ValidString(text))
	}
	if got := errorText(errors.New("  ")); got != "input resolution failed" {
		t.Fatalf("errorText(blank) = %q", got)
	}
	if got := errorText(errors.New("short")); got != "short" {
		t.Fatalf("errorText(short) = %q", got)
	}
}

// An answer the protocol would refuse becomes an error answer that says
// why, so one input cannot fail the whole call.
func TestAnAnswerTheProtocolRefusesBecomesAnError(t *testing.T) {
	err := validateAnswer(wsproto.InputResolveAnswer{
		Project: "app", Selector: goembed.BuildSelector, Files: []string{"b", "a"},
		Basis: wsproto.InputBasis{Content: [][]string{{"**"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid-order") {
		t.Fatalf("validateAnswer(unsorted files) = %v", err)
	}
}

// A link target climbs out of a directory it names only after a named
// segment.
func TestClimbsOutOfANamedDirectory(t *testing.T) {
	for target, want := range map[string]bool{
		"../shared/x.go":     false,
		"../../a/b.go":       false,
		"./x.go":             false,
		"a//b.go":            false,
		"a/../b.go":          true,
		"../a/../b.go":       true,
		"a/./../b.go":        true,
		"/abs/../x.go":       true,
		"..":                 false,
		"x.go":               false,
		"dir/sub/../x/y.go":  true,
		"dir/sub/x/../y.go":  true,
		"../../dir/file.go":  false,
		"../dir/./file.go":   false,
		"dir/.../file.go":    false,
		"dir/..file/file.go": false,
	} {
		if got := climbsOutOfANamedDirectory(target); got != want {
			t.Errorf("climbsOutOfANamedDirectory(%q) = %v, want %v", target, got, want)
		}
	}
}

// A file in a directory between the project and a listed file that holds a
// go.mod, written after the scan, fails the input.
func TestCheckFileModulesRefusesANestedModule(t *testing.T) {
	root := smallProject(t)
	writeFile(t, root, "app/static/go.mod", "module example.com/static\n")
	r := &resolution{dir: filepath.Join(root, "app"), files: []string{"static/a.txt"}, spelling: spellingCache{}}
	err := r.checkFileModules(r.fileAncestors())
	if err == nil || !strings.Contains(err.Error(), "nested module") {
		t.Fatalf("checkFileModules = %v", err)
	}
	if err := os.Remove(filepath.Join(root, "app", "static", "go.mod")); err != nil {
		t.Fatal(err)
	}
	if err := r.checkFileModules(r.fileAncestors()); err != nil {
		t.Fatalf("checkFileModules without a go.mod = %v", err)
	}
}
