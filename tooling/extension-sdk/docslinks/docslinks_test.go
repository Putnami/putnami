package docslinks

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func messages(t *testing.T, root string, findings []Finding) []string {
	t.Helper()
	out := make([]string, 0, len(findings))
	for _, finding := range findings {
		rel, err := filepath.Rel(root, finding.File)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, filepath.ToSlash(rel)+":"+strconv.Itoa(finding.Line)+":"+strconv.Itoa(finding.Column)+" "+finding.Target)
	}
	return out
}

func TestCheckReportsMissingFilesAndAnchors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib/doc/guide.md", "# Guide\n\n## Set up `putnami.json`\n\nText.\n")
	readme := writeFile(t, root, "lib/README.md", strings.Join([]string{
		"# Lib",
		"",
		"Read [the guide](doc/guide.md) and [set up](doc/guide.md#set-up-putnamijson).",
		"A [missing page](doc/missing.md) and a [missing anchor](doc/guide.md#nowhere).",
		"Jump to [usage](#usage) or [nothing](#nothing).",
		"",
		"## Usage",
		"",
		"[ref]: ./doc/gone.md",
	}, "\n"))

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	got := messages(t, root, findings)
	want := []string{
		"lib/README.md:4:18 doc/missing.md",
		"lib/README.md:4:57 doc/guide.md#nowhere",
		"lib/README.md:5:38 #nothing",
		"lib/README.md:9:8 ./doc/gone.md",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %q, want %q", got, want)
	}
	if !strings.Contains(findings[0].Message, "lib/doc/missing.md, which does not exist") {
		t.Fatalf("message = %q", findings[0].Message)
	}
	if !strings.Contains(findings[2].Message, "names no heading of this file") {
		t.Fatalf("message = %q", findings[2].Message)
	}
}

func TestCheckIgnoresNonRelativeLinksAndCode(t *testing.T) {
	root := t.TempDir()
	readme := writeFile(t, root, "README.md", strings.Join([]string{
		"---",
		"title: [front](missing-front.md)",
		"---",
		"# Title",
		"",
		"[site](https://putnami.dev/docs) [mail](mailto:a@b.c) [route](/docs/x)",
		"[proto](//cdn.example/x) `[code](missing-code.md)` ``[a `b`](missing.md)``",
		"<!-- [comment](missing-comment.md) -->",
		"<!--",
		"[multi](missing-multi.md)",
		"-->",
		"```md",
		"[fenced](missing-fenced.md)",
		"```",
		"~~~~",
		"```",
		"[tilde](missing-tilde.md)",
		"~~~~",
		`\[not a link](missing-escaped.md)`,
	}, "\n"))

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %q, want none", messages(t, root, findings))
	}
}

func TestCheckReadsImagesHTMLAndAngleDestinations(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "logo.svg", "<svg/>")
	writeFile(t, root, "a b.md", "# A\n")
	readme := writeFile(t, root, "README.md", strings.Join([]string{
		`[![badge](logo.svg)](missing-badge.md) ![gone](gone.png)`,
		`<img src="logo.svg"> <a href="missing-html.md">x</a>`,
		`[spaced](<a b.md>) [escaped](a%20b.md#a) [titled](logo.svg "Logo")`,
		`[parens](file_(1).md)`,
	}, "\n"))

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	got := messages(t, root, findings)
	want := []string{
		"README.md:1:22 missing-badge.md",
		"README.md:1:48 gone.png",
		"README.md:2:31 missing-html.md",
		"README.md:4:10 file_(1).md",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %q, want %q", got, want)
	}
}

func TestCheckRequiresTheCaseOnDisk(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "doc/Guide.md", "# Guide\n")
	readme := writeFile(t, root, "README.md", "[exact](doc/Guide.md) [lower](doc/guide.md) [dir](doc) [dir slash](doc/)\n")

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	got := messages(t, root, findings)
	if want := []string{"README.md:1:31 doc/guide.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %q, want %q", got, want)
	}
}

func TestCheckRefusesLinksThatLeaveTheWorkspace(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ws")
	writeFile(t, parent, "outside.md", "# Outside\n")
	readme := writeFile(t, root, "README.md", "[out](../outside.md)\n")

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0].Message, "leaves the workspace") {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestCheckIgnoresAnchorsOfOtherFileTypes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n")
	readme := writeFile(t, root, "README.md", "[line](main.go#L1) [dir](.#x)\n")

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %q", messages(t, root, findings))
	}
}

func TestCheckFailsOnAnUnreadableFile(t *testing.T) {
	if _, err := Check(t.TempDir(), []string{filepath.Join(t.TempDir(), "missing.md")}); err == nil {
		t.Fatal("Check returned no error for a missing file")
	}
}

func TestCheckRejectsAnInvalidEscape(t *testing.T) {
	root := t.TempDir()
	readme := writeFile(t, root, "README.md", "[bad](a%zz.md)\n")
	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0].Message, "not a valid path") {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestAnchors(t *testing.T) {
	source := strings.Join([]string{
		"# Getting Started",
		"## Getting Started",
		"## Getting Started ##",
		"### `init` and `sync` — the authoring half",
		"#### What's _new_ in [v2](https://x) <sup>beta</sup>",
		"## `snake_case_name` and __bold__",
		"Setext Title",
		"============",
		"",
		"- list item",
		"---",
		"<a id=\"Custom-Anchor\"></a>",
		"```",
		"# Not a heading",
		"```",
		"## Unicode: Café Übung",
	}, "\n")
	got := Anchors([]byte(source))
	for _, want := range []string{
		"getting-started",
		"getting-started-1",
		"getting-started-2",
		"init-and-sync--the-authoring-half",
		"whats-new-in-v2-beta",
		"snake_case_name-and-bold",
		"setext-title",
		"custom-anchor",
		"unicode-café-übung",
	} {
		if !got[want] {
			t.Errorf("anchor %q missing from %v", want, got)
		}
	}
	for _, unwanted := range []string{"not-a-heading", "--list-item", "list-item"} {
		if got[unwanted] {
			t.Errorf("anchor %q present in %v", unwanted, got)
		}
	}
}

func TestProjectDocumentsSkipsNestedProjectsAndFixtures(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"README.md",
		"doc/guide.md",
		"doc/deep/page.md",
		"src/README.md",
		"src/notes.md",
		"nested/putnami.json",
		"nested/README.md",
		"module/go.mod",
		"module/README.md",
		"testdata/README.md",
		"node_modules/pkg/README.md",
		".gen/README.md",
		"_build/README.md",
		"CHANGELOG.md",
	} {
		writeFile(t, root, rel, "# x\n")
	}
	files, err := ProjectDocuments(root)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(files))
	for _, file := range files {
		rel, _ := filepath.Rel(root, file)
		got = append(got, filepath.ToSlash(rel))
	}
	want := []string{"README.md", "doc/deep/page.md", "doc/guide.md", "src/README.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("documents = %q, want %q", got, want)
	}
}

func TestWorkspaceDocumentsGroupsEachDocumentByItsProject(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"README.md",
		"scope/putnami.json",
		"scope/doc/page.md",
		"scope/notes.md",
		"lib/README.md",
		"lib/doc/guide.md",
		"lib/tool/go.mod",
		"lib/tool/README.md",
		"site/doc/app/README.md",
		"site/doc/app/notes.md",
		"site/doc/app/doc/page.md",
		"node_modules/pkg/README.md",
		"_hidden/app/README.md",
	} {
		writeFile(t, root, rel, "# x\n")
	}
	lib := filepath.Join(root, "lib")
	app := filepath.Join(root, "site", "doc", "app")
	hidden := filepath.Join(root, "_hidden", "app")
	grouped, err := WorkspaceDocuments(root, []string{lib, app, hidden, filepath.Join(root, "absent")})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string][]string, len(grouped))
	for owner, files := range grouped {
		key, _ := filepath.Rel(root, owner)
		for _, file := range files {
			rel, _ := filepath.Rel(root, file)
			got[filepath.ToSlash(key)] = append(got[filepath.ToSlash(key)], filepath.ToSlash(rel))
		}
	}
	want := map[string][]string{
		// A scope and the documents outside every project belong to the root.
		".": {"README.md", "scope/doc/page.md"},
		// A nested Go module that is not a project belongs to the project.
		"lib": {"lib/README.md", "lib/doc/guide.md", "lib/tool/README.md"},
		// A project under a doc directory is read as its lint reads it.
		"site/doc/app": {"site/doc/app/README.md", "site/doc/app/doc/page.md"},
		// A project the walk skips is read on its own.
		"_hidden/app": {"_hidden/app/README.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("documents = %q, want %q", got, want)
	}
}

func TestWorkspaceDocumentsReadsARootProject(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "# x\n")
	writeFile(t, root, "scope/putnami.json", "{}")
	writeFile(t, root, "scope/README.md", "# x\n")
	grouped, err := WorkspaceDocuments(root, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if files := grouped[root]; len(files) != 2 || len(grouped) != 1 {
		t.Fatalf("documents = %q, want README.md and scope/README.md under the root", grouped)
	}
}

func TestWorkspaceDocumentsFailsOnAMissingRoot(t *testing.T) {
	if _, err := WorkspaceDocuments(filepath.Join(t.TempDir(), "absent"), nil); err == nil {
		t.Fatal("WorkspaceDocuments returned no error for a missing root")
	}
}

func TestCheckProject(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib/README.md", "[gone](gone.md)\n")
	writeFile(t, root, "lib/nested/putnami.json", "{}")
	writeFile(t, root, "lib/nested/README.md", "[also gone](gone.md)\n")
	findings, err := CheckProject(root, filepath.Join(root, "lib"))
	if err != nil {
		t.Fatal(err)
	}
	if got := messages(t, root, findings); !reflect.DeepEqual(got, []string{"lib/README.md:1:8 gone.md"}) {
		t.Fatalf("findings = %q", got)
	}
	if _, err := CheckProject(root, filepath.Join(root, "absent")); err == nil {
		t.Fatal("CheckProject returned no error for a missing project")
	}
}

func TestCheckReadsReferenceDefinitionsOnly(t *testing.T) {
	root := t.TempDir()
	readme := writeFile(t, root, "README.md", strings.Join([]string{
		"Text[^1] and more.",
		"",
		"[^1]: See the docs for more.",
		"[Note]: this is important",
		`[titled]: ./gone.md "A title"`,
		"[bare]: ./also-gone.md",
	}, "\n"))

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	got := messages(t, root, findings)
	want := []string{"README.md:5:11 ./gone.md", "README.md:6:9 ./also-gone.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %q, want %q", got, want)
	}
}

func TestCheckSkipsCodeInsideListItems(t *testing.T) {
	root := t.TempDir()
	readme := writeFile(t, root, "README.md", strings.Join([]string{
		"1. Step",
		"",
		"    ```md",
		"    see [x](fenced-in-list.md)",
		"    ```",
		"",
		"- item",
		"",
		"      [y](indented-in-list.md)",
		"",
		"  A paragraph of the item with [a broken link](broken-in-list.md).",
		"",
		"Top level again.",
		"",
		"    [z](indented-top-level.md)",
		"",
		"[kept](broken-top-level.md)",
	}, "\n"))

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	got := messages(t, root, findings)
	want := []string{"README.md:11:48 broken-in-list.md", "README.md:17:8 broken-top-level.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %q, want %q", got, want)
	}
}

func TestCheckUnescapesAndRefusesBackslashes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "real_x.md", "# X\n")
	writeFile(t, root, "doc/guide.md", "# Guide\n")
	readme := writeFile(t, root, "README.md", `[escaped](real\_x.md) [separator](doc\guide.md)`+"\n")

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Column != 35 || !strings.Contains(findings[0].Message, "holds a backslash") {
		t.Fatalf("findings = %+v, want one backslash finding at column 35", findings)
	}
}

func TestCheckReadsALinkWhoseTextWrapsALine(t *testing.T) {
	root := t.TempDir()
	readme := writeFile(t, root, "README.md", strings.Join([]string{
		"A [link whose text",
		"wraps](wrapped-missing.md) and a closed [one] here.",
		"",
		"text](not-a-link.md)",
	}, "\n"))

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	got := messages(t, root, findings)
	if want := []string{"README.md:2:8 wrapped-missing.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %q, want %q", got, want)
	}
}

func TestAnchorsFollowGitHubOnEntitiesNumbersAndUnderscores(t *testing.T) {
	source := strings.Join([]string{
		"# Foo &amp; Bar",
		"## Step ½ done",
		"## a _ b _ c",
		"paragraph line",
		"```",
		"code",
		"```",
		"---",
	}, "\n")
	got := Anchors([]byte(source))
	for _, want := range []string{"foo--bar", "step-½-done", "a-_-b-_-c"} {
		if !got[want] {
			t.Errorf("anchor %q missing from %v", want, got)
		}
	}
	if got["paragraph-line"] {
		t.Errorf("a thematic break after a fence made a heading: %v", got)
	}
}

func TestCheckSkipsCodeInQuotesAndAfterAHeadingEndsAList(t *testing.T) {
	root := t.TempDir()
	readme := writeFile(t, root, "README.md", strings.Join([]string{
		"- item",
		"# Heading",
		"",
		"    code [h](after-heading.md)",
		"",
		"> ```",
		"> [q](quoted-fence.md)",
		"> ```",
		"",
		"> [kept](quoted-broken.md)",
	}, "\n"))

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	got := messages(t, root, findings)
	if want := []string{"README.md:10:10 quoted-broken.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %q, want %q", got, want)
	}
}

func TestCheckClosesAFenceOnlyAtItsOwnQuoteDepth(t *testing.T) {
	root := t.TempDir()
	readme := writeFile(t, root, "README.md", strings.Join([]string{
		"```markdown",
		"> ```",
		"> [sample](inside-a-top-level-fence.md)",
		"> ```",
		"```",
		"",
		"> ```",
		"> [q](inside-a-quoted-fence.md)",
		"",
		"[after](after-the-quote.md)",
	}, "\n"))

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	got := messages(t, root, findings)
	if want := []string{"README.md:10:9 after-the-quote.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %q, want %q", got, want)
	}
}

func TestCheckRefusesALinkThatLeavesThroughASymlink(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, outside, "secret.md", "# Secret\n")
	root := t.TempDir()
	writeFile(t, root, "doc/inside.md", "# Inside\n")
	readme := writeFile(t, root, "README.md", strings.Join([]string{
		"[out](out/secret.md#secret)",
		"[file](escape.md)",
		"[in](alias/inside.md#inside)",
	}, "\n"))
	for link, target := range map[string]string{"out": outside, "escape.md": filepath.Join(outside, "secret.md"), "alias": filepath.Join(root, "doc")} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Skipf("this host cannot create a symbolic link: %v", err)
		}
	}

	findings, err := Check(root, []string{readme})
	if err != nil {
		t.Fatal(err)
	}
	got := messages(t, root, findings)
	if want := []string{"README.md:1:7 out/secret.md#secret", "README.md:2:8 escape.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %q, want %q", got, want)
	}
	if !strings.Contains(findings[0].Message, "symbolic link that leaves the workspace") {
		t.Fatalf("message = %q", findings[0].Message)
	}
}

func TestProjectDocumentsFollowsAReadmeLinkToAFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "doc/index.md", "# Index\n")
	if err := os.Symlink(filepath.Join("doc", "index.md"), filepath.Join(root, "README.md")); err != nil {
		t.Skipf("this host cannot create a symbolic link: %v", err)
	}
	files, err := ProjectDocuments(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || filepath.Base(files[0]) != "README.md" {
		t.Fatalf("documents = %q, want README.md and doc/index.md", files)
	}
}
