// Package inputresolve answers the input-resolve control call for the two
// input selectors the Go extension declares: go-embed:build and
// go-embed:test.
//
// For each asked input it lists the files the selector reads in the tree, as
// the core collects them for a cache key: the Go sources the embed scan
// reads, the files their //go:embed directives name, the regular referent of
// each Go source link, and each Go source link the input's ordinary patterns
// select with its referent. It states the basis of that list, so the core keeps the
// answer while workspace.InputBasisDigest of the basis holds.
//
// The basis selects every path whose bytes, existence, kind or link target
// the answer read. A read the digest cannot see turns the answer into an
// error instead: a kept answer would otherwise go stale under an unchanged
// digest. doc/build.md lists those cases.
package inputresolve

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/goembed"
)

// maxErrorBytes is the protocol's bound on one answer's error.
const maxErrorBytes = 4096

// Resolve answers every input of request, in request order, for the
// workspace at root. The result names the request's extension; a caller that
// answers for one extension sets its own name.
func Resolve(root string, request wsproto.InputResolveRequest) wsproto.InputResolveResult {
	result := wsproto.InputResolveResult{
		Version:   wsproto.InputResolveProtocolVersion,
		Extension: request.Extension,
		Inputs:    make([]wsproto.InputResolveAnswer, 0, len(request.Inputs)),
	}
	for _, query := range request.Inputs {
		result.Inputs = append(result.Inputs, Answer(root, query))
	}
	return result
}

// Answer answers one input for the workspace at root: the files the
// selector reads in the tree and the basis they were computed from, or an
// error. The answer always passes the protocol's own validation: an answer
// that would not becomes an error answer that says why.
func Answer(root string, query wsproto.InputResolveQuery) wsproto.InputResolveAnswer {
	answer, err := resolveQuery(root, query)
	if err == nil {
		err = validateAnswer(answer)
	}
	if err != nil {
		return wsproto.InputResolveAnswer{Project: query.Project, Selector: query.Selector, Error: errorText(err)}
	}
	return answer
}

// resolveQuery computes one answer, or the reason the input is invalid.
func resolveQuery(root string, query wsproto.InputResolveQuery) (wsproto.InputResolveAnswer, error) {
	includeTests, err := selectorReadsTests(query.Selector)
	if err != nil {
		return wsproto.InputResolveAnswer{}, err
	}
	for _, pattern := range query.Patterns {
		if err := checkQueryPattern(pattern); err != nil {
			return wsproto.InputResolveAnswer{}, err
		}
	}
	dir := filepath.Join(root, filepath.FromSlash(query.Project))
	if err := checkProjectDir(dir); err != nil {
		return wsproto.InputResolveAnswer{}, err
	}

	scanned, err := goembed.ResolveInputs(dir, includeTests)
	if err != nil {
		return wsproto.InputResolveAnswer{}, fmt.Errorf("resolve %s inputs: %w", query.Selector, err)
	}
	directives, _, err := goembed.DirectivesAndLinks(dir, includeTests)
	if err != nil {
		return wsproto.InputResolveAnswer{}, fmt.Errorf("read %s directives: %w", query.Selector, err)
	}
	selected, err := selectedSourceFiles(dir, query.Patterns)
	if err != nil {
		return wsproto.InputResolveAnswer{}, err
	}
	files, err := relativeFiles(dir, append(scanned, selected...))
	if err != nil {
		return wsproto.InputResolveAnswer{}, err
	}

	r := &resolution{
		dir:          dir,
		includeTests: includeTests,
		patterns:     query.Patterns,
		files:        files,
		directives:   directives,
		spelling:     spellingCache{},
	}
	basis, err := r.basis()
	if err != nil {
		return wsproto.InputResolveAnswer{}, err
	}
	return wsproto.InputResolveAnswer{
		Project:  query.Project,
		Selector: query.Selector,
		Files:    files,
		Basis:    basis,
	}, nil
}

// selectorReadsTests reports whether the selector reads test sources, and
// refuses a selector this extension does not expand.
func selectorReadsTests(selector string) (bool, error) {
	switch selector {
	case goembed.BuildSelector:
		return false, nil
	case goembed.TestSelector:
		return true, nil
	}
	return false, fmt.Errorf("the Go extension expands %s and %s, not %q",
		goembed.BuildSelector, goembed.TestSelector, selector)
}

// checkQueryPattern refuses an ordinary pattern of the query that the basis
// cannot bind: a `git:` pattern, which selects from Git's candidate list
// rather than from the tree, and a pattern that is not a clean relative path
// inside the project.
func checkQueryPattern(pattern string) error {
	body := strings.TrimPrefix(pattern, "!")
	switch {
	case strings.HasPrefix(body, "git:"):
		return fmt.Errorf("pattern %q selects from Git's candidate list; the Go extension cannot bind it to a basis of the tree", pattern)
	case body == ".." || strings.HasPrefix(body, "../") || strings.Contains(body, "/../") || strings.HasSuffix(body, "/.."):
		return fmt.Errorf("pattern %q climbs out of a directory with \"..\"; the Go extension binds patterns inside the project only", pattern)
	case strings.ContainsRune(body, '\\') || strings.HasPrefix(body, "/") || path.Clean(body) != body || body == ".":
		return fmt.Errorf("pattern %q is not a clean relative slash path; the Go extension cannot bind it to a basis", pattern)
	}
	return nil
}

// checkProjectDir requires the project directory to be a directory, not a
// link to one: the basis digest walks it without following a link.
func checkProjectDir(dir string) error {
	info, err := os.Lstat(dir)
	switch {
	case err != nil:
		return fmt.Errorf("project directory: %w", err)
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("project directory %s is a symbolic link; the basis digest does not follow one", dir)
	case !info.IsDir():
		return fmt.Errorf("project directory %s is not a directory", dir)
	}
	return nil
}

// selectedSourceFiles returns each Go source link the query's ordinary
// patterns select and its referent, as absolute paths. A link the core
// would refuse, by goembed.SourceReferent's rules, is an error.
func selectedSourceFiles(dir string, patterns []string) ([]string, error) {
	links := selectedSourceLinks(dir, patterns)
	files := make([]string, 0, 2*len(links))
	for _, link := range links {
		referent, err := goembed.SourceReferent(dir, link)
		if err != nil {
			return nil, fmt.Errorf("resolve selected Go source link %s: %w", projectPath(dir, link), err)
		}
		if _, err := os.Lstat(referent); err != nil {
			return nil, fmt.Errorf("stat Go source referent %s: %w", projectPath(dir, referent), err)
		}
		files = append(files, link, referent)
	}
	return files, nil
}

// relativeFiles turns absolute paths under dir into the answer's files:
// relative slash paths, sorted, each named once. A path the protocol cannot
// carry, one with a backslash or one that is not UTF-8, is an error.
func relativeFiles(dir string, paths []string) ([]string, error) {
	seen := make(map[string]bool, len(paths))
	files := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		if err := checkCarriedPath(rel); err != nil {
			return nil, err
		}
		if !seen[rel] {
			seen[rel] = true
			files = append(files, rel)
		}
	}
	sort.Strings(files)
	return files, nil
}

// checkCarriedPath refuses a project-relative path the input-resolve
// exchange cannot carry: one with a backslash, which the protocol refuses,
// or one that is not UTF-8, which JSON would rewrite.
func checkCarriedPath(rel string) error {
	switch {
	case strings.ContainsRune(rel, '\\'):
		return fmt.Errorf("path %q contains a backslash, which an input-resolve answer cannot carry", rel)
	case !utf8.ValidString(rel):
		return fmt.Errorf("path %q is not valid UTF-8, which an input-resolve answer cannot carry", rel)
	case rel == "." || rel == ".." || strings.HasPrefix(rel, "../"):
		return fmt.Errorf("path %q is not inside the project", rel)
	}
	return nil
}

// validateAnswer holds one answer to the protocol's rules, so a defect here
// fails one input with its reason rather than the whole call.
func validateAnswer(answer wsproto.InputResolveAnswer) error {
	result := wsproto.InputResolveResult{
		Version:   wsproto.InputResolveProtocolVersion,
		Extension: "@putnami/go",
		Inputs:    []wsproto.InputResolveAnswer{answer},
	}
	if diags := wsproto.ValidateInputResolveResult(&result); diag.HasErrors(diags) {
		return errors.New("the answer breaks the input-resolve contract:\n" + diag.ErrorText(diags))
	}
	return nil
}

// errorText renders err as an answer's error: valid UTF-8, never blank, and
// cut at a character boundary to the protocol's 4096 bytes.
func errorText(err error) string {
	text := strings.ToValidUTF8(err.Error(), "�")
	if strings.TrimSpace(text) == "" {
		text = "input resolution failed"
	}
	if len(text) <= maxErrorBytes {
		return text
	}
	cut := maxErrorBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// projectPath names an absolute path under dir by its project-relative slash
// path, for messages.
func projectPath(dir, abs string) string {
	rel, err := filepath.Rel(dir, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}
