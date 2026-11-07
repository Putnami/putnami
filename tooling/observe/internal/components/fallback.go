package components

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
)

// Diagnostic codes of the fallback split.
const (
	// CodeFallbackSplit is the info diagnostic that names how a repository
	// no detector recognizes is split.
	CodeFallbackSplit = "observe.fallback_split"
	// CodeFallbackWalkBounded is the warning of a top-level directory whose
	// walk reached MaxFallbackWalkedEntries before it found a source file.
	CodeFallbackWalkBounded = "observe.fallback_walk_bounded"
)

// MaxFallbackWalkedEntries bounds the entries the directory split reads in
// one top-level directory while it looks for a source file.
const MaxFallbackWalkedEntries = 200000

// fallbackWalkBudget is the bound holdsSource applies, MaxFallbackWalkedEntries.
var fallbackWalkBudget = MaxFallbackWalkedEntries

// CodeownersFiles are the CODEOWNERS locations the split reads, in GitHub's
// order: the first that exists is the one read.
var CodeownersFiles = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

// fallbackSkipped are the top-level directories the directory split leaves
// out, besides dot directories.
var fallbackSkipped = map[string]bool{".github": true, "docs": true, "vendor": true, "node_modules": true}

// sourceExtensions are the file extensions the directory split counts as
// source files.
var sourceExtensions = map[string]bool{}

func init() {
	for _, ext := range strings.Fields(`
		.c .h .cc .cpp .cxx .hh .hpp .m .mm .cs .fs .fsx .vb .go .java .kt .kts .scala .groovy .clj .cljs
		.js .jsx .mjs .cjs .ts .tsx .mts .cts .vue .svelte .elm .py .pyi .rb .rake .php .rs .swift .dart
		.ex .exs .erl .hrl .hs .ml .mli .lua .pl .pm .r .jl .zig .nim .sh .bash .sql .proto .tf .sol`) {
		sourceExtensions[ext] = true
	}
}

// fallbackSplit gives the components of a repository no detector recognizes:
// the directories its CODEOWNERS names, else its top-level directories that
// hold a source file, else the repository as one component.
func fallbackSplit(root string) ([]Component, []diag.Diagnostic) {
	if file, dirs := codeownersDirectories(root); len(dirs) > 0 {
		return fallbackComponents(dirs, evidence.SourceFallbackCodeowners), []diag.Diagnostic{splitInfo(
			"no detector found a component, so the repository is split into the %d directories %s names", len(dirs), file)}
	}
	dirs, diags := sourceDirectories(root)
	// read qualifies the top-level directories when a bounded walk left one
	// unread to its end.
	read, readAll := "", ""
	if len(diags) > 0 {
		read, readAll = " read to its end", " read to their end"
	}
	if len(dirs) > 0 {
		return fallbackComponents(dirs, evidence.SourceFallbackDirectories), append(diags, splitInfo(
			"no detector found a component and no CODEOWNERS names a directory, so the repository is split into the %d top-level directories%s that hold a source file", len(dirs), readAll))
	}
	return []Component{{
		ID: evidence.RootComponent, Ecosystem: FallbackEcosystem, Source: evidence.SourceFallback, Detector: evidence.SourceFallback,
	}}, append(diags, splitInfo("no detector found a component, no CODEOWNERS names a directory and no top-level directory%s holds a source file, so the repository is one component", read))
}

func splitInfo(format string, args ...any) diag.Diagnostic {
	return diag.Diagnostic{Severity: diag.Info, Code: CodeFallbackSplit, Field: "components", Message: fmt.Sprintf(format, args...)}
}

func fallbackComponents(dirs []string, source string) []Component {
	out := make([]Component, 0, len(dirs))
	for _, dir := range dirs {
		out = append(out, Component{ID: dir, Name: path.Base(dir), Ecosystem: FallbackEcosystem, Source: source, Detector: source})
	}
	return out
}

// codeownersDirectories returns the first CODEOWNERS file that exists and
// the existing directories its patterns name, sorted. A pattern names a
// directory when, without its leading slash and a trailing /, /* or /**, it
// holds no wildcard and is a directory of the repository.
func codeownersDirectories(root string) (string, []string) {
	for _, file := range CodeownersFiles {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		var dirs []string
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			dir, ok := codeownersDirectory(scanner.Text())
			if !ok || seen[dir] {
				continue
			}
			if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir))); err != nil || !info.IsDir() {
				continue
			}
			seen[dir] = true
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		return file, dirs
	}
	return "", nil
}

// codeownersDirectory returns the repository-relative directory a CODEOWNERS
// line's pattern names, if it names one.
func codeownersDirectory(line string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", false
	}
	pattern := fields[0]
	// Comments, negations, escapes and GitLab sections name no directory.
	if strings.HasPrefix(pattern, "#") || strings.HasPrefix(pattern, "!") || strings.HasPrefix(pattern, "[") ||
		strings.HasPrefix(pattern, "^[") || strings.Contains(pattern, `\`) {
		return "", false
	}
	pattern = strings.TrimPrefix(pattern, "/")
	for _, suffix := range []string{"/**", "/*", "/"} {
		if trimmed, ok := strings.CutSuffix(pattern, suffix); ok {
			pattern = trimmed
			break
		}
	}
	if pattern == "" || strings.ContainsAny(pattern, "*?[") {
		return "", false
	}
	dir := path.Clean(pattern)
	if dir == "." || dir == ".." || strings.HasPrefix(dir, "../") || path.IsAbs(dir) {
		return "", false
	}
	return dir, true
}

// sourceDirectories returns the top-level directories that hold a source
// file at any depth, sorted. It leaves out fallbackSkipped and dot directories
// at the top level, and dot and node_modules directories below it. A
// directory whose walk reaches MaxFallbackWalkedEntries before a source file
// is left out with a warning.
func sourceDirectories(root string) ([]string, []diag.Diagnostic) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil
	}
	var dirs []string
	var diags []diag.Diagnostic
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") || fallbackSkipped[name] {
			continue
		}
		found, bounded := holdsSource(filepath.Join(root, name))
		if found {
			dirs = append(dirs, name)
		}
		if bounded {
			diags = append(diags, diag.Warningf(CodeFallbackWalkBounded, "components",
				"the directory split read %d entries of %s without finding a source file, so %s is no component", fallbackWalkBudget, name, name))
		}
	}
	return dirs, diags
}

// holdsSource reports whether the directory start holds a source file, and
// whether its walk stopped at MaxFallbackWalkedEntries first.
func holdsSource(start string) (found, bounded bool) {
	walked := 0
	_ = filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		walked++
		if walked > fallbackWalkBudget {
			bounded = true
			return fs.SkipAll
		}
		if d.IsDir() {
			if child := d.Name(); p != start && (strings.HasPrefix(child, ".") || child == "node_modules") {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && sourceExtensions[strings.ToLower(filepath.Ext(d.Name()))] {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found, bounded
}
