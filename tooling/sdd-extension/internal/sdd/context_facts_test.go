package sdd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	job "go.putnami.dev/protocol/job"
)

const historySHA = "0123456789abcdef0123456789abcdef01234567"

// withoutGit removes git from PATH for the test: a step that reads its facts
// from the job context starts no Git.
func withoutGit(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// The breaking marker reads the commits of the change from the context's
// change history, and names a dropped marker without Git.
func TestBreakingMarkerReadsTheChangeHistoryWithoutGit(t *testing.T) {
	withoutGit(t)
	history := &job.ChangeHistory{Baseline: "main", MergeBase: historySHA, Commits: []job.HistoryCommit{
		{SHA: strings.Repeat("a", 40), Subject: "feat(cli)!: drop the --all flag"},
		{SHA: strings.Repeat("b", 40), Subject: "fix(cli): stop the leak"},
	}}
	report, err := BuildBreakingMarkerResultFrom(history, t.TempDir(), "feat(cli): rework the selection", "main")
	if err == nil || !strings.Contains(err.Error(), "aaaaaaaaaaaa feat(cli)!: drop the --all flag") {
		t.Fatalf("err = %v, want the dropped marker named", err)
	}
	if report.Commits != 2 || len(report.Breaking) != 1 {
		t.Errorf("report = %+v", report)
	}
	if _, err := BuildBreakingMarkerResultFrom(history, t.TempDir(), "feat(cli)!: rework the selection", "main"); err != nil {
		t.Errorf("a title that keeps the marker failed: %v", err)
	}
	shallow := &job.ChangeHistory{Baseline: "main", Shallow: true}
	if _, err := BuildBreakingMarkerResultFrom(shallow, t.TempDir(), "feat: x", "main"); err == nil || !strings.Contains(err.Error(), "shallow clone") {
		t.Errorf("a shallow history passed: %v", err)
	}
}

// A history of another baseline is not the change's: the step reads Git, which
// is not on PATH here.
func TestBreakingMarkerIgnoresTheHistoryOfAnotherBaseline(t *testing.T) {
	withoutGit(t)
	history := &job.ChangeHistory{Baseline: "develop", MergeBase: historySHA, Commits: []job.HistoryCommit{}}
	if _, err := BuildBreakingMarkerResultFrom(history, t.TempDir(), "feat: x", "main"); err == nil {
		t.Error("the history of another baseline answered")
	}
}

// The coverage ratchet compares with the debt list of the workspace config the
// change history carries, and adopts the gate when the merge base holds none.
func TestCoverageRatchetReadsTheChangeHistoryWithoutGit(t *testing.T) {
	withoutGit(t)
	config := workspaceConfigWithDebt(t, []string{"/a", "/b"})
	history := &job.ChangeHistory{Baseline: "main", MergeBase: historySHA, Commits: []job.HistoryCommit{}, WorkspaceConfig: &config}
	report, err := BuildFeatureCoverageRatchetResultFrom(history, t.TempDir(), coverageOptions("/b", "/c"), "main")
	if err == nil || !reflect.DeepEqual(report.Grown, []string{"/c"}) || report.MergeBase != historySHA {
		t.Fatalf("report = %+v, err = %v, want /c grown against the merge base", report, err)
	}
	if _, err := BuildFeatureCoverageRatchetResultFrom(history, t.TempDir(), coverageOptions("/b"), "main"); err != nil {
		t.Errorf("a removed entry failed: %v", err)
	}
	adopting := &job.ChangeHistory{Baseline: "main", MergeBase: historySHA, Commits: []job.HistoryCommit{}}
	report, err = BuildFeatureCoverageRatchetResultFrom(adopting, t.TempDir(), coverageOptions("/c"), "main")
	if err != nil || !strings.Contains(report.Note, "adopts the gate") {
		t.Errorf("report = %+v, err = %v, want the change to adopt the gate", report, err)
	}
	shallow := &job.ChangeHistory{Baseline: "main", Shallow: true}
	if _, err := BuildFeatureCoverageRatchetResultFrom(shallow, t.TempDir(), coverageOptions("/c"), "main"); err == nil {
		t.Error("a shallow history passed")
	}
}

// A workspace step whose context carries the cut reads it without Git: a file
// the cut does not list does not exist for it.
func TestTheWorktreeReadsTheCutFromTheContextWithoutGit(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{"decisions/a.json": "{}\n", "decisions/ignored.json": "{}\n"} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	withoutGit(t)
	tree, err := openWorktreeFrom(&job.Tree{Directory: ".", CandidateDirectories: []string{"."},
		CandidateFiles: []job.CandidateFile{{Path: "decisions/a.json", Mode: job.CandidateModeRegular}}}, root)
	if err != nil || tree.tree == nil {
		t.Fatalf("openWorktreeFrom = %+v, %v", tree, err)
	}
	if _, err := tree.readRegular("decisions/a.json"); err != nil {
		t.Errorf("read a candidate: %v", err)
	}
	if _, err := tree.readRegular("decisions/ignored.json"); !os.IsNotExist(err) {
		t.Errorf("read a file outside the cut: %v, want it absent", err)
	}
}
