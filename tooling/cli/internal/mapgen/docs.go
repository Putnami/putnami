package mapgen

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// summaryRuneCap bounds a README summary so one project with a long opening
// paragraph cannot dominate the map. The cut is on a RUNE boundary and the
// budget is a constant, so the truncation is deterministic.
const summaryRuneCap = 200

// docsDir is the workspace-relative documentation root whose whole *.md tree the
// reduce indexes when it exists. It is part of the one genuinely global file
// input (the other global input is the workspace manifest itself).
const docsDir = "docs"

// readmeFilename is the second half of that input: a README anywhere in the
// workspace is documentation too. Scope-level READMEs (`business/README.md`)
// describe a whole domain and have no other representation in the map at all,
// and a README inside a project (`…/internal/gomod/README.md`) documents a
// package the map otherwise only names.
const readmeFilename = "README.md"

// prunedDirNames are the directory names the document walk never descends into,
// matched by NAME at any depth and skipped BEFORE descending — the walk covers
// the whole workspace now and its cost is paid on every build that refreshes the
// map. Anything dot-prefixed is covered separately (it catches .git, .gen,
// .putnami, .claude, .agents in one rule), which leaves the three names that hold
// bulk non-authored content.
var prunedDirNames = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"testdata":     true,
}

// readmeSummary returns the first prose line of a README: the first non-empty
// line that is not a heading, not a fence, not inside one, and not an HTML
// comment (READMEs in this tree open with `<!-- protocol-version: … -->`
// markers). A leading blockquote marker is stripped, so a README that opens with
// a `> **Status:** …` callout still yields readable prose. Empty when the README
// has no prose at all.
func readmeSummary(data []byte) string {
	inFence := false
	inComment := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		line, inComment = stripHTMLComments(line, inComment)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, ">"))
		if line == "" {
			continue
		}
		return truncateRunes(oneLine(line), summaryRuneCap)
	}
	return ""
}

// stripHTMLComments removes every HTML-comment span from one line and carries
// the open-comment state across lines. Only skipping lines that BEGIN with
// `<!--` mis-selects the middle of a multiline comment as prose — which is how
// three protocol READMEs' `ProtocolVersion` conformance-anchor comments once
// became their public summaries. Text after a closing `-->` on the same line
// survives, so an inline `<!-- marker --> prose` still yields its prose.
func stripHTMLComments(line string, inComment bool) (string, bool) {
	var out strings.Builder
	for {
		if inComment {
			end := strings.Index(line, "-->")
			if end < 0 {
				return strings.TrimSpace(out.String()), true
			}
			line = line[end+len("-->"):]
			inComment = false
			continue
		}
		start := strings.Index(line, "<!--")
		if start < 0 {
			out.WriteString(line)
			return strings.TrimSpace(out.String()), false
		}
		out.WriteString(line[:start])
		line = line[start+len("<!--"):]
		inComment = true
	}
}

// firstHeading returns a markdown document's first heading text, with the
// leading hashes stripped. Lines inside fenced code blocks are not headings even
// when they start with '#' (a shell comment in a fenced example). Empty when the
// document has none.
func firstHeading(data []byte) string {
	inFence := false
	inComment := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		// Same comment-state rule as readmeSummary: a `# line` inside a
		// multiline HTML comment is commentary, not the document's heading.
		line, inComment = stripHTMLComments(line, inComment)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(line, "#") {
			return truncateRunes(strings.TrimSpace(strings.TrimLeft(line, "#")), summaryRuneCap)
		}
	}
	return ""
}

// indexDocs walks the workspace and returns its authored documents — every *.md
// under the workspace-root docs/ tree, plus every README.md anywhere that is not
// a project root's — sorted by workspace-relative path, each with its first
// heading as a title.
//
// projectReadmes is the exclusion set: a project-root README is already carried
// by that project's entry (`readme` and `summary`), so indexing it here would
// duplicate it. Passing the set in is why this stays a reduce-stage global input
// with no fragment change.
//
// A workspace with neither a docs/ tree nor a non-project README yields nil —
// the section is simply absent.
func indexDocs(wsRoot string, projectReadmes map[string]bool) ([]DocEntry, error) {
	var docs []DocEntry
	walkErr := filepath.WalkDir(wsRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != wsRoot && (strings.HasPrefix(d.Name(), ".") || prunedDirNames[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		// Symlinks and other irregular entries are not read: a dangling link must
		// not fail the build that happened to walk over it.
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(wsRoot, p)
		if err != nil {
			return err
		}
		slashed := filepath.ToSlash(rel)
		if !indexedDoc(slashed, d.Name(), projectReadmes) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		docs = append(docs, DocEntry{Path: slashed, Title: firstHeading(data)})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("index workspace docs: %w", walkErr)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })
	return docs, nil
}

// indexedDoc applies the two inclusion rules to one workspace-relative file
// path: anything markdown under docs/, and any README.md a project root does not
// already own.
func indexedDoc(rel, name string, projectReadmes map[string]bool) bool {
	if strings.HasPrefix(rel, docsDir+"/") && strings.HasSuffix(name, ".md") {
		return true
	}
	return name == readmeFilename && !projectReadmes[rel]
}

// oneLine collapses a string to a single trimmed line, so a value lifted out of
// a schema or a document can never break the markdown table it lands in.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// truncateRunes cuts s to at most limit runes, appending a horizontal ellipsis
// when it cut. Rune-aware so the result is always valid UTF-8.
func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:limit]), " ") + "…"
}
