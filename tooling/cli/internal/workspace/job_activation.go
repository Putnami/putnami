package workspace

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	return matchUnderWitness(root, searchRoot, suffix, projectRoots)
}

// activationWitnesses holds, for the life of the process, each recursive
// activation match with its witness (CLI ADR 0116 §6): a found match by the
// stats of the matching file and of the directories down to it, a missing one
// by the stats of every directory its walk listed. A machine worker answers
// an unchanged plan's matches with a stat per path instead of a walk; a stat
// that differs walks again.
var activationWitnesses sync.Map // searchRoot NUL suffix NUL root NUL project roots → *activationWitness

// activationWalks counts the walks matchUnderWitness made, for tests.
var activationWalks atomic.Int64

type activationWitness struct {
	found bool
	paths map[string]fileindex.Stat
}

func (w *activationWitness) holds() bool {
	for path, recorded := range w.paths {
		if now, ok := fileindex.LstatAny(path); !ok || now != recorded {
			return false
		}
	}
	return true
}

// settled reports whether every recorded time lies more than the file
// index's margin before start, the clock read before the first stat (ADR
// 0009 §2 of protocols/workspace).
func (w *activationWitness) settled(start time.Time) bool {
	limit := start.Add(-fileindex.Margin).UnixNano()
	for _, st := range w.paths {
		if st.ModNano >= limit || st.ChangeNano >= limit {
			return false
		}
	}
	return true
}

// matchUnderWitness walks searchRoot, below root, for a file whose name
// matches suffix, entering no excluded directory and no other project's root.
// The answer is held under its witness when the file system's stats answer
// for the tree.
func matchUnderWitness(root, searchRoot, suffix string, projectRoots map[string]bool) bool {
	key := searchRoot + "\x00" + suffix + "\x00" + root + "\x00" + nestedRootsKey(root, projectRoots)
	if held, ok := activationWitnesses.Load(key); ok {
		if w, typed := held.(*activationWitness); typed && w.holds() {
			return w.found
		}
	}
	// The clock is read before the first stat (ADR 0009 §2 of
	// protocols/workspace); the walk's paths are held only on the root's
	// device, which TrustsStats vouches for.
	// Round(0) drops the monotonic reading, so the comparisons below see a
	// wall clock that went back.
	start := time.Now().Round(0)
	rootStat, rootOK := fileindex.LstatAny(root)
	trusted := rootOK && fileindex.TrustsStats(root)
	walked := map[string]fileindex.Stat{}
	found, match := false, ""
	activationWalks.Add(1)
	_ = filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			trusted = false // an unreadable member the witness cannot hold
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
			if trusted {
				if st, ok := fileindex.LstatAny(path); ok && st.Dev == rootStat.Dev {
					walked[path] = st
				} else {
					trusted = false
				}
			}
			return nil
		}
		// Match the filename against the suffix pattern
		if matched, _ := filepath.Match(suffix, d.Name()); matched {
			found, match = true, path
			return filepath.SkipAll
		}
		return nil
	})
	// A walk that did not list its search root (a missing prefix, or one
	// that is not a directory) has nothing to hold its answer by.
	if _, listed := walked[searchRoot]; !trusted || !listed || time.Now().Round(0).Before(start) {
		activationWitnesses.Delete(key)
		return found
	}
	w := &activationWitness{found: found, paths: map[string]fileindex.Stat{}}
	if found {
		// The match holds while it and the directories down to it hold;
		// the directories the walk listed before reaching them do not
		// matter.
		st, ok := fileindex.LstatAny(match)
		if !ok || st.Dev != rootStat.Dev {
			activationWitnesses.Delete(key)
			return found
		}
		w.paths[match] = st
		for dir := filepath.Dir(match); ; dir = filepath.Dir(dir) {
			before, listed := walked[dir]
			if !listed {
				activationWitnesses.Delete(key)
				return found
			}
			w.paths[dir] = before
			if dir == searchRoot {
				break
			}
		}
	} else {
		w.paths = walked
	}
	// A directory whose stat moved during the walk leaves the answer
	// unheld.
	for path, before := range w.paths {
		if after, ok := fileindex.LstatAny(path); !ok || after != before {
			activationWitnesses.Delete(key)
			return found
		}
	}
	if w.settled(start) {
		activationWitnesses.Store(key, w)
	} else {
		activationWitnesses.Delete(key)
	}
	return found
}

// nestedRootsKey is the set of project roots below root, which a walk under
// root skips, as a comparable string.
func nestedRootsKey(root string, projectRoots map[string]bool) string {
	var nested []string
	for dir := range projectRoots {
		if strings.HasPrefix(dir, root+string(filepath.Separator)) {
			nested = append(nested, dir)
		}
	}
	sort.Strings(nested)
	return strings.Join(nested, "\x01")
}
