package sdd

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const coverageRatchetFeature = "tooling/specification-driven-development"
const coverageRatchetRequirement = "coverage-debt-only-shrinks"

// workspaceConfigWithDebt is a putnami.workspace.json whose debt list is debt,
// or one that declares no options.sdd.featureCoverage when debt is nil.
func workspaceConfigWithDebt(t *testing.T, debt []string) string {
	t.Helper()
	sdd := map[string]any{"owners": []string{"@team"}}
	if debt != nil {
		sdd[FeatureCoverageOption] = map[string]any{"debt": debt}
	}
	encoded, err := json.MarshalIndent(map[string]any{"name": "w", "options": map[string]any{"sdd": sdd}}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n"
}

// coverageRatchetRepository is a repository whose main branch commits a
// workspace config with the debt list base under dir, and whose checked-out
// branch "change" starts there. It returns the repository root and the
// workspace root.
func coverageRatchetRepository(t *testing.T, dir string, base []string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := decisionsWorkspace(t)
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
		{"checkout", "-q", "-b", "main"},
	} {
		gitIn(t, repo, args...)
	}
	commitFile(t, repo, filepath.Join(dir, "putnami.workspace.json"), workspaceConfigWithDebt(t, base), "chore: start")
	gitIn(t, repo, "checkout", "-q", "-b", "change")
	return repo, filepath.Join(repo, dir)
}

func coverageRatchetFindings(report FeatureCoverageRatchetReport) []string {
	findings := make([]string, 0, len(report.Diagnostics))
	for _, finding := range report.Diagnostics {
		findings = append(findings, finding.Code+" "+finding.Field)
	}
	return findings
}

// A new entry fails naming it, on the index a reader finds it at, even when
// the change also removes another entry.
func TestCoverageRatchetFailsANewEntryNamingIt(t *testing.T) {
	spectest.Proves(t, coverageRatchetFeature, coverageRatchetRequirement, "a-new-debt-entry-fails-naming-it")
	repo, root := coverageRatchetRepository(t, "", []string{"/a", "/b"})
	current := []string{"/b", "/c"}
	commitFile(t, repo, "putnami.workspace.json", workspaceConfigWithDebt(t, current), "chore: defer /c")

	report, err := BuildFeatureCoverageRatchetResult(root, coverageOptions("/b", "/c"), "main")
	if err == nil {
		t.Fatal("a new debt entry passed")
	}
	if !strings.Contains(err.Error(), "1 new debt entry") {
		t.Errorf("error = %v, want it to count one new entry", err)
	}
	want := []string{ErrorCodeGrownCoverageDebt + " putnami.workspace.json#options.sdd.featureCoverage.debt[1]"}
	if got := coverageRatchetFindings(report); !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
	if message := report.Diagnostics[0].Message; !strings.Contains(message, "/c") || !strings.Contains(message, "main") {
		t.Errorf("message = %q, want it to name the entry and the baseline", message)
	}
	if !reflect.DeepEqual(report.Grown, []string{"/c"}) || !reflect.DeepEqual(report.BaselineDebt, []string{"/a", "/b"}) {
		t.Errorf("report = %+v, want /c grown against [/a /b]", report)
	}
}

// Paying a debt, or leaving the list alone, passes.
func TestCoverageRatchetPassesARemovedOrKeptEntry(t *testing.T) {
	spectest.Proves(t, coverageRatchetFeature, coverageRatchetRequirement, "a-removed-or-kept-entry-passes")
	for name, current := range map[string][]string{"removed": {"/b"}, "kept": {"/a", "/b"}, "emptied": {}} {
		t.Run(name, func(t *testing.T) {
			repo, root := coverageRatchetRepository(t, "", []string{"/a", "/b"})
			writeFixtureFile(t, filepath.Join(repo, "putnami.workspace.json"), workspaceConfigWithDebt(t, current))
			commitFile(t, repo, "src.go", "package src\n", "feat: work")
			debt := make([]any, 0, len(current))
			for _, id := range current {
				debt = append(debt, id)
			}
			report, err := BuildFeatureCoverageRatchetResult(root, coverageOptions(debt...), "main")
			if err != nil {
				t.Fatalf("the ratchet failed: %v (%v)", err, coverageRatchetFindings(report))
			}
			if report.Note != "" || len(report.Grown) != 0 || report.MergeBase == "" {
				t.Errorf("report = %+v, want a comparison at the merge base with nothing grown", report)
			}
		})
	}
}

// The comparison point is the merge base. A branch behind its base keeps an
// entry the base removed since, and a branch that merged its base takes the
// base's list: neither entry is new.
func TestCoverageRatchetComparesWithTheMergeBase(t *testing.T) {
	spectest.Proves(t, coverageRatchetFeature, coverageRatchetRequirement, "base-commits-merged-in-do-not-count")
	t.Run("behind", func(t *testing.T) {
		repo, root := coverageRatchetRepository(t, "", []string{"/a", "/b"})
		commitFile(t, repo, "src.go", "package src\n", "feat: work")
		gitIn(t, repo, "checkout", "-q", "main")
		commitFile(t, repo, "putnami.workspace.json", workspaceConfigWithDebt(t, []string{"/b"}), "feat: pay /a")
		gitIn(t, repo, "checkout", "-q", "change")

		if report, err := BuildFeatureCoverageRatchetResult(root, coverageOptions("/a", "/b"), "main"); err != nil {
			t.Fatalf("an entry the base removed after the branch started failed: %v (%v)", err, coverageRatchetFindings(report))
		}
	})
	t.Run("merged", func(t *testing.T) {
		repo, root := coverageRatchetRepository(t, "", []string{"/a"})
		gitIn(t, repo, "checkout", "-q", "main")
		commitFile(t, repo, "putnami.workspace.json", workspaceConfigWithDebt(t, []string{"/a", "/c"}), "chore: defer /c")
		gitIn(t, repo, "checkout", "-q", "change")
		commitFile(t, repo, "src.go", "package src\n", "feat: work")
		gitIn(t, repo, "merge", "-q", "--no-ff", "-m", "Merge branch 'main' into change", "main")

		if report, err := BuildFeatureCoverageRatchetResult(root, coverageOptions("/a", "/c"), "main"); err != nil {
			t.Fatalf("an entry of the merged base failed: %v (%v)", err, coverageRatchetFindings(report))
		}
	})
}

// A run that names no impacted baseline compares nothing and says so.
func TestCoverageRatchetPassesWithANoteWithoutABaseline(t *testing.T) {
	spectest.Proves(t, coverageRatchetFeature, coverageRatchetRequirement, "a-run-without-an-impacted-baseline-passes-with-a-note")
	for _, baseline := range []string{"", "  "} {
		report, err := BuildFeatureCoverageRatchetResult(t.TempDir(), coverageOptions("/a"), baseline)
		if err != nil {
			t.Fatalf("baseline %q: the ratchet failed: %v", baseline, err)
		}
		if !strings.Contains(report.Note, "--impacted") || !report.Adopted {
			t.Errorf("baseline %q: report = %+v, want an adopted run with a note that names --impacted", baseline, report)
		}
	}
	report, err := BuildFeatureCoverageRatchetResult(t.TempDir(), map[string]map[string]any{}, "main")
	if err != nil || report.Adopted || !strings.Contains(report.Note, "not adopted") {
		t.Errorf("an unadopted workspace: report = %+v, err = %v, want a pass with a note", report, err)
	}
}

// The change that adopts the gate starts the list: a merge base without the
// block, or without a workspace config, compares nothing and says so.
func TestCoverageRatchetPassesWithANoteAtAnUnadoptedMergeBase(t *testing.T) {
	spectest.Proves(t, coverageRatchetFeature, coverageRatchetRequirement, "an-unadopted-merge-base-passes-with-a-note")
	repo, root := coverageRatchetRepository(t, "", nil)
	commitFile(t, repo, "putnami.workspace.json", workspaceConfigWithDebt(t, []string{"/a"}), "chore: adopt the gate")
	report, err := BuildFeatureCoverageRatchetResult(root, coverageOptions("/a"), "main")
	if err != nil || !strings.Contains(report.Note, "adopts the gate") {
		t.Errorf("report = %+v, err = %v, want a pass with a note that the change adopts the gate", report, err)
	}

	repo, _ = coverageRatchetRepository(t, "ws", []string{"/a"})
	commitFile(t, repo, "other/putnami.workspace.json", workspaceConfigWithDebt(t, []string{"/a"}), "chore: add a workspace")
	report, err = BuildFeatureCoverageRatchetResult(filepath.Join(repo, "other"), coverageOptions("/a"), "main")
	if err != nil || !strings.Contains(report.Note, "adopts the gate") {
		t.Errorf("a merge base without the workspace config: report = %+v, err = %v, want a pass with a note", report, err)
	}
}

// A workspace in a subdirectory of its repository reads its own config at
// the merge base.
func TestCoverageRatchetReadsAWorkspaceInASubdirectory(t *testing.T) {
	repo, root := coverageRatchetRepository(t, "ws", []string{"/a"})
	commitFile(t, repo, "ws/putnami.workspace.json", workspaceConfigWithDebt(t, []string{"/a", "/b"}), "chore: defer /b")
	report, err := BuildFeatureCoverageRatchetResult(root, coverageOptions("/a", "/b"), "main")
	if err == nil || !reflect.DeepEqual(report.Grown, []string{"/b"}) {
		t.Errorf("report = %+v, err = %v, want /b grown", report, err)
	}
}

// A shallow clone can stop before the merge base: the ratchet refuses it.
func TestCoverageRatchetRefusesAShallowClone(t *testing.T) {
	spectest.Proves(t, coverageRatchetFeature, coverageRatchetRequirement, "a-shallow-clone-fails")
	origin, _ := coverageRatchetRepository(t, "", []string{"/a"})
	commitFile(t, origin, "src.go", "package src\n", "feat: one")
	commitFile(t, origin, "src.go", "package src\n\nconst x = 1\n", "feat: two")
	clone := filepath.Join(t.TempDir(), "clone")
	gitIn(t, t.TempDir(), "clone", "-q", "--depth", "1", "--no-single-branch", "file://"+origin, clone)
	gitIn(t, clone, "checkout", "-q", "change")

	_, err := BuildFeatureCoverageRatchetResult(clone, coverageOptions("/a"), "origin/main")
	if err == nil || !strings.Contains(err.Error(), "shallow clone") {
		t.Fatalf("error = %v, want a refusal that names the shallow clone", err)
	}
}

// A list the step cannot read fails closed, at HEAD and at the merge base, and
// a baseline that names no commit is an error, never a pass.
func TestCoverageRatchetFailsClosed(t *testing.T) {
	if _, err := BuildFeatureCoverageRatchetResult(t.TempDir(), coverageOptions("/a", "/a"), "main"); err == nil {
		t.Error("a malformed current list passed")
	}

	_, root := coverageRatchetRepository(t, "", []string{"/a"})
	if _, err := BuildFeatureCoverageRatchetResult(root, coverageOptions("/a"), "origin/missing"); err == nil {
		t.Error("a baseline that names no commit passed")
	}

	for name, config := range map[string]string{
		"invalid list":         `{"options": {"sdd": {"featureCoverage": {"debt": "/a"}}}}`,
		"undecodable config":   `{"options": `,
		"options.sdd a string": `{"options": {"sdd": "on"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			repo, root := coverageRatchetRepository(t, "", []string{"/a"})
			gitIn(t, repo, "checkout", "-q", "main")
			commitFile(t, repo, "putnami.workspace.json", config, "chore: break the config")
			gitIn(t, repo, "checkout", "-q", "change")
			gitIn(t, repo, "reset", "-q", "--hard", "main")
			commitFile(t, repo, "putnami.workspace.json", workspaceConfigWithDebt(t, []string{"/a"}), "chore: mend the config")
			report, err := BuildFeatureCoverageRatchetResult(root, coverageOptions("/a"), "main")
			if err == nil {
				t.Fatal("an unreadable list at the merge base passed")
			}
			want := []string{ErrorCodeCoverageDebtBaselineUnavailable + " putnami.workspace.json"}
			if got := coverageRatchetFindings(report); !reflect.DeepEqual(got, want) {
				t.Errorf("findings = %v, want %v", got, want)
			}
		})
	}
}
