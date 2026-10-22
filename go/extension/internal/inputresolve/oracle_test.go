package inputresolve

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/goembed"
)

// coreRawEmbedFiles is the oracle the equivalence tests compare an answer's
// files with. It reproduces, statement for statement, the files the core's
// cache key marks rawEmbed for one task input of a project directory that
// names selector beside patterns: collectFiles in
// tooling/cli/internal/store/cache_hash.go with collectOrdinaryFiles,
// ordinaryFileEntry, globDoubleStar, appendSelectedGoReferents and
// appendGoEmbedInputs. It returns absolute paths, sorted and unique.
func coreRawEmbedFiles(dir string, patterns []string, selector string) ([]string, error) {
	includes, excludes := wsproto.SplitFilePatterns(patterns)
	var ordinary []string
	for _, pattern := range includes {
		if _, ok := wsproto.GitFilePattern(pattern); ok {
			return nil, errors.New("the oracle does not collect git: patterns")
		}
		ordinary = append(ordinary, pattern)
	}
	files := oracleCollectOrdinaryFiles(dir, ordinary, excludes)
	files, err := oracleAppendSelectedGoReferents(dir, files)
	if err != nil {
		return nil, fmt.Errorf("selected Go source: %w", err)
	}
	files, err = oracleAppendGoEmbedInputs(dir, files, selector)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var raw []string
	for _, file := range files {
		if file.rawEmbed && !seen[file.path] {
			seen[file.path] = true
			raw = append(raw, file.path)
		}
	}
	sort.Strings(raw)
	return raw, nil
}

type oracleFile struct {
	path     string
	rawEmbed bool
}

func oracleGlobDoubleStar(dir, pattern string) []string {
	pattern = filepath.ToSlash(pattern)
	parts := strings.SplitN(pattern, "**", 2)
	prefix := strings.Trim(strings.TrimSuffix(parts[0], "/"), "/")

	searchRoot := dir
	if prefix != "" {
		searchRoot = filepath.Join(dir, filepath.FromSlash(prefix))
	}

	var matches []string
	_ = filepath.WalkDir(searchRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p == searchRoot {
				return nil
			}
			switch d.Name() {
			case "node_modules", ".git", ".putnami", "out", "dist", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return nil
		}
		if wsproto.MatchFilePattern(filepath.ToSlash(rel), pattern) {
			matches = append(matches, p)
		}
		return nil
	})
	return matches
}

func oracleCollectOrdinaryFiles(dir string, patterns, excludes []string) []oracleFile {
	var files []oracleFile
	for _, pattern := range patterns {
		var matches []string
		if strings.Contains(pattern, "**") {
			matches = oracleGlobDoubleStar(dir, pattern)
		} else {
			matches, _ = filepath.Glob(filepath.Join(dir, pattern))
		}
		for _, match := range matches {
			if file, ok := oracleOrdinaryFileEntry(dir, match, excludes); ok {
				files = append(files, file)
			}
		}
	}
	return files
}

func oracleOrdinaryFileEntry(dir, path string, excludes []string) (oracleFile, bool) {
	info, err := os.Stat(path)
	if err != nil {
		if filepath.Ext(path) != ".go" {
			return oracleFile{}, false
		}
		info, err = os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return oracleFile{}, false
		}
	}
	if info.IsDir() {
		if filepath.Ext(path) != ".go" {
			return oracleFile{}, false
		}
		linkInfo, linkErr := os.Lstat(path)
		if linkErr != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
			return oracleFile{}, false
		}
	}
	if len(excludes) > 0 {
		rel, _ := filepath.Rel(dir, path)
		if wsproto.MatchesAnyFilePattern(filepath.ToSlash(rel), excludes) {
			return oracleFile{}, false
		}
	}
	return oracleFile{path: path}, true
}

func oracleAppendSelectedGoReferents(dir string, files []oracleFile) ([]oracleFile, error) {
	for index := range files {
		file := files[index]
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
			return nil, fmt.Errorf("resolve selected Go source link %s: %w", file.path, err)
		}
		if _, err := os.Lstat(referent); err != nil {
			return nil, fmt.Errorf("stat Go source referent %s: %w", referent, err)
		}
		files[index].rawEmbed = true
		files = append(files, oracleFile{path: referent, rawEmbed: true})
	}
	return files, nil
}

func oracleAppendGoEmbedInputs(dir string, files []oracleFile, selector string) ([]oracleFile, error) {
	targets, err := goembed.ResolveInputs(dir, selector == goembed.TestSelector)
	if err != nil {
		return nil, fmt.Errorf("resolve %s inputs: %w", selector, err)
	}
	for _, target := range targets {
		if _, err := os.Lstat(target); err != nil {
			return nil, fmt.Errorf("stat resolved input %s: %w", target, err)
		}
		files = append(files, oracleFile{path: target, rawEmbed: true})
	}
	return files, nil
}
