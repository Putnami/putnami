// A task cache key describes the tree a run reads. The commit, the branch, the
// line's base version and the directory the tree is checked out in are not part
// of it, except for a task that declares its output embeds the publish version.
package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
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

// treeKeyedFiles is the tree both lanes build, as workspace-relative path to
// content: a Go library, a TypeScript library that carries a feature manifest
// and a spec, a TypeScript site that depends on it, and the two paths the site
// declares as generate assets. Git ignores what a run writes.
func treeKeyedFiles() map[string]string {
	return map[string]string{
		".gitignore":                    ".gen/\n.putnami/\nnode_modules/\n",
		"api/api.go":                    "package api\n\nconst A = 1\n",
		"api/api_test.go":               "package api\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
		"api/go.mod":                    "module example.com/api\n\ngo 1.25\n",
		"docs/guide/intro.md":           "# Intro\n",
		"docs/index.md":                 "# Docs\n",
		"library/README.md":             "# Library\n",
		"library/package.json":          `{"name":"library"}` + "\n",
		"library/putnami.features.json": `{"features":[]}` + "\n",
		"library/specs/library.json":    `{"requirements":[]}` + "\n",
		"library/src/index.test.ts":     "import { a } from './index'\n",
		"library/src/index.ts":          "export const a = 1\n",
		"site/package.json":             `{"name":"site"}` + "\n",
		"site/putnami.json":             treeKeyedSiteConfig,
		"site/src/page.test.ts":         "import { page } from './page'\n",
		"site/src/page.ts":              "export const page = 'home'\n",
	}
}

// treeKeyedCommands are the four commands of the gate.
var treeKeyedCommands = []string{"test", "lint", "build", "validate"}

// treeKeyedPullRequestBuildTime and treeKeyedMainBuildTime are the build times
// of the two lanes' runs.
const (
	treeKeyedPullRequestBuildTime = "2026-10-03T15:25:06Z"
	treeKeyedMainBuildTime        = "2026-10-04T09:49:24Z"
)

// isolateTreeKeyedGit keeps the machine's Git configuration and repository
// variables away from every git process of the test, its own and the ones the
// code under test starts: no global or system config, and no GIT_DIR,
// GIT_WORK_TREE or GIT_INDEX_FILE.
func isolateTreeKeyedGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

// treeKeyedGit runs git in dir with a fixed identity and fixed dates, and
// returns its trimmed output.
func treeKeyedGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "user.name=T", "-c", "user.email=t@t.com",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
	}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z", "GIT_AUTHOR_DATE=2026-01-02T03:04:05Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// treeKeyedSquashMerge builds the history of a squash merge in a repository
// under parent, and returns the checkout of its pull request:
//   - The base commit carries the line tag v0.3.0.
//   - The pull request branch, feature/tree-keys, adds one fix.
//   - main adds an empty commit, then the squash commit: the pull request's tree
//     on another parent, with a title that calls for another bump.
//
// The pull request head adds one fix since the tag and resolves the base
// version 0.3.1. The squash title marks a breaking change, so main resolves
// 0.4.0.
func treeKeyedSquashMerge(t *testing.T, parent string) string {
	t.Helper()
	root := filepath.Join(parent, "pull-request")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	treeKeyedGit(t, root, "init", "-q", "-b", "main")
	for rel, content := range treeKeyedFiles() {
		writeFileAt(t, filepath.Join(root, filepath.FromSlash(rel)), content)
	}
	treeKeyedGit(t, root, "add", "-A")
	treeKeyedGit(t, root, "commit", "-q", "-m", "feat: the workspace")
	treeKeyedGit(t, root, "tag", "-a", "v0.3.0", "-m", "release 0.3.0")

	treeKeyedGit(t, root, "checkout", "-q", "-b", "feature/tree-keys")
	writeFileAt(t, filepath.Join(root, "library", "src", "index.ts"), "export const a = 2\n")
	treeKeyedGit(t, root, "commit", "-q", "-am", "fix(library): raise a")

	other := treeKeyedGit(t, root, "commit-tree", "main^{tree}", "-p", "main", "-m", "chore: an unrelated merge")
	squash := treeKeyedGit(t, root, "commit-tree", "HEAD^{tree}", "-p", other,
		"-m", "fix(library)!: raise a, squashed")
	treeKeyedGit(t, root, "update-ref", "refs/heads/main", squash)
	return root
}

// treeKeyedCloneMain clones the main branch of the repository at origin into a
// new checkout directory under parent.
func treeKeyedCloneMain(t *testing.T, origin, parent, name string) string {
	t.Helper()
	root := filepath.Join(parent, name, "checkout")
	treeKeyedGit(t, parent, "clone", "-q", "--branch", "main", origin, root)
	return root
}

// treeKeyedExtensions loads the shipped Go, TypeScript and SDD extensions:
// their tasks are what a run plans for lint, test, build, validate and package.
// A run records each extension's runtime digest when it prepares the runtime,
// before any key. One preparation serves both lanes, so each extension gets one
// fixed digest here, and the comparison does not read the extension's sources,
// which a concurrent build may rewrite.
func treeKeyedExtensions(t *testing.T) []*extension.ExtensionDescription {
	t.Helper()
	repoRoot := findJobsRepoRoot(t)
	var exts []*extension.ExtensionDescription
	for _, entry := range []struct{ name, dir string }{
		{"@putnami/go", "go/extension"},
		{"@putnami/typescript", "typescript/extension"},
		{"@putnami/sdd", "tooling/sdd-extension"},
	} {
		ext := extension.LoadExtensionFromDir(filepath.Join(repoRoot, filepath.FromSlash(entry.dir)), entry.name)
		if ext == nil {
			t.Fatalf("load the %s extension from %s", entry.name, entry.dir)
		}
		ext.RuntimeDigest = "prepared " + entry.name
		exts = append(exts, ext)
	}
	return exts
}

// treeKeyedLane is a run over one checkout: its workspace and the versions Git
// gives each line at the checkout's head.
type treeKeyedLane struct {
	ws       *workspace.Workspace
	versions RunVersions
}

// openTreeKeyedLane starts a run in the checkout at root: it reads the three
// projects, captures the tree state and derives the run versions from Git.
// Every project uses the SDD extension, whose validate command activates only
// where a feature manifest or a spec exists.
func openTreeKeyedLane(t *testing.T, root string) *treeKeyedLane {
	t.Helper()
	project := func(name, projectType, language string, dependencies ...string) *workspace.Project {
		return &workspace.Project{
			ID: "/" + name, Name: name, Path: name, Type: projectType,
			Extensions:   []string{language, "@putnami/sdd"},
			Dependencies: dependencies,
			Config:       wsproto.LoadProjectConfig(filepath.Join(root, name)),
		}
	}
	ws := testWorkspace(root,
		project("api", "library", "@putnami/go"),
		project("library", "", "@putnami/typescript"),
		project("site", "", "@putnami/typescript", "library"))
	snapshot, err := putnamigit.TreeState(root)
	if err != nil {
		t.Fatalf("capture the tree state of %s: %v", root, err)
	}
	versions, err := BuildRunVersions(ws, snapshot)
	if err != nil {
		t.Fatalf("a full clone degraded a version line: %v", err)
	}
	return &treeKeyedLane{ws: ws, versions: versions}
}

// keys plans commands over the lane, then keys the plan the way a run does: it
// seeds each project's build stamp for the lane's commit, then keys every
// cacheable job in one pass. A fresh CacheManager stands for a new process.
func (l *treeKeyedLane) keys(
	t *testing.T,
	exts []*extension.ExtensionDescription,
	commands []string,
	params extension.ParamMap,
	buildTime string,
) ([]*ScheduledJob, map[string]string) {
	t.Helper()
	planned, err := Plan(l.ws, commands, l.ws.Projects, exts, params, nil, nil)
	if err != nil {
		t.Fatalf("plan %v: %v", commands, err)
	}
	preserveMatchingVersionFiles(l.ws, planned, l.versions, buildTime)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(l.ws.Root, ".putnami", "store")))
	keys, err := PrecomputeKeys(l.ws, planned, params, l.versions, cache, CacheBypass{})
	if err != nil {
		t.Fatalf("precompute keys: %v", err)
	}
	return planned, keys
}

// sortedKeyNames returns the job keys of keys in order, for a failure message.
func sortedKeyNames(keys map[string]string) []string {
	names := make([]string, 0, len(keys))
	for name := range keys {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// A pull request lane and the main lane that follows its squash merge build the
// same tree at two commits, on two branches, at two base versions, in two
// checkout directories. Both lanes are Git checkouts, derive their versions
// from Git, and plan the shipped Go, TypeScript and SDD task declarations.
// Every cacheable test, lint, build
// and validate key is the same in both, so the second lane reuses what the
// first stored. The npm package task, which declares that its output embeds the
// publish version, keeps one key per commit.
//
// The test builds the projects in place of workspace discovery, gives each
// extension one fixed runtime digest, and resolves no runtime toolchain: one
// installation on one machine gives both lanes the same value for each.
func TestTwoLanesOnOneTreeShareTheirCacheKeys(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "tree-keyed-task-cache",
		"one-tree-in-two-checkouts-at-two-commits-shares-its-keys")
	spectest.Proves(t, "cli/job-planning-execution", "tree-keyed-task-cache",
		"a-version-aware-task-still-keys-on-the-commit")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	isolateTreeKeyedGit(t)
	exts := treeKeyedExtensions(t)
	parent := t.TempDir()
	origin := treeKeyedSquashMerge(t, parent)
	pullRequest := openTreeKeyedLane(t, origin)
	trunk := openTreeKeyedLane(t, treeKeyedCloneMain(t, origin, parent, "main"))

	// The lanes differ in everything a key must not read, the base version
	// included.
	head, squash := pullRequest.versions[""], trunk.versions[""]
	if head == nil || squash == nil {
		t.Fatalf("versions = %+v and %+v, want the root line in both", pullRequest.versions, trunk.versions)
	}
	if head.Base != "0.3.1" || squash.Base != "0.4.0" {
		t.Fatalf("base versions = %q and %q, want 0.3.1 and 0.4.0", head.Base, squash.Base)
	}
	if head.SHA == squash.SHA || head.Branch == squash.Branch || head.Full == squash.Full {
		t.Fatalf("the two lanes resolved one commit (%+v, %+v); the comparison would prove nothing", head, squash)
	}

	planned, first := pullRequest.keys(t, exts, treeKeyedCommands, nil, treeKeyedPullRequestBuildTime)
	_, second := trunk.keys(t, exts, treeKeyedCommands, nil, treeKeyedMainBuildTime)
	plannedProjects := map[string]bool{}
	for _, job := range planned {
		plannedProjects[job.Project.ID] = true
	}
	if !plannedProjects["/api"] || !plannedProjects["/library"] || !plannedProjects["/site"] {
		t.Fatalf("planned %d jobs over %v, want jobs for every project; keyed %v",
			len(planned), plannedProjects, sortedKeyNames(first))
	}

	for _, project := range []string{"api", "library", "site"} {
		before := readVersionStamp(t, filepath.Join(pullRequest.ws.Root, project))
		after := readVersionStamp(t, filepath.Join(trunk.ws.Root, project))
		if before.SHA == after.SHA || before.Suffix == after.Suffix || before.Version == after.Version ||
			before.Branch == after.Branch || before.BuildTime == after.BuildTime {
			t.Fatalf("%s: the two lanes stamped one identity (%+v, %+v); the comparison would prove nothing", project, before, after)
		}
		// Git computes each package's source binding from the checkout, and
		// the two checkouts hold one tree.
		for _, pkg := range before.CapabilityPackages {
			if pkg.SourceBinding == "" || pkg.SourceBindingUnavailable {
				t.Fatalf("%s: package %s carries no source binding; Git did not read the checkout", project, pkg.Package)
			}
		}
		if len(before.CapabilityPackages) == 0 || !reflect.DeepEqual(before.CapabilityPackages, after.CapabilityPackages) {
			t.Errorf("%s: the two checkouts stamped different packages (%+v, %+v)",
				project, before.CapabilityPackages, after.CapabilityPackages)
		}
	}

	if !reflect.DeepEqual(sortedKeyNames(first), sortedKeyNames(second)) {
		t.Fatalf("the two lanes keyed different jobs:\n%v\n%v", sortedKeyNames(first), sortedKeyNames(second))
	}
	keyedCommands, keyedProjects := map[string]int{}, map[string]int{}
	for _, job := range planned {
		key := job.Key()
		hash, keyed := first[key]
		if !keyed {
			continue
		}
		command, _ := jobCommandAndStep(job.JobDef.Name)
		keyedCommands[command]++
		keyedProjects[job.Project.ID]++
		if second[key] != hash {
			t.Errorf("%s: the two lanes keyed one tree differently (%s != %s)", key, hash, second[key])
		}
	}
	for _, command := range treeKeyedCommands {
		if keyedCommands[command] == 0 {
			t.Errorf("no %s task was keyed in %v; the comparison does not cover the command", command, sortedKeyNames(first))
		}
	}
	for _, project := range []string{"/api", "/library", "/site"} {
		if keyedProjects[project] == 0 {
			t.Errorf("no task of %s was keyed in %v; the comparison does not cover the project", project, sortedKeyNames(first))
		}
	}

	t.Run("a version-aware task keys on the commit", func(t *testing.T) {
		packageParams := extension.ParamMap{"npm": true}
		packaged, atHead := pullRequest.keys(t, exts, []string{"package"}, packageParams, treeKeyedPullRequestBuildTime)
		_, atSquash := trunk.keys(t, exts, []string{"package"}, packageParams, treeKeyedMainBuildTime)
		versionAware := 0
		for _, job := range packaged {
			key := job.Key()
			hash, keyed := atHead[key]
			if !keyed {
				continue
			}
			if !taskIsVersionAware(job) {
				if atSquash[key] != hash {
					t.Errorf("%s: the two lanes keyed one tree differently (%s != %s)", key, hash, atSquash[key])
				}
				continue
			}
			versionAware++
			if atSquash[key] == hash {
				t.Errorf("%s: a version-aware task kept one key at two commits; it would serve an artifact that embeds the other commit's version", key)
			}
		}
		if versionAware == 0 {
			t.Fatalf("no version-aware task was keyed in %v", sortedKeyNames(atHead))
		}
	})

	// The controls. Each one keys a fresh clone of main, changes one thing in
	// it, and keys it again: equal keys above mean nothing unless a key still
	// reads the stamp and the asset content.
	t.Run("a tree-describing stamp field moves the keys that read the stamp", func(t *testing.T) {
		lane := openTreeKeyedLane(t, treeKeyedCloneMain(t, origin, parent, "stamp"))
		_, unmerged := lane.keys(t, exts, treeKeyedCommands, nil, treeKeyedMainBuildTime)
		for _, project := range []string{"library", "site"} {
			mergeStampField(t, filepath.Join(lane.ws.Root, project), "contentHash", producedContentHash)
		}
		_, merged := lane.keys(t, exts, treeKeyedCommands, nil, treeKeyedMainBuildTime)
		for _, key := range []string{"/library:lint~format", "/library:lint~check", "/site:lint~format", "/site:lint~check"} {
			if _, keyed := unmerged[key]; !keyed {
				t.Fatalf("%s is not keyed in %v; the control reads no stamp", key, sortedKeyNames(unmerged))
			}
			if merged[key] == unmerged[key] {
				t.Errorf("%s: a new contentHash in the build stamp kept the key; the task's patterns do not reach the stamp", key)
			}
		}
	})

	t.Run("an edit under a generate asset moves the keys of the project that declares it", func(t *testing.T) {
		root := treeKeyedCloneMain(t, origin, parent, "asset-content")
		_, clean := openTreeKeyedLane(t, root).keys(t, exts, treeKeyedCommands, nil, treeKeyedMainBuildTime)
		writeFileAt(t, filepath.Join(root, "docs", "guide", "intro.md"), "# Intro, edited\n")
		// The edit leaves the checkout dirty, so the run stamps another
		// identity; only the edited content may move a key.
		dirty := openTreeKeyedLane(t, root)
		if version := dirty.versions[""]; version == nil || !version.IsDirty {
			t.Fatalf("version after the edit = %+v, want a dirty checkout", version)
		}
		planned, edited := dirty.keys(t, exts, treeKeyedCommands, nil, treeKeyedMainBuildTime)
		// A task that keys on the workspace's Git candidate cut reads the edited
		// file, whichever project declares it: lint-docs checks links into it.
		readsTheCut := map[string]bool{}
		for _, job := range planned {
			readsTheCut[job.Key()] = keysOnCandidateCut(job)
		}
		cutReaders := 0
		for _, key := range sortedKeyNames(clean) {
			if readsTheCut[key] {
				cutReaders++
			}
			switch {
			case (strings.HasPrefix(key, "/site:") || readsTheCut[key]) && edited[key] == clean[key]:
				t.Errorf("%s: an edited file the task reads kept the key", key)
			case (strings.HasPrefix(key, "/api:") || strings.HasPrefix(key, "/library:")) && !readsTheCut[key] && edited[key] != clean[key]:
				t.Errorf("%s: another project's generate asset moved the key", key)
			}
		}
		if cutReaders == 0 {
			t.Fatalf("no task keyed on the candidate cut in %v; the control reads no workspace input", sortedKeyNames(clean))
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
	versions := rootLineVersions(&JobContextVersion{
		Base: "0.3.1", Full: "0.3.1-20261003152506-4da6833", Suffix: "20261003152506-4da6833",
		SHA: "4da6833c0ffee0123456789abcdef0123456789a", Branch: "feature/tree-keys",
	})
	generateVersionFilesAt(ws, []*ScheduledJob{job}, versions, treeKeyedPullRequestBuildTime, false)
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
