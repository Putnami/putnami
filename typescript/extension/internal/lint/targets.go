package lint

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
)

// projectManifest is the file that makes a directory a Putnami project root.
const projectManifest = "putnami.json"

// errNoSharedRoot reports selected projects that cannot run from one directory.
var errNoSharedRoot = errors.New("selected projects do not share workspace root")

// biomeTargets returns the directory Biome runs from and the paths it is given
// for the selected project roots, in slash form relative to that directory.
//
// Each project contributes the paths that cover its own files. A subdirectory
// that holds its own putnami.json is a nested project: Biome is never given it
// or anything under it, so linting one project cannot read or rewrite another
// project's sources. The scan skips the directories the input hasher skips, so
// a marker it finds is a keyed input of the lint tasks (`**/*.json`), and
// adding or removing a nested project changes the task key. The one exception
// is `.gen`, which the read-only task leaves out of its key: a lint that only
// checks never gives Biome the `.gen` of a project root, so a marker there
// changes nothing Biome lints.
//
// With checkOnly set, the lint writes nothing, and each project also leaves
// out what Putnami writes at its root (writtenByPutnami): the repository's
// Biome configuration never formatted those files, and a lint that only
// checks cannot bring them to it.
//
// An empty list with a nil error means every file of the selected projects
// belongs to a nested project or is written by Putnami, and there is nothing
// left for Biome to read.
func biomeTargets(workspaceRoot string, projectPaths []string, configPath string, checkOnly bool) (string, []string, error) {
	runDir, roots := biomeBatchRunContext(workspaceRoot, projectPaths, configPath)
	if len(roots) == 0 {
		return "", nil, errNoSharedRoot
	}
	targets := make([]string, 0, len(roots))
	for _, root := range roots {
		cover, err := projectCover(runDir, root, checkOnly)
		if err != nil {
			return "", nil, err
		}
		targets = append(targets, cover...)
	}
	return runDir, targets, nil
}

// projectCover returns the paths that cover the project at target, relative to
// runDir, without entering a nested project. A project with no nested project
// and no left-out file is covered by target itself, so Biome keeps one
// directory argument for it.
func projectCover(runDir, target string, checkOnly bool) ([]string, error) {
	kind, paths, err := coverDirectory(filepath.Join(runDir, filepath.FromSlash(target)), target, true, checkOnly)
	if err != nil {
		return nil, err
	}
	if kind == coverWhole {
		return []string{target}, nil
	}
	return paths, nil
}

// coverage classifies one directory of a project.
type coverage int

const (
	// coverWhole: no nested project lies below the directory.
	coverWhole coverage = iota
	// coverPartial: some entries lie in a nested project.
	coverPartial
	// coverNested: the directory is a nested project root.
	coverNested
)

// unscannedDirectories are passed to Biome whole, never scanned for nested
// projects. They are the directories the input hasher skips
// (tooling/cli/internal/store/cache_hash.go), so a marker inside one could
// change what Biome reads without changing the task key.
var unscannedDirectories = map[string]bool{
	"node_modules": true,
	".git":         true,
	".putnami":     true,
	"out":          true,
	"dist":         true,
	"vendor":       true,
}

// coverDirectory lists one directory of a project. For coverPartial it also
// returns the paths of the entries that stay outside every nested project, in
// directory order.
//
// A project root that cannot be read is an error. A subdirectory that cannot
// be read is passed to Biome whole, as before the scan existed: Biome cannot
// rewrite what this process cannot read, and it applies its own ignore rules
// to it. A symbolic link is passed by name and never followed; a link whose
// target is missing is left out, because Biome reports it as an error when it
// is named explicitly. With checkOnly set, the entries of a project root that
// Putnami writes are left out.
func coverDirectory(dir, rel string, root, checkOnly bool) (coverage, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if root {
			return coverWhole, nil, err
		}
		return coverWhole, nil, nil
	}
	if !root && holdsProjectManifest(entries) {
		return coverNested, nil, nil
	}
	kind := coverWhole
	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		entryRel := path.Join(rel, name)
		if root && checkOnly && writtenByPutnami(dir, entry) {
			kind = coverPartial
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				paths = append(paths, entryRel)
			}
			continue
		}
		if !entry.IsDir() || unscannedDirectories[name] {
			paths = append(paths, entryRel)
			continue
		}
		childKind, childPaths, err := coverDirectory(filepath.Join(dir, name), entryRel, false, checkOnly)
		if err != nil {
			return coverWhole, nil, err
		}
		switch childKind {
		case coverWhole:
			paths = append(paths, entryRel)
		case coverPartial:
			kind = coverPartial
			paths = append(paths, childPaths...)
		case coverNested:
			kind = coverPartial
		}
	}
	if kind == coverWhole {
		return coverWhole, nil, nil
	}
	return coverPartial, paths, nil
}

func holdsProjectManifest(entries []os.DirEntry) bool {
	for _, entry := range entries {
		if entry.Name() == projectManifest && !entry.IsDir() {
			return true
		}
	}
	return false
}

// importedTSConfig is the tsconfig.json an import writes in a project that
// holds none: it extends the workspace root's and sets nothing else.
var importedTSConfig = regexp.MustCompile(`^\{\n  "extends": "(\.\./)+tsconfig\.json"\n\}\n$`)

// writtenByPutnami reports whether an entry of the project root dir is one
// Putnami writes: the project manifest, a generated directory, or a
// tsconfig.json with the bytes an import writes.
func writtenByPutnami(dir string, entry os.DirEntry) bool {
	name := entry.Name()
	switch {
	case entry.IsDir():
		return slices.Contains(generatedDirectories, name)
	case !entry.Type().IsRegular():
		return false
	case name == projectManifest:
		return true
	case name == "tsconfig.json":
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a file of the project being linted
		return err == nil && importedTSConfig.Match(data)
	}
	return false
}
