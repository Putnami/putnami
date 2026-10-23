package inputresolve

import (
	"reflect"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/goembed"
)

// TestAnswerKeepsTheCoreKeyFixtureFiles answers the tree of the CLI store's
// TestGoEmbedSelectorKeysArePinned and holds the files to the lists that test
// hashes (goEmbedAnswers in tooling/cli/internal/store/input_expander_test.go).
// That test pins the key of a Go task whose selectors list those files, so the
// two tests together pin the key a Go task computes through this extension:
// an answer that moves here moves the key of every Go task.
func TestAnswerKeepsTheCoreKeyFixtureFiles(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		"app/go.mod":               "module example.com/app\n\ngo 1.26\n",
		"app/main.go":              "package main\n\nimport \"embed\"\n\n//go:embed static.txt\nvar static string\n\n//go:embed assets\nvar assets embed.FS\n\nfunc main() {}\n",
		"app/main_test.go":         "package main\n\nimport (\n\t_ \"embed\"\n\t\"testing\"\n)\n\n//go:embed testdata/fixture.txt\nvar fixture string\n\nfunc TestRun(t *testing.T) {}\n",
		"app/static.txt":           "static\n",
		"app/assets/a.txt":         "a\n",
		"app/assets/sub/b.txt":     "b\n",
		"app/assets/.hidden":       "hidden\n",
		"app/src/alias.txt":        "package main\n\nconst Alias = 1\n",
		"app/shared/linked.txt":    "package testdata\n",
		"app/testdata/fixture.txt": "fixture\n",
		"app/testdata/helper.go":   "package testdata\n",
		"app/.gen/client.gen.go":   "package client\n",
		"app/README.md":            "not an input\n",
	} {
		writeFile(t, root, rel, content)
	}
	symlink(t, root, "app/alias.go", "src/alias.txt")
	symlink(t, root, "app/testdata/linked.go", "../shared/linked.txt")

	for _, tc := range []struct {
		selector string
		patterns []string
		want     []string
	}{
		{
			selector: goembed.BuildSelector,
			patterns: buildPatterns,
			want: []string{
				"alias.go", "assets/a.txt", "assets/sub/b.txt", "main.go", "shared/linked.txt",
				"src/alias.txt", "static.txt", "testdata/linked.go",
			},
		},
		{
			selector: goembed.TestSelector,
			patterns: testPatterns,
			want: []string{
				"alias.go", "assets/a.txt", "assets/sub/b.txt", "main.go", "main_test.go", "shared/linked.txt",
				"src/alias.txt", "static.txt", "testdata/fixture.txt", "testdata/linked.go",
			},
		},
	} {
		query := wsproto.InputResolveQuery{Project: "app", Selector: tc.selector, Patterns: tc.patterns}
		answer := Answer(root, query)
		if answer.Error != "" || !reflect.DeepEqual(answer.Files, tc.want) {
			t.Errorf("%s answer = %q %v\nwant %v", tc.selector, answer.Error, answer.Files, tc.want)
		}
		assertValidAnswer(t, query, answer)
	}
}

// go-embed:build reads no directive of a test source, and go-embed:test reads
// it, so an edit to a file only a test embeds moves the test key alone.
func TestATestOnlyEmbedIsATestInputAlone(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "embedded-source-cache-inputs", "a-test-only-embed-is-not-a-build-input")
	root := smallProject(t)
	writeFile(t, root, "app/main_test.go", goSource("main", "testdata/fixture.txt"))
	writeFile(t, root, "app/testdata/fixture.txt", "fixture")
	build := answerFor(root, buildPatterns...)
	if build.Error != "" || slices.Contains(build.Files, "testdata/fixture.txt") || slices.Contains(build.Files, "main_test.go") {
		t.Fatalf("build answer = %q %v, want neither the test source nor its embed", build.Error, build.Files)
	}
	test := Answer(root, wsproto.InputResolveQuery{Project: "app", Selector: goembed.TestSelector, Patterns: testPatterns})
	if test.Error != "" || !slices.Contains(test.Files, "testdata/fixture.txt") || !slices.Contains(test.Files, "main_test.go") {
		t.Fatalf("test answer = %q %v, want the test source and its embed", test.Error, test.Files)
	}
}

// A .go file under .gen, which another task writes during the run, is no
// source input and its directives are not read; a file under .gen that a
// source's //go:embed names is listed, and the core hashes a listed file's
// bytes, so its edit moves the key.
func TestAGeneratedTreeAddsOnlyWhatAnEmbedNames(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-generated-tree-sources", "an-embedded-file-under-the-generated-tree-is-listed")
	for _, tc := range []struct {
		selector string
		patterns []string
	}{{goembed.BuildSelector, buildPatterns}, {goembed.TestSelector, testPatterns}} {
		t.Run(tc.selector, func(t *testing.T) {
			root := smallProject(t)
			writeFile(t, root, "app/.gen/clientgen/go/client.gen.go", goSource("client", "spec.json"))
			writeFile(t, root, "app/.gen/clientgen/go/spec.json", "{}")
			query := wsproto.InputResolveQuery{Project: "app", Selector: tc.selector, Patterns: tc.patterns}
			before := Answer(root, query)
			if before.Error != "" || !reflect.DeepEqual(before.Files, []string{"main.go", "static/a.txt"}) {
				t.Fatalf("answer = %q %v, want no file under .gen", before.Error, before.Files)
			}
			writeFile(t, root, "app/assets.go", goSource("main", ".gen/assets/security.txt"))
			writeFile(t, root, "app/.gen/assets/security.txt", "A")
			after := Answer(root, query)
			if after.Error != "" || !slices.Contains(after.Files, ".gen/assets/security.txt") || slices.Contains(after.Files, ".gen/clientgen/go/client.gen.go") {
				t.Fatalf("answer = %q %v, want the embedded file under .gen and no generated source", after.Error, after.Files)
			}
		})
	}
}
