package pkg

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.putnami.dev/go/extension/internal/gosource"
	"go.putnami.dev/sdk/extension/goembed"
)

// The staged-source allowlist in prepareGoModule only knows about fixed file
// classes (.go, go.mod, docs, ...), so a //go:embed target outside that list
// would vanish from the published module zip and every downstream `go build`
// would fail with "no matching files found". stageEmbedTargets closes the gap
// at staging time and verifyZipEmbedTargets re-checks the finished artifact,
// so a staging regression cannot reach the registry.

// stageEmbedTargets scans every staged .go file for //go:embed directives,
// resolves each pattern against the corresponding source directory under
// projectRoot, and copies the matched files into stageDir. A pattern that
// matches nothing fails packaging: the module could never compile for a
// consumer, so the zip must not be produced.
func stageEmbedTargets(projectRoot, stageDir string) error {
	var goFiles []string
	err := filepath.Walk(stageDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(stageDir, p)
		if relErr != nil {
			return relErr
		}
		if info.IsDir() {
			// The go toolchain ignores testdata trees (fixtures there may be
			// intentionally unparseable) and _-/.-prefixed directories, so
			// embed scanning skips them too.
			if rel != "." && goToolingIgnoresPath(filepath.ToSlash(rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) == ".go" && !goToolingIgnoresPath(filepath.ToSlash(rel)) {
			goFiles = append(goFiles, p)
		}
		return nil
	})
	if err != nil {
		return err
	}

	for _, goFile := range goFiles {
		patterns, err := goEmbedPatternsFromFile(goFile)
		if err != nil {
			return err
		}
		if len(patterns) == 0 {
			continue
		}
		rel, err := filepath.Rel(stageDir, goFile)
		if err != nil {
			return err
		}
		srcDir := filepath.Dir(filepath.Join(projectRoot, rel))
		for _, pattern := range patterns {
			files, err := resolveEmbedPattern(srcDir, pattern)
			if err != nil {
				return fmt.Errorf("%s: go:embed %q: %w", rel, pattern, err)
			}
			if len(files) == 0 {
				return fmt.Errorf("%s: go:embed pattern %q matched no files in the module source", rel, pattern)
			}
			for _, src := range files {
				relTarget, err := filepath.Rel(projectRoot, src)
				if err != nil || strings.HasPrefix(relTarget, "..") {
					return fmt.Errorf("%s: go:embed %q resolves outside the module: %s", rel, pattern, src)
				}
				// Embedding across a nested-module boundary is rejected by go
				// build ("in different module"); the staging walk skipped that
				// go.mod, so copying the target would ship a file the source
				// module could never have embedded.
				if nested, err := insideNestedModule(projectRoot, relTarget); err != nil {
					return err
				} else if nested {
					return fmt.Errorf("%s: cannot embed file %s: in different module", rel, filepath.ToSlash(relTarget))
				}
				if err := copyFilePreservingMode(src, filepath.Join(stageDir, relTarget)); err != nil {
					return fmt.Errorf("%s: go:embed %q: stage %s: %w", rel, pattern, relTarget, err)
				}
			}
		}
	}
	return nil
}

// goEmbedPatternsFromFile returns the //go:embed patterns declared in the Go
// source file at path. The file is parsed (comments only) rather than
// line-scanned so directive-looking text inside string literals is ignored.
func goEmbedPatternsFromFile(path string) ([]string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return goEmbedPatterns(path, src)
}

func goEmbedPatterns(filename string, src []byte) ([]string, error) {
	return goembed.Patterns(filename, src)
}

// splitEmbedPatterns tokenizes the argument list of a //go:embed directive:
// space-separated patterns, individually quotable with " or `.
func splitEmbedPatterns(args string) ([]string, error) {
	return goembed.Patterns("embed.go", []byte("package p\n//go:embed "+args+"\nvar x []byte\n"))
}

// resolveEmbedPattern returns the files under dir matched by a single pattern
// from a Go embed directive, mirroring the embed package's rules closely enough for
// packaging: the pattern itself resolves via filepath.Glob, and a match that
// is a directory embeds its whole subtree, skipping .- and _-prefixed entries
// unless the pattern carries the all: prefix.
func resolveEmbedPattern(dir, pattern string) ([]string, error) {
	return goembed.ResolvePattern(dir, pattern)
}

// insideNestedModule reports whether relTarget (slash- or OS-separated,
// relative to projectRoot) lives under a directory carrying its own go.mod —
// i.e. in a different Go module than the one being packaged.
func insideNestedModule(projectRoot, relTarget string) (bool, error) {
	segments := strings.Split(filepath.ToSlash(relTarget), "/")
	dir := projectRoot
	for _, segment := range segments[:len(segments)-1] {
		dir = filepath.Join(dir, segment)
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	return false, nil
}

func copyFilePreservingMode(src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, info.Mode())
}

// verifyZipEmbedTargets re-parses every .go entry in the finished module zip
// and confirms each //go:embed pattern matches at least one entry in the same
// zip. It inspects only the zip, never the source tree.
func verifyZipEmbedTargets(zipPath, modulePath, version string) (resultErr error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := zr.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close module zip: %w", err))
		}
	}()

	prefix := modulePath + "@" + version + "/"
	var names []string
	for _, f := range zr.File {
		if rel, ok := strings.CutPrefix(f.Name, prefix); ok {
			names = append(names, rel)
		}
	}

	for _, f := range zr.File {
		rel, ok := strings.CutPrefix(f.Name, prefix)
		if !ok || path.Ext(rel) != ".go" || goToolingIgnoresPath(rel) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		src, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		patterns, err := goEmbedPatterns(rel, src)
		if err != nil {
			return err
		}
		dir := path.Dir(rel)
		for _, pattern := range patterns {
			if !embedPatternMatchesAny(names, dir, pattern) {
				return fmt.Errorf("module zip is missing go:embed target: %s declares %q but no zip entry matches", rel, pattern)
			}
		}
	}
	return nil
}

// goToolingIgnoresPath reports whether the go toolchain ignores the
// slash-separated path outright — testdata trees and _-/.-prefixed names never
// contribute Go source (or embed directives) to a build.
func goToolingIgnoresPath(rel string) bool {
	return gosource.ToolingIgnoresPath(rel)
}

// embedPatternMatchesAny reports whether any of the slash-separated file
// paths (relative to the module root) is embedded by pattern declared in a
// .go file living in dir. Segments match via path.Match; a file strictly
// below the matched path is accepted (directory embedding), with .- and
// _-prefixed remainder segments excluded unless the all: prefix is present.
func embedPatternMatchesAny(files []string, dir, pattern string) bool {
	includeHidden := strings.HasPrefix(pattern, "all:")
	pattern = strings.TrimPrefix(pattern, "all:")
	full := path.Join(dir, pattern)
	patternSegs := strings.Split(full, "/")

	for _, file := range files {
		fileSegs := strings.Split(file, "/")
		if len(fileSegs) < len(patternSegs) {
			continue
		}
		matched := true
		for i, ps := range patternSegs {
			ok, err := path.Match(ps, fileSegs[i])
			if err != nil || !ok {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		if len(fileSegs) == len(patternSegs) {
			return true
		}
		if !includeHidden {
			hidden := false
			for _, s := range fileSegs[len(patternSegs):] {
				if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "_") {
					hidden = true
					break
				}
			}
			if hidden {
				continue
			}
		}
		return true
	}
	return false
}
