package jobs

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// interactiveTaskMarker is the sentence a manifest gives a task that a CLI
// subcommand runs on the terminal's own stdout: the CLI captures nothing, so a
// cache entry would have nothing to replay.
const interactiveTaskMarker = "Interactive: the CLI runs it with the terminal's own stdout"

// uncachedByVerdict names the tasks whose verdict is about the commit or the
// invocation itself, so a stored verdict from another one would be wrong.
var uncachedByVerdict = map[string]string{
	"version-report":     "its output is the commit's identity: sha, branch and dirty bit",
	"sdd-selfcheck-exec": "its verdict is the environment of this invocation",
}

// Every task of this repository's extension manifests is cached unless its run
// has an effect a cache entry cannot replay (#82): a declared effect, the
// terminal of an interactive subcommand, or a verdict about the commit or the
// invocation itself. A task that turns its cache off for any other reason
// re-executes on every lane and every run, whatever its inputs.
func TestEveryUncachedTaskHasAnEffectACacheEntryCannotReplay(t *testing.T) {
	t.Parallel()
	root := findJobsRepoRoot(t)

	var manifests []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "dist" || name == "out" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == "putnami.extension.json" {
			manifests = append(manifests, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(manifests) == 0 {
		t.Fatalf("no extension manifest under %s", root)
	}

	var offenders []string
	uncached := 0
	for _, path := range manifests {
		manifest, err := extension.LoadManifest(path)
		if err != nil {
			t.Fatalf("LoadManifest(%s): %v", path, err)
		}
		relative, _ := filepath.Rel(root, path)
		for name, task := range manifest.Tasks {
			if task.Cache.IsEnabled() {
				continue
			}
			uncached++
			switch {
			case task.Declares != nil && len(task.Declares.Effects) > 0:
			case strings.Contains(task.Description, interactiveTaskMarker):
			case uncachedByVerdict[name] != "":
			default:
				offenders = append(offenders, relative+": "+name)
			}
		}
	}
	if uncached == 0 {
		t.Fatalf("no uncached task in %d manifests; the walk reads nothing", len(manifests))
	}
	sort.Strings(offenders)
	for _, offender := range offenders {
		t.Errorf("%s turns its cache off with no effect a cache entry cannot replay: give it a key, or declare the effect", offender)
	}
}
