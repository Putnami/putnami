package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/tooling/observe/internal/observe"
)

var fixedHistoryNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// historyFixture is a repository on a host that is neither GitHub nor
// GitLab, so no run asks a code host API, whose main branch holds a root
// commit 3 days old and two commits 2 days and 1 day old. It returns the
// repository and the two commits a --history run observes, oldest first.
func historyFixture(t *testing.T) (string, []string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "shop")
	git(t, t.TempDir(), "init", "--quiet", "--initial-branch=main", repo)
	var shas []string
	for i, age := range []int{3, 2, 1} {
		write(t, repo, "README.md", strings.Repeat("shop\n", i+1))
		git(t, repo, "add", "-A")
		date := fixedHistoryNow.Add(-time.Duration(age) * 24 * time.Hour).Format(time.RFC3339)
		cmd := gittest.Command(repo, "-c", "maintenance.auto=false", "-c", "gc.auto=0", "commit", "--quiet", "-m", "commit")
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git commit: %v\n%s", err, out)
		}
		shas = append(shas, git(t, repo, "rev-parse", "HEAD"))
	}
	git(t, repo, "remote", "add", "origin", "https://git.example.com/acme/shop.git")
	git(t, repo, "update-ref", "refs/remotes/origin/main", shas[2])
	git(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	historyNow = func() time.Time { return fixedHistoryNow }
	t.Cleanup(func() { historyNow = time.Now })
	return repo, shas[1:]
}

// fakeReporter is a CLI that appends the run id of each document it gets to
// a record and exits with code; it returns the CLI and the record.
func fakeReporter(t *testing.T, code string) (string, string) {
	t.Helper()
	record := filepath.Join(t.TempDir(), "uploads")
	cli := filepath.Join(t.TempDir(), "putnami")
	script := "#!/bin/sh\n" +
		"sed -n 's/.*\"runId\": \"\\([^\"]*\\)\".*/\\1/p' \"$4\" >> '" + record + "'\n" +
		"exit " + code + "\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli, record
}

func uploads(t *testing.T, record string) []string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		return nil
	}
	return strings.Fields(string(data))
}

func TestHistoryWritesOneDocumentPerCommit(t *testing.T) {
	spectest.Proves(t, feature, "history-evidence", "history-writes-one-document-per-commit")
	repo, shas := historyFixture(t)
	t.Chdir(repo)
	dir := filepath.Join(t.TempDir(), "history")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--history", "--evidence", dir}, func(string) string { return "" }, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want := []string{shas[0] + ".json", shas[1] + ".json"}; !slices.Equal(names, want) && !slices.Equal(names, []string{want[1], want[0]}) {
		t.Fatalf("documents %v, want %v", names, want)
	}
	for _, sha := range shas {
		data, err := os.ReadFile(filepath.Join(dir, sha+".json"))
		if err != nil {
			t.Fatal(err)
		}
		doc, problems := evidence.ParseAndValidate(data)
		if diag.HasErrors(problems) {
			t.Fatalf("%s: %v", sha, problems)
		}
		if doc.Subject.SHA != sha || doc.Subject.Branch != "main" || doc.CI.RunID != "history-"+sha || doc.Subject.Repository != "acme/shop" {
			t.Errorf("%s: subject %+v, run %+v", sha, doc.Subject, doc.CI)
		}
	}
	summary := stdout.String()
	for _, want := range []string{"putnami observe --history: git.example.com/acme/shop, main", "3 commits in the window, 2 observed", shas[0][:12], filepath.Join(dir, shas[1]+".json")} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary lacks %q:\n%s", want, summary)
		}
	}

	stdout.Reset()
	if code := run([]string{"run", "--history=60h", "--putnamiContext", jobContext(t, repo, repo, "json")}, func(string) string { return "" }, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var h observe.History
	if err := json.Unmarshal(stdout.Bytes(), &h); err != nil {
		t.Fatalf("%v\n%s", err, stdout.String())
	}
	if h.Branch != "main" || h.InWindow != 2 || len(h.Commits) != 2 || h.Commits[1].Commit.SHA != shas[1] {
		t.Fatalf("history %+v", h)
	}
}

func TestHistoryUploadsEachCommitUnderTheSameRunID(t *testing.T) {
	spectest.Proves(t, feature, "history-evidence", "history-run-ids-derive-from-the-commit")
	repo, shas := historyFixture(t)
	t.Chdir(repo)
	cli, record := fakeReporter(t, "0")
	env := map[string]string{"PUTNAMI_CLI_EXECUTABLE": cli}
	for range 2 {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run", "--history", "--upload"}, func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "uploaded") {
			t.Fatalf("the summary does not say the commits were uploaded:\n%s", stdout.String())
		}
	}
	// The newest commit is uploaded first.
	once := []string{"history-" + shas[1], "history-" + shas[0]}
	if got := uploads(t, record); !slices.Equal(got, slices.Concat(once, once)) {
		t.Fatalf("uploaded run ids %v, want %v twice", got, once)
	}
}

func TestHistoryStopsUploadingAtTheFirstFailure(t *testing.T) {
	repo, shas := historyFixture(t)
	t.Chdir(repo)
	cli, record := fakeReporter(t, "1")
	env := map[string]string{"PUTNAMI_CLI_EXECUTABLE": cli}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--history", "--upload", "--putnamiContext", jobContext(t, repo, repo, "json")}, func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
		t.Fatalf("a refused upload exits %d", code)
	}
	if got := uploads(t, record); !slices.Equal(got, []string{"history-" + shas[1]}) {
		t.Fatalf("uploads %v, want the newest commit alone", got)
	}
	var h observe.History
	if err := json.Unmarshal(stdout.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(h.Diagnostics, func(d diag.Diagnostic) bool {
		return d.Code == "observe.upload_failed" && strings.Contains(d.Message, "1 more commit was not uploaded")
	}) {
		t.Fatalf("diagnostics %+v", h.Diagnostics)
	}
}

func TestHistoryUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--history", "--base", "main"},
		{"run", "--history", "--junit", "reports/*.xml"},
		{"run", "--history=30d", "--publish"},
		{"run", "--history", "0d"},
		{"run", "--history", "400d"},
		{"run", "--history", "--fetch"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, func(string) string { return "" }, &stdout, &stderr); code != 2 {
			t.Errorf("%v exits %d, want 2", args, code)
		}
	}
}
