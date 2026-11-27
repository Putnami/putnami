package workspace

import (
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fileindex"

	wsproto "go.putnami.dev/protocol/workspace"
)

// ProjectWalkExcludedDirs are the directories a walk of a project's tree does
// not enter.
var ProjectWalkExcludedDirs = map[string]bool{
	".git": true, ".putnami": true, ".venv": true,
	"dist": true, "node_modules": true, "vendor": true,
}

// MayPlanJob reports whether the planner may run job, one command of an
// extension, in proj: a workspace-once command always may; any other one when
// UsesJobExtension holds and its activation files, when it names any, match a
// file of the project. hasFiles answers HasActivationFiles for the project's
// directory.
//
// The planner plans no job that fails these conditions in the project. It
// also leaves out a disabled job, a job of another channel and a step whose
// condition is false, so a job this reports true for may still not run.
func MayPlanJob(proj *Project, job *extension.JobDefinition, hasFiles func(patterns []string) bool) bool {
	if job.Activation == workspaceOnceActivation {
		return true
	}
	if !UsesJobExtension(proj, job, len(job.ActivationFiles) > 0) {
		return false
	}
	return len(job.ActivationFiles) == 0 || hasFiles(job.ActivationFiles)
}

// UsesJobExtension reports whether proj may use the jobs of job's extension.
// A project qualifies when:
//  1. The command has workspace activation (activates for all projects), OR
//  2. The command's activation files matched the project and the command is
//     config-extract, OR
//  3. The extension is listed in the project's Dependencies or Extensions, by
//     its name or its /path ID, OR
//  4. The project IS the extension itself and the command's traits declare
//     injectPublishChannels: an extension packages and publishes itself.
func UsesJobExtension(proj *Project, job *extension.JobDefinition, matchedActivationFiles bool) bool {
	extName := job.ExtensionName
	extID := ""
	if job.ExtensionPath != "" {
		extID = wsproto.ProjectIDFromPath(job.ExtensionPath)
	}
	if job.Activation == "workspace" || job.Activation == workspaceOnceActivation {
		return true
	}
	if matchedActivationFiles && job.Name == "config-extract" {
		return true
	}
	if proj.Name == extName && job.Traits.InjectPublishChannels {
		return true
	}
	for _, dep := range proj.Dependencies {
		if dep == extName || (extID != "" && dep == extID) {
			return true
		}
	}
	for _, ext := range proj.Extensions {
		if ext == extName || (extID != "" && ext == extID) {
			return true
		}
	}
	return false
}

// HasActivationFiles reports whether a file under projRoot matches one of
// patterns. A pattern without "**" is a filepath.Glob under projRoot; one with
// "**" walks the tree under its static prefix and matches each file name
// against the part after "**". The walk enters no directory of
// ProjectWalkExcludedDirs and no other project's root in projectRoots.
func HasActivationFiles(projRoot string, patterns []string, projectRoots map[string]bool) bool {
	return HasActivationFilesProbed(projRoot, patterns, projectRoots, nil)
}

// HasActivationFilesProbed is HasActivationFiles recording what it read into
// probe, which may be nil.
func HasActivationFilesProbed(projRoot string, patterns []string, projectRoots map[string]bool, probe *ActivationProbe) bool {
	for _, pattern := range patterns {
		found := false
		visitActivationGlob(projRoot, pattern, projectRoots, probe, func(string) bool {
			found = true
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// visitActivationGlob calls visit with each file under root that matches
// pattern, until it returns true. A pattern without "**" is a filepath.Glob
// under root, its matches in lexical order; one with "**" walks the tree under
// its static prefix, entering no directory of ProjectWalkExcludedDirs below
// its search root and no other project's root in projectRoots, and matches
// each file name against the part after "**". probe, which may be nil,
// records each directory listed and each absent search root.
func visitActivationGlob(root, pattern string, projectRoots map[string]bool, probe *ActivationProbe, visit func(path string) bool) {
	root = filepath.Clean(root)
	if !strings.Contains(pattern, "**") {
		full := filepath.Join(root, pattern)
		dir := filepath.Dir(full)
		if hasGlobMeta(strings.TrimPrefix(dir, root)) {
			probe.Untrusted()
		} else if st, ok := fileindex.LstatAny(dir); ok && st.Mode&modeType == modeDirectory {
			probe.Dir(dir)
		} else {
			probe.Absent(full)
		}
		matches, _ := filepath.Glob(full)
		for _, match := range matches {
			if visit(match) {
				return
			}
		}
		return
	}
	// Split on "**/" or "**" to get the suffix pattern.
	// e.g. "**/*.test.ts" → suffix "*.test.ts"
	// e.g. "src/**/*.go"  → prefix "src", suffix "*.go"
	parts := strings.SplitN(pattern, "**", 2)
	prefix := strings.TrimSuffix(strings.TrimSuffix(parts[0], "/"), string(filepath.Separator))
	suffix := strings.TrimPrefix(parts[1], string(filepath.Separator))
	// Also handle forward slashes (JSON manifests use them)
	suffix = strings.TrimPrefix(suffix, "/")

	searchRoot := root
	if prefix != "" {
		searchRoot = filepath.Join(root, prefix)
	}
	// A static prefix cannot start inside a nested declared project.
	for dir := searchRoot; dir != root; dir = filepath.Dir(dir) {
		if projectRoots[dir] {
			probe.Dir(dir)
			return
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if st, ok := fileindex.LstatAny(searchRoot); !ok || st.Mode&modeType != modeDirectory {
		probe.Absent(filepath.Join(searchRoot, "x"))
		if !ok {
			return
		}
	}

	_ = filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			probe.Untrusted()
			return nil
		}
		if d.IsDir() {
			// Keep the starting project's own root eligible; a prefix may
			// start the walk directly at a different project's root.
			if path != root && projectRoots[filepath.Clean(path)] {
				return filepath.SkipDir
			}
			// Skip common non-project directories for performance, below
			// the search root: a project rooted at a directory named dist is
			// still walked.
			if path != searchRoot && ProjectWalkExcludedDirs[d.Name()] {
				return filepath.SkipDir
			}
			probe.Dir(path)
			return nil
		}
		// Match the filename against the suffix pattern
		if matched, _ := filepath.Match(suffix, d.Name()); matched && visit(path) {
			return filepath.SkipAll
		}
		return nil
	})
}
