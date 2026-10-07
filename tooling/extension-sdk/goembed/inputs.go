// Package goembed resolves Go embed directives for source packaging and task inputs.
// Its file set is deliberately independent of the host OS: every satisfiable
// build constraint is included, so a shared cache key covers cross builds too.
package goembed

import (
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
	var expr constraint.Expr
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") || line == "package" {
			break
		}
		if constraint.IsGoBuild(line) {
			parsed, err := constraint.Parse(line)
			if err != nil {
				return false
			}
			expr = parsed
			break
		}
		if constraint.IsPlusBuild(line) {
			parsed, err := constraint.Parse(line)
			if err == nil {
				if expr == nil {
					expr = parsed
				} else {
					expr = &constraint.AndExpr{X: expr, Y: parsed}
				}
			}
		}
	}
	if expr == nil {
		return false
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
		return false
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
			return false
		}
	}
	return true
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

func ignoresSource(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "testdata" || part == "vendor" || part == "node_modules" || part == "out" || part == "dist" || strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
			return true
		}
	}
	return false
}

// Directives scans Go sources selected by the ordinary project source contract.
// It returns the source-relative path and its patterns; callers can resolve
// current files or match a deleted path against the declaration.
func Directives(root string, includeTests bool) (map[string][]string, error) {
	found := make(map[string][]string)
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
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() || filepath.Ext(p) != ".go" || (!includeTests && strings.HasSuffix(p, "_test.go")) {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
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
	return found, err
}

// Resolve selects embedded assets for a project source key.
func Resolve(root string, includeTests bool) ([]string, error) {
	directives, err := Directives(root, includeTests)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool)
	for source, patterns := range directives {
		for _, declared := range patterns {
			files, err := ResolvePattern(filepath.Join(root, filepath.FromSlash(path.Dir(source))), declared)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", source, err)
			}
			for _, file := range files {
				if err := rejectNestedModule(root, file); err != nil {
					return nil, fmt.Errorf("%s: %w", source, err)
				}
				set[file] = true
			}
		}
	}
	files := make([]string, 0, len(set))
	for file := range set {
		files = append(files, file)
	}
	sort.Strings(files)
	return files, nil
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

// SelectsPath matches an existing or deleted project-relative asset against
// directives. It intentionally does not resolve targets, so deletion still
// reaches the task that previously read the file.
func SelectsPath(root, relative string, includeTests bool) (bool, error) {
	directives, err := Directives(root, includeTests)
	if err != nil {
		return false, err
	}
	relative = filepath.ToSlash(filepath.Clean(relative))
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
