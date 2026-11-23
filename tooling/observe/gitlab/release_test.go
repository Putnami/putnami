package gitlab

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// catalogProject makes a bare origin holding one commit on main, with files
// the release must remove, and returns the origin and a clone of it.
func catalogProject(t *testing.T) (origin, clone string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the release runs on Linux or macOS")
	}
	dir := t.TempDir()
	origin = filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	clone = filepath.Join(dir, "clone")
	git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	git(t, dir, "init", "--quiet", "--initial-branch=main", seed)
	write(t, filepath.Join(seed, "README.md"), "# observe\n", 0o644)
	if err := os.MkdirAll(filepath.Join(seed, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(seed, "templates", "old.yml"), "old: {}\n", 0o644)
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

func TestAReleaseCopiesTheComponentToTheProjectRoot(t *testing.T) {
	spectest.Proves(t, feature, "component-release", "a-release-copies-the-component-to-the-project-root")

	origin, clone := catalogProject(t)
	if out, err := release(t, "1.0.0", clone, "Putnami/putnami@abc123"); err != nil {
		t.Fatalf("release.sh: %v\n%s", err, out)
	}

	files := strings.Split(git(t, origin, "ls-tree", "-r", "--name-only", "main"), "\n")
	if want := []string{".gitlab-ci.yml", "LICENSE.md", "README.md", "templates/observe.yml"}; !slices.Equal(files, want) {
		t.Fatalf("main holds %q, want %q", files, want)
	}
	for published, local := range map[string]string{
		"templates/observe.yml": filepath.Join("templates", "observe.yml"),
		"README.md":             "README.md",
		".gitlab-ci.yml":        "project.gitlab-ci.yml",
		"LICENSE.md":            filepath.Join("..", "..", "..", "LICENSE.md"),
	} {
		data, err := os.ReadFile(local)
		if err != nil {
			t.Fatal(err)
		}
		if got := git(t, origin, "show", "main:"+published); got != strings.TrimSpace(string(data)) {
			t.Fatalf("main:%s differs from %s", published, local)
		}
	}
	if message := git(t, origin, "log", "-1", "--format=%B", "main"); message != "Release 1.0.0\n\nSource: Putnami/putnami@abc123" {
		t.Fatalf("commit message = %q", message)
	}
	head := git(t, origin, "rev-parse", "main")
	if got := git(t, origin, "rev-parse", "1.0.0^{commit}"); got != head {
		t.Fatalf("tag 1.0.0 = %s, want main %s", got, head)
	}
	if out, err := gitCommand(origin, "rev-parse", "--verify", "--quiet", "refs/tags/v1.0.0").CombinedOutput(); err == nil {
		t.Fatalf("a v-prefixed tag exists: %s", out)
	}
}

func TestAReleaseRefusesABadOrExistingVersion(t *testing.T) {
	spectest.Proves(t, feature, "component-release", "a-release-refuses-a-bad-or-existing-version")

	origin, clone := catalogProject(t)
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
	if got := git(t, origin, "rev-parse", "main"); got != before {
		t.Fatal("a refused version moved main")
	}
	if out, err := release(t, "1.0.0", clone); err != nil {
		t.Fatalf("release.sh 1.0.0: %v\n%s", err, out)
	}
	released := git(t, origin, "rev-parse", "main")

	other := filepath.Join(t.TempDir(), "other")
	git(t, ".", "clone", "--quiet", origin, other)
	out, err := release(t, "1.0.0", other)
	if err == nil || !strings.Contains(out, "tag 1.0.0 already exists") {
		t.Fatalf("a second 1.0.0 release: err %v, output %q", err, out)
	}
	if got := git(t, origin, "rev-parse", "main"); got != released {
		t.Fatalf("a refused release moved main from %s to %s", released, got)
	}
}

func TestARejectedPushMovesNoRef(t *testing.T) {
	spectest.Proves(t, feature, "component-release", "a-rejected-push-moves-no-ref")

	origin, clone := catalogProject(t)
	if out, err := release(t, "1.0.0", clone); err != nil {
		t.Fatalf("release.sh 1.0.0: %v\n%s", err, out)
	}

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
	if out, err := gitCommand(origin, "rev-parse", "--verify", "--quiet", "refs/tags/1.1.0").CombinedOutput(); err == nil {
		t.Fatalf("1.1.0 exists on origin: %s", out)
	}
	if out, err := gitCommand(clone, "rev-parse", "--verify", "--quiet", "refs/tags/1.1.0").CombinedOutput(); err == nil {
		t.Fatalf("1.1.0 stays in the clone, so a retry would be refused: %s", out)
	}
}

func TestTheProjectPipelinePublishesASemverTagToTheCatalog(t *testing.T) {
	spectest.Proves(t, feature, "component-release-pipeline", "the-project-pipeline-publishes-a-semver-tag-to-the-catalog")

	data, err := os.ReadFile("project.gitlab-ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := yaml.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, _ := doc.(yaml.Map)
	var jobs []string
	for name := range pipeline {
		if name != "stages" {
			jobs = append(jobs, name)
		}
	}
	if !slices.Equal(jobs, []string{"create-release"}) {
		t.Fatalf("jobs = %q, want create-release alone", jobs)
	}
	job := pipeline["create-release"]
	if got := yaml.String(job, "release", "tag_name"); got != "$CI_COMMIT_TAG" {
		t.Fatalf("release.tag_name = %q, want the pipeline's tag", got)
	}
	rules, _ := yaml.Get(job, "rules").([]any)
	if len(rules) != 1 {
		t.Fatalf("rules = %v, want one", rules)
	}
	condition := yaml.String(rules[0], "if")
	pattern, ok := strings.CutPrefix(condition, "$CI_COMMIT_TAG =~ /")
	pattern, ok2 := strings.CutSuffix(pattern, "/")
	if !ok || !ok2 {
		t.Fatalf("rules[0].if = %q, want a match of the tag", condition)
	}
	tag := regexp.MustCompile(pattern)
	for _, version := range []string{"1.0.0", "0.1.0", "12.30.4"} {
		if !tag.MatchString(version) {
			t.Fatalf("the rule does not release %s", version)
		}
	}
	for _, version := range []string{"v1.0.0", "1.0", "1.0.0-rc.1", "01.0.0", "main"} {
		if tag.MatchString(version) {
			t.Fatalf("the rule releases %s", version)
		}
	}
}
