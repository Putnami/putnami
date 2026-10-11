package cloudcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	"go.putnami.dev/sdk/extension/gitcandidate"
)

// These tests pin the cache key of cloud-validate-site-content. The task keys
// on the project's putnami.json and on the project's Git candidate files, and
// the check reads the sections through the same candidate cut. A cache hit
// replays the verdict of an earlier run, so two project states the key cannot
// tell apart must get one verdict.

const validateSiteContentTaskName = "cloud-validate-site-content"

// TestValidateSiteContentCacheContract pins the declared contract. The field
// set is closed so a new field such as writes, outputs or env cannot change
// what a hit replays without a reviewed test change.
func TestValidateSiteContentCacheContract(t *testing.T) {
	raw, ok := readCloudManifestTasks(t)[validateSiteContentTaskName]
	if !ok {
		t.Fatalf("manifest has no %s task", validateSiteContentTaskName)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if want := []string{"args", "cache", "command", "cwd", "declares", "description", "inputs", "kind", "timeoutMs"}; !slices.Equal(keys, want) {
		t.Fatalf("%s fields = %v, want %v", validateSiteContentTaskName, keys, want)
	}

	var task validateConfigTaskContract
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	if task.Cache != (taskCacheSetting{Declared: true, Object: true, Enabled: true, NoOutput: true}) {
		t.Fatalf("cache = %+v, want {enabled: true, noOutput: true}", task.Cache)
	}
	var declares map[string]json.RawMessage
	if err := json.Unmarshal(task.Declares, &declares); err != nil || declares == nil || len(declares) != 0 {
		t.Fatalf("declares = %s, want an empty object", task.Declares)
	}
	if task.CWD != "{projectRoot}" || !slices.Equal(task.Args, []string{"validate-site-content"}) {
		t.Fatalf("cwd = %q args = %v", task.CWD, task.Args)
	}
	want := map[string]manifestInputPort{
		// A glob keeps every byte of putnami.json, and keys a manifest Git
		// ignores, which the candidate files do not hold.
		"manifest": {From: "project", Files: []string{"putnami.jso[n]"}},
		// "git:**" alone selects the whole repository. The exclusion keeps the
		// candidate files of the project and nothing above it.
		"sections": {From: "project", Files: []string{"git:**", "!../**"}},
		// `putnami validate --app X` reaches each project's step, and the check
		// refuses a project other than its own.
		"app": {From: "params"}, "appName": {From: "params"},
	}
	if !reflect.DeepEqual(task.Inputs, want) {
		t.Fatalf("input ports = %v, want %v", task.Inputs, want)
	}
}

// siteContentKeyFixture writes a site project with two sections in a Git
// workspace that ignores *.draft. It returns the workspace root and the
// project directory.
func siteContentKeyFixture(t *testing.T) (root, project string) {
	t.Helper()
	root = t.TempDir()
	project = filepath.Join(root, "docs", "example.test")
	writeFixtureFile(t, filepath.Join(root, ".gitignore"), "*.draft\n")
	writeFixtureJSON(t, filepath.Join(project, "putnami.json"), map[string]any{"name": "docs/example.test", "publish": []string{"site-content"}})
	writeFixtureFile(t, filepath.Join(project, "guide", "index.md"), "# Guide\n")
	writeFixtureFile(t, filepath.Join(project, "guide", "start", "install.md"), "# Install\n")
	writeFixtureFile(t, filepath.Join(project, "reference", "index.md"), "# Reference\n")
	isolateGit(t)
	gitCommand(t, root, "init", "-q")
	gitCommand(t, root, "config", "user.email", "test@example.com")
	gitCommand(t, root, "config", "user.name", "Test")
	gitCommand(t, root, "config", "commit.gpgsign", "false")
	gitCommand(t, root, "add", "-A")
	gitCommand(t, root, "commit", "-qm", "fixture")
	return root, project
}

// siteContentOutcomeOf runs the check from the project directory, as the task
// does. The result holds the error and the printed verdict.
func siteContentOutcomeOf(t *testing.T, root, project string, params map[string]any) string {
	t.Helper()
	t.Chdir(project)
	var stdout []string
	err := distributioncli.ValidateSiteContentSources(params, nil, root, IO{Stdout: func(line string) { stdout = append(stdout, line) }})
	return strings.ReplaceAll(fmt.Sprintf("error: %v\nstdout: %s", err, strings.Join(stdout, "\n")), root, "<root>")
}

// siteContentKeyOf digests what the task's file ports hold for the project:
// its candidate files, putnami.json among them, as a `git:**` input reads them.
func siteContentKeyOf(t *testing.T, project string) string {
	t.Helper()
	tree, err := gitcandidate.Open(project)
	if err != nil || tree == nil {
		t.Fatalf("open the candidate files of %s: tree = %v, err = %v", project, tree, err)
	}
	key, err := tree.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// TestValidateSiteContentVerdictMovesOnlyWithItsKey changes the project one
// state at a time. A state the key does not hold must leave the verdict as it
// was: an empty directory and a file Git ignores are absent for the check, as
// they are from a clone. A state the key holds must move the verdict, so the
// fixture exercises every policy the check applies.
func TestValidateSiteContentVerdictMovesOnlyWithItsKey(t *testing.T) {
	write := func(rel, content string) func(*testing.T, string) {
		return func(t *testing.T, project string) {
			writeFixtureFile(t, filepath.Join(project, filepath.FromSlash(rel)), content)
		}
	}
	link := func(rel, target string) func(*testing.T, string) {
		return func(t *testing.T, project string) {
			path := filepath.Join(project, filepath.FromSlash(rel))
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*testing.T, string)
		// want is part of the new verdict, or empty when the key does not hold
		// the change.
		want string
	}{
		{name: "an empty directory", change: func(t *testing.T, project string) {
			if err := os.Mkdir(filepath.Join(project, "empty"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "an ignored file in a section", change: write("guide/notes.draft", "notes\n")},
		{name: "an ignored symbolic link in a section", change: link("guide/alias.draft", "index.md")},
		{name: "a directory of ignored files", change: write("scratch/notes.draft", "notes\n")},

		{name: "an untracked page", change: write("guide/new.md", "# New\n"), want: "(2 sections, 4 files)"},
		{name: "a new section", change: write("howto/index.md", "# How to\n"), want: "(3 sections, 4 files)"},
		{name: "a deleted page", change: func(t *testing.T, project string) {
			if err := os.Remove(filepath.Join(project, "reference", "index.md")); err != nil {
				t.Fatal(err)
			}
		}, want: "(1 sections, 2 files)"},
		{name: "a page replaced by a symbolic link", change: link("guide/index.md", "start/install.md"), want: "is not a regular file"},
		{name: "a section that cannot be a mount", change: write("Drafts/index.md", "# Drafts\n"), want: "maps to invalid mount"},
		{name: "a page whose name cannot be a payload path", change: write("guide/bad\\page.md", "# Bad\n"), want: "maps to invalid payload path"},
		{name: "a withdrawn publication", change: func(t *testing.T, project string) {
			writeFixtureJSON(t, filepath.Join(project, "putnami.json"), map[string]any{"name": "docs/example.test"})
		}, want: "skipping source validation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, project := siteContentKeyFixture(t)
			params := func() map[string]any { return map[string]any{"app": "docs/example.test"} }
			baselineKey, baseline := siteContentKeyOf(t, project), siteContentOutcomeOf(t, root, project, params())
			if !strings.Contains(baseline, "Validated site-content sources for /docs/example.test (2 sections, 3 files).") {
				t.Fatalf("fixture does not validate:\n%s", baseline)
			}

			tc.change(t, project)
			key, outcome := siteContentKeyOf(t, project), siteContentOutcomeOf(t, root, project, params())
			if tc.want == "" {
				if key != baselineKey {
					t.Fatalf("the key holds this change, so the case proves nothing")
				}
				if outcome != baseline {
					t.Fatalf("the verdict moved with a state the key does not hold\nbefore:\n%s\nafter:\n%s", baseline, outcome)
				}
				return
			}
			if key == baselineKey {
				t.Fatalf("the key does not hold a change the verdict depends on:\n%s", outcome)
			}
			if !strings.Contains(outcome, tc.want) {
				t.Fatalf("verdict = %s, want %q", outcome, tc.want)
			}
		})
	}
}

// TestValidateSiteContentRefusesAnotherProject covers the parameter that would
// point the cached check at files its key never hashed. It must fail rather
// than store a verdict.
func TestValidateSiteContentRefusesAnotherProject(t *testing.T) {
	root, project := siteContentKeyFixture(t)
	writeProjectManifest(t, filepath.Join(root, "docs", "other.test"), "docs/other.test")
	for _, name := range []string{"app", "appName"} {
		t.Run(name, func(t *testing.T) {
			outcome := siteContentOutcomeOf(t, root, project, map[string]any{name: "docs/other.test"})
			if !strings.Contains(outcome, "checks only the project in its working directory") {
				t.Fatalf("outcome = %s, want a refusal", outcome)
			}
			if !strings.HasSuffix(outcome, "stdout: ") {
				t.Fatalf("a refused check printed a verdict: %s", outcome)
			}
		})
	}
}
