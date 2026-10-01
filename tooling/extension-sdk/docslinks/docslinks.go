// Package docslinks checks the relative links in a project's documentation:
// every README.md, and every Markdown file under a doc/ directory. A link
// whose file does not exist, or whose anchor names no heading of its target,
// is a finding. The language extensions run it as a lint step over each
// project and the SDD extension runs it over the whole workspace, so one rule
// decides what a broken link is everywhere.
//
// The check reads the filesystem as a reader of the repository would: a path
// must match the case of the file it names, because a link that resolves only
// on a case-insensitive disk is broken on Linux and on the forge. Links with a
// scheme (https:, mailto:), protocol-relative links and absolute paths are not
// relative and are not checked: an absolute path is a site route, which only
// the site that serves it can resolve.
package docslinks

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"go.putnami.dev/sdk/extension/dirlink"
)

// Code is the diagnostic code every finding is reported under.
const Code = "docs-links"

// Finding is one broken link. File is absolute; Line and Column are 1-based
// and point at the link's destination.
type Finding struct {
	File    string
	Line    int
	Column  int
	Target  string
	Message string
}

// skippedDirectories are never documentation a reader follows: installed
// dependencies, vendored code, test fixtures and build output.
var skippedDirectories = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"testdata":     true,
	"dist":         true,
}

// ProjectDocuments returns the documents of the project at root, sorted: every
// README.md, and every .md file under a directory named doc. It skips nested
// projects and Go modules, dot and underscore directories, the directories in
// skippedDirectories and the directories git ignores, as the lint tasks do.
func ProjectDocuments(root string) ([]string, error) {
	return documents(root, func(path string) bool {
		return fileExists(filepath.Join(path, "putnami.json")) || fileExists(filepath.Join(path, "go.mod"))
	})
}

// WorkspaceDocuments returns every document under the workspace root in one
// walk, grouped by owner: the nearest of projects (absolute directories) that
// holds it, or root. Each project is read as ProjectDocuments reads it, so a
// Markdown file counts when a doc directory sits between its owner and itself.
// A nested directory that is not in projects, a Go module or a scope, belongs
// to the project around it. A project the walk cannot reach, under a skipped
// or ignored directory, is read on its own. Each group is sorted.
func WorkspaceDocuments(root string, projects []string) (map[string][]string, error) {
	root = filepath.Clean(root)
	unreached := make(map[string]bool, len(projects))
	for _, dir := range projects {
		unreached[filepath.Clean(dir)] = true
	}
	isProject := maps.Clone(unreached)
	ignored := ignoredDirectories(root)
	owners := map[string]string{root: root}
	grouped := make(map[string][]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if entry.IsDir() {
			if path == root {
				delete(unreached, root)
				return nil
			}
			if skippedDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			if rel, relErr := filepath.Rel(root, path); relErr == nil && ignored[filepath.ToSlash(rel)] {
				return filepath.SkipDir
			}
			owners[path] = owners[filepath.Dir(path)]
			if isProject[path] {
				owners[path] = path
				delete(unreached, path)
			}
			return nil
		}
		owner := owners[filepath.Dir(path)]
		if isDocument(owner, path) && regularFile(path, entry) {
			grouped[owner] = append(grouped[owner], path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for dir := range unreached {
		files, err := ProjectDocuments(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(files) > 0 {
			grouped[dir] = files
		}
	}
	for owner := range grouped {
		sort.Strings(grouped[owner])
	}
	return grouped, nil
}

func documents(root string, skip func(dir string) bool) ([]string, error) {
	ignored := ignoredDirectories(root)
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if entry.IsDir() {
			if path == root {
				return nil
			}
			if skippedDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			if rel, relErr := filepath.Rel(root, path); relErr == nil && ignored[filepath.ToSlash(rel)] {
				return filepath.SkipDir
			}
			if skip != nil && skip(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isDocument(root, path) || !regularFile(path, entry) {
			return nil
		}
		files = append(files, path)
		return nil
	})
	sort.Strings(files)
	return files, err
}

// skippedDirectory reports whether a directory named name is never walked: a
// dot or underscore directory, or one of skippedDirectories.
func skippedDirectory(name string) bool {
	return skippedDirectories[name] || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// isDocument reports whether path is a README.md, or a Markdown file with a
// doc directory between root and itself.
func isDocument(root, path string) bool {
	name := filepath.Base(path)
	if name == "README.md" {
		return true
	}
	if !strings.HasSuffix(name, ".md") {
		return false
	}
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return false
	}
	return slices.Contains(strings.Split(filepath.ToSlash(rel), "/"), "doc")
}

// ignoredDirectories returns the directories under root that git ignores
// entirely, as slash-separated paths relative to root. Outside a git
// repository, or without git, it returns nil and nothing is ignored.
func ignoredDirectories(root string) map[string]bool {
	cmd := exec.Command("git", "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	dirs := make(map[string]bool)
	for entry := range bytes.SplitSeq(output, []byte{0}) {
		if name := string(entry); strings.HasSuffix(name, "/") {
			dirs[strings.TrimSuffix(name, "/")] = true
		}
	}
	return dirs
}

// regularFile reports whether entry is a regular file, or a symbolic link to
// one: a README.md that links to doc/index.md is the project's README.
func regularFile(path string, entry fs.DirEntry) bool {
	if entry.Type().IsRegular() {
		return true
	}
	if entry.Type()&fs.ModeSymlink == 0 {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Check reads each file and returns its broken links, sorted by file, line
// and column. workspaceRoot bounds resolution: a link that leaves it is broken,
// because no reader of the repository can follow it. A file that cannot be
// read is an error.
func Check(workspaceRoot string, files []string) ([]Finding, error) {
	root := filepath.Clean(workspaceRoot)
	realRoot, err := dirlink.Resolve(root)
	if err != nil {
		realRoot = root
	}
	checker := &checker{
		root:     root,
		realRoot: realRoot,
		anchors:  make(map[string]map[string]bool),
		entries:  make(map[string]map[string]bool),
	}
	var findings []Finding
	for _, file := range files {
		source, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		findings = append(findings, checker.checkFile(file, source)...)
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Column < findings[j].Column
	})
	return findings, nil
}

// CheckProject checks the documents ProjectDocuments returns for root.
func CheckProject(workspaceRoot, root string) ([]Finding, error) {
	files, err := ProjectDocuments(root)
	if err != nil {
		return nil, err
	}
	return Check(workspaceRoot, files)
}

type checker struct {
	root string
	// realRoot is root with its symbolic links and directory junctions
	// resolved: a link target whose own links lead out of it leaves the
	// workspace.
	realRoot string
	// anchors caches the anchors of each Markdown file a link names.
	anchors map[string]map[string]bool
	// entries caches the exact names in each directory a link walks through.
	entries map[string]map[string]bool
}

func (c *checker) checkFile(file string, source []byte) []Finding {
	var findings []Finding
	for _, link := range extractLinks(source) {
		if message := c.resolve(file, source, link.target); message != "" {
			findings = append(findings, Finding{
				File:    file,
				Line:    link.line,
				Column:  link.column,
				Target:  link.target,
				Message: message,
			})
		}
	}
	return findings
}

var schemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// resolve returns why target, written in file, is broken, or "" when it
// resolves or is not a relative link.
func (c *checker) resolve(file string, source []byte, target string) string {
	if target == "" || schemePattern.MatchString(target) || strings.HasPrefix(target, "/") {
		return ""
	}
	path, anchor, _ := strings.Cut(unescapePunctuation(target), "#")
	path, _, _ = strings.Cut(path, "?")
	if strings.Contains(path, `\`) {
		// The forge reads a backslash as part of a name, and Windows as a
		// separator: refusing it keeps the verdict the same on every host.
		return fmt.Sprintf("link %q holds a backslash, which is not a path separator in a link", target)
	}
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return fmt.Sprintf("link %q is not a valid path: %v", target, err)
	}
	resolved := file
	if decoded != "" {
		resolved = filepath.Join(filepath.Dir(file), filepath.FromSlash(decoded))
		rel, inside := within(c.root, resolved)
		if !inside {
			return fmt.Sprintf("link %q leaves the workspace", target)
		}
		if !c.exists(rel) {
			return fmt.Sprintf("link %q names %s, which does not exist", target, filepath.ToSlash(rel))
		}
		// A reader of the repository cannot follow a symbolic link out of it,
		// and the check must not read what lies outside either.
		real, evalErr := dirlink.Resolve(resolved)
		if evalErr != nil {
			return fmt.Sprintf("link %q names %s, which does not resolve: %v", target, filepath.ToSlash(rel), evalErr)
		}
		if _, inside := within(c.realRoot, real); !inside {
			return fmt.Sprintf("link %q names %s, a symbolic link that leaves the workspace", target, filepath.ToSlash(rel))
		}
	}
	if anchor == "" || !isMarkdown(resolved) {
		return ""
	}
	if info, statErr := os.Stat(resolved); statErr != nil || info.IsDir() {
		return ""
	}
	var anchors map[string]bool
	if resolved == file {
		anchors = c.anchorsOf(file, source)
	} else {
		anchors = c.anchorsOf(resolved, nil)
	}
	name, err := url.PathUnescape(anchor)
	if err != nil {
		name = anchor
	}
	if anchors[strings.ToLower(name)] {
		return ""
	}
	if decoded == "" {
		return fmt.Sprintf("link %q names no heading of this file", target)
	}
	return fmt.Sprintf("link %q names no heading of %s", target, filepath.ToSlash(relOrSelf(c.root, resolved)))
}

// within returns path relative to root, and whether path lies inside root.
func within(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel, false
	}
	return rel, true
}

// unescapePunctuation removes the backslash before each escaped ASCII
// punctuation character, as CommonMark does in a link destination.
func unescapePunctuation(target string) string {
	if !strings.Contains(target, `\`) {
		return target
	}
	var out strings.Builder
	for i := 0; i < len(target); i++ {
		if target[i] == '\\' && i+1 < len(target) && strings.IndexByte(asciiPunctuation, target[i+1]) >= 0 {
			i++
		}
		out.WriteByte(target[i])
	}
	return out.String()
}

// asciiPunctuation is the set of characters CommonMark lets a backslash escape.
const asciiPunctuation = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

func relOrSelf(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}

func isMarkdown(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".md" || ext == ".markdown"
}

// exists reports whether rel, relative to the workspace root, names an entry
// whose every segment matches the case on disk.
func (c *checker) exists(rel string) bool {
	if rel == "." {
		return true
	}
	dir := c.root
	for segment := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if segment == "" || segment == "." {
			continue
		}
		names, ok := c.entries[dir]
		if !ok {
			names = make(map[string]bool)
			if listing, err := os.ReadDir(dir); err == nil {
				for _, entry := range listing {
					names[entry.Name()] = true
				}
			}
			c.entries[dir] = names
		}
		if !names[segment] {
			return false
		}
		dir = filepath.Join(dir, segment)
	}
	_, err := os.Stat(dir)
	return err == nil
}

func (c *checker) anchorsOf(file string, source []byte) map[string]bool {
	if anchors, ok := c.anchors[file]; ok {
		return anchors
	}
	if source == nil {
		source, _ = os.ReadFile(file)
	}
	anchors := Anchors(source)
	c.anchors[file] = anchors
	return anchors
}

// link is one link destination and the position it starts at.
type link struct {
	target string
	line   int
	column int
}

var (
	fencePattern = regexp.MustCompile("^[ \t]*(`{3,}|~{3,})")
	// referencePattern matches a link reference definition: a label that is
	// not a footnote (`[^1]:`), a destination, then nothing or a title.
	referencePattern     = regexp.MustCompile(`^ {0,3}\[[^\]^][^\]]*\]:[ \t]*(<[^>]*>|\S+)(?:[ \t]+(?:"[^"]*"|'[^']*'|\([^)]*\)))?[ \t]*$`)
	quotePattern         = regexp.MustCompile(`^(?: {0,3}> ?)+`)
	quoteMarkerPattern   = regexp.MustCompile(`^ {0,3}> ?`)
	thematicBreakPattern = regexp.MustCompile(`^ {0,3}(?:(?:-[ \t]*){3,}|(?:\*[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	listItemPattern      = regexp.MustCompile(`^[ \t]*(?:[-*+]|\d{1,9}[.)])(?:[ \t]+|$)`)
	htmlAttributePattern = regexp.MustCompile(`<(?:a|img|source)\b[^>]*?\b(?:href|src)\s*=\s*["']([^"']*)["']`)
)

// extractLinks returns the link destinations of a Markdown document: inline
// links and images, reference definitions and the href or src of an a, img or
// source tag. It skips front matter, fenced code, inline code and HTML
// comments.
func extractLinks(source []byte) []link {
	var links []link
	// open counts the link texts earlier lines of the same paragraph left
	// open, so a link whose text wraps a line is still read.
	open, last := 0, 0
	eachProseLine(source, func(number int, line, _ string) {
		if number != last+1 || strings.TrimSpace(line) == "" {
			open = 0
		}
		last = number
		if match := referencePattern.FindStringSubmatchIndex(line); match != nil {
			target := strings.TrimSuffix(strings.TrimPrefix(line[match[2]:match[3]], "<"), ">")
			links = append(links, link{target: target, line: number, column: match[2] + 1})
			open = 0
			return
		}
		for _, match := range htmlAttributePattern.FindAllStringSubmatchIndex(line, -1) {
			links = append(links, link{target: strings.TrimSpace(line[match[2]:match[3]]), line: number, column: match[2] + 1})
		}
		links = append(links, inlineLinks(number, line, open)...)
		open = openBrackets(line, open)
	})
	return links
}

// eachProseLine calls visit with every line outside front matter and fenced
// code: once with its inline code spans and HTML comments blanked, so their
// bytes keep their columns but hold no link, and once with only its comments
// blanked, which is the text a heading renders.
func eachProseLine(source []byte, visit func(number int, prose, text string)) {
	lines := strings.Split(strings.ReplaceAll(string(source), "\r\n", "\n"), "\n")
	start := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if trimmed := strings.TrimSpace(lines[i]); trimmed == "---" || trimmed == "..." {
				start = i + 1
				break
			}
		}
	}
	fence, fenceDepth := "", 0
	inComment := false
	// lists holds the content column of each list item the current line may
	// belong to: a fence or an indented code block is read relative to it.
	var lists []int
	afterBlank := true
	for i := start; i < len(lines); i++ {
		line := lines[i]
		// body is the line without its block quote markers: a fence or a list
		// inside a quote is read as it would be outside one.
		quote := quotePattern.FindString(line)
		depth := strings.Count(quote, ">")
		body := line[len(quote):]
		if fence != "" {
			if depth >= fenceDepth {
				// Only the markers of the quote the fence opened in are
				// stripped: a quoted fence line inside a top-level fence is code.
				inner := stripQuotes(line, fenceDepth)
				if match := fencePattern.FindStringSubmatch(inner); match != nil &&
					match[1][0] == fence[0] && len(match[1]) >= len(fence) &&
					strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(inner), string(fence[0]))) == "" {
					fence = ""
				}
				continue
			}
			// The quote the fence opened in has ended, and the fence with it.
			fence = ""
		}
		blank := strings.TrimSpace(body) == ""
		indent := columns(body[:len(body)-len(strings.TrimLeft(body, " \t"))])
		fenceMatch := fencePattern.FindStringSubmatch(body)
		breaks := thematicBreakPattern.MatchString(body)
		item := ""
		if !breaks {
			item = listItemPattern.FindString(body)
		}
		if !blank && !inComment && indent == 0 && (breaks || atxHeadingPattern.MatchString(body)) {
			// A heading or a thematic break at the margin ends every list.
			lists = lists[:0]
		}
		if !blank && !inComment && (afterBlank || fenceMatch != nil || item != "") {
			// A line that is not a paragraph's lazy continuation leaves every
			// list item it is indented less than.
			for len(lists) > 0 && indent < lists[len(lists)-1] {
				lists = lists[:len(lists)-1]
			}
		}
		base := 0
		if len(lists) > 0 {
			base = lists[len(lists)-1]
		}
		if !blank && !inComment && afterBlank && indent >= base+4 {
			// An indented code block: it follows a blank line and sits four
			// columns past its container.
			continue
		}
		afterBlank = blank
		if fenceMatch != nil && !inComment && indent-base <= 3 {
			fence, fenceDepth = fenceMatch[1], depth
			continue
		}
		if item != "" && !inComment {
			content := columns(item)
			if strings.TrimSpace(item) == strings.TrimSpace(body) {
				// An item that starts empty: its content column is one past
				// the marker.
				content = columns(strings.TrimRight(item, " \t")) + 1
			}
			lists = append(lists, content)
		}
		line, inComment = blankComments(line, inComment)
		visit(i+1, blankCodeSpans(line), line)
	}
}

// stripQuotes returns line without its first depth block quote markers.
func stripQuotes(line string, depth int) string {
	for ; depth > 0; depth-- {
		marker := quoteMarkerPattern.FindStringIndex(line)
		if marker == nil {
			break
		}
		line = line[marker[1]:]
	}
	return line
}

// columns returns the width of prefix, a tab advancing to the next multiple
// of four.
func columns(prefix string) int {
	width := 0
	for i := 0; i < len(prefix); i++ {
		if prefix[i] == '\t' {
			width += 4 - width%4
			continue
		}
		width++
	}
	return width
}

// blankComments replaces the bytes of HTML comments with spaces. open reports
// whether the line starts inside a comment; the result reports whether it ends
// inside one.
func blankComments(line string, open bool) (string, bool) {
	out := []byte(line)
	i := 0
	for i < len(out) {
		if open {
			end := strings.Index(line[i:], "-->")
			stop := len(out)
			if end >= 0 {
				stop = i + end + len("-->")
			}
			for j := i; j < stop; j++ {
				out[j] = ' '
			}
			if end < 0 {
				return string(out), true
			}
			i, open = stop, false
			continue
		}
		begin := strings.Index(line[i:], "<!--")
		if begin < 0 {
			break
		}
		i += begin
		open = true
	}
	return string(out), open
}

// blankCodeSpans replaces the bytes of inline code spans with spaces. A run of
// backticks closes only on a run of the same length.
func blankCodeSpans(line string) string {
	out := []byte(line)
	i := 0
	for i < len(line) {
		if line[i] != '`' {
			i++
			continue
		}
		run := 0
		for i+run < len(line) && line[i+run] == '`' {
			run++
		}
		closing := -1
		for j := i + run; j < len(line); {
			if line[j] != '`' {
				j++
				continue
			}
			k := 0
			for j+k < len(line) && line[j+k] == '`' {
				k++
			}
			if k == run {
				closing = j
				break
			}
			j += k
		}
		if closing < 0 {
			i += run
			continue
		}
		for j := i; j < closing+run; j++ {
			out[j] = ' '
		}
		i = closing + run
	}
	return string(out)
}

// inlineLinks returns the destination of every `[text](destination)` and
// `![alt](destination)` on a line. open is the number of link texts earlier
// lines of the paragraph left open.
func inlineLinks(number int, line string, open int) []link {
	var links []link
	for i := 0; i+1 < len(line); i++ {
		if line[i] != ']' || line[i+1] != '(' || escaped(line, i) {
			continue
		}
		if !opened(line, i, open) {
			continue
		}
		start := i + 2
		for start < len(line) && (line[start] == ' ' || line[start] == '\t') {
			start++
		}
		target, ok := destination(line[start:])
		if !ok {
			continue
		}
		links = append(links, link{target: target, line: number, column: start + 1})
	}
	return links
}

// opened reports whether the `]` at close ends a link text: an unescaped `[`
// before it, or one that earlier lines left open, opens that text, counting
// the brackets nested inside it.
func opened(line string, close, open int) bool {
	depth := 0
	for i := close - 1; i >= 0; i-- {
		if escaped(line, i) {
			continue
		}
		switch line[i] {
		case ']':
			depth++
		case '[':
			if depth == 0 {
				return true
			}
			depth--
		}
	}
	return open > depth
}

// openBrackets returns the number of unescaped `[` still open at the end of
// line, starting from open.
func openBrackets(line string, open int) int {
	for i := 0; i < len(line); i++ {
		if escaped(line, i) {
			continue
		}
		switch line[i] {
		case '[':
			open++
		case ']':
			if open > 0 {
				open--
			}
		}
	}
	return open
}

func escaped(line string, i int) bool {
	backslashes := 0
	for j := i - 1; j >= 0 && line[j] == '\\'; j-- {
		backslashes++
	}
	return backslashes%2 == 1
}

// destination reads a link destination at the start of rest: `<...>`, or a
// run without spaces whose parentheses balance.
func destination(rest string) (string, bool) {
	if strings.HasPrefix(rest, "<") {
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			return "", false
		}
		return rest[1:end], true
	}
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '\\':
			i++
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return rest[:i], true
			}
			depth--
		case ' ', '\t':
			return rest[:i], depth == 0
		}
	}
	return "", false
}

var (
	atxHeadingPattern    = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)
	setextPattern        = regexp.MustCompile(`^ {0,3}(=+|-+)[ \t]*$`)
	htmlIDPattern        = regexp.MustCompile(`<[A-Za-z][^>]*?\b(?:id|name)\s*=\s*["']([^"']+)["']`)
	imagePattern         = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	inlineLinkPattern    = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	referenceLinkPattern = regexp.MustCompile(`\[([^\]]*)\]\[[^\]]*\]`)
	tagPattern           = regexp.MustCompile(`</?[A-Za-z][^>]*>`)
	// underscorePattern matches underscore emphasis: a run that no letter,
	// digit or underscore touches on its outer side and no space on its inner
	// side.
	underscorePattern = regexp.MustCompile(`(^|[^\p{L}\p{N}_])_+([^_\s](?:[^_]*?[^_\s])?)_+([^\p{L}\p{N}_]|$)`)
)

// Anchors returns the anchors a Markdown document defines, lowercased: the
// GitHub slug of each heading, numbered from -1 on repetition as GitHub
// numbers them, and the id or name attribute of each HTML tag.
func Anchors(source []byte) map[string]bool {
	anchors := make(map[string]bool)
	seen := make(map[string]int)
	add := func(text string) {
		slug := Slug(text)
		if count, ok := seen[slug]; ok {
			seen[slug] = count + 1
			anchors[fmt.Sprintf("%s-%d", slug, count+1)] = true
			return
		}
		seen[slug] = 0
		anchors[slug] = true
	}
	previous, last := "", 0
	eachProseLine(source, func(number int, prose, line string) {
		if number != last+1 {
			// A skipped fence or code block ends the paragraph before it.
			previous = ""
		}
		last = number
		for _, match := range htmlIDPattern.FindAllStringSubmatch(prose, -1) {
			anchors[strings.ToLower(match[1])] = true
		}
		if match := atxHeadingPattern.FindStringSubmatch(line); match != nil {
			add(match[2])
			previous = ""
			return
		}
		if setextPattern.MatchString(line) && strings.TrimSpace(previous) != "" && paragraphLine(previous) {
			add(strings.TrimSpace(previous))
			previous = ""
			return
		}
		previous = line
	})
	return anchors
}

// paragraphLine reports whether line can be the text of a setext heading: not
// a list item, a block quote, a table row or a thematic break.
func paragraphLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if setextPattern.MatchString(line) {
		return false
	}
	for _, prefix := range []string{"- ", "* ", "+ ", "> ", "|"} {
		if strings.HasPrefix(trimmed, prefix) {
			return false
		}
	}
	return true
}

// Slug returns the anchor GitHub gives a heading whose Markdown text is text:
// the rendered text lowercased, with every character that is not a letter, a
// mark, a number, a connector, a hyphen or a space removed, and each space
// replaced by a hyphen. HTML entities render as the character they name; code
// spans render their content as written.
func Slug(text string) string {
	var rendered strings.Builder
	for i, segment := range strings.Split(strings.TrimSpace(text), "`") {
		if i%2 == 1 {
			rendered.WriteString(segment)
			continue
		}
		segment = imagePattern.ReplaceAllString(segment, "$1")
		segment = inlineLinkPattern.ReplaceAllString(segment, "$1")
		segment = referenceLinkPattern.ReplaceAllString(segment, "$1")
		segment = tagPattern.ReplaceAllString(segment, "")
		segment = html.UnescapeString(segment)
		for underscorePattern.MatchString(segment) {
			segment = underscorePattern.ReplaceAllString(segment, "$1$2$3")
		}
		rendered.WriteString(segment)
	}
	var slug strings.Builder
	for _, r := range strings.ToLower(rendered.String()) {
		switch {
		case r == ' ':
			slug.WriteByte('-')
		case r == '-' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || unicode.Is(unicode.Pc, r):
			slug.WriteRune(r)
		}
	}
	return slug.String()
}
