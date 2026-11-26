package repositorychecks

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// The governance record answers the questions a public contributor asks: who
// decides, how a rule is waived, how a release is cut and rolled back, how a
// vulnerability is handled, and how to contribute without an account.
//
// The gate checks that the record answers the questions it claims to, never
// that the answers are correct. Presence and shape are machine-checkable;
// whether the named approver is the right one is what human approval is for.

const (
	contributorGuideRepoPath = "CONTRIBUTING.md"
	governanceRepoPath       = "GOVERNANCE.md"
	releasingRepoPath        = "RELEASING.md"
	securityRepoPath         = "SECURITY.md"
	readmeRepoPath           = "README.md"
	supportProtocolRepoPath  = "protocols/support/README.md"

	// The markers around the neutral contributor recipe. The recipe parser of
	// @putnami/cli-document-set (neutral_contributor_ci_test.go) reads the
	// same two markers to run the recipe against the CLI.
	neutralRecipeBeginMarker = "<!-- neutral-contributor-ci:begin -->"
	neutralRecipeEndMarker   = "<!-- neutral-contributor-ci:end -->"
)

// TestGovernanceRecordAnswersItsQuestions is the completeness gate over the
// public governance record.
func TestGovernanceRecordAnswersItsQuestions(t *testing.T) {
	t.Parallel()
	documents := make(map[string]string, len(governanceQuestions))
	for _, question := range governanceQuestions {
		if _, loaded := documents[question.path]; loaded {
			continue
		}
		documents[question.path] = readRepositoryFile(t, question.path)
	}
	for _, unanswered := range missingGovernanceAnswers(documents) {
		t.Error(unanswered)
	}
}

// TestGovernanceCompletenessScannerFailsOnAnEmptyRecord is the non-vacuity pair
// required by ADR 0002 §5 of the CLI (tooling/cli/doc/adr/0002-cli-vnext-contracts.md): a scan that matches nothing would pass forever and
// report a guarded invariant that nothing guards.
func TestGovernanceCompletenessScannerFailsOnAnEmptyRecord(t *testing.T) {
	t.Parallel()
	empty := make(map[string]string)
	for _, question := range governanceQuestions {
		empty[question.path] = "unrelated prose\n"
	}
	missing := missingGovernanceAnswers(empty)
	if len(missing) != len(governanceQuestions) {
		t.Fatalf("empty record reported %d unanswered questions, want all %d: %v",
			len(missing), len(governanceQuestions), missing)
	}
	if len(governanceQuestions) == 0 {
		t.Fatal("the governance question list is empty — the gate guards nothing")
	}
}

// governanceQuestion is one question the public record must answer, expressed
// as the phrases that answer it. Every phrase must appear (case-insensitively)
// in the named document.
type governanceQuestion struct {
	path     string
	question string
	phrases  []string
}

var governanceQuestions = []governanceQuestion{
	{governanceRepoPath, "who the release owner is, bound to an existing record",
		[]string{"release owner", "license.md", "copyright notice"}},
	{governanceRepoPath, "what happens when no approver is available",
		[]string{"absence is never approval", "delegat"}},
	{governanceRepoPath, "how a rule gets waived, and how each waiver ends",
		[]string{"doctor.waivers.json", "public-cut-baseline.tsv", "public-cut-allowlist.tsv", "expires", "shrink"}},
	{governanceRepoPath, "which rules have no exception path at all",
		[]string{"unwaivable"}},
	{governanceRepoPath, "who approves a support-status change and where it is recorded",
		[]string{"promotion and demotion", "putnami.support.json"}},
	{governanceRepoPath, "which license applies today and what the MIT move requires",
		[]string{"fsl-1.1-mit", "v1.0.0", "second anniversary"}},
	{governanceRepoPath, "where the roadmap lives and what it does not promise",
		[]string{"roadmap", "not a promise"}},

	{releasingRepoPath, "the release cadence",
		[]string{"cadence", "event-driven"}},
	{releasingRepoPath, "what a patch release may contain",
		[]string{"patch", "version window"}},
	{releasingRepoPath, "the release checklist and the gate it runs",
		[]string{"release checklist", "--enforce-coverage", "putnami.support.json"}},
	{releasingRepoPath, "who owns a rollback and how a user takes one",
		[]string{"rollback", "release owner", "putnami pin"}},
	{releasingRepoPath, "where the per-format compatibility window is stated",
		[]string{"21-compatibility-and-migration.md"}},
	{releasingRepoPath, "that the public-root switch stays a human operation",
		[]string{"public-root switch"}},

	{securityRepoPath, "the first-72-hour triage clock",
		[]string{"the first 72 hours", "72 wall-clock hours"}},
	{securityRepoPath, "how the triage clock reconciles with the published windows",
		[]string{"3 business days", "10 business days"}},
	{securityRepoPath, "the patch policy for a confirmed vulnerability",
		[]string{"patch policy", "30 calendar days"}},

	{contributorGuideRepoPath, "the neutral contributor path, and that it needs no account",
		[]string{"contributor ci without putnami cloud credentials", neutralRecipeBeginMarker, neutralRecipeEndMarker}},
	{contributorGuideRepoPath, "what maintainer CI runs, and that it needs no secret",
		[]string{"native runner", "no credential"}},
	{contributorGuideRepoPath, "where governance is recorded",
		[]string{"governance.md", "releasing.md"}},

	{supportProtocolRepoPath, "what each status requires and how a subject moves",
		[]string{"promotion and demotion", "experimental` → `preview` → `stable`", "demotion"}},
	{supportProtocolRepoPath, "that Python stays experimental and non-default",
		[]string{"@putnami/python", "experimental", "default: false", "parity"}},

	{readmeRepoPath, "where a reader finds who decides",
		[]string{"governance.md", "releasing.md"}},
}

// missingGovernanceAnswers reports one message per unanswered question. It is a
// pure function of the documents so the non-vacuity test can drive it with an
// empty record.
func missingGovernanceAnswers(documents map[string]string) []string {
	var missing []string
	for _, question := range governanceQuestions {
		lowered := strings.ToLower(documents[question.path])
		var absent []string
		for _, phrase := range question.phrases {
			if !strings.Contains(lowered, strings.ToLower(phrase)) {
				absent = append(absent, phrase)
			}
		}
		if len(absent) > 0 {
			missing = append(missing, fmt.Sprintf("%s does not record %s (missing: %s)",
				question.path, question.question, strings.Join(absent, ", ")))
		}
	}
	sort.Strings(missing)
	return missing
}
