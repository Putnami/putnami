package documents

import (
	"encoding/json"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

// siteManifest is the project manifest of the site that serves
// https://putnami.dev/docs. Its generate assets name the documentation folders
// it copies under public/docs.
const siteManifest = "sites/putnami.dev/putnami.json"

// TestCommandsThatLeftTheCoreURLNamesAPublishedHeading holds the reference the
// CLI cites for moved commands to a heading the site publishes: the URL path is
// the page of exactly one Markdown file the site copies under public/docs, and
// the fragment is the anchor of one of that file's headings.
func TestCommandsThatLeftTheCoreURLNamesAPublishedHeading(t *testing.T) {
	t.Parallel()
	reference, err := url.Parse(commandmeta.CommandsThatLeftTheCoreURL)
	if err != nil {
		t.Fatalf("parse %s: %v", commandmeta.CommandsThatLeftTheCoreURL, err)
	}
	if reference.Scheme != "https" || reference.Host != "putnami.dev" || reference.Fragment == "" {
		t.Fatalf("%s is not a heading of a putnami.dev page", commandmeta.CommandsThatLeftTheCoreURL)
	}
	page := publishedPage(t, repositoryRoot(t), reference.Path)
	data, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	if !headingAnchors(string(data))[reference.Fragment] {
		t.Fatalf("%s has no heading whose anchor is #%s", page, reference.Fragment)
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
	var pages []string
	for _, asset := range manifest.Options.Generate.Assets {
		if asset.To != "public/docs" && !strings.HasPrefix(asset.To, "public/docs/") {
			continue
		}
		section := strings.TrimPrefix(strings.TrimPrefix(asset.To, "public/docs"), "/")
		source := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(asset.From, "/")))
		err := filepath.WalkDir(source, func(file string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || filepath.Ext(file) != ".md" {
				return err
			}
			rel, err := filepath.Rel(source, file)
			if err != nil {
				return err
			}
			if docsURLPath(path.Join(section, filepath.ToSlash(rel))) == urlPath {
				pages = append(pages, file)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", source, err)
		}
	}
	if len(pages) != 1 {
		t.Fatalf("the site serves %s from %d files %v, want exactly one", urlPath, len(pages), pages)
	}
	return pages[0]
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
