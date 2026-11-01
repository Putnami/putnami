package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/observe"
)

const feature = "tooling/observe"

func readManifest(t *testing.T) *extension.Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", extension.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := extension.ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("the manifest does not parse strictly: %v", diags)
	}
	return manifest
}

func TestTheManifestDeclaresAWorkspaceOptionalCommand(t *testing.T) {
	spectest.Proves(t, feature, "runs-without-a-workspace", "the-manifest-declares-a-workspace-optional-command")
	manifest := readManifest(t)
	if manifest.Name != extensionName {
		t.Fatalf("manifest name %q, runtime answers as %q", manifest.Name, extensionName)
	}
	if diags := extension.ValidateManifest(manifest); diag.HasErrors(diags) {
		t.Fatalf("the manifest fails extension validation: %v", diags)
	}
	group, ok := manifest.CommandGroups["observe"]
	if !ok {
		t.Fatal("the manifest declares no observe group")
	}
	sub, ok := group.Subcommands[group.Default]
	if !ok {
		t.Fatalf("the observe group's default %q names no subcommand", group.Default)
	}
	if !sub.RunsWithoutWorkspace() {
		t.Fatal("putnami observe does not run outside a workspace")
	}
	command := manifest.Commands[sub.Command]
	flags := make([]string, 0, len(command.Flags))
	for name := range command.Flags {
		flags = append(flags, name)
	}
	slices.Sort(flags)
	want := slices.Clone(knownFlags)
	slices.Sort(want)
	if !slices.Equal(flags, want) {
		t.Fatalf("the manifest declares flags %v, the runtime reads %v", flags, want)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// jobContext writes the job context the CLI gives a user-scope launch: no
// workspace, the caller's directory, and the output global.
func jobContext(t *testing.T, callerDir, invocationDir, output string) string {
	t.Helper()
	params := map[string]any{}
	if output != "" {
		params["output"] = output
	}
	data, err := json.Marshal(map[string]any{
		"protocolVersion": 2,
		"workspaceRoot":   t.TempDir(),
		"outputPath":      t.TempDir(),
		"cacheRoot":       t.TempDir(),
		"workspace":       map[string]any{"name": "user"},
		"project":         map[string]any{"name": "user", "path": "."},
		"extension":       map[string]any{"name": extensionName},
		"job":             map[string]any{"name": "observe-run"},
		"userScope":       map[string]any{"callerDir": callerDir},
		"invocationDir":   invocationDir,
		"params":          params,
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "context.json")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestObserveRunsInARepositoryWithoutAWorkspace(t *testing.T) {
	spectest.Proves(t, feature, "runs-without-a-workspace", "observe-runs-in-a-repository-without-a-workspace")
	spectest.Proves(t, feature, "caller-directory-writes", "observe-writes-only-git-history-in-the-caller-directory")
	repo := filepath.Join(t.TempDir(), "shop")
	git(t, t.TempDir(), "init", "--quiet", "--initial-branch=main", repo)
	write(t, repo, "package.json", `{"name":"shop","workspaces":["packages/*"]}`)
	write(t, repo, "packages/web/index.js", "export {}\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "--quiet", "-m", "root")
	git(t, repo, "checkout", "--quiet", "-b", "feature")
	write(t, repo, "packages/web/index.js", "export const a = 1\n")
	git(t, repo, "commit", "--quiet", "-am", "change")
	write(t, repo, "packages/web/reports/junit.xml", `<testsuites><testsuite name="web"/></testsuites>`)
	write(t, repo, ".gitignore", "reports/\n")
	git(t, repo, "add", ".gitignore")
	git(t, repo, "commit", "--quiet", "-m", "ignore reports")
	before := git(t, repo, "status", "--porcelain=v2", "--ignored", "--branch", "--untracked-files=all")
	if _, err := os.Stat(filepath.Join(repo, "putnami.workspace.json")); !os.IsNotExist(err) {
		t.Fatal("the fixture must not be a Putnami workspace")
	}

	invocation := filepath.Join(repo, "packages", "web")
	t.Chdir(invocation)
	env := map[string]string{
		"GITLAB_CI":                           "true",
		"CI_SERVER_HOST":                      "gitlab.com",
		"CI_PROJECT_ID":                       "4242",
		"CI_PROJECT_PATH":                     "acme/shop",
		"CI_COMMIT_SHA":                       strings.Repeat("c", 40),
		"CI_MERGE_REQUEST_IID":                "7",
		"CI_MERGE_REQUEST_DIFF_BASE_SHA":      git(t, repo, "rev-parse", "main"),
		"CI_MERGE_REQUEST_TARGET_BRANCH_NAME": "main",
		"CI_MERGE_REQUEST_SOURCE_BRANCH_NAME": "feature",
		"CI_PIPELINE_ID":                      "1",
		"CI_JOB_ID":                           "2",
	}
	var stdout, stderr bytes.Buffer
	evidencePath := filepath.Join(t.TempDir(), "evidence.json")
	code := run([]string{"run", "--junit", "reports/*.xml", "--evidence", evidencePath, "--putnamiContext", jobContext(t, invocation, invocation, "json")},
		func(k string) string { return env[k] }, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var o observe.Observation
	if err := json.Unmarshal(stdout.Bytes(), &o); err != nil {
		t.Fatalf("output is not one JSON document: %v\n%s", err, stdout.String())
	}
	if o.Subject.ProjectID != "4242" || o.Subject.Host != "gitlab.com" || o.Subject.ChangeRequest == nil || o.Subject.ChangeRequest.Number != 7 {
		t.Fatalf("subject %+v", o.Subject)
	}
	if o.Subject.BaseSHA != git(t, repo, "rev-parse", "main") || o.Subject.SHA != git(t, repo, "rev-parse", "HEAD") {
		t.Fatalf("base %s sha %s", o.Subject.BaseSHA, o.Subject.SHA)
	}
	changed := make([]string, 0, len(o.ChangedFiles))
	for _, f := range o.ChangedFiles {
		changed = append(changed, f.Status+" "+f.Path)
	}
	if want := []string{"added .gitignore", "modified packages/web/index.js"}; !slices.Equal(changed, want) {
		t.Fatalf("changed %v, want %v", changed, want)
	}
	if len(o.Reports) != 1 || o.Reports[0].Path != "packages/web/reports/junit.xml" {
		t.Fatalf("reports %+v", o.Reports)
	}
	codes := make([]string, 0, len(o.Diagnostics))
	for _, d := range o.Diagnostics {
		codes = append(codes, d.Code)
	}
	if !slices.Equal(codes, []string{"observe.report_missing"}) {
		t.Fatalf("diagnostics %v: the coverage report is missing and nothing else", o.Diagnostics)
	}
	data, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatalf("no evidence document: %v", err)
	}
	if doc, problems := evidence.ParseAndValidate(data); diag.HasErrors(problems) || doc.Subject.BaseSHA != o.Subject.BaseSHA || len(doc.ChangedFiles) != 2 {
		t.Fatalf("evidence %s: %v", data, problems)
	}
	if after := git(t, repo, "status", "--porcelain=v2", "--ignored", "--branch", "--untracked-files=all"); after != before {
		t.Fatalf("observe changed the caller's directory:\nbefore %s\nafter  %s", before, after)
	}

	stdout.Reset()
	if code := run([]string{"run", "--no-fetch", "--putnamiContext", jobContext(t, invocation, invocation, "")}, func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
		t.Fatalf("text run exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{"putnami observe: GitLab CI, gitlab.com/acme/shop (project 4242)", "merge request #7", "merge base with main from the merge request", "modified  packages/web/index.js", "junit     packages/web/reports/junit.xml"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("the summary lacks %q:\n%s", want, stdout.String())
		}
	}
}

func TestAUsageErrorExitsTwo(t *testing.T) {
	spectest.Proves(t, feature, "report-discovery", "a-missing-or-malformed-report-is-a-diagnostic")
	t.Chdir(t.TempDir())
	for name, args := range map[string][]string{
		"no subcommand":   {},
		"another command": {"verify"},
		"an unknown flag": {"run", "--jnuit", "a.xml"},
		"a positional":    {"run", "extra"},
		"an empty base":   {"run", "--base"},
		"a fetch value":   {"run", "--fetch=maybe"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, func(string) string { return "" }, &stdout, &stderr); code != 2 {
				t.Fatalf("exit %d, want 2: %s", code, stderr.String())
			}
		})
	}

	// A job context that cannot be read is the runtime's failure, not a usage error.
	var out, errOut bytes.Buffer
	if code := run([]string{"run", "--putnamiContext", filepath.Join(t.TempDir(), "missing.json")}, func(string) string { return "" }, &out, &errOut); code != 1 {
		t.Fatalf("an unreadable job context exits %d, want 1: %s", code, errOut.String())
	}

	// Outside a repository, with nothing to read, observe still exits 0.
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--base", "main"}, func(string) string { return "" }, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "observe.not_a_repository") {
		t.Fatalf("output %s", stdout.String())
	}
}

func TestTheRuntimeAnswersTheHandshake(t *testing.T) {
	spectest.Proves(t, feature, "runs-without-a-workspace", "the-manifest-declares-a-workspace-optional-command")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"__putnami", "runtime-info"}, os.Getenv, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var info struct {
		Extension string `json:"extension"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil || info.Extension != extensionName {
		t.Fatalf("handshake %s (%v)", stdout.String(), err)
	}
}
