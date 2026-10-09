// Package goembed resolves Go embed directives for source packaging and task inputs.
// Its file set is deliberately independent of the host OS: every satisfiable
// build constraint is included, so a shared cache key covers cross builds too.
package goembed

import (
	"bytes"
	"fmt"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	// BuildSelector selects embedded inputs from potentially buildable non-test sources.
	BuildSelector = "go-embed:build"
	// TestSelector also selects directives in Go test sources.
	TestSelector = "go-embed:test"
)

// IsSelector reports whether a file input names a supported Go embed selector.
func IsSelector(pattern string) bool { return pattern == BuildSelector || pattern == TestSelector }

// Patterns parses actual directives, excluding only source that no ordinary
// build can compile (notably //go:build ignore).
func Patterns(filename string, src []byte) ([]string, error) {
	if excludedByConstraint(src) {
		return nil, nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filename, err)
	}
	var patterns []string
	for _, group := range f.Comments {
		for _, comment := range group.List {
			rest, ok := strings.CutPrefix(comment.Text, "//go:embed")
			if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
				continue
			}
			for len(rest) > 0 {
				switch rest[0] {
				case ' ', '\t':
					rest = rest[1:]
				case '"', '`':
					quoted, e := strconv.QuotedPrefix(rest)
					if e != nil {
						return nil, fmt.Errorf("%s: malformed go:embed quoted pattern: %w", filename, e)
					}
					value, e := strconv.Unquote(quoted)
					if e != nil {
						return nil, fmt.Errorf("%s: malformed go:embed quoted pattern: %w", filename, e)
					}
					patterns = append(patterns, value)
					rest = rest[len(quoted):]
				default:
					i := strings.IndexAny(rest, " \t")
					if i < 0 {
						i = len(rest)
					}
					patterns = append(patterns, rest[:i])
					rest = rest[i:]
				}
			}
		}
	}
	return patterns, nil
}

func excludedByConstraint(src []byte) bool {
	return !ConstraintBuildable(BuildConstraintExpr(src))
}

// BuildConstraintExpr reads only Go's leading build-comment header. A line
// inside a block comment or a legacy +build line without the required blank
// line before source is not a build directive.
func BuildConstraintExpr(src []byte) constraint.Expr {
	legacyEnd := 0
	remaining := src
	ended, inBlock := false, false
	var goBuild []byte
	for len(remaining) > 0 {
		line := remaining
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, remaining = line[:i], line[i+1:]
		} else {
			remaining = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 && !ended {
			legacyEnd = len(src) - len(remaining)
			continue
		}
		if !bytes.HasPrefix(line, []byte("//")) {
			ended = true
		}
		if !inBlock && constraint.IsGoBuild(string(line)) {
			if goBuild != nil {
				return nil // malformed duplicate: never exclude source by guessing
			}
			goBuild = line
		}
		for len(line) > 0 {
			if inBlock {
				if i := bytes.Index(line, []byte("*/")); i >= 0 {
					inBlock = false
					line = bytes.TrimSpace(line[i+2:])
					continue
				}
				break
			}
			if bytes.HasPrefix(line, []byte("//")) {
				break
			}
			if bytes.HasPrefix(line, []byte("/*")) {
				inBlock = true
				line = bytes.TrimSpace(line[2:])
				continue
			}
			remaining = nil // package or another token ends the header
			break
		}
	}
	if goBuild != nil {
		expr, err := constraint.Parse(string(goBuild))
		if err == nil {
			return expr
		}
		return nil
	}
	var expr constraint.Expr
	for _, line := range bytes.Split(src[:legacyEnd], []byte{'\n'}) {
		text := string(bytes.TrimSpace(line))
		if constraint.IsPlusBuild(text) {
			parsed, err := constraint.Parse(text)
			if err == nil {
				if expr == nil {
					expr = parsed
				} else {
					expr = &constraint.AndExpr{X: expr, Y: parsed}
				}
			}
		}
	}
	return expr
}

// ConstraintBuildable conservatively accepts a source if any ordinary tag
// assignment can build it. The conventional ignore tag stays false.
func ConstraintBuildable(expr constraint.Expr) bool {
	if expr == nil {
		return true
	}
	tags := make(map[string]bool)
	var visit func(constraint.Expr)
	visit = func(e constraint.Expr) {
		switch v := e.(type) {
		case *constraint.TagExpr:
			if v.Tag != "ignore" {
				tags[v.Tag] = true
			}
		case *constraint.NotExpr:
			visit(v.X)
		case *constraint.AndExpr:
			visit(v.X)
			visit(v.Y)
		case *constraint.OrExpr:
			visit(v.X)
			visit(v.Y)
		}
	}
	visit(expr)
	if len(tags) > 12 {
		return true
	}
	names := make([]string, 0, len(tags))
	for tag := range tags {
		names = append(names, tag)
	}
	sort.Strings(names)
	for mask := 0; mask < 1<<len(names); mask++ {
		assignment := make(map[string]bool, len(names))
		for i, tag := range names {
			assignment[tag] = mask&(1<<i) != 0
		}
		if expr.Eval(func(tag string) bool { return assignment[tag] }) {
			return true
		}
	}
	return false
}

func validPattern(pattern string) (string, bool, error) {
	all := strings.HasPrefix(pattern, "all:")
	pattern = strings.TrimPrefix(pattern, "all:")
	if pattern == "" || strings.Contains(pattern, "\\") || path.IsAbs(pattern) {
		return "", false, fmt.Errorf("invalid go:embed pattern %q", pattern)
	}
	for _, part := range strings.Split(pattern, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false, fmt.Errorf("invalid go:embed pattern %q", pattern)
		}
	}
	if _, err := path.Match(pattern, pattern); err != nil {
		return "", false, fmt.Errorf("invalid go:embed pattern %q: %w", pattern, err)
	}
	return pattern, all, nil
}

// ResolvePattern returns regular embedded files. A missing or unsafe target is
// an error, never an empty input set that could restore an old cached output.
func ResolvePattern(dir, declared string) ([]string, error) {
	pattern, all, err := validPattern(declared)
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(dir, filepath.FromSlash(pattern)))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("go:embed %q matched no files", declared)
	}
	var files []string
	for _, match := range matches {
		if err := rejectSymlinkComponents(dir, match); err != nil {
			return nil, err
		}
		info, err := os.Lstat(match)
		if err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() {
			files = append(files, match)
			continue
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("cannot embed irregular file %s", match)
		}
		err = filepath.WalkDir(match, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if p == match {
				return nil
			}
			if !all && (strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if d.Type().IsRegular() {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("go:embed %q matched no regular files", declared)
	}
	sort.Strings(files)
	return files, nil
}

func rejectSymlinkComponents(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("go:embed target %q escapes source directory", target)
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("cannot embed symlink %s", current)
		}
	}
	return nil
}

// ToolingIgnoresPath is the portable Go source-directory exclusion shared
// with the Go packager's source probe.
func ToolingIgnoresPath(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "testdata" || strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
			return true
		}
	}
	return false
}

func ignoresSource(rel string) bool {
	if ToolingIgnoresPath(rel) {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "vendor" {
			return true
		}
	}
	return false
}

// ReadSource reads a Go source file, including a source-file symlink that Go
// accepts. The link target must resolve to a regular file inside the project;
// directives still use the source's lexical package path.
func ReadSource(root, filename string) ([]byte, error) {
	src, _, err := readSource(root, filename)
	return src, err
}

// SourceReferent validates a source-file link under the same policy as
// ReadSource and returns its regular target. A regular source returns "".
func SourceReferent(root, filename string) (string, error) {
	_, referent, err := readSource(root, filename)
	return referent, err
}

func readSource(root, filename string) ([]byte, string, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		if !info.Mode().IsRegular() {
			return nil, "", fmt.Errorf("irregular Go source %s", filename)
		}
		src, err := os.ReadFile(filename)
		return src, "", err
	}
	if err := rejectNestedModule(root, filename); err != nil {
		return nil, "", fmt.Errorf("go source symlink %s: %w", filename, err)
	}
	linkText, err := os.Readlink(filename)
	if err != nil {
		return nil, "", err
	}
	if filepath.IsAbs(linkText) {
		return nil, "", fmt.Errorf("go source symlink %s has an absolute target", filename)
	}
	linkTarget := filepath.Clean(filepath.Join(filepath.Dir(filename), linkText))
	linkRel, err := filepath.Rel(root, linkTarget)
	if err != nil || linkRel == ".." || strings.HasPrefix(linkRel, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("go source symlink %s escapes project", filename)
	}
	// A remote snapshot needs every link in the chain. Accept a direct regular
	// referent so the existing input admission can bind its exact bytes.
	if err := rejectSymlinkComponents(root, linkTarget); err != nil {
		return nil, "", fmt.Errorf("go source symlink %s has an unsafe referent: %w", filename, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, "", fmt.Errorf("resolve Go project %s: %w", root, err)
	}
	resolvedSource, err := filepath.EvalSymlinks(filename)
	if err != nil {
		return nil, "", fmt.Errorf("resolve Go source symlink %s: %w", filename, err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedSource)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("go source symlink %s escapes project", filename)
	}
	target, err := os.Stat(resolvedSource)
	if err != nil {
		return nil, "", fmt.Errorf("stat Go source symlink %s: %w", filename, err)
	}
	if !target.Mode().IsRegular() {
		return nil, "", fmt.Errorf("go source symlink %s targets an irregular file", filename)
	}
	if err := rejectNestedModule(resolvedRoot, resolvedSource); err != nil {
		return nil, "", fmt.Errorf("go source symlink %s: %w", filename, err)
	}
	src, err := os.ReadFile(resolvedSource)
	return src, filepath.Join(root, linkRel), err
}

// Directives scans Go sources selected by the ordinary project source contract.
// It returns the source-relative path and its patterns; callers can resolve
// current files or match a deleted path against the declaration.
func Directives(root string, includeTests bool) (map[string][]string, error) {
	found, _, _, err := directivesAndSourceLinks(root, includeTests)
	return found, err
}

func directivesAndSourceLinks(root string, includeTests bool) (map[string][]string, []string, []string, error) {
	found := make(map[string][]string)
	var sourceLinks []string
	var lexicalSources []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if ignoresSource(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir
			} else if !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		if filepath.Ext(p) != ".go" || (!includeTests && strings.HasSuffix(p, "_test.go")) {
			return nil
		}
		if !d.Type().IsRegular() && d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		lexicalSources = append(lexicalSources, p)
		src, linkTarget, err := readSource(root, p)
		if err != nil {
			return err
		}
		if linkTarget != "" {
			sourceLinks = append(sourceLinks, linkTarget)
		}
		patterns, err := Patterns(p, src)
		if err != nil {
			return err
		}
		if len(patterns) > 0 {
			found[filepath.ToSlash(rel)] = patterns
		}
		return nil
	})
	sort.Strings(sourceLinks)
	sort.Strings(lexicalSources)
	return found, sourceLinks, lexicalSources, err
}

// Resolve selects embedded assets for a project source key.
func Resolve(root string, includeTests bool) ([]string, error) {
	files, _, _, err := resolve(root, includeTests)
	return files, err
}

// ResolveInputs selects lexical Go sources, embedded assets and the regular
// targets of source-file symlinks. They all contribute to the key and must
// travel in a remote snapshot when ignored by Git.
func ResolveInputs(root string, includeTests bool) ([]string, error) {
	files, links, sources, err := resolve(root, includeTests)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(files)+len(links)+len(sources))
	for _, file := range files {
		set[file] = true
	}
	for _, file := range links {
		set[file] = true
	}
	for _, file := range sources {
		set[file] = true
	}
	return sortedPaths(set), nil
}

func resolve(root string, includeTests bool) ([]string, []string, []string, error) {
	directives, links, sources, err := directivesAndSourceLinks(root, includeTests)
	if err != nil {
		return nil, nil, nil, err
	}
	set := make(map[string]bool)
	for source, patterns := range directives {
		for _, declared := range patterns {
			files, err := ResolvePattern(filepath.Join(root, filepath.FromSlash(path.Dir(source))), declared)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("%s: %w", source, err)
			}
			for _, file := range files {
				if err := rejectNestedModule(root, file); err != nil {
					return nil, nil, nil, fmt.Errorf("%s: %w", source, err)
				}
				set[file] = true
			}
		}
	}
	return sortedPaths(set), links, sources, nil
}

func sortedPaths(set map[string]bool) []string {
	files := make([]string, 0, len(set))
	for file := range set {
		files = append(files, file)
	}
	sort.Strings(files)
	return files
}

func rejectNestedModule(root, file string) error {
	rel, err := filepath.Rel(root, file)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("embedded file %q escapes project", file)
	}
	dir := root
	for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if part == "." {
			break
		}
		dir = filepath.Join(dir, part)
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return fmt.Errorf("embedded file %q crosses nested module", file)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// SelectsPath matches an existing or deleted project-relative source or asset.
// It intentionally does not resolve targets, so deletion still reaches the
// task that previously read the file.
func SelectsPath(root, relative string, includeTests bool) (bool, error) {
	directives, links, sources, err := directivesAndSourceLinks(root, includeTests)
	if err != nil {
		return false, err
	}
	relative = filepath.ToSlash(filepath.Clean(relative))
	for _, source := range sources {
		if rel, err := filepath.Rel(root, source); err == nil && filepath.ToSlash(rel) == relative {
			return true, nil
		}
	}
	// A removed source is absent from this scan. The lexical Go source policy
	// still identifies it without needing its old bytes or directives.
	if filepath.Ext(relative) == ".go" && (includeTests || !strings.HasSuffix(relative, "_test.go")) && !ignoresSource(relative) {
		if err := rejectNestedModule(root, filepath.Join(root, filepath.FromSlash(relative))); err == nil {
			return true, nil
		}
	}
	for _, link := range links {
		if rel, err := filepath.Rel(root, link); err == nil && filepath.ToSlash(rel) == relative {
			return true, nil
		}
	}
	for source, patterns := range directives {
		for _, declared := range patterns {
			pattern, all, err := validPattern(declared)
			if err != nil {
				return false, err
			}
			base := path.Dir(source)
			candidate, err := filepath.Rel(filepath.FromSlash(base), filepath.FromSlash(relative))
			if err != nil {
				return false, err
			}
			candidate = filepath.ToSlash(candidate)
			if candidate == ".." || strings.HasPrefix(candidate, "../") {
				continue
			}
			segments := strings.Split(pattern, "/")
			parts := strings.Split(candidate, "/")
			if len(parts) < len(segments) {
				continue
			}
			match := true
			for i, segment := range segments {
				ok, err := path.Match(segment, parts[i])
				if err != nil {
					return false, err
				}
				if !ok {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			if len(parts) > len(segments) && !all {
				for _, part := range parts[len(segments):] {
					if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
						match = false
						break
					}
				}
			}
			if match {
				return true, nil
			}
		}
	}
	return false, nil
}
