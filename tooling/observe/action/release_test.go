package action

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/yaml"
)

// git runs git in dir with a fixed identity and returns its trimmed output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitCommand(dir, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=release", "GIT_AUTHOR_EMAIL=release@example.com",
		"GIT_COMMITTER_NAME=release", "GIT_COMMITTER_EMAIL=release@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	)
	return cmd
}

// actionRepository makes a bare origin holding one commit on main, with a
// file the release must remove, and returns the origin and a clone of it.
func actionRepository(t *testing.T) (origin, clone string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the release runs on a Linux runner")
	}
	dir := t.TempDir()
	origin = filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	clone = filepath.Join(dir, "clone")
	git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	git(t, dir, "init", "--quiet", "--initial-branch=main", seed)
	write(t, filepath.Join(seed, "README.md"), "# observe-action\n", 0o644)
	if err := os.MkdirAll(filepath.Join(seed, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(seed, ".github", "workflows", "old.yml"), "name: old\n", 0o644)
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "--quiet", "-m", "Initial commit")
	git(t, seed, "push", "--quiet", origin, "main")
	git(t, dir, "clone", "--quiet", origin, clone)
	return origin, clone
}

func release(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"release.sh"}, args...)...)
	cmd.Env = gitCommand(".").Env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestAReleaseCopiesTheActionToTheRepositoryRoot(t *testing.T) {
	spectest.Proves(t, feature, "action-release", "a-release-copies-the-action-to-the-repository-root")

	origin, clone := actionRepository(t)
	if out, err := release(t, "1.0.0", clone, "Putnami/putnami@abc123"); err != nil {
		t.Fatalf("release.sh: %v\n%s", err, out)
	}

	files := strings.Split(git(t, origin, "ls-tree", "-r", "--name-only", "main"), "\n")
	if want := []string{"LICENSE.md", "README.md", "action.yml", "observe.sh"}; !slices.Equal(files, want) {
		t.Fatalf("main holds %q, want %q", files, want)
	}
	for _, name := range []string{"action.yml", "observe.sh", "README.md"} {
		local, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := git(t, origin, "show", "main:"+name); got != strings.TrimSpace(string(local)) {
			t.Fatalf("main:%s differs from the action directory", name)
		}
	}
	if mode := git(t, origin, "ls-tree", "main", "observe.sh"); !strings.HasPrefix(mode, "100755 ") {
		t.Fatalf("observe.sh is not executable: %s", mode)
	}
	if message := git(t, origin, "log", "-1", "--format=%B", "main"); message != "Release v1.0.0\n\nSource: Putnami/putnami@abc123" {
		t.Fatalf("commit message = %q", message)
	}
}

func TestAReleaseTagsTheVersionAndMovesTheMajorTag(t *testing.T) {
	spectest.Proves(t, feature, "action-release", "a-release-tags-the-version-and-moves-the-major-tag")

	origin, clone := actionRepository(t)
	for _, version := range []string{"1.0.0", "1.1.0"} {
		if out, err := release(t, version, clone); err != nil {
			t.Fatalf("release.sh %s: %v\n%s", version, err, out)
		}
		head := git(t, origin, "rev-parse", "main")
		for _, tag := range []string{"v" + version, "v1"} {
			if got := git(t, origin, "rev-parse", tag+"^{commit}"); got != head {
				t.Fatalf("after %s, %s = %s, want main %s", version, tag, got, head)
			}
		}
	}
	if first, second := git(t, origin, "rev-parse", "v1.0.0"), git(t, origin, "rev-parse", "v1.1.0"); first == second {
		t.Fatal("v1.0.0 moved with v1")
	}
}

func TestAReleaseRefusesABadOrExistingVersion(t *testing.T) {
	spectest.Proves(t, feature, "action-release", "a-release-refuses-a-bad-or-existing-version")

	origin, clone := actionRepository(t)
	before := git(t, origin, "rev-parse", "main")
	for _, version := range []string{"1.0", "v1.0.0", "1.0.0-rc.1", "01.0.0", "1.0.0+build"} {
		out, err := release(t, version, clone)
		if err == nil {
			t.Fatalf("release.sh %s passed:\n%s", version, out)
		}
		if !strings.Contains(out, "is not X.Y.Z") {
			t.Fatalf("release.sh %s: message %q does not say X.Y.Z", version, out)
		}
	}
	if out, err := release(t, "1.0.0", clone); err != nil {
		t.Fatalf("release.sh 1.0.0: %v\n%s", err, out)
	}
	released := git(t, origin, "rev-parse", "main")
	if released == before {
		t.Fatal("the release pushed nothing")
	}

	other := filepath.Join(t.TempDir(), "other")
	git(t, ".", "clone", "--quiet", origin, other)
	out, err := release(t, "1.0.0", other)
	if err == nil || !strings.Contains(out, "tag v1.0.0 already exists") {
		t.Fatalf("a second 1.0.0 release: err %v, output %q", err, out)
	}
	if got := git(t, origin, "rev-parse", "main"); got != released {
		t.Fatalf("a refused release moved main from %s to %s", released, got)
	}
}

func TestARejectedPushMovesNoRef(t *testing.T) {
	spectest.Proves(t, feature, "action-release", "a-rejected-push-moves-no-ref")

	origin, clone := actionRepository(t)
	if out, err := release(t, "1.0.0", clone); err != nil {
		t.Fatalf("release.sh 1.0.0: %v\n%s", err, out)
	}
	released := git(t, origin, "rev-parse", "main")

	moved := filepath.Join(t.TempDir(), "moved")
	git(t, ".", "clone", "--quiet", origin, moved)
	write(t, filepath.Join(moved, "README.md"), "# edited by hand\n", 0o644)
	git(t, moved, "commit", "--quiet", "-am", "Edit by hand")
	git(t, moved, "push", "--quiet", "origin", "main")
	edited := git(t, origin, "rev-parse", "main")

	if out, err := release(t, "1.1.0", clone); err == nil {
		t.Fatalf("release.sh 1.1.0 passed on a branch that moved:\n%s", out)
	}
	if got := git(t, origin, "rev-parse", "main"); got != edited {
		t.Fatalf("main = %s, want %s", got, edited)
	}
	if got := git(t, origin, "rev-parse", "v1^{commit}"); got != released {
		t.Fatalf("v1 = %s, want the 1.0.0 release %s", got, released)
	}
	if out, err := gitCommand(origin, "rev-parse", "--verify", "--quiet", "refs/tags/v1.1.0").CombinedOutput(); err == nil {
		t.Fatalf("v1.1.0 exists on origin: %s", out)
	}
}

func TestTheReleaseJobRunsFromMainInItsEnvironment(t *testing.T) {
	spectest.Proves(t, feature, "action-release-runs-from-main", "the-release-job-runs-from-main-in-its-environment")

	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "observe-action-release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := yaml.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	on, _ := yaml.Get(doc, "on").(yaml.Map)
	if _, ok := on["workflow_dispatch"]; !ok || len(on) != 1 {
		t.Fatalf("on = %v, want workflow_dispatch only", on)
	}
	jobs, _ := yaml.Get(doc, "jobs").(yaml.Map)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %v, want the release job only", jobs)
	}
	if got := yaml.String(doc, "jobs", "release", "if"); got != "github.ref == 'refs/heads/main'" {
		t.Fatalf("jobs.release.if = %q", got)
	}
	if got := yaml.String(doc, "jobs", "release", "environment"); got != "observe-action" {
		t.Fatalf("jobs.release.environment = %q, want observe-action", got)
	}
	if got := yaml.String(doc, "permissions", "contents"); got != "read" {
		t.Fatalf("permissions.contents = %q, want read", got)
	}
}
