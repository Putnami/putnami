// A task cache key describes the tree a run reads. The commit, the branch and
// the directory the tree is checked out in are not part of it, except for a
// task that declares its output embeds the publish version.
package jobs

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// treeKeyedSiteConfig is the site's project config: it declares two generate
// assets, the workspace's docs directory and the library's README.
const treeKeyedSiteConfig = `{
  "name": "site",
  "dependencies": ["library"],
  "options": {
    "generate": {
      "assets": [
        { "from": "/docs", "to": "public/docs" },
        { "from": "../library/README.md", "to": "public/library.md" }
      ]
    }
  }
}
`

// treeKeyedFiles is the fixture tree, as workspace-relative path to content: a
// library, a site that depends on it, and the two paths the site declares as
// generate assets.
func treeKeyedFiles() map[string]string {
	return map[string]string{
		"docs/guide/intro.md":  "# Intro\n",
		"docs/index.md":        "# Docs\n",
		"library/README.md":    "# Library\n",
		"library/package.json": `{"name":"library"}` + "\n",
		"library/src/index.ts": "export const a = 1\n",
		"site/package.json":    `{"name":"site"}` + "\n",
		"site/putnami.json":    treeKeyedSiteConfig,
		"site/src/page.ts":     "export const page = 'home'\n",
	}
}

// treeKeyedCommands are the four commands of the gate. Each is planned as one
// job per project, on patterns that reach the project's build stamp the way the
// TypeScript lint's **/*.json does.
var treeKeyedCommands = []string{"test", "lint", "build", "validate"}

// treeKeyedPackageJob is the name of the fixture's version-aware job: a package
// step whose artifact carries the publish version.
const treeKeyedPackageJob = "package"

// treeKeyedLane is one checkout of the fixture tree: a workspace root, the plan
// a run builds over it, and the commit the run builds it at.
type treeKeyedLane struct {
	ws       *workspace.Workspace
	planned  []*ScheduledJob
	versions RunVersions
}

// checkOutTreeKeyedLane writes files under root and plans the lane's jobs: the
// four gate commands for the library and for the site, the site's after the
// library's build, and the library's version-aware package job.
func checkOutTreeKeyedLane(t *testing.T, root string, files map[string]string, versions RunVersions) *treeKeyedLane {
	t.Helper()
	for rel, content := range files {
		writeFileAt(t, filepath.Join(root, filepath.FromSlash(rel)), content)
	}

	siteConfig := &wsproto.ProjectConfig{}
	if err := json.Unmarshal([]byte(files["site/putnami.json"]), siteConfig); err != nil {
		t.Fatalf("decode the site's project config: %v", err)
	}
	library := &workspace.Project{ID: "/library", Name: "library", Path: "library"}
	site := &workspace.Project{
		ID: "/site", Name: "site", Path: "site",
		Dependencies: []string{"library"},
		Config:       siteConfig,
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "fixture"}, []*workspace.Project{library, site})

	ext := &extension.ExtensionDescription{
		Name:  "@test/ext",
		Path:  t.TempDir(),
		Tasks: map[string]extension.TaskDefinition{},
	}
	newJob := func(project *workspace.Project, name string, policy *extension.TaskCachePolicy, deps ...string) *ScheduledJob {
		ext.Tasks[name] = extension.TaskDefinition{Declares: &extension.TaskDeclaration{}}
		return &ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &extension.PipelineStep{Task: name},
			JobDef: &extension.JobDefinition{
				Name:            name,
				CommandName:     name,
				ExtensionName:   "@test/ext",
				Cache:           true,
				FilePatterns:    []string{"**/*.ts", "**/*.json"},
				TaskCachePolicy: policy,
			},
			DependsOn: deps,
		}
	}

	lane := &treeKeyedLane{ws: ws, versions: versions}
	var libraryBuild string
	for _, command := range treeKeyedCommands {
		job := newJob(library, command, nil)
		if command == "build" {
			libraryBuild = job.Key()
		}
		lane.planned = append(lane.planned, job)
	}
	for _, command := range treeKeyedCommands {
		lane.planned = append(lane.planned, newJob(site, command, nil, libraryBuild))
	}
	lane.planned = append(lane.planned,
		newJob(library, treeKeyedPackageJob, &extension.TaskCachePolicy{VersionAware: true}, libraryBuild))
	return lane
}

// keys computes the key of every planned job the way a run does: it seeds each
// project's build stamp for the lane's commit, then keys the plan in one pass. A
// fresh CacheManager per call stands for a new process.
func (l *treeKeyedLane) keys(t *testing.T, buildTime string) map[string]string {
	t.Helper()
	preserveMatchingVersionFiles(l.ws, l.planned, l.versions, buildTime)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(l.ws.Root, ".putnami", "store")))
	keys, err := PrecomputeKeys(l.ws, l.planned, nil, l.versions, cache, CacheBypass{})
	if err != nil {
		t.Fatalf("precompute keys: %v", err)
	}
	if len(keys) != len(l.planned) {
		t.Fatalf("keyed %d of %d planned jobs", len(keys), len(l.planned))
	}
	return keys
}

// treeKeyedPullRequest and treeKeyedMain are the two commits of the squash
// merge: one tree, one line base version, and a different sha, suffix, publish
// version and branch.
func treeKeyedPullRequest() RunVersions {
	return rootLineVersions(&JobContextVersion{
		Base: "0.3.1", Full: "0.3.1-20261003152506-4da6833", Suffix: "20261003152506-4da6833",
		SHA: "4da6833c0ffee0123456789abcdef0123456789a", Branch: "feature/tree-keys",
	})
}

func treeKeyedMain() RunVersions {
	return rootLineVersions(&JobContextVersion{
		Base: "0.3.1", Full: "0.3.1-20261004094924-a389c95", Suffix: "20261004094924-a389c95",
		SHA: "a389c95f00dfeed0123456789abcdef012345678", Branch: "main",
	})
}

// A pull request lane and the main lane that follows its squash merge build the
// same tree at two commits, on two branches, in two checkout directories. Every
// test, lint, build and validate key is the same in both, so the second lane
// reuses what the first stored. The task that declares its output embeds the
// publish version keeps one key per commit.
func TestTwoLanesOnOneTreeShareTheirCacheKeys(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "tree-keyed-task-cache",
		"one-tree-in-two-checkouts-at-two-commits-shares-its-keys")
	spectest.Proves(t, "cli/job-planning-execution", "tree-keyed-task-cache",
		"a-version-aware-task-still-keys-on-the-commit")
	// Both lanes are repository checkouts. The fixture directories are not, so
	// the source state is pinned and Git discovery stops below their parent.
	keyAsSourceState(t, "")
	parent := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", parent)

	pullRequest := checkOutTreeKeyedLane(t,
		filepath.Join(parent, "pull-request", "workspace"), treeKeyedFiles(), treeKeyedPullRequest())
	trunk := checkOutTreeKeyedLane(t,
		filepath.Join(parent, "main", "checkout"), treeKeyedFiles(), treeKeyedMain())

	first := pullRequest.keys(t, "2026-10-03T15:25:06Z")
	second := trunk.keys(t, "2026-10-04T09:49:24Z")

	// The lanes really differ in everything a key must not read.
	for _, project := range []string{"library", "site"} {
		before := readVersionStamp(t, filepath.Join(pullRequest.ws.Root, project))
		after := readVersionStamp(t, filepath.Join(trunk.ws.Root, project))
		if before.SHA == after.SHA || before.Suffix == after.Suffix || before.Version == after.Version ||
			before.Branch == after.Branch || before.BuildTime == after.BuildTime {
			t.Fatalf("%s: the two lanes stamped one identity (%+v, %+v); the comparison would prove nothing", project, before, after)
		}
	}

	var compared []string
	for _, job := range pullRequest.planned {
		key := job.Key()
		if job.JobDef.Name == treeKeyedPackageJob {
			if first[key] == second[key] {
				t.Errorf("%s: a version-aware task kept one key at two commits; it would serve an artifact that embeds the other commit's version", key)
			}
			continue
		}
		compared = append(compared, key)
		if first[key] != second[key] {
			t.Errorf("%s: the two lanes keyed one tree differently (%s != %s)", key, first[key], second[key])
		}
	}
	sort.Strings(compared)
	want := []string{
		"/library:build", "/library:lint", "/library:test", "/library:validate",
		"/site:build", "/site:lint", "/site:test", "/site:validate",
	}
	if len(compared) != len(want) {
		t.Fatalf("compared %v, want %v", compared, want)
	}
	for i := range want {
		if compared[i] != want[i] {
			t.Fatalf("compared %v, want %v", compared, want)
		}
	}

	// The controls. Each lane below is a fresh checkout of a tree that differs
	// from the fixture in one place, at the main commit: equal keys above mean
	// nothing unless a key still reads the stamp and the asset content.
	controlKeys := func(name string, files map[string]string, prepare func(lane *treeKeyedLane)) map[string]string {
		t.Helper()
		lane := checkOutTreeKeyedLane(t, filepath.Join(parent, name, "checkout"), files, treeKeyedMain())
		// A run seeds the stamp before anything merges into it.
		preserveMatchingVersionFiles(lane.ws, lane.planned, lane.versions, "2026-10-04T09:49:24Z")
		if prepare != nil {
			prepare(lane)
		}
		return lane.keys(t, "2026-10-04T09:49:24Z")
	}

	t.Run("a tree-describing stamp field moves every key that reads the stamp", func(t *testing.T) {
		keys := controlKeys("stamp", treeKeyedFiles(), func(lane *treeKeyedLane) {
			mergeStampField(t, filepath.Join(lane.ws.Root, "library"), "contentHash", producedContentHash)
			mergeStampField(t, filepath.Join(lane.ws.Root, "site"), "contentHash", producedContentHash)
		})
		for _, key := range want {
			if keys[key] == second[key] {
				t.Errorf("%s: a new contentHash in the build stamp kept the key; the job's patterns do not reach the stamp", key)
			}
		}
	})

	t.Run("an edit under a generate asset moves the keys of the project that declares it", func(t *testing.T) {
		files := treeKeyedFiles()
		files["docs/guide/intro.md"] = "# Intro, edited\n"
		keys := controlKeys("asset-content", files, nil)
		for _, command := range treeKeyedCommands {
			if key := "/site:" + command; keys[key] == second[key] {
				t.Errorf("%s: an edited generate asset kept the key", key)
			}
			if key := "/library:" + command; keys[key] != second[key] {
				t.Errorf("%s: another project's generate asset moved the key", key)
			}
		}
	})
}

// The scheduler owns a closed set of stamp fields, and each is either a
// description of the tree, which a key reads, or a name for the commit or the
// invocation, which it does not. A field added to the stamp fails here until it
// is classified.
func TestStampFieldsThatReachACacheKey(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "tree-keyed-task-cache",
		"the-build-stamp-keys-without-its-commit-fields")
	t.Parallel()

	fields := []struct {
		name string
		// changed differs from whatever the scheduler wrote, in the field's own
		// JSON type.
		changed          any
		describesTheTree bool
	}{
		{"name", "renamed", true},
		{"capabilityRoot", "another/root", true},
		{"capabilityPackages", []CapabilityPackageStamp{{Package: "another", Version: "9.9.9"}}, true},
		{"version", "0.3.1-20261004094924-a389c95", false},
		{"suffix", "20261004094924-a389c95", false},
		{"sha", "a389c95f00dfeed0123456789abcdef012345678", false},
		{"branch", "main", false},
		{"isDirty", true, false},
		{"buildTime", "2026-10-04T09:49:24Z", false},
	}
	classified := make(map[string]bool, len(fields))
	for _, field := range fields {
		classified[field.name] = true
		if !versionStampOwnedFields[field.name] {
			t.Errorf("classified field %q is not a field the scheduler writes to the stamp", field.name)
		}
	}
	for field := range versionStampOwnedFields {
		if !classified[field] {
			t.Errorf("stamp field %q is not classified as describing the tree or naming the commit", field)
		}
	}

	ws := makeExecutorTestWorkspace(t)
	projectRoot := filepath.Join(ws.Root, "proj")
	stampPath := filepath.Join(projectRoot, filepath.FromSlash(versionStampRelPath))
	job := cacheableJob("lint", "/proj", "proj", "proj")
	generateVersionFilesAt(ws, []*ScheduledJob{job}, treeKeyedPullRequest(), "2026-10-03T15:25:06Z", false)
	base := stampKeyInput(t, ws, "proj")

	original := readFileAt(t, stampPath)
	for _, field := range fields {
		mergeStampField(t, projectRoot, field.name, field.changed)
		if moved := stampKeyInput(t, ws, "proj") != base; moved != field.describesTheTree {
			t.Errorf("stamp field %q: moved the key input = %v, want %v", field.name, moved, field.describesTheTree)
		}
		writeFileAt(t, stampPath, original)
	}
}
