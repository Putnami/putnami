package documents

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/tooling/cli/internal/store"
)

// The release rehearsal answers one question over the current tree:
// could this tree become the intended public release? It is read-only by
// construction — it enumerates the candidate cut through the public-cut
// helpers, reads the files that decide the answer, and renders a verdict. It
// never builds, publishes, tags, pushes, rewrites history, or changes
// repository visibility, and TestReleaseRehearsalPerformsNoDestructiveOperation
// keeps it that way.
//
// The gate lives in a test for the same reason the public-cut scrub does: it is
// release policy over the repository, not a user-facing command. It reuses that
// scrub rather than reimplementing it.
//
// NO-GO is a first-class outcome. The rehearsal never fails because a release
// input is missing; it fails when the RECORDED verdict stops matching the tree.
// That is what makes it repeatable: the same tree always renders the same
// block, and any change to a release input forces a human to re-record it.

const (
	// The plan and the recorded report stay in the CLI module: RELEASE.md and
	// ADR 0009 link them by path. Both are declared cache-key inputs of THIS
	// project, which is what TestReleaseRehearsalVerdictInputsAreDeclaredCacheKeyInputs
	// checks.
	releasePlanModulePath            = "internal/cli/testdata/release-plan.json"
	releasePlanRepoPath              = cliModuleDir + "/" + releasePlanModulePath
	releaseReportModulePath          = "doc/reports/release-rehearsal.md"
	releaseReportRepoPath            = cliModuleDir + "/" + releaseReportModulePath
	releaseRehearsalSourceModule     = "release_rehearsal_test.go"
	releaseRehearsalSourceRepoPath   = cliDocumentsProjectDir + "/" + releaseRehearsalSourceModule
	releaseRehearsalBlockBegin       = "<!-- rehearsal:generated:begin -->"
	releaseRehearsalBlockEnd         = "<!-- rehearsal:generated:end -->"
	releaseWorkspaceManifestPath     = "putnami.workspace.json"
	releaseSeriesChangelogPath       = "tooling/CHANGELOG.md"
	releaseProjectManifestName       = "putnami.json"
	releaseSupportManifestPath       = "putnami.support.json"
	releaseIgnoreFilePath            = ".gitignore"
	releaseScopeSchema               = "https://putnami.dev/schemas/putnami-scope.json"
	releasePlatformMatrixPath        = "tooling/extension-sdk/pkgmeta/platforms.go"
	releaseLicensePath               = "LICENSE.md"
	releaseContractPath              = "RELEASE.md"
	releaseProvenancePolicyPath      = "tooling/cli/doc/release-provenance.md"
	releaseMigrationGuidePath        = "tooling/cli/doc/21-compatibility-and-migration.md"
	releaseCompatibilityDecisionPath = "tooling/cli/doc/adr/0010-compatibility-budget.md"
	releaseInstallerPath             = "tooling/cli/scripts/install.sh"
	releaseSmokePath                 = "tooling/cli/scripts/smoke-check-release.sh"
	releasePlanProtocolVersion       = 1
	releaseArchiveMatrixDeclaration  = "archivePlatforms = []ArchivePlatform{"
	// releaseOwnerSelf marks a blocker this repeatable rehearsal owns, as
	// opposed to one waiting on a sibling release workstream.
	releaseOwnerSelf = "release-rehearsal"
	// releaseMaxBlockers bounds how many blockers one check reports, so a
	// systemic failure produces a reviewable verdict instead of a wall of text.
	releaseMaxBlockers = 3
)

// releaseInterestingFiles are read in full because a check reads their content.
// Every one of them is a declared test input of @putnami/cli-documents
// (putnami.json, options.test.filePatterns), so a change to any of them re-runs
// this gate instead of restoring a stale pass.
var releaseInterestingFiles = []string{
	releaseWorkspaceManifestPath,
	releaseSeriesChangelogPath,
	releaseSupportManifestPath,
	releaseIgnoreFilePath,
	releasePlatformMatrixPath,
	releaseLicensePath,
	releaseContractPath,
	releaseProvenancePolicyPath,
	releaseMigrationGuidePath,
	releaseCompatibilityDecisionPath,
	releaseInstallerPath,
	releaseSmokePath,
	"tooling/cli/internal/lockfile/testdata/prior-releases/provenance.json",
	"protocols/cli/testdata/prior-releases/provenance.json",
	"protocols/extension/testdata/prior-releases/provenance.json",
}

// releasePriorReleaseCorpora are the canonical compatibility artifacts. Each
// corpus is guarded by its own reader tests; the rehearsal additionally reads
// the provenance records and bytes so the migration check cannot pass against
// an obsolete umbrella directory or an empty placeholder.
var releasePriorReleaseCorpora = []struct {
	name   string
	prefix string
}{
	{name: "lock", prefix: "tooling/cli/internal/lockfile/testdata/prior-releases/"},
	{name: "machine-result", prefix: "protocols/cli/testdata/prior-releases/"},
	{name: "extension-contract", prefix: "protocols/extension/testdata/prior-releases/"},
}

// releaseGovernanceFiles is the code-owned minimum governance surface of a
// public root. The plan may add requirements; it may not remove one of these.
var releaseGovernanceFiles = []string{
	".github/ISSUE_TEMPLATE/bug_report.md",
	".github/ISSUE_TEMPLATE/feature_request.md",
	".github/PULL_REQUEST_TEMPLATE.md",
	"CODE_OF_CONDUCT.md",
	"CONTRIBUTING.md",
	"LICENSE.md",
	"RELEASE.md",
	"SECURITY.md",
}

// releaseSupportStatuses is the classification vocabulary of the support
// catalog. A status outside it is an unreviewable public promise.
var releaseSupportStatuses = map[string]bool{
	"stable":       true,
	"preview":      true,
	"experimental": true,
}

type releaseRequirementKind string

const (
	releaseRequirementFile        releaseRequirementKind = "file"
	releaseRequirementTreePrefix  releaseRequirementKind = "tree-prefix"
	releaseRequirementPhrase      releaseRequirementKind = "phrase"
	releaseRequirementIgnoreEntry releaseRequirementKind = "ignore-entry"
)

var releaseRequirementKinds = map[releaseRequirementKind]bool{
	releaseRequirementFile:        true,
	releaseRequirementTreePrefix:  true,
	releaseRequirementPhrase:      true,
	releaseRequirementIgnoreEntry: true,
}

// releaseRequirement is one declarative input the release needs. It is data,
// never a command: the plan decoder rejects unknown fields, so no field can
// smuggle something executable into a read-only rehearsal.
type releaseRequirement struct {
	Kind   releaseRequirementKind `json:"kind"`
	Target string                 `json:"target"`
	Phrase string                 `json:"phrase,omitempty"`
	Input  string                 `json:"input"`
	Owner  string                 `json:"owner"`
}

type releasePlanCheck struct {
	ID           string               `json:"id"`
	Evidence     string               `json:"evidence"`
	Requirements []releaseRequirement `json:"requirements"`
}

type releasePublishStep struct {
	ID            string   `json:"id"`
	Channels      []string `json:"channels"`
	Artifacts     []string `json:"artifacts"`
	RollbackPoint string   `json:"rollbackPoint"`
	Reversal      string   `json:"reversal"`
}

type releaseExclusion struct {
	ID        string   `json:"id"`
	Channels  []string `json:"channels"`
	Artifacts []string `json:"artifacts"`
	Reason    string   `json:"reason"`
}

type releaseFinalSwitch struct {
	Step          string `json:"step"`
	Owner         string `json:"owner"`
	Excluded      bool   `json:"excluded"`
	RollbackPoint string `json:"rollbackPoint"`
	Reversal      string `json:"reversal"`
}

type releaseLabelAxis struct {
	Prefix    string `json:"prefix"`
	Authority string `json:"authority,omitempty"`
}

type releaseFreshRoot struct {
	DefaultBranch string             `json:"defaultBranch"`
	Visibility    string             `json:"visibility"`
	Description   string             `json:"description"`
	Topics        []string           `json:"topics"`
	Scopes        []string           `json:"scopes"`
	Labels        []string           `json:"labels"`
	LabelAxes     []releaseLabelAxis `json:"labelAxes"`
}

type releaseSeriesPlan struct {
	Series        string   `json:"series"`
	License       string   `json:"license"`
	FutureLicense string   `json:"futureLicense"`
	Targets       []string `json:"targets"`
}

type releasePlan struct {
	ProtocolVersion int                  `json:"protocolVersion"`
	Release         releaseSeriesPlan    `json:"release"`
	FreshRoot       releaseFreshRoot     `json:"freshRoot"`
	PublishOrder    []releasePublishStep `json:"publishOrder"`
	Excluded        []releaseExclusion   `json:"excluded"`
	FinalSwitch     releaseFinalSwitch   `json:"finalSwitch"`
	Checks          []releasePlanCheck   `json:"checks"`
}

// releaseArtifact is one publishable project resolved from the candidate cut.
type releaseArtifact struct {
	name     string
	channels []string
}

// releaseRehearsalInput is everything a check may read. It is built once from
// the candidate cut for the repository run and constructed literally by the
// focused tests, so every check is provable without a repository.
type releaseRehearsalInput struct {
	paths       []string
	contents    map[string]string
	plan        *releasePlan
	artifacts   []releaseArtifact
	scrubErrors []string
}

func (in *releaseRehearsalInput) has(target string) bool {
	index := sort.SearchStrings(in.paths, target)
	return index < len(in.paths) && in.paths[index] == target
}

func (in *releaseRehearsalInput) hasPrefix(prefix string) bool {
	index := sort.SearchStrings(in.paths, prefix)
	return index < len(in.paths) && strings.HasPrefix(in.paths[index], prefix)
}

func (in *releaseRehearsalInput) read(target string) (string, bool) {
	contents, ok := in.contents[target]
	return contents, ok
}

type releaseBlocker struct {
	input string
	owner string
}

func (b releaseBlocker) String() string {
	return b.input + " (" + b.owner + ")"
}

type releaseCheckResult struct {
	id       string
	evidence string
	blockers []releaseBlocker
}

// releaseCheck binds one required evidence item to the assertion that proves
// it. The table is the contract: the release plan must declare exactly these
// ten evidence items, and a check with neither an assertion nor a declared
// requirement is reported as vacuous rather than passing.
type releaseCheck struct {
	id       string
	evidence string
	assert   func(*releaseRehearsalInput) []releaseBlocker
}

var releaseChecks = []releaseCheck{
	{id: "version-and-artifact-manifest", evidence: "version and artifact manifest", assert: releaseAssertVersionManifest},
	{id: "checksums-and-provenance", evidence: "checksums and provenance", assert: releaseAssertChecksumsAndProvenance},
	{id: "support-report", evidence: "support report", assert: releaseAssertSupportReport},
	{id: "migration-report", evidence: "migration report", assert: releaseAssertMigrationReport},
	{id: "scrub-result", evidence: "scrub result", assert: releaseAssertScrubResult},
	{id: "golden-path-result", evidence: "golden-path result"},
	{id: "governance-checklist", evidence: "governance checklist", assert: releaseAssertGovernance},
	{id: "label-scope-bootstrap-plan", evidence: "label/scope bootstrap plan", assert: releaseAssertLabelScopeBootstrap},
	{id: "publish-order", evidence: "publish order", assert: releaseAssertPublishOrder},
	{id: "rollback-plan", evidence: "rollback plan", assert: releaseAssertRollbackPlan},
}

// TestReleaseRehearsalReportRecordsTheCurrentVerdict is the rehearsal. It
// renders the verdict for this tree and compares it with the committed report.
// A blocked check does NOT fail it — an unrecorded verdict does.
func TestReleaseRehearsalReportRecordsTheCurrentVerdict(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "recorded-verdict", "the-verdict-is-deterministic-for-a-tree")
	input := buildReleaseRehearsalInput(t)
	results := runReleaseRehearsal(input)
	t.Log(releaseEvidenceDump(input))

	reportPath := filepath.Join(cliModuleRoot(t), filepath.FromSlash(releaseReportModulePath))
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read the rehearsal report %s: %v", releaseReportRepoPath, err)
	}
	recorded, err := releaseRecordedBlock(string(data))
	if err != nil {
		t.Fatalf("read the recorded verdict in %s: %v", releaseReportRepoPath, err)
	}
	computed := renderReleaseRehearsalBlock(results)
	if recorded != computed {
		t.Errorf("the recorded release verdict no longer matches this tree.\n"+
			"Replace the generated block in %s with the block below; never hand-edit a verdict.\n\n"+
			"recorded:\n%s\n\ncomputed:\n%s", releaseReportRepoPath, recorded, computed)
	}
}

// TestReleaseRehearsalVerdictInputsAreDeclaredCacheKeyInputs pins the property
// the recorded verdict rests on. The gate above only fails on drift if it RUNS:
// a task whose cache key ignores a file can be answered from a warm entry
// computed against a different version of that file, so a verdict-bearing file
// outside the key turns the drift check into a no-op that reports success.
//
// It is not a theoretical hole. The recorded report and the release plan are a
// Markdown file and a testdata JSON file, and neither shape is selected by the
// Go task's own inputs (**/*.go, go.mod, putnami.json), so both need an
// explicit declaration. Asserting it here, against the same matcher the cache
// key uses, is what keeps the declaration honest as the plan names new targets.
func TestReleaseRehearsalVerdictInputsAreDeclaredCacheKeyInputs(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "keyed-verdict-inputs", "verdict-inputs-are-declared-cache-key-inputs")
	patterns := releaseDeclaredTestInputs(t)
	plan := releaseCommittedPlan(t)

	required := map[string]string{
		releaseReportRepoPath: "the recorded verdict",
		releasePlanRepoPath:   "the release plan",
		// The artifact scan reads every project manifest in the tree, so a new
		// publishable project must be able to block the publish-order check.
		"a/b/" + releaseProjectManifestName: "the project manifests the artifact scan reads",
	}
	for _, file := range releaseInterestingFiles {
		required[file] = "a file a check reads in full"
	}
	for _, file := range releaseGovernanceFiles {
		required[file] = "the governance surface"
	}
	for _, axis := range plan.FreshRoot.LabelAxes {
		if axis.Authority != "" {
			required[axis.Authority] = "the authority for label axis " + axis.Prefix
		}
	}
	for _, check := range plan.Checks {
		for _, requirement := range check.Requirements {
			target := requirement.Target
			switch requirement.Kind {
			case releaseRequirementIgnoreEntry:
				target = releaseIgnoreFilePath
			case releaseRequirementTreePrefix:
				// A prefix is satisfied by any path under it, so the pattern has
				// to select the files, not the directory name.
				target += "any-candidate-file"
			}
			required[target] = "the target of a declared requirement of check " + check.ID
		}
	}

	for _, target := range releaseSorted(releaseKeys(required)) {
		if !store.SelectsPath(releaseProjectRelativePath(target), patterns) {
			t.Errorf("%s is %s, but no declared test input of @putnami/cli-documents selects it, "+
				"so a warm cache answers a changed tree with the recorded verdict.\n"+
				"Add a pattern covering %q to options.test.filePatterns in %s.",
				target, required[target], releaseProjectRelativePath(target), releaseProjectManifestName)
		}
	}
}

// TestReleaseRehearsalPerformsNoDestructiveOperation pins the hard invariant:
// rehearsing must never be able to release. The rehearsal reads files and the
// already-enumerated candidate cut, so its own source may not name a process,
// a filesystem mutation, or a publishing verb.
//
// Honest limitation: the scan is FILE-scoped. It proves this file names no
// mutating operation; it cannot prove that of a helper it calls. The rehearsal
// deliberately reuses the public-cut helpers, which do run git to enumerate the
// candidate cut, so "read-only" here means it neither mutates the tree nor
// publishes — not that no subprocess exists anywhere beneath it. Keep the
// mutating surface out of THIS file for the guard to stay meaningful.
func TestReleaseRehearsalPerformsNoDestructiveOperation(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "read-only-rehearsal", "the-rehearsal-performs-no-destructive-operation")
	source, err := os.ReadFile(filepath.Join(projectRoot(t), filepath.FromSlash(releaseRehearsalSourceModule)))
	if err != nil {
		t.Fatalf("read the rehearsal source: %v", err)
	}
	// An ALLOWLIST, not a denylist. A list of forbidden calls only rules out the
	// mutating primitives someone thought to name: the one-call
	// create-truncate-write opener, and the hard-link maker, are both absent
	// from any plausible denylist, and either would let this file mutate the
	// tree with the guard still green. (They are described rather than spelled
	// because this scan reads its own source.) Every os/exec symbol the file
	// names must instead be admitted here explicitly, so adding a new one is a
	// deliberate act with a reviewer looking at this list.
	allowed := map[string]bool{
		// Reads only. Nothing that creates, writes, moves, deletes, or spawns.
		"os." + "ReadFile": true,
		"os." + "ReadDir":  true,
		"os." + "Stat":     true,
		"os." + "DirFS":    true,
	}
	symbol := regexp.MustCompile(`\b(?:os|exec)\.[A-Z][A-Za-z0-9_]*`)
	matched, reported := 0, map[string]bool{}
	for _, match := range symbol.FindAllString(string(source), -1) {
		matched++
		if allowed[match] || reported[match] {
			continue
		}
		reported[match] = true
		t.Errorf("the release rehearsal names %s, which is not in the read-only allowlist; "+
			"the rehearsal must neither mutate the tree nor spawn a process", match)
	}
	if matched == 0 {
		t.Fatal("the scan matched no os/exec symbol at all; the read-only guard is vacuous")
	}

	// Publishing verbs stay a denylist: they are command text in strings and
	// comments, not Go symbols, so there is no closed vocabulary to allow from.
	// Spelled in halves so the list never matches itself.
	forbidden := []string{
		"git " + "push", "git " + "tag", "git " + "commit", "git " + "filter",
		"gh " + "repo", "gh " + "release", "npm " + "publish",
		"putnami " + "publish", "putnami " + "deploy",
	}
	for _, token := range forbidden {
		if strings.Contains(string(source), token) {
			t.Errorf("the release rehearsal names the publishing operation %q; it must publish nothing", token)
		}
	}
}

func TestReleaseRehearsalVerdictIsGoOnlyWhenEveryCheckPasses(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "recorded-verdict", "go-only-when-every-check-passes")
	input := releaseRehearsalFixture()
	block := renderReleaseRehearsalBlock(runReleaseRehearsal(input))
	if !strings.Contains(block, "**Verdict: GO** — all 10 release checks pass.") {
		t.Fatalf("a complete fixture did not reach GO:\n%s", block)
	}

	// Removing one declared input must produce NO-GO naming the owning workstream,
	// never a silent pass.
	input.paths = releaseWithout(input.paths, releaseMigrationGuidePath)
	delete(input.contents, releaseMigrationGuidePath)
	block = renderReleaseRehearsalBlock(runReleaseRehearsal(input))
	if !strings.Contains(block, "**Verdict: NO-GO** — 1 of 10 release checks are blocked: migration-report.") {
		t.Fatalf("a missing declared input did not block the migration report:\n%s", block)
	}
	if !strings.Contains(block, "the public compatibility and migration guide (release-rehearsal)") {
		t.Fatalf("the verdict did not name the missing input and its owner:\n%s", block)
	}
}

func TestReleaseRehearsalRendersTheSameVerdictForTheSameTree(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "recorded-verdict", "the-verdict-is-deterministic-for-a-tree")
	input := releaseRehearsalFixture()
	first := renderReleaseRehearsalBlock(runReleaseRehearsal(input))

	// Input order must not reach the verdict: the plan is a set of checks and
	// the candidate cut is a set of artifacts.
	sort.Slice(input.plan.Checks, func(i, j int) bool { return input.plan.Checks[i].ID > input.plan.Checks[j].ID })
	sort.Slice(input.artifacts, func(i, j int) bool { return input.artifacts[i].name > input.artifacts[j].name })
	if second := renderReleaseRehearsalBlock(runReleaseRehearsal(input)); second != first {
		t.Fatalf("the verdict moved with input order.\nfirst:\n%s\n\nsecond:\n%s", first, second)
	}
}

func TestReleaseRehearsalReportsEveryRequiredEvidenceItem(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "ten-evidence-items", "every-required-evidence-item-is-reported")
	want := []string{
		"version and artifact manifest", "checksums and provenance", "support report",
		"migration report", "scrub result", "golden-path result", "governance checklist",
		"label/scope bootstrap plan", "publish order", "rollback plan",
	}
	got := make([]string, 0, len(releaseChecks))
	ids := make(map[string]bool, len(releaseChecks))
	for _, check := range releaseChecks {
		got = append(got, check.evidence)
		if ids[check.id] {
			t.Fatalf("duplicate release check id %q", check.id)
		}
		ids[check.id] = true
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("release evidence items = %v, want %v", got, want)
	}
}

func TestReleaseRehearsalRejectsAVacuousCheck(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "no-vacuous-check", "a-vacuous-or-undeclared-check-proves-nothing")
	input := releaseRehearsalFixture()
	for index, check := range input.plan.Checks {
		if check.ID == "golden-path-result" {
			input.plan.Checks[index].Requirements = nil
		}
	}
	block := renderReleaseRehearsalBlock(runReleaseRehearsal(input))
	if !strings.Contains(block, "no requirement and no assertion") {
		t.Fatalf("a check with neither a requirement nor an assertion passed vacuously:\n%s", block)
	}
}

func TestReleaseRehearsalReportsAnUndeclaredCheck(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "no-vacuous-check", "a-vacuous-or-undeclared-check-proves-nothing")
	input := releaseRehearsalFixture()
	input.plan.Checks = input.plan.Checks[1:]
	block := renderReleaseRehearsalBlock(runReleaseRehearsal(input))
	if !strings.Contains(block, "the release plan declares no evidence entry") {
		t.Fatalf("a check missing from the plan did not block:\n%s", block)
	}

	input = releaseRehearsalFixture()
	input.plan.Checks[0].Evidence = "renamed evidence"
	block = renderReleaseRehearsalBlock(runReleaseRehearsal(input))
	if !strings.Contains(block, "declares evidence \"renamed evidence\"") {
		t.Fatalf("a renamed evidence item did not block:\n%s", block)
	}
}

func TestReleaseRequirementKindsReadOnlyTheTreeTheyName(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "read-only-rehearsal", "the-rehearsal-performs-no-destructive-operation")
	input := releaseRehearsalFixture()
	for _, testCase := range []struct {
		name        string
		requirement releaseRequirement
		want        bool
	}{
		{"present file", releaseRequirement{Kind: releaseRequirementFile, Target: "LICENSE.md"}, true},
		{"absent file", releaseRequirement{Kind: releaseRequirementFile, Target: "ABSENT.md"}, false},
		{"present prefix", releaseRequirement{Kind: releaseRequirementTreePrefix, Target: ".github/workflows/"}, true},
		{"absent prefix", releaseRequirement{Kind: releaseRequirementTreePrefix, Target: ".circleci/"}, false},
		{"wrapped phrase", releaseRequirement{Kind: releaseRequirementPhrase, Target: "RELEASE.md", Phrase: "release rehearsal records"}, true},
		{"absent phrase", releaseRequirement{Kind: releaseRequirementPhrase, Target: "RELEASE.md", Phrase: "compatibility budget"}, false},
		{"unreadable phrase target", releaseRequirement{Kind: releaseRequirementPhrase, Target: "ABSENT.md", Phrase: "anything"}, false},
		{"root-anchored ignore entry", releaseRequirement{Kind: releaseRequirementIgnoreEntry, Target: ".context/"}, true},
		{"ignore entry committed without a trailing slash", releaseRequirement{Kind: releaseRequirementIgnoreEntry, Target: ".putnami/"}, true},
		{"negated ignore entry", releaseRequirement{Kind: releaseRequirementIgnoreEntry, Target: ".build/"}, false},
		{"absent ignore entry", releaseRequirement{Kind: releaseRequirementIgnoreEntry, Target: ".secrets/"}, false},
		{"unknown kind", releaseRequirement{Kind: "run", Target: "anything"}, false},
	} {
		if got := releaseRequirementSatisfied(input, testCase.requirement); got != testCase.want {
			t.Errorf("%s: satisfied = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

func TestReleaseVersionManifestFollowsTheBuilderMatrixAndWorkspaceVersion(t *testing.T) {
	t.Parallel()
	input := releaseRehearsalFixture()
	if blockers := releaseAssertVersionManifest(input); len(blockers) != 0 {
		t.Fatalf("a consistent manifest blocked: %v", blockers)
	}

	input.plan.Release.Series = "9.9.9"
	if !releaseBlockersMention(releaseAssertVersionManifest(input), "declares series") {
		t.Error("a release series that disagrees with the workspace version did not block")
	}

	input = releaseRehearsalFixture()
	input.plan.Release.Targets = []string{"linux/amd64"}
	if !releaseBlockersMention(releaseAssertVersionManifest(input), "builder archive matrix") {
		t.Error("release targets narrower than the builder matrix did not block")
	}

	input = releaseRehearsalFixture()
	input.contents[releasePlatformMatrixPath] = "package pkgmeta\n"
	if !releaseBlockersMention(releaseAssertVersionManifest(input), "builder archive matrix") {
		t.Error("an unreadable builder matrix did not block")
	}

	input = releaseRehearsalFixture()
	input.artifacts = nil
	if !releaseBlockersMention(releaseAssertVersionManifest(input), "no publishable artifact") {
		t.Error("an empty artifact manifest did not block")
	}

	input = releaseRehearsalFixture()
	delete(input.contents, releaseSeriesChangelogPath)
	if !releaseBlockersMention(releaseAssertVersionManifest(input), "released series") {
		t.Error("a missing released series did not block")
	}
}

func TestReleaseChecksumsAndProvenancePinsTheImplementedTrustContract(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "provenance-contract", "the-trust-contract-is-pinned")
	input := releaseRehearsalFixture()
	if blockers := releaseAssertChecksumsAndProvenance(input); len(blockers) != 0 {
		t.Fatalf("a complete provenance contract blocked: %v", blockers)
	}

	for _, testCase := range []struct {
		name, target, remove, want string
	}{
		{
			name: "policy stops naming unsigned artifacts", target: releaseProvenancePolicyPath,
			remove: "Current Putnami release artifacts are unsigned.", want: "release provenance policy",
		},
		{
			name: "installer stops calling its verifier", target: releaseInstallerPath,
			remove: "verify_download_integrity", want: "public installer",
		},
		{
			name: "smoke stops requiring an advertised digest", target: releaseSmokePath,
			remove: "advertised no SHA-256 (X-Integrity or RFC 9530 Digest)", want: "release smoke",
		},
	} {
		input = releaseRehearsalFixture()
		input.contents[testCase.target] = strings.ReplaceAll(input.contents[testCase.target], testCase.remove, "removed")
		if !releaseBlockersMention(releaseAssertChecksumsAndProvenance(input), testCase.want) {
			t.Errorf("%s did not block on %q", testCase.name, testCase.want)
		}
	}
}

func TestReleaseMigrationReportUsesCanonicalImmutableEvidence(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "canonical-migration-evidence", "migration-evidence-is-canonical-and-immutable")
	input := releaseRehearsalFixture()
	if blockers := releaseAssertMigrationReport(input); len(blockers) != 0 {
		t.Fatalf("canonical migration evidence blocked: %v", blockers)
	}

	input = releaseRehearsalFixture()
	input.contents[releaseCompatibilityDecisionPath] = "# compatibility decision without the evidence rule\n"
	if !releaseBlockersMention(releaseAssertMigrationReport(input), "compatibility decision") {
		t.Error("a compatibility ADR without its immutable-evidence rule did not block")
	}

	input = releaseRehearsalFixture()
	fixturePath := "protocols/cli/testdata/prior-releases/result-v1.json"
	input.contents[fixturePath] = "changed bytes"
	if !releaseBlockersMention(releaseAssertMigrationReport(input), "SHA-256") {
		t.Error("changed prior-release bytes did not block the migration report")
	}

	input = releaseRehearsalFixture()
	undeclared := "protocols/extension/testdata/prior-releases/unrecorded.json"
	input.paths = append(input.paths, undeclared)
	sort.Strings(input.paths)
	input.contents[undeclared] = "{}"
	if !releaseBlockersMention(releaseAssertMigrationReport(input), "no provenance record") {
		t.Error("an unprovenanced compatibility fixture did not block the migration report")
	}
}

func TestReleaseSupportReportRejectsAnUnreviewablePromise(t *testing.T) {
	t.Parallel()
	input := releaseRehearsalFixture()
	if blockers := releaseAssertSupportReport(input); len(blockers) != 0 {
		t.Fatalf("a valid support catalog blocked: %v", blockers)
	}

	for _, testCase := range []struct {
		name, catalog, want string
	}{
		{"unreadable", "", "support catalog"},
		{"invalid json", "{", "support catalog"},
		{"wrong protocol", `{"protocolVersion":2,"entries":[{"id":"@putnami/cli","kind":"package","status":"stable"}]}`, "protocol version"},
		{"empty", `{"protocolVersion":1,"entries":[]}`, "classifies nothing"},
		{"unknown status", `{"protocolVersion":1,"entries":[{"id":"@putnami/cli","kind":"package","status":"blessed"}]}`, "unknown status"},
		{"missing kind", `{"protocolVersion":1,"entries":[{"id":"@putnami/cli","kind":"","status":"stable"}]}`, "incomplete entry"},
		{"duplicate", `{"protocolVersion":1,"entries":[{"id":"@putnami/cli","kind":"package","status":"stable"},{"id":"@putnami/cli","kind":"package","status":"preview"}]}`, "classifies @putnami/cli twice"},
		{"cli unclassified", `{"protocolVersion":1,"entries":[{"id":"@putnami/runtime","kind":"package","status":"stable"}]}`, "@putnami/cli"},
	} {
		input = releaseRehearsalFixture()
		if testCase.catalog == "" {
			delete(input.contents, releaseSupportManifestPath)
		} else {
			input.contents[releaseSupportManifestPath] = testCase.catalog
		}
		if !releaseBlockersMention(releaseAssertSupportReport(input), testCase.want) {
			t.Errorf("%s support catalog did not block on %q", testCase.name, testCase.want)
		}
	}
}

func TestReleaseScrubResultReportsThePublicCutOutcome(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "reused-scrub", "the-scrub-result-comes-from-the-public-cut-scanner")
	input := releaseRehearsalFixture()
	if blockers := releaseAssertScrubResult(input); len(blockers) != 0 {
		t.Fatalf("a clean scrub blocked: %v", blockers)
	}
	input.scrubErrors = []string{"new debt: orphaned-history notes.md:7 (history)", "second"}
	blockers := releaseAssertScrubResult(input)
	if !releaseBlockersMention(blockers, "2 unresolved finding") || !releaseBlockersMention(blockers, "notes.md:7") {
		t.Fatalf("scrub findings were not carried into the verdict: %v", blockers)
	}
}

func TestReleaseGovernanceChecklistCoversTheMinimumPublicSurface(t *testing.T) {
	t.Parallel()
	input := releaseRehearsalFixture()
	if blockers := releaseAssertGovernance(input); len(blockers) != 0 {
		t.Fatalf("a complete governance surface blocked: %v", blockers)
	}

	input.paths = releaseWithout(input.paths, "SECURITY.md")
	if !releaseBlockersMention(releaseAssertGovernance(input), "SECURITY.md") {
		t.Error("a missing governance document did not block")
	}

	input = releaseRehearsalFixture()
	input.contents[releaseLicensePath] = "# Some other license\n"
	if !releaseBlockersMention(releaseAssertGovernance(input), "Functional Source License") {
		t.Error("a license that is not the declared one did not block")
	}

	input = releaseRehearsalFixture()
	delete(input.contents, releaseLicensePath)
	if !releaseBlockersMention(releaseAssertGovernance(input), "cannot be read") {
		t.Error("an unreadable license did not block")
	}
}

func TestReleaseLabelScopeBootstrapCoversEveryScopeAndGroup(t *testing.T) {
	t.Parallel()
	input := releaseRehearsalFixture()
	if blockers := releaseAssertLabelScopeBootstrap(input); len(blockers) != 0 {
		t.Fatalf("a complete bootstrap plan blocked: %v", blockers)
	}

	for _, testCase := range []struct {
		name string
		bend func(*releaseRehearsalInput)
		want string
	}{
		{"missing scope label", func(in *releaseRehearsalInput) {
			in.plan.FreshRoot.Labels = releaseWithout(in.plan.FreshRoot.Labels, "scope/tooling")
		}, "scope/tooling"},
		{"unsorted labels", func(in *releaseRehearsalInput) {
			in.plan.FreshRoot.Labels = append([]string{"zz/last"}, in.plan.FreshRoot.Labels...)
		}, "sorted and unique"},
		{"axis with neither label nor authority", func(in *releaseRehearsalInput) {
			in.plan.FreshRoot.LabelAxes = append(in.plan.FreshRoot.LabelAxes, releaseLabelAxis{Prefix: "kind/"})
		}, "kind/"},
		{"axis authority absent from the tree", func(in *releaseRehearsalInput) {
			in.plan.FreshRoot.LabelAxes = append(in.plan.FreshRoot.LabelAxes, releaseLabelAxis{Prefix: "kind/", Authority: "ABSENT.md"})
		}, "ABSENT.md"},
		{"scope drift", func(in *releaseRehearsalInput) {
			in.plan.FreshRoot.Scopes = []string{"tooling"}
		}, "scope set"},
		{"no scope manifest", func(in *releaseRehearsalInput) {
			delete(in.contents, "go/"+releaseProjectManifestName)
			delete(in.contents, "tooling/"+releaseProjectManifestName)
		}, "top-level scope"},
		{"private fresh root", func(in *releaseRehearsalInput) { in.plan.FreshRoot.Visibility = "private" }, "visibility"},
		{"no description", func(in *releaseRehearsalInput) { in.plan.FreshRoot.Description = "" }, "fresh-root metadata"},
		{"no topics", func(in *releaseRehearsalInput) { in.plan.FreshRoot.Topics = nil }, "fresh-root metadata"},
		{"no default branch", func(in *releaseRehearsalInput) { in.plan.FreshRoot.DefaultBranch = "" }, "fresh-root metadata"},
	} {
		input = releaseRehearsalFixture()
		testCase.bend(input)
		if !releaseBlockersMention(releaseAssertLabelScopeBootstrap(input), testCase.want) {
			t.Errorf("%s did not block on %q", testCase.name, testCase.want)
		}
	}
}

func TestReleasePublishOrderCoversEveryArtifactChannelPairExactlyOnce(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "publish-order-coverage", "every-artifact-channel-pair-is-claimed-exactly-once")
	input := releaseRehearsalFixture()
	if blockers := releaseAssertPublishOrder(input); len(blockers) != 0 {
		t.Fatalf("a complete publish order blocked: %v", blockers)
	}

	input = releaseRehearsalFixture()
	input.artifacts = append(input.artifacts, releaseArtifact{name: "brand-new", channels: []string{"npm"}})
	if !releaseBlockersMention(releaseAssertPublishOrder(input), "brand-new") {
		t.Error("a new publishable artifact did not need a publish position")
	}

	input = releaseRehearsalFixture()
	input.plan.PublishOrder = append(input.plan.PublishOrder, releasePublishStep{
		ID: "duplicate", Channels: []string{"go"}, Artifacts: []string{"go.putnami.dev/*"},
		RollbackPoint: "point", Reversal: "reversal",
	})
	if !releaseBlockersMention(releaseAssertPublishOrder(input), "twice") {
		t.Error("an artifact claimed by two steps did not block")
	}

	input = releaseRehearsalFixture()
	input.plan.PublishOrder[0].Artifacts = append(input.plan.PublishOrder[0].Artifacts, "never/matches")
	if !releaseBlockersMention(releaseAssertPublishOrder(input), "never/matches") {
		t.Error("a dead publish pattern did not block")
	}

	input = releaseRehearsalFixture()
	input.plan.PublishOrder[1].ID = input.plan.PublishOrder[0].ID
	if !releaseBlockersMention(releaseAssertPublishOrder(input), "reused") {
		t.Error("a reused publish step id did not block")
	}
}

func TestReleaseRollbackPlanNamesAReversalForEveryStep(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "rollback-completeness", "every-step-names-a-rollback-and-reversal")
	input := releaseRehearsalFixture()
	if blockers := releaseAssertRollbackPlan(input); len(blockers) != 0 {
		t.Fatalf("a complete rollback plan blocked: %v", blockers)
	}

	for _, testCase := range []struct {
		name string
		bend func(*releaseRehearsalInput)
		want string
	}{
		{"no rollback point", func(in *releaseRehearsalInput) { in.plan.PublishOrder[0].RollbackPoint = "" }, "rollback point"},
		{"no reversal", func(in *releaseRehearsalInput) { in.plan.PublishOrder[0].Reversal = "" }, "reversal"},
		{"no exclusion reason", func(in *releaseRehearsalInput) { in.plan.Excluded[0].Reason = "" }, "reason"},
		{"switch not excluded", func(in *releaseRehearsalInput) { in.plan.FinalSwitch.Excluded = false }, "must stay excluded"},
		{"switch unowned", func(in *releaseRehearsalInput) { in.plan.FinalSwitch.Owner = "" }, "final switch"},
	} {
		input = releaseRehearsalFixture()
		testCase.bend(input)
		if !releaseBlockersMention(releaseAssertRollbackPlan(input), testCase.want) {
			t.Errorf("%s did not block on %q", testCase.name, testCase.want)
		}
	}
}

func TestReleaseBlockersAreSortedDeduplicatedAndBounded(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "named-blockers", "blockers-are-named-sorted-and-bounded")
	blockers := releaseNormalizeBlockers([]releaseBlocker{
		{input: "b", owner: "#1"}, {input: "a", owner: "#1"}, {input: "a", owner: "#1"},
		{input: "c", owner: "#1"}, {input: "d", owner: "#1"}, {input: "e", owner: "#1"},
	})
	if len(blockers) != releaseMaxBlockers+1 {
		t.Fatalf("blockers = %v, want %d entries plus a summary", blockers, releaseMaxBlockers)
	}
	if blockers[0].input != "a" || blockers[1].input != "b" || blockers[2].input != "c" {
		t.Fatalf("blockers are not sorted and deduplicated: %v", blockers)
	}
	if !strings.Contains(blockers[3].input, "2 further blocking input") {
		t.Fatalf("the overflow summary is missing: %v", blockers)
	}
}

func TestReleasePlanParsesOnlyDeclarativeReviewedData(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "declarative-plan", "the-plan-is-declarative-reviewed-data")
	valid := releaseFixturePlanJSON
	if _, err := parseReleasePlan(valid); err != nil {
		t.Fatalf("the fixture plan did not parse: %v", err)
	}

	for _, testCase := range []struct{ name, plan, want string }{
		{"executable field", strings.Replace(valid, `"protocolVersion": 1`, `"protocolVersion": 1, "command": "gh api"`, 1), "command"},
		{"wrong protocol", strings.Replace(valid, `"protocolVersion": 1`, `"protocolVersion": 2`, 1), "protocol version"},
		{"unknown check", strings.Replace(valid, `"id": "publish-order"`, `"id": "invented-check"`, 1), "invented-check"},
		{"duplicate check", strings.Replace(valid, `"id": "publish-order"`, `"id": "rollback-plan"`, 1), "twice"},
		{"unknown requirement kind", strings.Replace(valid, `"kind": "file"`, `"kind": "shell"`, 1), "shell"},
		{"unowned requirement", strings.Replace(valid, `"owner": "release-rehearsal"`, `"owner": ""`, 1), "owner"},
		{"broken json", "{", "release plan"},
	} {
		if _, err := parseReleasePlan(testCase.plan); err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: err = %v, want it to mention %q", testCase.name, err, testCase.want)
		}
	}

	missing := strings.Replace(valid, `"id": "publish-order"`, `"id": "publish-order-typo"`, 1)
	if _, err := parseReleasePlan(missing); err == nil || !strings.Contains(err.Error(), "publish-order") {
		t.Errorf("a plan missing a required evidence item parsed: %v", err)
	}
}

func TestReleaseCommittedPlanMatchesTheGateContract(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "declarative-plan", "the-plan-is-declarative-reviewed-data")
	plan := releaseCommittedPlan(t)
	if !plan.FinalSwitch.Excluded {
		t.Fatal("the committed plan does not exclude the private-archive/public-root switch")
	}
	for _, check := range plan.Checks {
		for _, requirement := range check.Requirements {
			if requirement.Owner == releaseOwnerSelf {
				continue
			}
			if !releaseOwners[requirement.Owner] {
				t.Errorf("requirement %q names owner %q; owners are the release workstreams %v", requirement.Input, requirement.Owner, releaseSortedOwners())
			}
		}
	}
}

func TestReleaseCommittedPlanNamesCanonicalProvenanceAndMigrationEvidence(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "canonical-migration-evidence", "migration-evidence-is-canonical-and-immutable")
	plan := releaseCommittedPlan(t)
	want := map[string][]string{
		"checksums-and-provenance": {
			releaseProvenancePolicyPath,
			releaseInstallerPath,
			releaseSmokePath,
		},
		"migration-report": {
			releaseMigrationGuidePath,
			releaseCompatibilityDecisionPath,
			releasePriorReleaseCorpora[0].prefix,
			releasePriorReleaseCorpora[1].prefix,
			releasePriorReleaseCorpora[2].prefix,
		},
	}
	for _, check := range plan.Checks {
		targets, ok := want[check.ID]
		if !ok {
			continue
		}
		got := make([]string, 0, len(check.Requirements))
		for _, requirement := range check.Requirements {
			got = append(got, requirement.Target)
		}
		if strings.Join(releaseSorted(got), "|") != strings.Join(releaseSorted(targets), "|") {
			t.Errorf("%s targets = %v, want canonical %v", check.ID, releaseSorted(got), releaseSorted(targets))
		}
		delete(want, check.ID)
	}
	for check := range want {
		t.Errorf("release plan has no %s entry", check)
	}
}

func TestReleaseRecordedBlockRequiresExactlyOneGeneratedRegion(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "stable-recorded-fields", "the-recorded-block-is-stable-and-unbreakable")
	body := releaseRehearsalBlockBegin + "\nverdict\n" + releaseRehearsalBlockEnd
	got, err := releaseRecordedBlock("intro\n" + body + "\noutro\n")
	if err != nil || got != body {
		t.Fatalf("recorded block = %q, err = %v", got, err)
	}
	for _, testCase := range []struct{ name, report, want string }{
		{"no region", "intro only\n", "generated block"},
		{"unterminated", "intro\n" + releaseRehearsalBlockBegin + "\nverdict\n", "generated block"},
		{"two regions", body + "\n" + body + "\n", "exactly one"},
	} {
		if _, err := releaseRecordedBlock(testCase.report); err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: err = %v, want it to mention %q", testCase.name, err, testCase.want)
		}
	}
}

func TestReleaseTableCellsCannotBreakTheReport(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/release-rehearsal", "stable-recorded-fields", "the-recorded-block-is-stable-and-unbreakable")
	if got := releaseEscapeCell("a | b\nc"); got != `a \| b c` {
		t.Fatalf("escaped cell = %q", got)
	}
}

func runReleaseRehearsal(input *releaseRehearsalInput) []releaseCheckResult {
	declared := make(map[string]releasePlanCheck, len(input.plan.Checks))
	for _, check := range input.plan.Checks {
		declared[check.ID] = check
	}
	results := make([]releaseCheckResult, 0, len(releaseChecks))
	for _, check := range releaseChecks {
		var blockers []releaseBlocker
		planned, ok := declared[check.id]
		switch {
		case !ok:
			blockers = append(blockers, releaseBlocker{
				input: "the release plan declares no evidence entry for check " + check.id,
				owner: releaseOwnerSelf,
			})
		case planned.Evidence != check.evidence:
			blockers = append(blockers, releaseBlocker{
				input: fmt.Sprintf("the release plan declares evidence %q for check %s, the gate requires %q",
					planned.Evidence, check.id, check.evidence),
				owner: releaseOwnerSelf,
			})
		default:
			blockers = append(blockers, releaseRequirementBlockers(input, planned)...)
			if check.assert == nil && len(planned.Requirements) == 0 {
				blockers = append(blockers, releaseBlocker{
					input: "check " + check.id + " has no requirement and no assertion, so it proves nothing",
					owner: releaseOwnerSelf,
				})
			}
		}
		if check.assert != nil {
			blockers = append(blockers, check.assert(input)...)
		}
		results = append(results, releaseCheckResult{
			id: check.id, evidence: check.evidence, blockers: releaseNormalizeBlockers(blockers),
		})
	}
	return results
}

func releaseRequirementBlockers(input *releaseRehearsalInput, planned releasePlanCheck) []releaseBlocker {
	var blockers []releaseBlocker
	for _, requirement := range planned.Requirements {
		if releaseRequirementSatisfied(input, requirement) {
			continue
		}
		blockers = append(blockers, releaseBlocker{input: requirement.Input, owner: requirement.Owner})
	}
	return blockers
}

func releaseRequirementSatisfied(input *releaseRehearsalInput, requirement releaseRequirement) bool {
	switch requirement.Kind {
	case releaseRequirementFile:
		return input.has(requirement.Target)
	case releaseRequirementTreePrefix:
		return input.hasPrefix(requirement.Target)
	case releaseRequirementPhrase:
		contents, ok := input.read(requirement.Target)
		return ok && strings.Contains(releaseNormalize(contents), releaseNormalize(requirement.Phrase))
	case releaseRequirementIgnoreEntry:
		contents, ok := input.read(releaseIgnoreFilePath)
		if !ok {
			return false
		}
		want := releaseIgnoredPath(requirement.Target)
		if want == "" {
			return false
		}
		for line := range strings.SplitSeq(contents, "\n") {
			if releaseIgnoredPath(strings.TrimSpace(line)) == want {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// releaseIgnoredPath reduces a .gitignore entry to the path it guards. Git
// treats ".context/", ".context", "/.context/" and "/.context" as the same
// guard for a root-level directory, so a requirement that names one spelling
// must not report the tree as unguarded because a committed line chose
// another. A negation keeps its "!" and therefore never satisfies a
// requirement, which is the intended reading: "!x" un-ignores x.
func releaseIgnoredPath(entry string) string {
	return strings.Trim(entry, "/")
}

func releaseAssertVersionManifest(input *releaseRehearsalInput) []releaseBlocker {
	var blockers []releaseBlocker
	version, ok := releaseWorkspaceVersion(input)
	switch {
	case !ok:
		blockers = append(blockers, releaseBlocker{
			input: "the released series in " + releaseSeriesChangelogPath + " cannot be read", owner: releaseOwnerSelf,
		})
	case version != input.plan.Release.Series:
		blockers = append(blockers, releaseBlocker{
			input: fmt.Sprintf("the release plan declares series %q while %s declares %q",
				input.plan.Release.Series, releaseSeriesChangelogPath, version), owner: releaseOwnerSelf,
		})
	}
	matrix, ok := releaseBuilderMatrix(input)
	if !ok || strings.Join(matrix, ",") != strings.Join(releaseSorted(input.plan.Release.Targets), ",") {
		blockers = append(blockers, releaseBlocker{
			input: fmt.Sprintf("the release plan targets %v do not match the builder archive matrix %v in %s",
				releaseSorted(input.plan.Release.Targets), matrix, releasePlatformMatrixPath), owner: releaseOwnerSelf,
		})
	}
	if len(input.artifacts) == 0 {
		blockers = append(blockers, releaseBlocker{
			input: "no publishable artifact was resolved from the candidate cut", owner: releaseOwnerSelf,
		})
	}
	return blockers
}

func releaseAssertChecksumsAndProvenance(input *releaseRehearsalInput) []releaseBlocker {
	var blockers []releaseBlocker
	blockers = append(blockers, releaseAssertPhrases(input, releaseProvenancePolicyPath,
		"release provenance policy", []string{
			"The only digest algorithm accepted for release artifacts is **SHA-256**.",
			"The authoritative digest source for a registry install is the successful HTTPS download response",
			"`X-Integrity` takes precedence",
			"The release record must identify the builder before a candidate is promoted.",
			"Current Putnami release artifacts are unsigned.",
			"CLI archives are not currently promised to be byte-for-byte reproducible.",
			"Never replace bytes at an existing immutable version.",
		})...)
	blockers = append(blockers, releaseAssertPhrases(input, releaseInstallerPath,
		"public installer integrity path", []string{
			"read_advertised_integrity", "X-Integrity", "Digest", "sha-256",
			"compute_sha256", "verify_download_integrity", "refusing to install an unverified binary",
		})...)
	blockers = append(blockers, releaseAssertPhrases(input, releaseSmokePath,
		"release smoke integrity path", []string{
			"advertised no SHA-256 (X-Integrity or RFC 9530 Digest)",
			"Integrity verified", "without integrity verification", "installed_digest",
		})...)
	return blockers
}

func releaseAssertMigrationReport(input *releaseRehearsalInput) []releaseBlocker {
	var blockers []releaseBlocker
	blockers = append(blockers, releaseAssertPhrases(input, releaseMigrationGuidePath,
		"public compatibility and migration guide", []string{
			"This document is the **compatibility budget**",
			"## Lock file — `putnami.lock.json`",
			"## Machine result envelope",
			"## Extension contract stamp — `cliContract`",
			"## Support catalog — `putnami.support.json`",
			"## Evidence",
		})...)
	blockers = append(blockers, releaseAssertPhrases(input, releaseCompatibilityDecisionPath,
		"compatibility decision", []string{
			"A version increment ships either a MIGRATION",
			"Evidence is prior-release bytes, and the bytes are immutable",
		})...)
	for _, corpus := range releasePriorReleaseCorpora {
		blockers = append(blockers, releaseAssertPriorReleaseCorpus(input, corpus.name, corpus.prefix)...)
	}
	return blockers
}

func releaseAssertPhrases(input *releaseRehearsalInput, target, label string, phrases []string) []releaseBlocker {
	contents, ok := input.read(target)
	if !ok {
		return []releaseBlocker{{
			input: "the " + label + " " + target + " cannot be read", owner: releaseOwnerSelf,
		}}
	}
	normalized := releaseNormalize(contents)
	for _, phrase := range phrases {
		if !strings.Contains(normalized, releaseNormalize(phrase)) {
			return []releaseBlocker{{
				input: fmt.Sprintf("the %s %s no longer states %q", label, target, phrase), owner: releaseOwnerSelf,
			}}
		}
	}
	return nil
}

type releasePriorReleaseFixture struct {
	File         string `json:"file"`
	SourceCommit string `json:"sourceCommit"`
	SourcePath   string `json:"sourcePath"`
	Committed    string `json:"committed"`
	SHA256       string `json:"sha256"`
	Why          string `json:"why"`
}

type releasePriorReleaseProvenance struct {
	Fixtures []releasePriorReleaseFixture `json:"fixtures"`
	Gaps     []json.RawMessage            `json:"gaps"`
}

func releaseAssertPriorReleaseCorpus(input *releaseRehearsalInput, name, prefix string) []releaseBlocker {
	provenancePath := prefix + "provenance.json"
	contents, ok := input.read(provenancePath)
	if !ok {
		return []releaseBlocker{{
			input: "the " + name + " prior-release corpus has no readable provenance.json", owner: releaseOwnerSelf,
		}}
	}
	var provenance releasePriorReleaseProvenance
	if err := json.Unmarshal([]byte(contents), &provenance); err != nil {
		return []releaseBlocker{{
			input: "the " + name + " prior-release corpus provenance is not valid JSON", owner: releaseOwnerSelf,
		}}
	}
	var blockers []releaseBlocker
	if len(provenance.Fixtures) == 0 {
		blockers = append(blockers, releaseBlocker{
			input: "the " + name + " prior-release corpus declares no fixture", owner: releaseOwnerSelf,
		})
	}
	if provenance.Gaps == nil {
		blockers = append(blockers, releaseBlocker{
			input: "the " + name + " prior-release corpus does not explicitly record its gaps", owner: releaseOwnerSelf,
		})
	}
	recorded := make(map[string]bool, len(provenance.Fixtures))
	for _, fixture := range provenance.Fixtures {
		if fixture.File == "" || path.Base(fixture.File) != fixture.File ||
			!releaseIsLowerHex(fixture.SourceCommit, 40) || fixture.SourcePath == "" ||
			fixture.Committed == "" || fixture.Why == "" ||
			!releaseIsLowerHex(fixture.SHA256, 64) {
			blockers = append(blockers, releaseBlocker{
				input: fmt.Sprintf("the %s prior-release corpus has incomplete provenance for %q", name, fixture.File),
				owner: releaseOwnerSelf,
			})
			continue
		}
		if recorded[fixture.File] {
			blockers = append(blockers, releaseBlocker{
				input: fmt.Sprintf("the %s prior-release corpus records %s twice", name, fixture.File), owner: releaseOwnerSelf,
			})
			continue
		}
		recorded[fixture.File] = true
		fixturePath := prefix + fixture.File
		data, found := input.read(fixturePath)
		if !found {
			blockers = append(blockers, releaseBlocker{
				input: fmt.Sprintf("the %s prior-release corpus is missing recorded fixture %s", name, fixturePath),
				owner: releaseOwnerSelf,
			})
			continue
		}
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(data))); got != fixture.SHA256 {
			blockers = append(blockers, releaseBlocker{
				input: fmt.Sprintf("the %s prior-release fixture %s has SHA-256 %s, provenance records %s",
					name, fixturePath, got, fixture.SHA256), owner: releaseOwnerSelf,
			})
		}
	}
	for _, candidate := range input.paths {
		nameWithinCorpus, found := strings.CutPrefix(candidate, prefix)
		if !found || strings.Contains(nameWithinCorpus, "/") ||
			nameWithinCorpus == "provenance.json" || nameWithinCorpus == "README.md" {
			continue
		}
		if !recorded[nameWithinCorpus] {
			blockers = append(blockers, releaseBlocker{
				input: fmt.Sprintf("the %s prior-release fixture %s has no provenance record", name, candidate),
				owner: releaseOwnerSelf,
			})
		}
	}
	return blockers
}

func releaseIsLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func releaseAssertSupportReport(input *releaseRehearsalInput) []releaseBlocker {
	contents, ok := input.read(releaseSupportManifestPath)
	if !ok {
		return []releaseBlocker{{input: "the support catalog " + releaseSupportManifestPath + " cannot be read", owner: releaseOwnerSelf}}
	}
	var catalog struct {
		ProtocolVersion int `json:"protocolVersion"`
		Entries         []struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Status string `json:"status"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(contents), &catalog); err != nil {
		return []releaseBlocker{{input: "the support catalog " + releaseSupportManifestPath + " is not valid JSON", owner: releaseOwnerSelf}}
	}
	var blockers []releaseBlocker
	if catalog.ProtocolVersion != releasePlanProtocolVersion {
		blockers = append(blockers, releaseBlocker{
			input: fmt.Sprintf("the support catalog declares protocol version %d, the release reads version %d",
				catalog.ProtocolVersion, releasePlanProtocolVersion), owner: releaseOwnerSelf,
		})
	}
	if len(catalog.Entries) == 0 {
		blockers = append(blockers, releaseBlocker{input: "the support catalog classifies nothing", owner: releaseOwnerSelf})
	}
	seen := make(map[string]bool, len(catalog.Entries))
	for _, entry := range catalog.Entries {
		switch {
		case entry.ID == "" || entry.Kind == "":
			blockers = append(blockers, releaseBlocker{
				input: fmt.Sprintf("the support catalog has an incomplete entry %q", entry.ID), owner: releaseOwnerSelf,
			})
			// An entry that does not name what it classifies classifies
			// nothing, so it must not register an id the checks below read.
			continue
		case !releaseSupportStatuses[entry.Status]:
			blockers = append(blockers, releaseBlocker{
				input: fmt.Sprintf("the support catalog gives %s the unknown status %q", entry.ID, entry.Status), owner: releaseOwnerSelf,
			})
		case seen[entry.ID]:
			blockers = append(blockers, releaseBlocker{
				input: fmt.Sprintf("the support catalog classifies %s twice", entry.ID), owner: releaseOwnerSelf,
			})
		}
		seen[entry.ID] = true
	}
	if !seen["@putnami/cli"] {
		blockers = append(blockers, releaseBlocker{
			input: "the support catalog does not classify @putnami/cli, the artifact the release ships first", owner: releaseOwnerSelf,
		})
	}
	return blockers
}

func releaseAssertScrubResult(input *releaseRehearsalInput) []releaseBlocker {
	if len(input.scrubErrors) == 0 {
		return nil
	}
	return []releaseBlocker{{
		input: fmt.Sprintf("the public-cut scrub reports %d unresolved finding(s), first: %s",
			len(input.scrubErrors), input.scrubErrors[0]), owner: releaseOwnerSelf,
	}}
}

func releaseAssertGovernance(input *releaseRehearsalInput) []releaseBlocker {
	var blockers []releaseBlocker
	for _, file := range releaseGovernanceFiles {
		if !input.has(file) {
			blockers = append(blockers, releaseBlocker{
				input: "the public root has no " + file, owner: releaseOwnerSelf,
			})
		}
	}
	license, ok := input.read(releaseLicensePath)
	if !ok {
		return append(blockers, releaseBlocker{
			input: "the checked-in " + releaseLicensePath + " cannot be read", owner: releaseOwnerSelf,
		})
	}
	normalized := releaseNormalize(license)
	for _, phrase := range []string{
		"Functional Source License",
		input.plan.Release.License,
		input.plan.Release.FutureLicense + " Future License",
	} {
		if !strings.Contains(normalized, phrase) {
			blockers = append(blockers, releaseBlocker{
				input: fmt.Sprintf("the checked-in %s does not state %q", releaseLicensePath, phrase), owner: releaseOwnerSelf,
			})
		}
	}
	return blockers
}

func releaseAssertLabelScopeBootstrap(input *releaseRehearsalInput) []releaseBlocker {
	var blockers []releaseBlocker
	root := input.plan.FreshRoot
	if root.DefaultBranch == "" || root.Description == "" || len(root.Topics) == 0 {
		blockers = append(blockers, releaseBlocker{
			input: "the fresh-root metadata is incomplete: a default branch, a description and at least one topic are required",
			owner: releaseOwnerSelf,
		})
	}
	if root.Visibility != "public" {
		blockers = append(blockers, releaseBlocker{
			input: fmt.Sprintf("the fresh root declares visibility %q; the intended public release is public", root.Visibility),
			owner: releaseOwnerSelf,
		})
	}
	if !releaseIsSortedSet(root.Labels) {
		blockers = append(blockers, releaseBlocker{
			input: "the bootstrap labels must be sorted and unique so the plan is reviewable", owner: releaseOwnerSelf,
		})
	}
	labels := make(map[string]bool, len(root.Labels))
	for _, label := range root.Labels {
		labels[label] = true
	}
	scopes := releaseScopes(input)
	if len(scopes) == 0 {
		return append(blockers, releaseBlocker{
			input: "no top-level scope manifest exists", owner: releaseOwnerSelf,
		})
	}
	if strings.Join(scopes, ",") != strings.Join(releaseSorted(root.Scopes), ",") {
		blockers = append(blockers, releaseBlocker{
			input: fmt.Sprintf("the bootstrap scope set %v does not match the top-level scopes %v",
				releaseSorted(root.Scopes), scopes), owner: releaseOwnerSelf,
		})
	}
	for _, scope := range scopes {
		if !labels["scope/"+scope] {
			blockers = append(blockers, releaseBlocker{
				input: "the fresh root has no label scope/" + scope, owner: releaseOwnerSelf,
			})
		}
	}
	return append(blockers, releaseLabelAxisBlockers(input, root)...)
}

func releaseLabelAxisBlockers(input *releaseRehearsalInput, root releaseFreshRoot) []releaseBlocker {
	var blockers []releaseBlocker
	for _, axis := range root.LabelAxes {
		enumerated := false
		for _, label := range root.Labels {
			if strings.HasPrefix(label, axis.Prefix) {
				enumerated = true
				break
			}
		}
		if enumerated {
			continue
		}
		if axis.Authority == "" {
			blockers = append(blockers, releaseBlocker{
				input: "label axis " + axis.Prefix + " enumerates no label and names no authority document", owner: releaseOwnerSelf,
			})
			continue
		}
		if !input.has(axis.Authority) {
			blockers = append(blockers, releaseBlocker{
				input: "label axis " + axis.Prefix + " names the authority " + axis.Authority + ", which is not in the candidate cut",
				owner: releaseOwnerSelf,
			})
		}
	}
	return blockers
}

func releaseAssertPublishOrder(input *releaseRehearsalInput) []releaseBlocker {
	type releasePair struct{ artifact, channel string }
	claimed := make(map[releasePair]string)
	used := make(map[string]bool)
	ids := make(map[string]bool)
	var blockers []releaseBlocker

	claim := func(id string, channels, patterns []string) {
		if ids[id] {
			blockers = append(blockers, releaseBlocker{
				input: "publish step id " + id + " is reused; every step needs one reviewable position", owner: releaseOwnerSelf,
			})
		}
		ids[id] = true
		channelSet := make(map[string]bool, len(channels))
		for _, channel := range channels {
			channelSet[channel] = true
		}
		for _, pattern := range patterns {
			for _, artifact := range input.artifacts {
				matched, err := path.Match(pattern, artifact.name)
				if err != nil || !matched {
					continue
				}
				for _, channel := range artifact.channels {
					if !channelSet[channel] {
						continue
					}
					// Recorded before the duplicate check: a pattern that
					// reaches an already-claimed pair is a double claim, not a
					// dead pattern, and reporting it as both hides the first.
					used[id+" "+pattern] = true
					pair := releasePair{artifact: artifact.name, channel: channel}
					if owner, taken := claimed[pair]; taken {
						blockers = append(blockers, releaseBlocker{
							input: fmt.Sprintf("%s on channel %s is claimed twice, by %s and %s", pair.artifact, pair.channel, owner, id),
							owner: releaseOwnerSelf,
						})
						continue
					}
					claimed[pair] = id
				}
			}
		}
		for _, pattern := range patterns {
			if !used[id+" "+pattern] {
				blockers = append(blockers, releaseBlocker{
					input: "publish step " + id + " lists the pattern " + pattern + ", which matches no publishable artifact",
					owner: releaseOwnerSelf,
				})
			}
		}
	}

	for _, step := range input.plan.PublishOrder {
		claim(step.ID, step.Channels, step.Artifacts)
	}
	for _, exclusion := range input.plan.Excluded {
		claim(exclusion.ID, exclusion.Channels, exclusion.Artifacts)
	}
	for _, artifact := range input.artifacts {
		for _, channel := range artifact.channels {
			if _, taken := claimed[releasePair{artifact: artifact.name, channel: channel}]; !taken {
				blockers = append(blockers, releaseBlocker{
					input: fmt.Sprintf("%s publishes to %s but has no position in the publish order and no exclusion",
						artifact.name, channel), owner: releaseOwnerSelf,
				})
			}
		}
	}
	return blockers
}

func releaseAssertRollbackPlan(input *releaseRehearsalInput) []releaseBlocker {
	var blockers []releaseBlocker
	for _, step := range input.plan.PublishOrder {
		if strings.TrimSpace(step.RollbackPoint) == "" {
			blockers = append(blockers, releaseBlocker{
				input: "publish step " + step.ID + " names no rollback point", owner: releaseOwnerSelf,
			})
		}
		if strings.TrimSpace(step.Reversal) == "" {
			blockers = append(blockers, releaseBlocker{
				input: "publish step " + step.ID + " names no reversal", owner: releaseOwnerSelf,
			})
		}
	}
	for _, exclusion := range input.plan.Excluded {
		if strings.TrimSpace(exclusion.Reason) == "" {
			blockers = append(blockers, releaseBlocker{
				input: "release exclusion " + exclusion.ID + " gives no reason", owner: releaseOwnerSelf,
			})
		}
	}
	final := input.plan.FinalSwitch
	if !final.Excluded {
		blockers = append(blockers, releaseBlocker{
			input: "the private-archive/public-root switch must stay excluded from the rehearsal", owner: releaseOwnerSelf,
		})
	}
	if strings.TrimSpace(final.Step) == "" || strings.TrimSpace(final.Owner) == "" ||
		strings.TrimSpace(final.RollbackPoint) == "" || strings.TrimSpace(final.Reversal) == "" {
		blockers = append(blockers, releaseBlocker{
			input: "the final switch needs a step, a human owner, a rollback point and a reversal", owner: releaseOwnerSelf,
		})
	}
	return blockers
}

func renderReleaseRehearsalBlock(results []releaseCheckResult) string {
	var report strings.Builder
	report.WriteString(releaseRehearsalBlockBegin + "\n")
	report.WriteString("| Evidence | Check | Result | Blocking input |\n")
	report.WriteString("| --- | --- | --- | --- |\n")
	var blocked []string
	for _, result := range results {
		outcome, detail := "pass", "—"
		if len(result.blockers) > 0 {
			blocked = append(blocked, result.id)
			outcome = "blocked"
			parts := make([]string, 0, len(result.blockers))
			for _, blocker := range result.blockers {
				parts = append(parts, blocker.String())
			}
			detail = strings.Join(parts, "; ")
		}
		fmt.Fprintf(&report, "| %s | `%s` | %s | %s |\n",
			releaseEscapeCell(result.evidence), result.id, outcome, releaseEscapeCell(detail))
	}
	report.WriteString("\n")
	if len(blocked) == 0 {
		fmt.Fprintf(&report, "**Verdict: GO** — all %d release checks pass.\n", len(results))
	} else {
		fmt.Fprintf(&report, "**Verdict: NO-GO** — %d of %d release checks are blocked: %s.\n",
			len(blocked), len(results), strings.Join(blocked, ", "))
	}
	report.WriteString(releaseRehearsalBlockEnd)
	return report.String()
}

func releaseRecordedBlock(report string) (string, error) {
	if strings.Count(report, releaseRehearsalBlockBegin) > 1 || strings.Count(report, releaseRehearsalBlockEnd) > 1 {
		return "", fmt.Errorf("the report must carry exactly one generated block")
	}
	_, after, found := strings.Cut(report, releaseRehearsalBlockBegin)
	if !found {
		return "", fmt.Errorf("the report carries no generated block")
	}
	body, _, found := strings.Cut(after, releaseRehearsalBlockEnd)
	if !found {
		return "", fmt.Errorf("the report's generated block is not terminated")
	}
	return releaseRehearsalBlockBegin + body + releaseRehearsalBlockEnd, nil
}

func buildReleaseRehearsalInput(t *testing.T) *releaseRehearsalInput {
	t.Helper()
	repoRoot := publicCutRepositoryRoot(t)
	files, err := publicCutTrackedFiles(repoRoot)
	if err != nil {
		t.Fatalf("enumerate the release candidate: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("the candidate cut is empty — refusing a vacuous release rehearsal")
	}

	plan := releaseCommittedPlan(t)
	wanted := releaseContentPaths(plan)
	input := &releaseRehearsalInput{plan: plan, contents: make(map[string]string, len(wanted))}
	for _, file := range files {
		input.paths = append(input.paths, file.path)
		if wanted[file.path] || releaseIsPriorReleaseCorpusPath(file.path) || path.Base(file.path) == releaseProjectManifestName {
			input.contents[file.path] = string(file.data)
		}
	}
	sort.Strings(input.paths)
	input.artifacts = releasePublishableArtifacts(input)

	baseline := readPublicCutInventory(t, filepath.Join(cliModuleRoot(t), publicCutBaselinePath), false)
	allowlist := readPublicCutInventory(t, filepath.Join(cliModuleRoot(t), publicCutAllowlistPath), true)
	input.scrubErrors = evaluatePublicCut(scanPublicCut(files), baseline, allowlist)
	return input
}

func releaseIsPriorReleaseCorpusPath(target string) bool {
	for _, corpus := range releasePriorReleaseCorpora {
		if strings.HasPrefix(target, corpus.prefix) {
			return true
		}
	}
	return false
}

// releaseContentPaths is the exact set of files a check reads in full: the
// fixed inputs, the governance surface, and every phrase target the plan names.
// Prior-release corpora are prefix-selected separately because their complete,
// provenance-bound file sets are themselves the evidence under review.
func releaseContentPaths(plan *releasePlan) map[string]bool {
	wanted := make(map[string]bool)
	for _, file := range releaseInterestingFiles {
		wanted[file] = true
	}
	for _, file := range releaseGovernanceFiles {
		wanted[file] = true
	}
	for _, check := range plan.Checks {
		for _, requirement := range check.Requirements {
			if requirement.Kind == releaseRequirementPhrase {
				wanted[requirement.Target] = true
			}
		}
	}
	return wanted
}

func releasePublishableArtifacts(input *releaseRehearsalInput) []releaseArtifact {
	var artifacts []releaseArtifact
	for _, candidate := range input.paths {
		if path.Base(candidate) != releaseProjectManifestName {
			continue
		}
		contents, ok := input.read(candidate)
		if !ok {
			continue
		}
		var manifest struct {
			Name    string   `json:"name"`
			Publish []string `json:"publish"`
			Options struct {
				Publish map[string]json.RawMessage `json:"publish"`
			} `json:"options"`
		}
		if err := json.Unmarshal([]byte(contents), &manifest); err != nil || manifest.Name == "" {
			continue
		}
		channels := make(map[string]bool, len(manifest.Publish))
		for _, channel := range manifest.Publish {
			channels[channel] = true
		}
		for channel, enabled := range manifest.Options.Publish {
			if string(enabled) == "true" {
				channels[channel] = true
			}
		}
		if len(channels) == 0 {
			continue
		}
		names := make([]string, 0, len(channels))
		for channel := range channels {
			names = append(names, channel)
		}
		sort.Strings(names)
		artifacts = append(artifacts, releaseArtifact{name: manifest.Name, channels: names})
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].name < artifacts[j].name })
	return artifacts
}

func parseReleasePlan(data string) (*releasePlan, error) {
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	plan := &releasePlan{}
	if err := decoder.Decode(plan); err != nil {
		return nil, fmt.Errorf("release plan: %w", err)
	}
	if plan.ProtocolVersion != releasePlanProtocolVersion {
		return nil, fmt.Errorf("release plan: protocol version %d, want %d", plan.ProtocolVersion, releasePlanProtocolVersion)
	}
	required := make(map[string]bool, len(releaseChecks))
	for _, check := range releaseChecks {
		required[check.id] = true
	}
	seen := make(map[string]bool, len(plan.Checks))
	for _, check := range plan.Checks {
		if !required[check.ID] {
			return nil, fmt.Errorf("release plan: check %q is not a release evidence item", check.ID)
		}
		if seen[check.ID] {
			return nil, fmt.Errorf("release plan: check %q is declared twice", check.ID)
		}
		seen[check.ID] = true
		for _, requirement := range check.Requirements {
			if !releaseRequirementKinds[requirement.Kind] {
				return nil, fmt.Errorf("release plan: check %q uses the unknown requirement kind %q", check.ID, requirement.Kind)
			}
			if requirement.Target == "" || requirement.Input == "" || requirement.Owner == "" {
				return nil, fmt.Errorf("release plan: check %q has a requirement without a target, input description or owner", check.ID)
			}
			if requirement.Kind == releaseRequirementPhrase && requirement.Phrase == "" {
				return nil, fmt.Errorf("release plan: check %q declares a phrase requirement with no phrase", check.ID)
			}
		}
	}
	for _, check := range releaseChecks {
		if !seen[check.id] {
			return nil, fmt.Errorf("release plan: no evidence entry for check %q", check.id)
		}
	}
	return plan, nil
}

// releaseCommittedPlan reads and validates the plan this repository ships, as
// opposed to the fixture the focused tests bend.
func releaseCommittedPlan(t *testing.T) *releasePlan {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cliModuleRoot(t), filepath.FromSlash(releasePlanModulePath)))
	if err != nil {
		t.Fatalf("read %s: %v", releasePlanRepoPath, err)
	}
	plan, err := parseReleasePlan(string(data))
	if err != nil {
		t.Fatalf("parse %s: %v", releasePlanRepoPath, err)
	}
	return plan
}

// releaseDeclaredTestInputs returns the file patterns @putnami/cli-documents
// declares as cache-key inputs of its test task. They are read from the
// manifest rather than restated here: a copy would let the assertion pass
// against a list the scheduler does not use.
//
// The declaration follows the reader. The verdict-bearing documents
// used to be declared by @putnami/cli because these gates lived there; they are
// declared by this project now, and this assertion moved with them so the
// property it protects is checked against the manifest that actually keys the
// run.
func releaseDeclaredTestInputs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(projectRoot(t), releaseProjectManifestName))
	if err != nil {
		t.Fatalf("read the project manifest: %v", err)
	}
	var manifest struct {
		Options struct {
			Test struct {
				FilePatterns []string `json:"filePatterns"`
			} `json:"test"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse the project manifest: %v", err)
	}
	if len(manifest.Options.Test.FilePatterns) == 0 {
		t.Fatal("the project declares no test filePatterns, so no release input is keyed")
	}
	return manifest.Options.Test.FilePatterns
}

// releaseProjectRelativePath rewrites a repository-relative path into the form
// filepath.Rel produces against this project's root, which is the form the
// cache key's file selection matches: inside the project it is relative, and
// outside it keeps its "../" prefix. The project's own directory is derived
// from the two spellings of the rehearsal source so the pair cannot drift
// apart — the source is the one file that is certainly inside this project.
//
// filepath.Rel is used rather than counting separators because a sibling
// project is one level up, not two: collectFiles matches the CLEANED relative
// path, so tooling/cli/doc/x is "../cli/doc/x" here and a pattern spelled
// "../../tooling/cli/doc/**" would select nothing at all.
func releaseProjectRelativePath(target string) string {
	projectDir := strings.TrimSuffix(releaseRehearsalSourceRepoPath, releaseRehearsalSourceModule)
	rel, err := filepath.Rel(filepath.FromSlash(projectDir), filepath.FromSlash(target))
	if err != nil {
		return target
	}
	return filepath.ToSlash(rel)
}

func releaseKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

// releaseWorkspaceVersion reads the series this repository is releasing from the
// FIRST "## " heading of the tooling changelog.
//
// It is no longer a declared field. A version is derived from git per line
// (D9), so the only committed statement of "which series is this" left in the
// tree is the changelog the release commit carries — and that is exactly the
// artifact the release plan has to agree with.
func releaseWorkspaceVersion(input *releaseRehearsalInput) (string, bool) {
	contents, ok := input.read(releaseSeriesChangelogPath)
	if !ok {
		return "", false
	}
	for _, line := range strings.Split(contents, "\n") {
		heading, isHeading := strings.CutPrefix(strings.TrimSpace(line), "## ")
		if !isHeading {
			continue
		}
		series, _, _ := strings.Cut(heading, " ")
		if series = strings.TrimSpace(series); series != "" {
			return series, true
		}
	}
	return "", false
}

// releaseBuilderMatrix reads the platform matrix the archive channels actually
// build, so a plan that advertises a target nobody cross-compiles is a blocker
// rather than a promise.
func releaseBuilderMatrix(input *releaseRehearsalInput) ([]string, bool) {
	source, ok := input.read(releasePlatformMatrixPath)
	if !ok {
		return nil, false
	}
	_, after, found := strings.Cut(source, releaseArchiveMatrixDeclaration)
	if !found {
		return nil, false
	}
	block, _, found := strings.Cut(after, "}\n")
	if !found {
		return nil, false
	}
	var targets []string
	for _, line := range strings.Split(block, "\n") {
		goos, hasOS := releaseQuotedValue(line, "GOOS:")
		goarch, hasArch := releaseQuotedValue(line, "GOARCH:")
		if hasOS && hasArch {
			targets = append(targets, goos+"/"+goarch)
		}
	}
	sort.Strings(targets)
	return targets, len(targets) > 0
}

func releaseQuotedValue(line, key string) (string, bool) {
	_, after, found := strings.Cut(line, key)
	if !found {
		return "", false
	}
	_, quoted, found := strings.Cut(after, `"`)
	if !found {
		return "", false
	}
	value, _, found := strings.Cut(quoted, `"`)
	if !found || value == "" {
		return "", false
	}
	return value, true
}

// releaseScopes lists the workspace's scopes: the top-level directories
// whose putnami.json is a scope manifest.
func releaseScopes(input *releaseRehearsalInput) []string {
	var scopes []string
	for _, candidate := range input.paths {
		dir, name, found := strings.Cut(candidate, "/")
		if !found || name != releaseProjectManifestName {
			continue
		}
		contents, ok := input.read(candidate)
		if !ok {
			continue
		}
		var manifest struct {
			Schema string `json:"$schema"`
		}
		if json.Unmarshal([]byte(contents), &manifest) == nil && manifest.Schema == releaseScopeSchema {
			scopes = append(scopes, dir)
		}
	}
	sort.Strings(scopes)
	return scopes
}

func releaseNormalizeBlockers(blockers []releaseBlocker) []releaseBlocker {
	if len(blockers) == 0 {
		return nil
	}
	sort.Slice(blockers, func(i, j int) bool { return blockers[i].String() < blockers[j].String() })
	unique := blockers[:0]
	for _, blocker := range blockers {
		if len(unique) == 0 || unique[len(unique)-1] != blocker {
			unique = append(unique, blocker)
		}
	}
	if len(unique) <= releaseMaxBlockers {
		return unique
	}
	capped := make([]releaseBlocker, 0, releaseMaxBlockers+1)
	capped = append(capped, unique[:releaseMaxBlockers]...)
	return append(capped, releaseBlocker{
		input: fmt.Sprintf("and %d further blocking input(s)", len(unique)-releaseMaxBlockers), owner: releaseOwnerSelf,
	})
}

// releaseEvidenceDump carries the point-in-time numbers a reader wants without
// putting them in the recorded verdict, where every unrelated project addition
// would force a re-record.
func releaseEvidenceDump(input *releaseRehearsalInput) string {
	channels := make(map[string]int)
	for _, artifact := range input.artifacts {
		for _, channel := range artifact.channels {
			channels[channel]++
		}
	}
	names := make([]string, 0, len(channels))
	for channel := range channels {
		names = append(names, fmt.Sprintf("%s=%d", channel, channels[channel]))
	}
	sort.Strings(names)
	version, _ := releaseWorkspaceVersion(input)
	matrix, _ := releaseBuilderMatrix(input)
	order := make([]string, 0, len(input.plan.PublishOrder))
	for _, step := range input.plan.PublishOrder {
		order = append(order, step.ID)
	}
	return strings.Join([]string{
		"release rehearsal evidence (not part of the recorded verdict)",
		"  candidate files:    " + fmt.Sprint(len(input.paths)),
		"  release series:     " + version,
		"  builder matrix:     " + strings.Join(matrix, " "),
		"  publishable:        " + fmt.Sprint(len(input.artifacts)) + " artifact(s), " + strings.Join(names, " "),
		"  publish order:      " + strings.Join(order, " -> "),
		"  scrub findings:     " + fmt.Sprint(len(input.scrubErrors)),
	}, "\n")
}

func releaseEscapeCell(value string) string {
	return strings.ReplaceAll(releaseNormalize(value), "|", `\|`)
}

func releaseNormalize(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// releaseOwners are the workstreams a release requirement can wait on.
var releaseOwners = map[string]bool{
	releaseOwnerSelf: true,
	"public-cut":     true,
	"release-smoke":  true,
	"governance":     true,
}

func releaseSortedOwners() []string {
	owners := make([]string, 0, len(releaseOwners))
	for owner := range releaseOwners {
		owners = append(owners, owner)
	}
	return releaseSorted(owners)
}

func releaseSorted(values []string) []string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return sorted
}

func releaseIsSortedSet(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] >= values[index] {
			return false
		}
	}
	return true
}

func releaseWithout(values []string, drop string) []string {
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if value != drop {
			kept = append(kept, value)
		}
	}
	return kept
}

func releaseBlockersMention(blockers []releaseBlocker, fragment string) bool {
	for _, blocker := range blockers {
		if strings.Contains(blocker.String(), fragment) {
			return true
		}
	}
	return false
}

// releaseFixturePlanJSON is a complete, passing miniature of the committed
// release plan. Every focused test starts from it and bends exactly one input,
// so each blocker is proven by the difference rather than by the whole tree.
const releaseFixturePlanJSON = `{
  "protocolVersion": 1,
  "release": {
    "series": "0.1.0",
    "license": "FSL-1.1-MIT",
    "futureLicense": "MIT",
    "targets": ["darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"]
  },
  "freshRoot": {
    "defaultBranch": "main",
    "visibility": "public",
    "description": "Fixture root.",
    "topics": ["fixture"],
    "scopes": ["go", "tooling"],
    "labels": ["group/design", "group/security", "group/testing", "scope/go", "scope/tooling"],
    "labelAxes": [
      {"prefix": "group/"},
      {"prefix": "prop/", "authority": ".agents/skills/audit/SKILL.md"},
      {"prefix": "scope/"}
    ]
  },
  "publishOrder": [
    {
      "id": "protocols",
      "channels": ["go"],
      "artifacts": ["go.putnami.dev/protocol/*"],
      "rollbackPoint": "the previously released protocol module versions",
      "reversal": "supersede with a patch version and retract the bad one"
    },
    {
      "id": "go-framework",
      "channels": ["go"],
      "artifacts": ["go.putnami.dev/*"],
      "rollbackPoint": "the previously released framework module versions",
      "reversal": "supersede with a patch version and retract the bad one"
    },
    {
      "id": "typescript-framework",
      "channels": ["npm"],
      "artifacts": ["@putnami/*"],
      "rollbackPoint": "the previously released package versions",
      "reversal": "supersede with a patch version and deprecate the bad one"
    },
    {
      "id": "cli",
      "channels": ["archives"],
      "artifacts": ["@putnami/cli"],
      "rollbackPoint": "the previous download channel pointer",
      "reversal": "repoint the channel at the previous version"
    },
    {
      "id": "templates",
      "channels": ["archives", "template-archives"],
      "artifacts": ["*-server"],
      "rollbackPoint": "the previous template channel pointer",
      "reversal": "repoint the channel at the previous version"
    }
  ],
  "excluded": [
    {
      "id": "container-images",
      "channels": ["docker"],
      "artifacts": ["putnami.dev"],
      "reason": "site and sample images are deployment artifacts, not release artifacts"
    }
  ],
  "finalSwitch": {
    "step": "switch the private repository to a public root",
    "owner": "the human release approver",
    "excluded": true,
    "rollbackPoint": "the private repository and its verified history archive",
    "reversal": "set the repository visibility back to private"
  },
  "checks": [
    {"id": "version-and-artifact-manifest", "evidence": "version and artifact manifest", "requirements": []},
    {
      "id": "checksums-and-provenance",
      "evidence": "checksums and provenance",
      "requirements": [
        {"kind": "file", "target": "tooling/cli/scripts/install.sh", "input": "the public installer integrity path", "owner": "release-rehearsal"},
        {"kind": "file", "target": "tooling/cli/scripts/smoke-check-release.sh", "input": "the release smoke integrity path", "owner": "release-rehearsal"},
        {"kind": "file", "target": "tooling/cli/doc/release-provenance.md", "input": "the durable release provenance policy", "owner": "release-rehearsal"}
      ]
    },
    {"id": "support-report", "evidence": "support report", "requirements": []},
    {
      "id": "migration-report",
      "evidence": "migration report",
      "requirements": [
        {"kind": "file", "target": "tooling/cli/doc/21-compatibility-and-migration.md", "input": "the public compatibility and migration guide", "owner": "release-rehearsal"},
        {"kind": "file", "target": "tooling/cli/doc/adr/0010-compatibility-budget.md", "input": "the accepted per-format compatibility budget", "owner": "release-rehearsal"},
        {"kind": "tree-prefix", "target": "tooling/cli/internal/lockfile/testdata/prior-releases/", "input": "the immutable prior-release lock corpus", "owner": "release-rehearsal"},
        {"kind": "tree-prefix", "target": "protocols/cli/testdata/prior-releases/", "input": "the immutable prior-release machine-result corpus", "owner": "release-rehearsal"},
        {"kind": "tree-prefix", "target": "protocols/extension/testdata/prior-releases/", "input": "the immutable prior-release extension-contract corpus", "owner": "release-rehearsal"}
      ]
    },
    {
      "id": "scrub-result",
      "evidence": "scrub result",
      "requirements": [
        {"kind": "ignore-entry", "target": ".context/", "input": "the committed ignore guard for private workspace state", "owner": "public-cut"}
      ]
    },
    {
      "id": "golden-path-result",
      "evidence": "golden-path result",
      "requirements": [
        {"kind": "file", "target": "tooling/cli/scripts/smoke-check-release.sh", "input": "the release smoke runner", "owner": "release-smoke"},
        {"kind": "phrase", "target": "tooling/cli/scripts/smoke-check-release.sh", "phrase": "putnami init", "input": "the initialized-workspace leg of the golden path", "owner": "release-smoke"},
        {"kind": "phrase", "target": "tooling/cli/scripts/smoke-check-release.sh", "phrase": "putnami serve", "input": "the served-HTTP leg of the golden path", "owner": "release-smoke"}
      ]
    },
    {
      "id": "governance-checklist",
      "evidence": "governance checklist",
      "requirements": [
        {"kind": "tree-prefix", "target": ".github/workflows/", "input": "a neutral contributor CI path", "owner": "governance"},
        {"kind": "file", "target": "GOVERNANCE.md", "input": "the public governance record", "owner": "governance"}
      ]
    },
    {"id": "label-scope-bootstrap-plan", "evidence": "label/scope bootstrap plan", "requirements": []},
    {"id": "publish-order", "evidence": "publish order", "requirements": []},
    {"id": "rollback-plan", "evidence": "rollback plan", "requirements": []}
  ]
}`

const releaseFixturePlatformSource = `package pkgmeta

var archivePlatforms = []ArchivePlatform{
	{GOOS: "linux", GOARCH: "amd64", Suffix: "linux-x64"},
	{GOOS: "linux", GOARCH: "arm64", Suffix: "linux-arm64"},
	{GOOS: "darwin", GOARCH: "amd64", Suffix: "darwin-x64"},
	{GOOS: "darwin", GOARCH: "arm64", Suffix: "darwin-arm64"},
}
`

// releaseRehearsalFixture builds a miniature tree whose every release check
// passes, so a focused test proves one blocker by removing one input.
func releaseRehearsalFixture() *releaseRehearsalInput {
	plan, err := parseReleasePlan(releaseFixturePlanJSON)
	if err != nil {
		panic("release rehearsal fixture plan is invalid: " + err.Error())
	}
	contents := map[string]string{
		releaseWorkspaceManifestPath: `{"name":"fixture"}`,
		releaseSeriesChangelogPath:   "# Changelog — tooling\n\n## 0.1.0 — 2026-09-04\n\nBaseline.\n",
		releasePlatformMatrixPath:    releaseFixturePlatformSource,
		releaseSupportManifestPath: `{"protocolVersion":1,"entries":[` +
			`{"id":"@putnami/cli","kind":"package","status":"stable"},` +
			`{"id":"@putnami/python","kind":"package","status":"experimental"}]}`,
		// Deliberately mixed spellings: git guards the same path whether the
		// committed line is root-anchored or carries a trailing slash, and a
		// "!" line un-ignores rather than guards.
		releaseIgnoreFilePath:                   "# private workspace state\n/.context/\n.putnami\n!.build/\n",
		"go/" + releaseProjectManifestName:      `{"$schema":"` + releaseScopeSchema + `","includes":["extension"]}`,
		"tooling/" + releaseProjectManifestName: `{"$schema":"` + releaseScopeSchema + `","includes":["cli"]}`,
		releaseLicensePath:                      "# Functional Source License, Version 1.1, MIT Future License\n\nFSL-1.1-MIT\n",
		// The phrase spans a line break on purpose: requirement matching
		// normalizes whitespace, so a reflowed document keeps its meaning.
		releaseContractPath: "# Release\n\nThe release\nrehearsal records the verdict.\n",
		releaseProvenancePolicyPath: strings.Join([]string{
			"The only digest algorithm accepted for release artifacts is **SHA-256**.",
			"The authoritative digest source for a registry install is the successful HTTPS download response",
			"`X-Integrity` takes precedence",
			"The release record must identify the builder before a candidate is promoted.",
			"Current Putnami release artifacts are unsigned.",
			"CLI archives are not currently promised to be byte-for-byte reproducible.",
			"Never replace bytes at an existing immutable version.",
		}, "\n"),
		releaseMigrationGuidePath: strings.Join([]string{
			"This document is the **compatibility budget**",
			"## Lock file — `putnami.lock.json`",
			"## Machine result envelope",
			"## Extension contract stamp — `cliContract`",
			"## Support catalog — `putnami.support.json`",
			"## Evidence",
		}, "\n"),
		releaseCompatibilityDecisionPath: strings.Join([]string{
			"A version increment ships either a MIGRATION",
			"Evidence is prior-release bytes, and the bytes are immutable",
		}, "\n"),
		releaseInstallerPath: "#!/usr/bin/env bash\n# X-Integrity Digest sha-256 compute_sha256 " +
			"read_advertised_integrity verify_download_integrity refusing to install an unverified binary\n",
		releaseSmokePath: "#!/usr/bin/env bash\n# advertised no SHA-256 (X-Integrity or RFC 9530 Digest)\n" +
			"# Integrity verified; reject without integrity verification; compare installed_digest\n" +
			"putnami init\nputnami serve\n",
		"CODE_OF_CONDUCT.md":                        "# Code of conduct\n",
		"CONTRIBUTING.md":                           "# Contributing\n",
		"SECURITY.md":                               "# Security\n",
		".github/ISSUE_TEMPLATE/bug_report.md":      "# Bug\n",
		".github/ISSUE_TEMPLATE/feature_request.md": "# Feature\n",
		".github/PULL_REQUEST_TEMPLATE.md":          "# Pull request\n",
	}
	for _, fixture := range []struct {
		prefix, file, data string
	}{
		{releasePriorReleaseCorpora[0].prefix, "lock-v2.json", `{"version":2}`},
		{releasePriorReleaseCorpora[1].prefix, "result-v1.json", `{"result":"v1"}`},
		{releasePriorReleaseCorpora[2].prefix, "extension-v0.json", `{"cliContract":0}`},
	} {
		contents[fixture.prefix+fixture.file] = fixture.data
		sum := fmt.Sprintf("%x", sha256.Sum256([]byte(fixture.data)))
		contents[fixture.prefix+"provenance.json"] = fmt.Sprintf(`{
  "fixtures": [{
    "file": %q,
    "sourceCommit": "0123456789abcdef0123456789abcdef01234567",
    "sourcePath": "fixture/source.json",
    "committed": "2026-01-01",
    "sha256": %q,
    "why": "fixture provenance"
  }],
  "gaps": []
}`, fixture.file, sum)
	}
	paths := []string{
		".agents/skills/audit/SKILL.md",
		"go/putnami.json",
		"tooling/putnami.json",
		".github/ISSUE_TEMPLATE/bug_report.md",
		".github/ISSUE_TEMPLATE/feature_request.md",
		".github/PULL_REQUEST_TEMPLATE.md",
		".github/workflows/ci.yml",
		".gitignore",
		"CODE_OF_CONDUCT.md",
		"CONTRIBUTING.md",
		"GOVERNANCE.md",
		"LICENSE.md",
		"RELEASE.md",
		"SECURITY.md",
		"tooling/extension-sdk/pkgmeta/platforms.go",
		"putnami.support.json",
		"putnami.workspace.json",
		"tooling/cli/doc/release-provenance.md",
		"tooling/cli/doc/21-compatibility-and-migration.md",
		"tooling/cli/doc/adr/0010-compatibility-budget.md",
		"tooling/cli/internal/lockfile/testdata/prior-releases/lock-v2.json",
		"tooling/cli/internal/lockfile/testdata/prior-releases/provenance.json",
		"protocols/cli/testdata/prior-releases/result-v1.json",
		"protocols/cli/testdata/prior-releases/provenance.json",
		"protocols/extension/testdata/prior-releases/extension-v0.json",
		"protocols/extension/testdata/prior-releases/provenance.json",
		"tooling/cli/scripts/install.sh",
		"tooling/cli/scripts/smoke-check-release.sh",
	}
	sort.Strings(paths)
	return &releaseRehearsalInput{
		paths:    paths,
		contents: contents,
		plan:     plan,
		artifacts: []releaseArtifact{
			{name: "@putnami/cli", channels: []string{"archives"}},
			{name: "@putnami/runtime", channels: []string{"npm"}},
			{name: "go-server", channels: []string{"archives", "template-archives"}},
			{name: "go.putnami.dev/http", channels: []string{"go"}},
			{name: "go.putnami.dev/protocol/cli", channels: []string{"go"}},
			{name: "putnami.dev", channels: []string{"docker"}},
		},
	}
}
