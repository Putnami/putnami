package workspace

import (
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"

	wsproto "go.putnami.dev/protocol/workspace"
)

// ProjectWalkExcludedDirs are the directories a walk of a project's tree does
// not enter.
var ProjectWalkExcludedDirs = map[string]bool{
	".git": true, ".putnami": true, ".venv": true,
	"dist": true, "node_modules": true, "vendor": true,
}

// MayPlanJob reports whether the planner may run job, one command of an
// extension, in proj, whose absolute directory is projRoot: the command's
// activation files, when it names any, match a file of the project, and
// UsesJobExtension holds. projectRoots holds the absolute directories of the
// workspace's projects, which a "**" walk does not enter; nil enters every
// directory.
//
// The planner checks these conditions before it picks one of the jobs that
// pass them, and it leaves out a disabled job and a job of another channel,
// so a job this reports false for is never planned in the project.
func MayPlanJob(proj *Project, job *extension.JobDefinition, projRoot string, projectRoots map[string]bool) bool {
	if !UsesJobExtension(proj, job, len(job.ActivationFiles) > 0) {
		return false
	}
	if len(job.ActivationFiles) == 0 {
		return true
	}
	return HasActivationFiles(projRoot, job.ActivationFiles, projectRoots)
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
	for _, pattern := range patterns {
		if strings.Contains(pattern, "**") {
			if matchDoubleStarGlob(projRoot, pattern, projectRoots) {
				return true
			}
		} else {
			matches, _ := filepath.Glob(filepath.Join(projRoot, pattern))
			if len(matches) > 0 {
				return true
			}
		}
	}
	return false
}

// matchDoubleStarGlob handles glob patterns containing ** (recursive match).
// Go's filepath.Glob does not support **. This function walks the directory
// tree within the owning project and matches each file against the non-**
// suffix of the pattern.
func matchDoubleStarGlob(root, pattern string, projectRoots map[string]bool) bool {
	root = filepath.Clean(root)
	// Split on "**/" or "**" to get the suffix pattern.
	// e.g. "**/*.test.ts" → suffix "*.test.ts"
	// e.g. "src/**/*.go"  → prefix "src", suffix "*.go"
	parts := strings.SplitN(pattern, "**", 2)
	prefix := strings.TrimSuffix(parts[0], string(filepath.Separator))
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
			return false
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}

	found := false
	_ = filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			if found {
				return filepath.SkipAll
			}
			return nil
		}
		if d.IsDir() {
			// Keep the starting project's own root eligible; a prefix may
			// start the walk directly at a different project's root.
			if path != root && projectRoots[filepath.Clean(path)] {
				return filepath.SkipDir
			}
			// Skip common non-project directories for performance
			if ProjectWalkExcludedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		// Match the filename against the suffix pattern
		matched, _ := filepath.Match(suffix, d.Name())
		if matched {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
