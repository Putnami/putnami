package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// releaseBaselineBuildTime is the build time of every run of these tests: the
// API check does not read the stamp.
const releaseBaselineBuildTime = "2026-10-05T10:00:00Z"

// releaseBaselineHistory builds a repository under parent whose api project
// breaks its API after the line tag v0.3.0, and returns the checkout of the
// pull request that breaks it:
//   - The base commit holds the tree-keyed workspace and carries v0.3.0.
//   - The pull request branch, feature/drop-a, replaces the constant A in two
//     commits, the first of which declares the break.
//   - main holds the squash commit: the pull request's tree on the base, with
//     the pull request's title, which declares the break.
func releaseBaselineHistory(t *testing.T, parent string) string {
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

	treeKeyedGit(t, root, "checkout", "-q", "-b", "feature/drop-a")
	writeFileAt(t, filepath.Join(root, "api", "api.go"), "package api\n\nconst B = 2\n")
	treeKeyedGit(t, root, "commit", "-q", "-am", "feat(api)!: replace A with B")
	writeFileAt(t, filepath.Join(root, "api", "api_test.go"), "package api\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n")
	treeKeyedGit(t, root, "commit", "-q", "-am", "test(api): cover B")

	squash := treeKeyedGit(t, root, "commit-tree", "HEAD^{tree}", "-p", "main",
		"-m", "feat(api)!: replace A with B (#7)")
	treeKeyedGit(t, root, "update-ref", "refs/heads/main", squash)
	return root
}

// openReleaseBaselineLane is openTreeKeyedLane for a checkout whose version
// lines may degrade, such as a shallow clone: the API check's key reads no
// version.
func openReleaseBaselineLane(t *testing.T, root string) *treeKeyedLane {
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
	versions, _ := BuildRunVersions(ws, snapshot)
	return &treeKeyedLane{ws: ws, versions: versions}
}

// apiCheckKey plans validate over the lane and returns the key of the api
// project's API check.
func apiCheckKey(t *testing.T, lane *treeKeyedLane, exts []*extension.ExtensionDescription) string {
	t.Helper()
	planned, keys := lane.keys(t, exts, []string{"validate"}, nil, releaseBaselineBuildTime)
	for _, job := range planned {
		if job.Project.ID != "/api" || job.Step.Task != "validate-api" {
			continue
		}
		key, keyed := keys[job.Key()]
		if !keyed || key == "" {
			t.Fatalf("%s is planned but not keyed in %v", job.Key(), sortedKeyNames(keys))
		}
		return key
	}
	t.Fatalf("no validate-api job for /api among %v", sortedKeyNames(keys))
	return ""
}

// headOf returns the commit HEAD names in the checkout at root.
func headOf(t *testing.T, root string) string {
	t.Helper()
	return treeKeyedGit(t, root, "rev-parse", "HEAD")
}

// The API check's verdict reads the line's last tag, the tree that tag holds at
// the project, and whether a commit since it declares a break. Its key reads
// the same, and no commit: a pull request and its squash merge share the key,
// and each input of the verdict moves it.
func TestTheAPICheckKeysOnTheReleaseBaselineNotTheCommit(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-baseline-cache-key",
		"a-release-baseline-keys-on-the-line-tag-and-the-breaking-marker")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	isolateTreeKeyedGit(t)
	exts := treeKeyedExtensions(t)
	parent := t.TempDir()
	origin := releaseBaselineHistory(t, parent)

	trunkRoot := treeKeyedCloneMain(t, origin, parent, "main")
	if headOf(t, origin) == headOf(t, trunkRoot) {
		t.Fatal("the pull request and main share a HEAD commit; the comparison would prove nothing")
	}
	if a, b := treeKeyedGit(t, origin, "rev-parse", "HEAD^{tree}"), treeKeyedGit(t, trunkRoot, "rev-parse", "HEAD^{tree}"); a != b {
		t.Fatalf("the pull request and its squash hold different trees (%s, %s)", a, b)
	}
	pullRequest := apiCheckKey(t, openReleaseBaselineLane(t, origin), exts)
	trunk := apiCheckKey(t, openReleaseBaselineLane(t, trunkRoot), exts)
	if pullRequest != trunk {
		t.Fatalf("the pull request and its squash merge keyed the API check differently (%s != %s)", pullRequest, trunk)
	}

	// Each control keys a fresh clone of main, changes one thing in it, and
	// keys it again.
	control := func(name string, change func(t *testing.T, root string), moves bool) {
		t.Run(name, func(t *testing.T) {
			root := treeKeyedCloneMain(t, origin, parent, strings.ReplaceAll(name, " ", "-"))
			before := apiCheckKey(t, openReleaseBaselineLane(t, root), exts)
			if before != trunk {
				t.Fatalf("a clone of main keyed the API check %s, want main's %s", before, trunk)
			}
			change(t, root)
			after := apiCheckKey(t, openReleaseBaselineLane(t, root), exts)
			if moves && after == before {
				t.Errorf("%s kept the API check's key", name)
			}
			if !moves && after != before {
				t.Errorf("%s moved the API check's key", name)
			}
		})
	}

	control("a commit that touches nothing", func(t *testing.T, root string) {
		treeKeyedGit(t, root, "commit", "-q", "--allow-empty", "-m", "chore: nothing")
	}, false)
	control("a break declared outside the project", func(t *testing.T, root string) {
		writeFileAt(t, filepath.Join(root, "docs", "index.md"), "# Docs, edited\n")
		treeKeyedGit(t, root, "commit", "-q", "-am", "docs!: rewrite the docs")
		writeFileAt(t, filepath.Join(root, "docs", "index.md"), "# Docs\n")
		treeKeyedGit(t, root, "commit", "-q", "-am", "docs: restore the docs")
	}, false)
	control("the same break declared by a footer", func(t *testing.T, root string) {
		treeKeyedGit(t, root, "commit", "-q", "--amend", "-m", "refactor(api): replace A with B",
			"-m", "BREAKING CHANGE: A is gone")
	}, false)
	control("a dropped breaking marker", func(t *testing.T, root string) {
		treeKeyedGit(t, root, "commit", "-q", "--amend", "-m", "feat(api): replace A with B")
	}, true)
	control("a new tag", func(t *testing.T, root string) {
		treeKeyedGit(t, root, "tag", "-a", "v0.4.0", "-m", "release 0.4.0")
	}, true)
	control("other content at the tag", func(t *testing.T, root string) {
		// v0.3.0 moves to a commit whose api differs from the base, which main
		// then merges without taking any of its content: the tag keeps its
		// name, HEAD keeps its tree, and the break since the tag stays declared.
		treeKeyedGit(t, root, "checkout", "-q", "-b", "restage", "v0.3.0")
		writeFileAt(t, filepath.Join(root, "api", "api.go"), "package api\n\nconst A = 0\n")
		treeKeyedGit(t, root, "commit", "-q", "-am", "chore(api): restage")
		treeKeyedGit(t, root, "tag", "-f", "-a", "v0.3.0", "-m", "release 0.3.0, restaged")
		treeKeyedGit(t, root, "checkout", "-q", "main")
		treeKeyedGit(t, root, "merge", "-q", "-s", "ours", "-m", "chore: keep main", "restage")
	}, true)

	t.Run("a shallow clone", func(t *testing.T) {
		root := filepath.Join(parent, "shallow", "checkout")
		treeKeyedGit(t, parent, "clone", "-q", "--depth", "1", "--branch", "main", "file://"+origin, root)
		if shallow := treeKeyedGit(t, root, "rev-parse", "--is-shallow-repository"); shallow != "true" {
			t.Fatalf("the clone is not shallow (%s); the control proves nothing", shallow)
		}
		if key := apiCheckKey(t, openReleaseBaselineLane(t, root), exts); key == trunk {
			t.Errorf("a shallow clone of main kept the full clone's key %s", key)
		}
	})
}
