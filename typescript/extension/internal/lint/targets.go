package lint

import (
	"errors"
	"os"
	"path"
	"path/filepath"
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
// is `.gen`, which the read-only task leaves out of its key: git ignores it and
// Biome never reads it, so a marker there changes nothing Biome lints.
//
// An empty list with a nil error means every file of the selected projects
// belongs to a nested project, and there is nothing left for Biome to read.
func biomeTargets(workspaceRoot string, projectPaths []string, configPath string) (string, []string, error) {
	runDir, roots := biomeBatchRunContext(workspaceRoot, projectPaths, configPath)
	if len(roots) == 0 {
		return "", nil, errNoSharedRoot
	}
	targets := make([]string, 0, len(roots))
	for _, root := range roots {
		cover, err := projectCover(runDir, root)
		if err != nil {
			return "", nil, err
		}
		targets = append(targets, cover...)
	}
	return runDir, targets, nil
}

// projectCover returns the paths that cover the project at target, relative to
// runDir, without entering a nested project. A project with no nested project
// is covered by target itself, so Biome keeps one directory argument for it.
func projectCover(runDir, target string) ([]string, error) {
	kind, paths, err := coverDirectory(filepath.Join(runDir, filepath.FromSlash(target)), target, true)
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
// is named explicitly.
func coverDirectory(dir, rel string, root bool) (coverage, []string, error) {
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
		childKind, childPaths, err := coverDirectory(filepath.Join(dir, name), entryRel, false)
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
