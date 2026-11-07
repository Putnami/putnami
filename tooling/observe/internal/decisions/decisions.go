// Package decisions reads the decision records a repository keeps as MADR
// files (Markdown Architectural Decision Records): their title, status, date
// and the path patterns of their scope, and matches the scope against
// repository paths. It reads; it converts, requires and validates nothing.
package decisions

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/yaml"
)

// DefaultDirs are the directories read when no --decisions names others.
var DefaultDirs = []string{"docs/decisions", "docs/adr"}

// Record is one decision record.
type Record struct {
	// Path is the record's repository path.
	Path string
	// Title is the front matter title, the first level-one heading, or the
	// file name.
	Title string
	// Status is one of the evidence decision statuses.
	Status string
	// Date is the record's YYYY-MM-DD date, empty when it states none.
	Date string
	// Scope are the path patterns the record governs, as written once
	// normalized.
	Scope []string
}

// Dirs normalizes the directories a flag names: repository-relative, slash
// separated, without a leading ./ or a trailing /. It returns the entries that
// are no repository directory apart: an absolute path, a drive letter, a ..
// segment, or the root itself. Naming no valid directory gives DefaultDirs.
func Dirs(named []string) (dirs, rejected []string) {
	for _, dir := range named {
		trimmed := strings.TrimSpace(dir)
		if trimmed == "" {
			continue
		}
		clean := normalize(trimmed)
		if strings.HasPrefix(strings.ReplaceAll(trimmed, `\`, "/"), "/") || clean == "" || !valid(clean) {
			rejected = append(rejected, trimmed)
			continue
		}
		if !slices.Contains(dirs, clean) {
			dirs = append(dirs, clean)
		}
	}
	if len(dirs) == 0 {
		dirs = slices.Clone(DefaultDirs)
	}
	return dirs, rejected
}

// IsRecord reports whether the repository path names a decision record under
// one of dirs.
func IsRecord(file string, dirs []string) bool {
	if !strings.EqualFold(path.Ext(file), ".md") {
		return false
	}
	name := strings.ToLower(path.Base(file))
	if name == ".md" || name == "readme.md" || name == "index.md" || strings.Contains(name, "template") {
		return false
	}
	for _, dir := range dirs {
		rest, ok := strings.CutPrefix(file, dir+"/")
		if !ok {
			continue
		}
		hidden := false
		for _, segment := range strings.Split(path.Dir(rest), "/") {
			if strings.HasPrefix(segment, ".") && segment != "." {
				hidden = true
			}
		}
		if !hidden {
			return true
		}
	}
	return false
}

// Find lists the decision records under dirs in the repository at root,
// sorted. A directory that does not exist holds none.
func Find(root string, dirs []string) ([]string, error) {
	var out []string
	for _, dir := range dirs {
		base := filepath.Join(root, filepath.FromSlash(dir))
		info, err := os.Stat(base)
		if err != nil || !info.IsDir() {
			continue
		}
		err = filepath.WalkDir(base, func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if p != base && strings.HasPrefix(entry.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if IsRecord(rel, dirs) && !slices.Contains(out, rel) {
				out = append(out, rel)
			}
			return nil
		})
		if err != nil {
			return out, err
		}
	}
	slices.Sort(out)
	return out, nil
}

// Parse reads the record at the repository path file.
func Parse(file string, data []byte) Record {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	front, body := frontMatter(text)
	record := Record{Path: file}
	var status, date string
	if front != nil {
		record.Title = strings.TrimSpace(yaml.String(front, "title"))
		status = scalar(yaml.Get(front, "status"))
		date = scalar(yaml.Get(front, "date"))
		if scope, ok := yaml.Strings(yaml.Get(front, "scope")); ok {
			record.Scope = patterns(scope)
		} else if single := scalar(yaml.Get(front, "scope")); single != "" {
			record.Scope = patterns([]string{single})
		}
	}
	lines := strings.Split(body, "\n")
	if record.Title == "" {
		record.Title = heading(lines)
	}
	if record.Title == "" {
		record.Title = strings.TrimSuffix(path.Base(file), path.Ext(file))
	}
	if status == "" {
		status = field(lines, "status")
	}
	if status == "" {
		status = firstLine(section(lines, "status"))
	}
	if date == "" {
		date = field(lines, "date")
	}
	if front == nil || yaml.Get(front, "scope") == nil {
		record.Scope = patterns(listItems(section(lines, "scope")))
	}
	record.Status = Status(status)
	record.Date = Date(date)
	return record
}

// Status maps the status a record writes to the evidence vocabulary: its
// first word, lowercased.
func Status(written string) string {
	word := strings.ToLower(strings.TrimSpace(written))
	word = strings.Trim(word, "*_`[]()\"' ")
	if index := strings.IndexFunc(word, func(r rune) bool { return r < 'a' || r > 'z' }); index >= 0 {
		word = word[:index]
	}
	switch word {
	case evidence.DecisionProposed, evidence.DecisionAccepted, evidence.DecisionRejected,
		evidence.DecisionDeprecated, evidence.DecisionSuperseded:
		return word
	}
	return evidence.DecisionUnknown
}

// Date keeps a written date when it is a valid YYYY-MM-DD date.
func Date(written string) string {
	written = strings.Trim(strings.TrimSpace(written), "*_`\"'")
	if len(written) != len("2006-01-02") {
		return ""
	}
	if _, err := time.Parse("2006-01-02", written); err != nil {
		return ""
	}
	return written
}

// frontMatter splits a leading YAML front matter from the body. A front matter
// that does not parse as a mapping is none, and the body is the whole text.
func frontMatter(text string) (yaml.Map, string) {
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return nil, text
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, text
	}
	after := rest[end+len("\n---"):]
	if after != "" && after[0] != '\n' {
		return nil, text
	}
	doc, err := yaml.Parse([]byte(rest[:end+1]))
	m, isMap := doc.(yaml.Map)
	if err != nil || !isMap {
		return nil, text
	}
	return m, strings.TrimPrefix(after, "\n")
}

// scalar returns a front matter scalar as text.
func scalar(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}

// outsideFences visits the lines outside fenced code blocks, in order, until
// visit returns false. A fence closes on a line that opens with at least its
// own run of backticks or tildes.
func outsideFences(lines []string, visit func(index int, line string) bool) {
	fence := ""
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			marker := trimmed[0]
			run := 0
			for run < len(trimmed) && trimmed[run] == marker {
				run++
			}
			fence = trimmed[:run]
			continue
		}
		if !visit(index, line) {
			return
		}
	}
}

// heading returns the text of the first level-one heading.
func heading(lines []string) string {
	var title string
	outsideFences(lines, func(_ int, line string) bool {
		if text, ok := strings.CutPrefix(line, "# "); ok {
			title = closingSequence(strings.TrimSpace(text))
			return false
		}
		return true
	})
	return title
}

// closingSequence removes the optional closing # sequence of an ATX heading,
// which a space separates from the text.
func closingSequence(text string) string {
	trimmed := strings.TrimRight(text, "#")
	if trimmed == "" {
		return ""
	}
	if trimmed != text && !strings.HasSuffix(trimmed, " ") {
		return text
	}
	return strings.TrimSpace(trimmed)
}

// field returns the value of the first "Name: value" line, plain, in a list
// item or in bold: "Status: accepted", "* Status: accepted",
// "- **Status**: Accepted".
func field(lines []string, name string) string {
	var value string
	outsideFences(lines, func(_ int, line string) bool {
		text := strings.TrimSpace(line)
		for _, bullet := range []string{"* ", "- ", "+ "} {
			text = strings.TrimPrefix(text, bullet)
		}
		key, rest, ok := strings.Cut(text, ":")
		if !ok {
			return true
		}
		key = strings.Trim(strings.TrimSpace(key), "*_")
		rest = strings.TrimSpace(strings.TrimLeft(rest, "*_"))
		if strings.EqualFold(key, name) && rest != "" {
			value = rest
			return false
		}
		return true
	})
	return value
}

// section returns the lines of the first heading of any level named name, up
// to the next heading.
func section(lines []string, name string) []string {
	var out []string
	inside := false
	outsideFences(lines, func(index int, line string) bool {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			if inside {
				return false
			}
			title := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			inside = strings.EqualFold(strings.Trim(title, "*_ "), name)
			return true
		}
		if inside {
			out = append(out, line)
		}
		return true
	})
	return out
}

// firstLine returns the first non-blank line.
func firstLine(lines []string) string {
	for _, line := range lines {
		if text := strings.TrimSpace(line); text != "" {
			return text
		}
	}
	return ""
}

// listItems returns the text of each list item.
func listItems(lines []string) []string {
	var out []string
	scanner := bufio.NewScanner(strings.NewReader(strings.Join(lines, "\n")))
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		for _, bullet := range []string{"* ", "- ", "+ "} {
			if item, ok := strings.CutPrefix(text, bullet); ok {
				out = append(out, item)
				break
			}
		}
	}
	return out
}

// patterns normalizes scope entries: backticks and a trailing comment after
// the first space are removed, then the pattern is normalized. An entry with
// a dot-dot segment, or that is empty, is dropped.
func patterns(entries []string) []string {
	var out []string
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if strings.HasPrefix(entry, "`") {
			if end := strings.Index(entry[1:], "`"); end >= 0 {
				entry = entry[1 : end+1]
			}
		} else if space := strings.IndexAny(entry, " \t"); space >= 0 {
			entry = entry[:space]
		}
		if clean := normalize(entry); clean != "" && valid(clean) && !slices.Contains(out, clean) {
			out = append(out, clean)
		}
	}
	return out
}

// normalize drops a leading ./ or / and a trailing /.
func normalize(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	for {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(p, "./"), "/")
		if trimmed == p {
			break
		}
		p = trimmed
	}
	return strings.TrimRight(p, "/")
}

// valid reports whether a normalized pattern stays in the repository, by the
// path rule of the evidence document: no drive letter, no backslash, no empty,
// dot or dot-dot segment and no control character.
func valid(p string) bool {
	if len(p) >= 2 && p[1] == ':' && (p[0]|0x20) >= 'a' && (p[0]|0x20) <= 'z' {
		return false
	}
	if strings.Contains(p, `\`) {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// Matches reports whether the scope pattern governs the repository path
// file. A pattern without * or ? governs itself and every path under it;
// otherwise * and ? match within one segment, a ** segment matches any number
// of segments, and every other character, [ included, matches itself.
func Matches(pattern, file string) bool {
	if !strings.ContainsAny(pattern, "*?") {
		return file == pattern || strings.HasPrefix(file, pattern+"/")
	}
	return match(strings.Split(pattern, "/"), strings.Split(file, "/"))
}

// literalBrackets escapes the characters path.Match reads as a class or an
// escape, so a pattern segment holds only the * and ? wildcards.
var literalBrackets = strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`)

func match(pattern, segments []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			rest := pattern[1:]
			for skip := 0; skip <= len(segments); skip++ {
				if match(rest, segments[skip:]) {
					return true
				}
			}
			return false
		}
		if len(segments) == 0 {
			return false
		}
		if ok, err := path.Match(literalBrackets.Replace(pattern[0]), segments[0]); err != nil || !ok {
			return false
		}
		pattern, segments = pattern[1:], segments[1:]
	}
	return len(segments) == 0
}
