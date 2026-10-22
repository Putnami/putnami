package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"go.putnami.dev/sdk/extension/goembed"
)

// ErrInputSelector identifies an invalid input selection: a selector no
// expander answers, a failed expansion, or a file it names that cannot be
// read. A scheduler must fail such a task before cache restoration or
// execution; an unrelated cache-store failure may still fall back to
// execution.
var ErrInputSelector = errors.New("invalid input selector")

// InputExpansion lists what an input selector names in one directory when the
// expander answers.
type InputExpansion struct {
	// Files are sorted, unique, clean slash paths relative to the directory.
	Files []string
}

// InputExpander expands an input selector for a directory. patterns are the
// other patterns of the pattern set that names the selector, includes and
// exclusions, without selector names. An error wraps ErrInputSelector.
//
// A CacheManager memoizes a digest by directory, patterns and config scope,
// not by expander, so every caller of one manager passes expanders that
// expand a selector alike.
type InputExpander interface {
	ExpandInput(dir, selector string, patterns []string) (InputExpansion, error)
}

// GoEmbedExpander returns the expander that expands the go-embed selectors in
// this process, with the SDK package goembed.
func GoEmbedExpander() InputExpander { return goEmbedExpander{} }

type goEmbedExpander struct{}

// ExpandInput lists what a go-embed selector reads in dir: the Go sources, the
// files their embed directives name and the referents of source links
// (goembed.ResolveInputs, which reads test sources for goembed.TestSelector),
// and every .go link patterns select with its referent. A glob can select a
// link in a directory the source scan skips, so the scan alone misses it.
func (goEmbedExpander) ExpandInput(dir, selector string, patterns []string) (InputExpansion, error) {
	if !goembed.IsSelector(selector) {
		return InputExpansion{}, fmt.Errorf("%w: unsupported selector %q", ErrInputSelector, selector)
	}
	links, err := selectedGoLinks(dir, patterns)
	if err != nil {
		return InputExpansion{}, fmt.Errorf("%w: selected Go source: %w", ErrInputSelector, err)
	}
	targets, err := goembed.ResolveInputs(dir, selector == goembed.TestSelector)
	if err != nil {
		return InputExpansion{}, fmt.Errorf("%w: resolve %s inputs: %w", ErrInputSelector, selector, err)
	}
	return relativeExpansion(dir, append(links, targets...))
}

// selectedGoLinks returns every .go symbolic link patterns select under dir,
// each followed by its referent (goembed.SourceReferent), as absolute paths.
// A selected link the source policy refuses is an error.
func selectedGoLinks(dir string, patterns []string) ([]string, error) {
	selected, err := collectPatternFiles(dir, patterns, true)
	if err != nil {
		return nil, err
	}
	var links []string
	for _, file := range selected {
		if filepath.Ext(file.path) != ".go" {
			continue
		}
		info, err := os.Lstat(file.path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		referent, err := goembed.SourceReferent(dir, file.path)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve selected Go source link %s: %w", ErrInputSelector, file.path, err)
		}
		links = append(links, file.path, referent)
	}
	return links, nil
}

// relativeExpansion lists paths, each at or under dir, relative to dir.
func relativeExpansion(dir string, paths []string) (InputExpansion, error) {
	files := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return InputExpansion{}, fmt.Errorf("%w: %s is not under %s: %w", ErrInputSelector, p, dir, err)
		}
		files = append(files, filepath.ToSlash(rel))
	}
	slices.Sort(files)
	return InputExpansion{Files: slices.Compact(files)}, nil
}
