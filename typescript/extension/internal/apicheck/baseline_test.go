package apicheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/gitcandidate"
	"go.putnami.dev/sdk/extension/linetag"
)

// releaseBaselineOf reads the release baseline of the project in projectDir
// from git, as the CLI writes it to the job context for a task that declares
// releaseBaseline and releaseBaselineFiles: the state, the line's last
// release, whether a commit since it that touches the project declares a
// breaking change, and a directory holding the regular files the tag holds at
// the project directory, absent when the tag holds none.
func releaseBaselineOf(t *testing.T, workspaceRoot, projectDir string) *job.ReleaseBaseline {
	t.Helper()
	pattern := linePattern(workspaceRoot, projectDir)
	state, err := readRepoState(projectDir)
	switch {
	case err != nil:
		return &job.ReleaseBaseline{State: job.ReleaseBaselineUnmanaged, Pattern: pattern}
	case !state.hasHead:
		return &job.ReleaseBaseline{State: job.ReleaseBaselineNoCommit, Pattern: pattern}
	case state.shallow:
		return &job.ReleaseBaseline{State: job.ReleaseBaselineShallow, Pattern: pattern}
	}
	release, ok, err := linetag.LastRelease(projectDir, pattern)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		tagged, err := hasTags(projectDir)
		if err != nil {
			t.Fatal(err)
		}
		return &job.ReleaseBaseline{State: job.ReleaseBaselineUntagged, Pattern: pattern, HasTags: tagged}
	}
	commits, err := commitsSince(projectDir, release.Tag, ".")
	if err != nil {
		t.Fatal(err)
	}
	baseline := &job.ReleaseBaseline{State: job.ReleaseBaselineTagged, Pattern: pattern, Tag: release.Tag,
		Breaking: slices.ContainsFunc(commits, func(c commit) bool {
			parsed, ok := protocolcli.ParseConventionalCommit(c.subject, c.body)
			return ok && parsed.Breaking
		})}
	entries, err := treeAtTag(projectDir, release.Tag, ".", treeListing{recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	var names, objects []string
	for _, entry := range entries {
		if entry.regular {
			names = append(names, entry.name)
			objects = append(objects, entry.object)
		}
	}
	if len(names) == 0 {
		return baseline
	}
	contents, err := readBlobs(projectDir, objects)
	if err != nil {
		t.Fatal(err)
	}
	baseline.Directory = filepath.Join(t.TempDir(), "project")
	for i, name := range names {
		path := filepath.Join(baseline.Directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents[i], 0o444); err != nil {
			t.Fatal(err)
		}
	}
	return baseline
}

// gitShim puts first on PATH a git that records each subcommand it runs in
// the returned file, then runs the real git.
func gitShim(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in -*) ;; *) echo \"$a\" >> '" + calls + "'; break;; esac; done\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func readCalls(t *testing.T, calls string) []string {
	t.Helper()
	data, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// The release baseline answers the history and the candidate cut the working
// tree: with both, the check starts no git.
func TestTheReleaseBaselineAnswersTheHistory(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("the git shim is a shell script")
	}
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/src/index.ts", "export const other = 1;\n")
	f.commit("feat!: drop greet")
	baseline := releaseBaselineOf(t, f.root, f.project)
	if baseline.Directory == "" || !baseline.Breaking {
		t.Fatalf("baseline = %+v, want a tagged line with its files and a breaking commit", baseline)
	}
	candidates := candidatesOf(t, f.root, f.project)
	calls := gitShim(t)
	report, err := Check(f.root, f.project, projectName, f.tags, baseline, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Changes) == 0 || !report.Breaking {
		t.Fatalf("report = %+v, want the removal, declared breaking", report)
	}
	if got := readCalls(t, calls); len(got) != 0 {
		t.Fatalf("git calls = %v, want none", got)
	}
}

// A line with nothing to compare is answered by the release baseline alone,
// with no git at all.
func TestALineWithNothingToCompareStartsNoGit(t *testing.T) {
	t.Setenv("PATH", "")
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ts/lib/package.json", manifest)
	project := filepath.Join(root, "ts", "lib")
	for _, tc := range []struct {
		baseline job.ReleaseBaseline
		warning  string
		note     string
	}{
		{baseline: job.ReleaseBaseline{State: job.ReleaseBaselineUnmanaged, Pattern: "v*"}, warning: "not in a git repository"},
		{baseline: job.ReleaseBaseline{State: job.ReleaseBaselineNoCommit, Pattern: "v*"}, note: "no commit"},
		{baseline: job.ReleaseBaseline{State: job.ReleaseBaselineShallow, Pattern: "v*"}, warning: "the clone is shallow"},
		{baseline: job.ReleaseBaseline{State: job.ReleaseBaselineUntagged, Pattern: "v*", HasTags: true}, warning: "though the repository has other tags"},
		{baseline: job.ReleaseBaseline{State: job.ReleaseBaselineUntagged, Pattern: "v*"}, note: "no release tag of line v*"},
	} {
		report, err := Check(root, project, projectName, []string{"ts"}, &tc.baseline, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.baseline.State, err)
		}
		if !strings.Contains(report.Warning, tc.warning) || !strings.Contains(report.Note, tc.note) ||
			(tc.warning == "") != (report.Warning == "") || (tc.note == "") != (report.Note == "") {
			t.Errorf("%s (hasTags %v): report = %+v, want warning %q and note %q", tc.baseline.State, tc.baseline.HasTags, report, tc.warning, tc.note)
		}
	}
}

// A released package.json over the size limit, or one the tag holds as a
// symbolic link, gives the same warning from the release baseline as from git.
func TestAReleasedManifestTheCheckRefusesGivesOneReportFromEitherSide(t *testing.T) {
	t.Parallel()
	oversized := newFixture(t, catalog("package", "stable"))
	oversized.write("ts/lib/package.json", `{"name":"@example/lib","description":"`+strings.Repeat("x", manifestMaxBytes)+`"}`+"\n")
	oversized.commit("chore: grow the manifest")
	oversized.git("tag", "-f", firstTag)
	if report := oversized.check(); !strings.Contains(report.Warning, "over the limit") {
		t.Errorf("an oversized released manifest = %+v, want the size warning", report)
	}

	linked := newFixture(t, catalog("package", "stable"))
	linked.write("ts/lib/manifest.json", manifest)
	linked.remove("ts/lib/package.json")
	if err := os.Symlink("manifest.json", filepath.Join(linked.project, "package.json")); err != nil {
		t.Fatal(err)
	}
	linked.commit("chore: link the manifest")
	linked.git("tag", "-f", firstTag)
	if report := linked.check(); !strings.Contains(report.Warning, "not a regular file") {
		t.Errorf("a released manifest held as a link = %+v, want the not-a-regular-file warning", report)
	}
}

// candidatesOf returns the candidate cut of projectDir as validate-api reads it
// from a job context whose task declares candidateFiles: the CLI's listing of
// the cut (gitcandidate.Paths, the command a `git:` input is enumerated with),
// carried as tree.candidateFiles and read back through gitcandidate.OpenFrom.
func candidatesOf(t *testing.T, workspaceRoot, projectDir string) []string {
	t.Helper()
	rel, err := filepath.Rel(workspaceRoot, projectDir)
	if err != nil {
		t.Fatal(err)
	}
	rel = filepath.ToSlash(rel)
	paths, err := gitcandidate.Paths(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	files := []job.CandidateFile{}
	for _, path := range paths {
		files = append(files, job.CandidateFile{Path: rel + "/" + path, Mode: job.CandidateModeUntracked})
	}
	ctx := &job.Context{WorkspaceRoot: workspaceRoot, Tree: &job.Tree{
		Directory: rel, CandidateDirectories: []string{rel}, CandidateFiles: files,
	}}
	candidates := contextCandidates(ctx, projectDir)
	if candidates == nil {
		t.Fatal("the context's candidate cut was not read")
	}
	return candidates
}

// A project in a repository nested in the workspace's, a clone or a
// submodule, is listed by that repository's index, which the context's cut
// does not read: the check lists the working tree with Git instead of reading
// an empty cut.
func TestANestedRepositoryReadsTheWorkingTreeFromGit(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "ts", "lib")
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := &job.Context{WorkspaceRoot: root, Tree: &job.Tree{
		Directory: "ts/lib", CandidateDirectories: []string{"ts/lib"}, CandidateFiles: []job.CandidateFile{},
	}}
	if got := contextCandidates(ctx, project); got != nil {
		t.Fatalf("candidates of a nested repository = %v, want none, so the check asks Git", got)
	}
	if err := os.Remove(filepath.Join(project, ".git")); err != nil {
		t.Fatal(err)
	}
	if got := contextCandidates(ctx, project); got == nil || len(got) != 0 {
		t.Fatalf("candidates of a project of the workspace's repository = %v, want the context's empty cut", got)
	}
}
