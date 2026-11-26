package repositorychecks

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// siteManifest is the project manifest of the site that serves
// https://putnami.dev/docs. Its generate assets name the documentation folders
// it copies under public/docs.
const siteManifest = "sites/putnami.dev/putnami.json"

// movedCommandsSourceRepoPath is the CLI source that declares the URL the
// CLI cites for moved commands, and movedCommandsConstant the constant that
// holds it. The CLI's own tests prove that its messages cite that constant;
// this check reads the declaration as committed text, so it needs no build of
// the CLI.
const (
	movedCommandsSourceRepoPath = "tooling/cli/internal/commandmeta/moved_commands.go"
	movedCommandsConstant       = "CommandsThatLeftTheCoreURL"
)

// TestCommandsThatLeftTheCoreURLNamesAPublishedHeading holds the reference the
// CLI cites for moved commands to a heading the site publishes: the URL path is
// the page of exactly one Markdown file the site copies under public/docs, and
// the fragment is the anchor of one of that file's headings.
func TestCommandsThatLeftTheCoreURLNamesAPublishedHeading(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	cited := stringConstant(t, filepath.Join(root, filepath.FromSlash(movedCommandsSourceRepoPath)), movedCommandsConstant)
	reference, err := url.Parse(cited)
	if err != nil {
		t.Fatalf("parse %s: %v", cited, err)
	}
	if reference.Scheme != "https" || reference.Host != "putnami.dev" || reference.Fragment == "" {
		t.Fatalf("%s is not a heading of a putnami.dev page", cited)
	}
	page := publishedPage(t, root, reference.Path)
	data, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	if !headingAnchors(string(data))[reference.Fragment] {
		t.Fatalf("%s has no heading whose anchor is #%s", page, reference.Fragment)
	}
}

// stringConstant returns the value of the string constant name declared in the
// Go file at path.
func stringConstant(t *testing.T, path, name string) string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, ident := range value.Names {
				if ident.Name != name || index >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[index].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("%s declares %s as something other than a string literal", path, name)
				}
				unquoted, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("unquote %s in %s: %v", name, path, err)
				}
				return unquoted
			}
		}
	}
	t.Fatalf("%s declares no constant %s", path, name)
	return ""
}

// TestPublishedDocumentationIsKeyed keeps every page the check above may read
// inside this project's cache key: every Markdown file under every
// documentation folder the site copies under public/docs is a file of the
// candidate cut, which the whole-cut declaration keys. A published page that
// Git ignores would be invisible to the key, and a warm cache would answer its
// edit with the previous pass.
func TestPublishedDocumentationIsKeyed(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	if !releaseKeySelects(releaseDeclaredTestInputs(t)) {
		t.Fatalf("this project no longer declares %q, so the published pages are not keyed", releaseWholeCutPattern)
	}
	paths, err := candidatePathsOnly(root)
	if err != nil {
		t.Fatalf("enumerate the candidate cut: %v", err)
	}
	candidates := make(map[string]bool)
	for _, candidate := range paths {
		candidates[candidate] = true
	}
	checked := 0
	for _, source := range publishedDocumentationFolders(t, root) {
		err := filepath.WalkDir(source, func(file string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || filepath.Ext(file) != ".md" {
				return err
			}
			rel, err := filepath.Rel(root, file)
			if err != nil {
				return err
			}
			checked++
			if !candidates[filepath.ToSlash(rel)] {
				t.Errorf("%s is published under public/docs but is not a file of the candidate cut, so no cache key covers it", filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", source, err)
		}
	}
	if checked == 0 {
		t.Fatalf("%s publishes no Markdown file under public/docs, so this check proves nothing", siteManifest)
	}
}

// TestHeadingAnchorsFollowTheSite pins the two derivations the check above
// shares with the site: the page path of a Markdown file under public/docs and
// the anchor of a heading.
func TestHeadingAnchorsFollowTheSite(t *testing.T) {
	t.Parallel()
	for contentPath, want := range map[string]string{
		"08-tooling-&-workspace/02-cli.md":        "/docs/tooling-&-workspace/cli",
		"02-concepts/cli-telemetry.md":            "/docs/concepts/cli-telemetry",
		"09-frameworks/01-typescript/00-intro.md": "/docs/frameworks/typescript/intro",
	} {
		if got := docsURLPath(contentPath); got != want {
			t.Errorf("docsURLPath(%q) = %q, want %q", contentPath, got, want)
		}
	}
	anchors := headingAnchors("# CLI\n\n## Commands that left the core CLI\n\n```bash\n# Bash\n```\n### `putnami workspace`\n#not-a-heading\n")
	for _, want := range []string{"cli", "commands-that-left-the-core-cli", "putnami-workspace"} {
		if !anchors[want] {
			t.Errorf("anchors = %v, want %q", anchors, want)
		}
	}
	for _, unwanted := range []string{"bash", "not-a-heading"} {
		if anchors[unwanted] {
			t.Errorf("anchors = %v, want no %q", anchors, unwanted)
		}
	}
}

// publishedPage returns the one Markdown file the site serves at urlPath, from
// the documentation folders the site manifest copies under public/docs.
func publishedPage(t *testing.T, root, urlPath string) string {
	t.Helper()
	var pages []string
	for _, folder := range publishedDocumentationAssets(t, root) {
		err := filepath.WalkDir(folder.source, func(file string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || filepath.Ext(file) != ".md" {
				return err
			}
			rel, err := filepath.Rel(folder.source, file)
			if err != nil {
				return err
			}
			if docsURLPath(path.Join(folder.section, filepath.ToSlash(rel))) == urlPath {
				pages = append(pages, file)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", folder.source, err)
		}
	}
	if len(pages) != 1 {
		t.Fatalf("the site serves %s from %d files %v, want exactly one", urlPath, len(pages), pages)
	}
	return pages[0]
}

// publishedDocumentationAsset is one documentation folder the site copies
// under public/docs: its source directory and the section it lands in.
type publishedDocumentationAsset struct {
	source  string
	section string
}

// publishedDocumentationAssets reads the site manifest's generate assets that
// land under public/docs.
func publishedDocumentationAssets(t *testing.T, root string) []publishedDocumentationAsset {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(siteManifest)))
	if err != nil {
		t.Fatalf("read %s: %v", siteManifest, err)
	}
	var manifest struct {
		Options struct {
			Generate struct {
				Assets []struct {
					From string `json:"from"`
					To   string `json:"to"`
				} `json:"assets"`
			} `json:"generate"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse %s: %v", siteManifest, err)
	}
	var assets []publishedDocumentationAsset
	for _, asset := range manifest.Options.Generate.Assets {
		if asset.To != "public/docs" && !strings.HasPrefix(asset.To, "public/docs/") {
			continue
		}
		assets = append(assets, publishedDocumentationAsset{
			source:  filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(asset.From, "/"))),
			section: strings.TrimPrefix(strings.TrimPrefix(asset.To, "public/docs"), "/"),
		})
	}
	return assets
}

// publishedDocumentationFolders returns the source directory of every
// documentation folder the site copies under public/docs.
func publishedDocumentationFolders(t *testing.T, root string) []string {
	t.Helper()
	var folders []string
	for _, asset := range publishedDocumentationAssets(t, root) {
		folders = append(folders, asset.source)
	}
	return folders
}

// contentPathOrder matches the order prefixes the site drops from a content
// path.
var contentPathOrder = regexp.MustCompile(`/?\d+-`)

// docsURLPath is the page path the site serves a Markdown file at, from its
// path under public/docs (toUrlPath in sites/putnami.dev/src/lib/docs/navigation.ts).
func docsURLPath(contentPath string) string {
	page := contentPathOrder.ReplaceAllString(strings.TrimSuffix(contentPath, ".md"), "/")
	return "/docs/" + strings.TrimPrefix(page, "/")
}

var (
	markdownHeading = regexp.MustCompile("^#{1,6}[ \t]+(.+?)[ \t#]*$")
	anchorSeparator = regexp.MustCompile(`[^a-z0-9]+`)
)

// headingAnchors returns the anchors of the headings of a Markdown page outside
// fenced code blocks, as the site derives them from the heading text
// (slugifyPlain in sites/putnami.dev/src/lib/markdown-utils.ts).
func headingAnchors(markdown string) map[string]bool {
	anchors := map[string]bool{}
	fenced := false
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		match := markdownHeading.FindStringSubmatch(line)
		if fenced || match == nil {
			continue
		}
		anchor := strings.Trim(anchorSeparator.ReplaceAllString(strings.ToLower(match[1]), "-"), "-")
		anchors[anchor] = true
	}
	return anchors
}
