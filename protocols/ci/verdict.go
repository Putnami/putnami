package ci

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

const (
	// VerdictVersion is the only Verdict wire version. It is the suffix of
	// VerdictPredicateType: a statement of another predicate type is refused.
	VerdictVersion = 1
	// VerdictStatementType is the in-toto Statement v1 type a Verdict carries
	// in `_type`.
	VerdictStatementType = "https://in-toto.io/Statement/v1"
	// VerdictPredicateType names the Verdict predicate, version 1.
	VerdictPredicateType = "https://putnami.dev/verdict/v1"
	// VerdictSchemaURL is the published identifier of the Verdict schema.
	VerdictSchemaURL = "https://putnami.dev/schemas/putnami-verdict.json"
	// MaxVerdictBytes bounds the encoded Verdict ParseVerdict reads.
	MaxVerdictBytes = 4 << 20
	// MaxVerdictChecks bounds the checks of one verdict.
	MaxVerdictChecks = 512
	// MaxVerdictProjects bounds the impacted projects of one verdict.
	MaxVerdictProjects = 4096
	// MaxVerdictDeclarations bounds the declarations of one verdict.
	MaxVerdictDeclarations = 1024
	// MaxVerdictGaps bounds the gaps of one verdict.
	MaxVerdictGaps = 16384
	// MaxVerdictChannels bounds the deploy channels of one verdict.
	MaxVerdictChannels = 64
)

// The answers of a verdict, a check and a deploy. A required check blocks
// unless it passed or is unsupported: FoldVerdictAnswers orders them.
const (
	// AnswerPassed: the check ran and passed.
	AnswerPassed = "passed"
	// AnswerFailed: the check ran and failed.
	AnswerFailed = "failed"
	// AnswerUnsupported: nothing can answer the check for this change, for
	// example a repository without a workspace. It never blocks.
	AnswerUnsupported = "unsupported"
	// AnswerUnanswered: the check was expected and nothing answered it.
	AnswerUnanswered = "unanswered"
	// AnswerCanceled: the check was canceled before it answered.
	AnswerCanceled = "canceled"
	// AnswerInfrastructure: the check could not run to an answer because the
	// machinery that runs it failed, not the change.
	AnswerInfrastructure = "infrastructure"
)

// verdictAnswerSeverity holds the answers that fold, the most severe first.
// AnswerUnsupported is not in it: it never blocks.
var verdictAnswerSeverity = []string{AnswerFailed, AnswerInfrastructure, AnswerCanceled, AnswerUnanswered, AnswerPassed}

// The marks of a passed verdict.
const (
	// MarkVerified: the verdict passed and records no gap.
	MarkVerified = "verified"
	// MarkChecked: the verdict passed and records at least one gap.
	MarkChecked = "checked"
)

// The levels of a check.
const (
	// LevelRequired: the check's answer folds into the verdict's answer.
	LevelRequired = "required"
	// LevelAdvisory: the check is reported and never changes the answer.
	LevelAdvisory = "advisory"
)

// What the tree of a verdict is the tree of.
const (
	// TestedCheckoutHead: the run tested Head as it is. Tree is Head's tree.
	TestedCheckoutHead = "head"
	// TestedCheckoutMerge: the run tested Head merged into Base. Tree is the
	// tree of that merge, which may be the tree of no commit in the repository.
	TestedCheckoutMerge = "merge"
)

// The relations of an impacted project to the change.
const (
	// RelationTouched: the project owns at least one changed path.
	RelationTouched = "touched"
	// RelationReached: the project depends on a touched project.
	RelationReached = "reached"
)

// The kinds of a declaration: a committed file that states what the
// repository promises or how it is checked.
const (
	// DeclarationSpec is a spec document.
	DeclarationSpec = "spec"
	// DeclarationBaseline is a specs baseline.
	DeclarationBaseline = "baseline"
	// DeclarationBinding is a binding between a spec and its proofs.
	DeclarationBinding = "binding"
	// DeclarationADR is an architecture decision record.
	DeclarationADR = "adr"
	// DeclarationDecisions is a decisions document.
	DeclarationDecisions = "decisions"
	// DeclarationArchitecture is an architecture manifest.
	DeclarationArchitecture = "architecture"
	// DeclarationCI is a CI document such as putnami.ci.json.
	DeclarationCI = "ci"
	// DeclarationOwners is an ownership document.
	DeclarationOwners = "owners"
)

var verdictDeclarationKinds = []string{
	DeclarationSpec, DeclarationBaseline, DeclarationBinding, DeclarationADR,
	DeclarationDecisions, DeclarationArchitecture, DeclarationCI, DeclarationOwners,
}

// The places an approval of a declaration change is given.
const (
	// ApprovalViaCodeHost: a review on the code host that holds the change.
	ApprovalViaCodeHost = "code-host"
	// ApprovalViaConsole: a person approved it in a console.
	ApprovalViaConsole = "console"
)

// The kinds of a gap: what keeps a passed verdict from being verified.
const (
	// GapNoRequirement: an impacted project states no requirement.
	GapNoRequirement = "no-requirement"
	// GapUnprovenRequirement: an impacted project's requirement is not
	// verified.
	GapUnprovenRequirement = "unproven-requirement"
	// GapUnapprovedDeclaration: a declaration the change modifies has no
	// approval, whether the change weakens it or not.
	GapUnapprovedDeclaration = "unapproved-declaration"
	// GapUnknownImpact: the producer does not know which projects the change
	// reaches. The verdict then lists no impacted project.
	GapUnknownImpact = "unknown-impact"
	// GapUnknownRequirements: the producer does not know how an impacted
	// project's requirements stand. The project then carries no counts.
	GapUnknownRequirements = "unknown-requirements"
	// GapUnknownDeclarations: the producer does not know which declarations
	// the change modifies. The verdict then lists no declaration.
	GapUnknownDeclarations = "unknown-declarations"
)

var (
	verdictRunIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	verdictCodePattern        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	verdictReasonPattern      = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
	verdictCheckPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/~@+-]{0,127}$`)
	verdictRequirementPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*(?:/[a-z0-9]+(?:-[a-z0-9]+)*)+#[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

//go:embed schemas/putnami-verdict.json
var verdictSchemaBytes []byte

// VerdictSchema returns a defensive copy of the Verdict JSON Schema.
func VerdictSchema() []byte { return append([]byte(nil), verdictSchemaBytes...) }

// Verdict is the one answer a CI run gives about one change: an unsigned
// in-toto Statement v1 whose subject is the repository at the change's head
// commit, and whose predicate says which tree the run tested, whether the
// change passed, how well its requirements are proven, which declarations it
// modifies, and what is missing or unknown. A producer may sign it; this
// package neither signs nor verifies a signature.
type Verdict struct {
	// Type is always VerdictStatementType.
	Type string `json:"_type"`
	// Subject names exactly one resource: the repository at the change's
	// head commit.
	Subject []VerdictSubject `json:"subject"`
	// PredicateType is always VerdictPredicateType.
	PredicateType string `json:"predicateType"`
	// Predicate is the verdict itself.
	Predicate VerdictPredicate `json:"predicate"`
}

// VerdictSubject is an in-toto resource descriptor: the repository, by URL,
// at one commit.
type VerdictSubject struct {
	// Name is the repository URL, VerdictChange.Repository.
	Name string `json:"name"`
	// Digest holds the head commit.
	Digest VerdictDigest `json:"digest"`
}

// VerdictDigest is the in-toto digest set of a subject. It holds no tree:
// VerdictChange.Tree is the tree the run tested, which is not always the tree
// of the head commit.
type VerdictDigest struct {
	// GitCommit is VerdictChange.Head.
	GitCommit string `json:"gitCommit"`
}

// VerdictPredicate is the verdict of one run attempt about one change.
type VerdictPredicate struct {
	// Answer is FoldRequiredChecks of Checks.
	Answer string `json:"answer"`
	// Mark is VerdictMarkFor of Answer and Gaps: present exactly when Answer
	// is AnswerPassed.
	Mark string `json:"mark,omitempty"`
	// Change is the change the run answered for.
	Change VerdictChange `json:"change"`
	// Run is the run attempt that produced the verdict.
	Run VerdictRun `json:"run"`
	// Checks is every check of the run, sorted by name.
	Checks []VerdictCheck `json:"checks"`
	// Impact is the project closure the change reaches.
	Impact VerdictImpact `json:"impact"`
	// Requirements sums the requirement counts of Impact.
	Requirements VerdictRequirements `json:"requirements"`
	// Declarations is every declaration the change modifies, the weakening
	// ones first, then by path.
	Declarations []VerdictDeclaration `json:"declarations"`
	// Gaps is everything that keeps a passed verdict from being verified,
	// sorted by kind, project, requirement, then path.
	Gaps []VerdictGap `json:"gaps"`
	// Deploy is the answer of the deploys the change triggered, when it
	// triggered any.
	Deploy *VerdictDeploy `json:"deploy,omitempty"`
	// ComputedAt is the RFC 3339 time the verdict was computed.
	ComputedAt string `json:"computedAt"`
	// Revision counts the computations of this run attempt's verdict from 1.
	// A later revision supersedes every earlier one.
	Revision int `json:"revision"`
}

// VerdictChange identifies the change: a repository, a reference, the commits
// the run saw and the tree it tested.
type VerdictChange struct {
	// Repository is the credential-free URL of the repository: an https, http
	// or ssh URL without user, query, fragment or trailing slash.
	Repository string `json:"repository"`
	// Reference is the full Git reference the change is on, such as
	// refs/heads/main or refs/tags/v1.4.0.
	Reference string `json:"reference"`
	// Head is the full commit ID of the change.
	Head string `json:"head"`
	// Base is the full commit ID the change is measured from, when the run
	// had one.
	Base string `json:"base,omitempty"`
	// Tree is the full ID of the tree the run tested.
	Tree string `json:"tree"`
	// TestedCheckout says what Tree is the tree of: TestedCheckoutHead or
	// TestedCheckoutMerge. A merge names its Base.
	TestedCheckout string `json:"testedCheckout"`
}

// VerdictRun identifies the run attempt that produced the verdict.
type VerdictRun struct {
	// ID is the run's identifier, opaque to this package.
	ID string `json:"id"`
	// Attempt counts the run's attempts from 1.
	Attempt int `json:"attempt"`
	// Conclusion is the run's own conclusion, a machine code the CI plane
	// defines, such as success or failure.
	Conclusion string `json:"conclusion"`
}

// VerdictCheck is one check of the run and its answer.
type VerdictCheck struct {
	// Check is the check's name, unique in the verdict.
	Check string `json:"check"`
	// Answer is the check's answer.
	Answer string `json:"answer"`
	// Level says whether Answer folds into the verdict's answer.
	Level string `json:"level"`
	// Phase is the command the check belongs to, such as test, when it
	// belongs to one.
	Phase string `json:"phase,omitempty"`
	// Reason is a machine code that says why the check did not pass.
	Reason string `json:"reason,omitempty"`
}

// VerdictImpact is the project closure the change reaches.
type VerdictImpact struct {
	// Projects is every impacted project, sorted by project ID.
	Projects []VerdictProject `json:"projects"`
}

// VerdictProject is one impacted project and how its requirements stand.
type VerdictProject struct {
	// Project is the workspace project ID, such as /tooling/cli.
	Project string `json:"project"`
	// Relation is RelationTouched or RelationReached.
	Relation string `json:"relation"`
	// Via names another impacted project the dependency reaches this one
	// through. Only a reached project names one.
	Via string `json:"via,omitempty"`
	// Requirements counts the project's requirements. It is absent when the
	// producer does not know how they stand, and the verdict then records a
	// GapUnknownRequirements for the project.
	Requirements *VerdictRequirementCounts `json:"requirements,omitempty"`
}

// VerdictRequirementCounts counts one project's requirements. Only a
// requirement whose state is verified is proven. Missing counts every other
// state, such as unmapped, stale or contradicted, so Proven plus Missing is
// Total.
type VerdictRequirementCounts struct {
	// Total is the number of requirements the project states.
	Total int `json:"total"`
	// Proven is the number of those whose state is verified.
	Proven int `json:"proven"`
	// Missing is Total minus Proven.
	Missing int `json:"missing"`
}

// VerdictRequirements sums the requirement counts of every impacted project
// that carries counts.
type VerdictRequirements struct {
	// Total sums VerdictRequirementCounts.Total.
	Total int `json:"total"`
	// Proven sums VerdictRequirementCounts.Proven.
	Proven int `json:"proven"`
	// Missing sums VerdictRequirementCounts.Missing.
	Missing int `json:"missing"`
	// ProjectsWithout counts the impacted projects that state no requirement.
	ProjectsWithout int `json:"projectsWithout"`
	// ProjectsUnknown counts the impacted projects that carry no counts.
	ProjectsUnknown int `json:"projectsUnknown"`
}

// VerdictDeclaration is one declaration the change modifies.
type VerdictDeclaration struct {
	// Path is the declaration's workspace-relative path, with forward
	// slashes.
	Path string `json:"path"`
	// Kind is the declaration's kind, such as spec.
	Kind string `json:"kind"`
	// Weakening is true when the producer found that the change weakens what
	// the declaration promises, such as a removed requirement or a lowered
	// floor. A producer that does not compare the declaration's content writes
	// false. It orders the declarations; it does not decide whether an
	// approval is needed.
	Weakening bool `json:"weakening"`
	// Approval records who approved the change to the declaration, when
	// someone did. A declaration without one is a gap.
	Approval *VerdictApproval `json:"approval,omitempty"`
}

// VerdictApproval records one approval of a declaration change.
type VerdictApproval struct {
	// By is the approver's identity on the place they approved in.
	By string `json:"by"`
	// Via is ApprovalViaCodeHost or ApprovalViaConsole.
	Via string `json:"via"`
	// At is the RFC 3339 time of the approval.
	At string `json:"at"`
}

// VerdictGap is one thing that keeps a passed verdict from being verified:
// something missing, or something the producer does not know. Its kind fixes
// which members it carries: a project for GapNoRequirement and
// GapUnknownRequirements, a project and a requirement for
// GapUnprovenRequirement, a path for GapUnapprovedDeclaration, and none for
// GapUnknownImpact and GapUnknownDeclarations.
type VerdictGap struct {
	// Kind is the gap's kind.
	Kind string `json:"kind"`
	// Project is the impacted project the gap is about.
	Project string `json:"project,omitempty"`
	// Requirement is the unproven requirement, as feature#requirement.
	Requirement string `json:"requirement,omitempty"`
	// Path is the declaration without an approval.
	Path string `json:"path,omitempty"`
}

// VerdictDeploy is the answer of the deploys a change triggered.
type VerdictDeploy struct {
	// Answer is FoldVerdictAnswers of the channels' answers.
	Answer string `json:"answer"`
	// Channels is every channel the deploys follow, sorted by name.
	Channels []VerdictChannel `json:"channels"`
}

// VerdictChannel is the deploy answer of one channel.
type VerdictChannel struct {
	// Channel is the channel's name.
	Channel string `json:"channel"`
	// Answer is the answer of the deploys that follow the channel.
	Answer string `json:"answer"`
}

// FoldVerdictAnswers folds answers into one, the most severe first: failed,
// infrastructure, canceled, unanswered, then passed. AnswerUnsupported never
// blocks: it is the fold of no answer, and of answers that are all
// unsupported. An answer outside the vocabulary ranks like AnswerUnsupported;
// ValidateVerdict refuses it.
func FoldVerdictAnswers(answers ...string) string {
	best := len(verdictAnswerSeverity)
	for _, answer := range answers {
		if rank := slices.Index(verdictAnswerSeverity, answer); rank >= 0 && rank < best {
			best = rank
		}
	}
	if best == len(verdictAnswerSeverity) {
		return AnswerUnsupported
	}
	return verdictAnswerSeverity[best]
}

// FoldRequiredChecks is the answer of a verdict whose checks are checks: the
// fold of its required checks' answers. An advisory check never changes it.
func FoldRequiredChecks(checks []VerdictCheck) string {
	answers := make([]string, 0, len(checks))
	for _, check := range checks {
		if check.Level == LevelRequired {
			answers = append(answers, check.Answer)
		}
	}
	return FoldVerdictAnswers(answers...)
}

// VerdictMarkFor is the mark of a verdict with answer and gaps: MarkVerified
// when it passed with no gap, MarkChecked when it passed with at least one,
// and none when it did not pass.
func VerdictMarkFor(answer string, gaps []VerdictGap) string {
	switch {
	case answer != AnswerPassed:
		return ""
	case len(gaps) == 0:
		return MarkVerified
	}
	return MarkChecked
}

// ParseVerdict strictly decodes one Verdict and validates it. It refuses a
// document above MaxVerdictBytes, a duplicate object key, an unknown member,
// trailing data, and every defect ValidateVerdict refuses.
func ParseVerdict(data []byte) (Verdict, error) {
	if len(data) > MaxVerdictBytes {
		return Verdict{}, fmt.Errorf("verdict is %d bytes; maximum is %d", len(data), MaxVerdictBytes)
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return Verdict{}, fmt.Errorf("verdict is not strict JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var verdict Verdict
	if err := decoder.Decode(&verdict); err != nil {
		return Verdict{}, fmt.Errorf("verdict is not strict JSON: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Verdict{}, fmt.Errorf("verdict is not strict JSON: %w", err)
	}
	if err := ValidateVerdict(verdict); err != nil {
		return Verdict{}, err
	}
	return verdict, nil
}

// ValidateVerdict is the check a consumer runs before it trusts a verdict. It
// refuses another statement or predicate type, a subject that is not the
// change's repository at its head commit, a list that is absent or null,
// malformed identities, an unknown vocabulary value, a merge without a base,
// a list that is unsorted or holds a duplicate, an answer that is not the
// fold of the required checks, requirement counts that do not add up, a list
// that holds entries beside the gap that says it is unknown, gaps that do not
// match the impact and the declarations, a mark that does not follow the
// answer and the gaps, a deploy answer that is not the fold of its channels,
// a computedAt that is not RFC 3339, and a revision below 1. It returns the
// first refusal, in that order.
func ValidateVerdict(verdict Verdict) error {
	if verdict.Type != VerdictStatementType {
		return errors.New("verdict is not an in-toto Statement v1")
	}
	if verdict.PredicateType != VerdictPredicateType {
		return fmt.Errorf("unsupported verdict predicate type %q", verdict.PredicateType)
	}
	p := verdict.Predicate
	if verdict.Subject == nil || p.Checks == nil || p.Impact.Projects == nil || p.Declarations == nil || p.Gaps == nil ||
		(p.Deploy != nil && p.Deploy.Channels == nil) {
		return errors.New("verdict omits a list or writes it as null")
	}
	if err := validateVerdictChange(p.Change); err != nil {
		return err
	}
	if len(verdict.Subject) != 1 || verdict.Subject[0] != (VerdictSubject{Name: p.Change.Repository,
		Digest: VerdictDigest{GitCommit: p.Change.Head}}) {
		return errors.New("verdict subject is not the change's repository at its head commit")
	}
	if !verdictRunIDPattern.MatchString(p.Run.ID) || p.Run.Attempt < 1 || !verdictCodePattern.MatchString(p.Run.Conclusion) {
		return errors.New("verdict run identity is malformed")
	}
	if err := validateVerdictChecks(p.Checks); err != nil {
		return err
	}
	if p.Answer != FoldRequiredChecks(p.Checks) {
		return errors.New("verdict answer is not the fold of its required checks")
	}
	if err := validateVerdictImpact(p.Impact, p.Requirements); err != nil {
		return err
	}
	if err := validateVerdictDeclarations(p.Declarations); err != nil {
		return err
	}
	if err := validateVerdictGaps(p); err != nil {
		return err
	}
	if p.Mark != VerdictMarkFor(p.Answer, p.Gaps) {
		return errors.New("verdict mark does not follow its answer and gaps")
	}
	if err := validateVerdictDeploy(p.Deploy); err != nil {
		return err
	}
	if !rfc3339(p.ComputedAt) {
		return errors.New("verdict computedAt is not an RFC 3339 time")
	}
	if p.Revision < 1 {
		return errors.New("verdict revision is below 1")
	}
	return nil
}

// validateVerdictChange refuses a repository URL that is not credential-free,
// a reference that is not a full Git reference, a revision that is not a full
// lowercase object ID, an unknown tested checkout, and a merge without a base.
func validateVerdictChange(change VerdictChange) error {
	if !credentialFreeRepositoryURL(change.Repository) {
		return errors.New("verdict repository is not a credential-free https, http or ssh URL")
	}
	if !fullGitReference(change.Reference) {
		return errors.New("verdict reference is not a full Git reference")
	}
	if !fullCommitID(change.Head) || !fullCommitID(change.Tree) ||
		(change.Base != "" && !fullCommitID(change.Base)) {
		return errors.New("verdict revisions must be full lowercase object IDs")
	}
	switch change.TestedCheckout {
	case TestedCheckoutHead:
	case TestedCheckoutMerge:
		if change.Base == "" {
			return errors.New("verdict tested a merge without a base")
		}
	default:
		return fmt.Errorf("unknown verdict tested checkout %q", change.TestedCheckout)
	}
	return nil
}

// validateVerdictChecks refuses an unknown answer or level, and checks that
// are not sorted by a well-formed unique name or that carry a malformed phase
// or reason. Only a check that did not pass carries a reason.
func validateVerdictChecks(checks []VerdictCheck) error {
	if len(checks) > MaxVerdictChecks {
		return fmt.Errorf("verdict has more than %d checks", MaxVerdictChecks)
	}
	for i, check := range checks {
		if !knownVerdictAnswer(check.Answer) {
			return fmt.Errorf("unknown verdict answer %q", check.Answer)
		}
		if check.Level != LevelRequired && check.Level != LevelAdvisory {
			return fmt.Errorf("unknown verdict check level %q", check.Level)
		}
		if !verdictCheckPattern.MatchString(check.Check) || (i > 0 && checks[i-1].Check >= check.Check) ||
			(check.Phase != "" && !commandPattern.MatchString(check.Phase)) ||
			(check.Reason != "" && (check.Answer == AnswerPassed || !verdictReasonPattern.MatchString(check.Reason))) {
			return errors.New("verdict checks are not canonical")
		}
	}
	return nil
}

// validateVerdictImpact refuses projects that are not sorted by a
// well-formed unique ID, an unknown relation, a via on a touched project or
// one that names no other impacted project, a project whose counts do not add
// up, and totals that are not the sums of the projects' counts. A project
// without counts adds to ProjectsUnknown only.
func validateVerdictImpact(impact VerdictImpact, totals VerdictRequirements) error {
	projects := impact.Projects
	if len(projects) > MaxVerdictProjects {
		return fmt.Errorf("verdict has more than %d impacted projects", MaxVerdictProjects)
	}
	listed := make(map[string]bool, len(projects))
	for _, project := range projects {
		listed[project.Project] = true
	}
	var sum VerdictRequirements
	for i, project := range projects {
		if project.Relation != RelationTouched && project.Relation != RelationReached {
			return fmt.Errorf("unknown verdict project relation %q", project.Relation)
		}
		if !verdictProjectID(project.Project) || (i > 0 && projects[i-1].Project >= project.Project) ||
			(project.Via != "" && (project.Relation != RelationReached || project.Via == project.Project || !listed[project.Via])) {
			return errors.New("verdict impacted projects are not canonical")
		}
		counts := project.Requirements
		if counts == nil {
			sum.ProjectsUnknown++
			continue
		}
		if counts.Proven < 0 || counts.Missing < 0 || counts.Proven+counts.Missing != counts.Total {
			return errors.New("verdict project requirement counts do not add up")
		}
		sum.Total += counts.Total
		sum.Proven += counts.Proven
		sum.Missing += counts.Missing
		if counts.Total == 0 {
			sum.ProjectsWithout++
		}
	}
	if totals != sum {
		return errors.New("verdict requirements are not the sums of its impacted projects")
	}
	return nil
}

// validateVerdictDeclarations refuses an unknown kind, an approval that is
// incomplete, and declarations that are not sorted weakening first, then by a
// well-formed unique path.
func validateVerdictDeclarations(declarations []VerdictDeclaration) error {
	if len(declarations) > MaxVerdictDeclarations {
		return fmt.Errorf("verdict has more than %d declarations", MaxVerdictDeclarations)
	}
	paths := make(map[string]bool, len(declarations))
	for i, declaration := range declarations {
		if !slices.Contains(verdictDeclarationKinds, declaration.Kind) {
			return fmt.Errorf("unknown verdict declaration kind %q", declaration.Kind)
		}
		if approval := declaration.Approval; approval != nil &&
			(!verdictText(approval.By, 256) || (approval.Via != ApprovalViaCodeHost && approval.Via != ApprovalViaConsole) || !rfc3339(approval.At)) {
			return errors.New("verdict approval is incomplete")
		}
		if !workspacePath(declaration.Path) || paths[declaration.Path] ||
			(i > 0 && !declarationSortsBefore(declarations[i-1], declaration)) {
			return errors.New("verdict declarations are not canonical")
		}
		paths[declaration.Path] = true
	}
	return nil
}

// declarationSortsBefore reports whether left sorts before right: a weakening
// declaration first, then by path.
func declarationSortsBefore(left, right VerdictDeclaration) bool {
	if left.Weakening != right.Weakening {
		return left.Weakening
	}
	return left.Path < right.Path
}

// validateVerdictGaps refuses gaps that are not sorted and unique, that carry
// the members of another kind, or that do not match the predicate: one
// unknown-requirements gap per impacted project without counts, one
// no-requirement gap per impacted project that states no requirement, as many
// unproven-requirement gaps for a project as its missing count, and one
// unapproved-declaration gap per declaration without an approval. Only the
// producer knows whether its impact or its declarations are unknown, so those
// two gaps are not derived: each one is refused beside the list it says is
// unknown.
func validateVerdictGaps(p VerdictPredicate) error {
	gaps := p.Gaps
	if len(gaps) > MaxVerdictGaps {
		return fmt.Errorf("verdict has more than %d gaps", MaxVerdictGaps)
	}
	for i, gap := range gaps {
		var shaped bool
		switch gap.Kind {
		case GapNoRequirement, GapUnknownRequirements:
			shaped = verdictProjectID(gap.Project) && gap.Requirement == "" && gap.Path == ""
		case GapUnprovenRequirement:
			shaped = verdictProjectID(gap.Project) && len(gap.Requirement) <= 512 &&
				verdictRequirementPattern.MatchString(gap.Requirement) && gap.Path == ""
		case GapUnapprovedDeclaration:
			shaped = gap.Project == "" && gap.Requirement == "" && workspacePath(gap.Path)
		case GapUnknownImpact, GapUnknownDeclarations:
			shaped = gap == VerdictGap{Kind: gap.Kind}
		default:
			return fmt.Errorf("unknown verdict gap kind %q", gap.Kind)
		}
		if !shaped || (i > 0 && !gapSortsBefore(gaps[i-1], gap)) {
			return errors.New("verdict gaps are not canonical")
		}
	}
	want := map[VerdictGap]int{}
	for _, project := range p.Impact.Projects {
		counts := project.Requirements
		switch {
		case counts == nil:
			want[VerdictGap{Kind: GapUnknownRequirements, Project: project.Project}] = 1
		case counts.Total == 0:
			want[VerdictGap{Kind: GapNoRequirement, Project: project.Project}] = 1
		case counts.Missing > 0:
			want[VerdictGap{Kind: GapUnprovenRequirement, Project: project.Project}] = counts.Missing
		}
	}
	for _, declaration := range p.Declarations {
		if declaration.Approval == nil {
			want[VerdictGap{Kind: GapUnapprovedDeclaration, Path: declaration.Path}] = 1
		}
	}
	got := map[VerdictGap]int{}
	for _, gap := range gaps {
		switch {
		case gap.Kind == GapUnknownImpact && len(p.Impact.Projects) > 0:
			return errors.New("verdict lists impacted projects beside an unknown impact")
		case gap.Kind == GapUnknownDeclarations && len(p.Declarations) > 0:
			return errors.New("verdict lists declarations beside unknown declarations")
		case gap.Kind == GapUnknownImpact || gap.Kind == GapUnknownDeclarations:
			continue
		}
		gap.Requirement = ""
		got[gap]++
	}
	if len(got) != len(want) {
		return errors.New("verdict gaps do not match its impact and declarations")
	}
	for gap, count := range want {
		if got[gap] != count {
			return errors.New("verdict gaps do not match its impact and declarations")
		}
	}
	return nil
}

// gapSortsBefore reports whether left sorts strictly before right: by kind,
// project, requirement, then path.
func gapSortsBefore(left, right VerdictGap) bool {
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if left.Project != right.Project {
		return left.Project < right.Project
	}
	if left.Requirement != right.Requirement {
		return left.Requirement < right.Requirement
	}
	return left.Path < right.Path
}

// validateVerdictDeploy refuses deploy channels that are empty, unsorted,
// duplicated, misnamed or unknown answers, and a deploy answer that is not
// their fold.
func validateVerdictDeploy(deploy *VerdictDeploy) error {
	if deploy == nil {
		return nil
	}
	channels := deploy.Channels
	if len(channels) == 0 || len(channels) > MaxVerdictChannels {
		return fmt.Errorf("verdict deploy names no channel or more than %d", MaxVerdictChannels)
	}
	answers := make([]string, 0, len(channels))
	for i, channel := range channels {
		if !knownVerdictAnswer(channel.Answer) {
			return fmt.Errorf("unknown verdict answer %q", channel.Answer)
		}
		if !validChannelName(channel.Channel, false) || (i > 0 && channels[i-1].Channel >= channel.Channel) {
			return errors.New("verdict deploy channels are not canonical")
		}
		answers = append(answers, channel.Answer)
	}
	if !knownVerdictAnswer(deploy.Answer) {
		return fmt.Errorf("unknown verdict answer %q", deploy.Answer)
	}
	if deploy.Answer != FoldVerdictAnswers(answers...) {
		return errors.New("verdict deploy answer is not the fold of its channels")
	}
	return nil
}

func knownVerdictAnswer(answer string) bool {
	return answer == AnswerUnsupported || slices.Contains(verdictAnswerSeverity, answer)
}

// credentialFreeRepositoryURL reports whether value spells a repository the
// way a ChangePlan does: an https, http or ssh URL with a host and a path, and
// no user, query, fragment or trailing slash. Like a ChangePlan, it refuses
// "@" anywhere, because "@" is how a URL carries a user.
func credentialFreeRepositoryURL(value string) bool {
	if !verdictText(value, 2048) || strings.ContainsAny(value, " \\@?#") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" {
		return false
	}
	switch parsed.Scheme {
	case "https", "http", "ssh":
	default:
		return false
	}
	return strings.HasPrefix(value, parsed.Scheme+"://") && parsed.Host != "" &&
		parsed.Path != "" && parsed.Path != "/" && !strings.HasSuffix(parsed.Path, "/")
}

// fullGitReference reports whether value is a full Git reference under refs/
// that git check-ref-format accepts.
func fullGitReference(value string) bool {
	if len(value) > 1024 || !strings.HasPrefix(value, "refs/") || strings.HasSuffix(value, ".") ||
		strings.Contains(value, "..") || strings.Contains(value, "@{") {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r == 0x7f || strings.ContainsRune("~^:?*[\\", r) {
			return false
		}
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return false
		}
	}
	return true
}

// verdictProjectID reports whether value is a workspace project ID, rooted at
// the workspace: a slash, then a project path without dot segments.
func verdictProjectID(value string) bool {
	return len(value) <= 512 && strings.HasPrefix(value, "/") && projectIDPattern.MatchString(value) && !containsDotSegment(value)
}

// workspacePath reports whether value is a workspace-relative file path with
// forward slashes and no dot or empty segment.
func workspacePath(value string) bool {
	return verdictText(value, 1024) && !strings.HasPrefix(value, "/") && !strings.Contains(value, "\\") && !containsDotSegment(value)
}

// verdictText reports whether value is 1 to limit bytes of valid text without
// control characters or surrounding whitespace.
func verdictText(value string, limit int) bool {
	if value == "" || len(value) > limit || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r == unicode.ReplacementChar || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// rfc3339 reports whether value is an RFC 3339 time.
func rfc3339(value string) bool {
	_, err := time.Parse(time.RFC3339, value)
	return err == nil
}
