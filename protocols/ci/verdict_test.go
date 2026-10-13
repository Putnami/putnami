package ci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// verdictExpectations is fixtures/verdict/expectations.json, the corpus index:
// the answer and mark of every valid fixture, and the refusal of every invalid
// one.
type verdictExpectations struct {
	Valid   map[string]verdictOutcome `json:"valid"`
	Invalid map[string]string         `json:"invalid"`
}

// verdictOutcome is the answer and the mark a valid fixture carries.
type verdictOutcome struct {
	Answer string `json:"answer"`
	Mark   string `json:"mark,omitempty"`
}

func readVerdictExpectations(t *testing.T) verdictExpectations {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(readFixture(t, "verdict", "expectations.json")))
	decoder.DisallowUnknownFields()
	var index verdictExpectations
	if err := decoder.Decode(&index); err != nil {
		t.Fatalf("decode expectations.json: %v", err)
	}
	return index
}

// readVerdictFixture parses one valid fixture and requires the file to be the
// exact indented encoding of the parsed verdict, which pins the wire member
// names, their order, and which members are omitted when empty.
func readVerdictFixture(t *testing.T, name string) Verdict {
	t.Helper()
	data := readFixture(t, filepath.Join("verdict", "valid"), name)
	verdict, err := ParseVerdict(data)
	if err != nil {
		t.Fatalf("ParseVerdict(%s): %v", name, err)
	}
	encoded, err := json.MarshalIndent(verdict, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	if !bytes.Equal(append(encoded, '\n'), data) {
		t.Fatalf("%s is not the wire encoding of the verdict it holds:\n--- file ---\n%s\n--- encoded ---\n%s",
			name, data, encoded)
	}
	return verdict
}

// cloneVerdict returns a copy of verdict that shares no list or pointer with
// it.
func cloneVerdict(t *testing.T, verdict Verdict) Verdict {
	t.Helper()
	data, err := json.Marshal(verdict)
	if err != nil {
		t.Fatal(err)
	}
	var clone Verdict
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func TestVerdictCorpusMatchesExpectations(t *testing.T) {
	index := readVerdictExpectations(t)
	for kind, names := range map[string][]string{"valid": sortedMapKeys(index.Valid), "invalid": sortedMapKeys(index.Invalid)} {
		entries, err := os.ReadDir(filepath.Join("fixtures", "verdict", kind))
		if err != nil {
			t.Fatal(err)
		}
		var onDisk []string
		for _, entry := range entries {
			onDisk = append(onDisk, entry.Name())
		}
		if len(onDisk) == 0 || !slices.Equal(onDisk, names) {
			t.Fatalf("fixtures/verdict/%s holds %v but expectations.json lists %v", kind, onDisk, names)
		}
	}
}

func TestVerdictValidCorpusCoversEveryAnswerAndMark(t *testing.T) {
	answers := map[string]bool{}
	marks := map[string]bool{}
	for _, outcome := range readVerdictExpectations(t).Valid {
		answers[outcome.Answer] = true
		marks[outcome.Mark] = true
	}
	for _, answer := range append([]string{AnswerUnsupported}, verdictAnswerSeverity...) {
		if !answers[answer] {
			t.Errorf("no valid fixture answers %q", answer)
		}
	}
	for _, mark := range []string{MarkVerified, MarkChecked, ""} {
		if !marks[mark] {
			t.Errorf("no valid fixture carries mark %q", mark)
		}
	}
}

func TestVerdictValidCorpusCoversEveryGapKindAndTestedCheckout(t *testing.T) {
	kinds := map[string]bool{}
	checkouts := map[string]bool{}
	for name := range readVerdictExpectations(t).Valid {
		verdict := readVerdictFixture(t, name)
		checkouts[verdict.Predicate.Change.TestedCheckout] = true
		for _, gap := range verdict.Predicate.Gaps {
			kinds[gap.Kind] = true
		}
	}
	for _, kind := range []string{GapNoRequirement, GapUnprovenRequirement, GapUnapprovedDeclaration,
		GapUnknownImpact, GapUnknownRequirements, GapUnknownDeclarations} {
		if !kinds[kind] {
			t.Errorf("no valid fixture records a %q gap", kind)
		}
	}
	for _, checkout := range []string{TestedCheckoutHead, TestedCheckoutMerge} {
		if !checkouts[checkout] {
			t.Errorf("no valid fixture tested a %q checkout", checkout)
		}
	}
}

func TestVerdictValidCorpus(t *testing.T) {
	for name, want := range readVerdictExpectations(t).Valid {
		t.Run(name, func(t *testing.T) {
			verdict := readVerdictFixture(t, name)
			got := verdictOutcome{Answer: verdict.Predicate.Answer, Mark: verdict.Predicate.Mark}
			if got != want {
				t.Fatalf("answer and mark = %+v, want %+v", got, want)
			}
		})
	}
}

func TestVerdictInvalidCorpus(t *testing.T) {
	for name, refusal := range readVerdictExpectations(t).Invalid {
		t.Run(name, func(t *testing.T) {
			_, err := ParseVerdict(readFixture(t, filepath.Join("verdict", "invalid"), name))
			if err == nil || err.Error() != refusal {
				t.Fatalf("ParseVerdict = %v, want %q", err, refusal)
			}
		})
	}
}

func TestVerdictJSONRoundTrip(t *testing.T) {
	for name := range readVerdictExpectations(t).Valid {
		t.Run(name, func(t *testing.T) {
			verdict := readVerdictFixture(t, name)
			compact, err := json.Marshal(verdict)
			if err != nil {
				t.Fatal(err)
			}
			again, err := ParseVerdict(compact)
			if err != nil {
				t.Fatalf("ParseVerdict of its own encoding: %v", err)
			}
			if !reflect.DeepEqual(again, verdict) {
				t.Fatalf("round trip changed the verdict:\n got %+v\nwant %+v", again, verdict)
			}
		})
	}
}

func TestFoldVerdictAnswersKeepsTheMostSevere(t *testing.T) {
	for _, tc := range []struct {
		answers []string
		want    string
	}{
		{nil, AnswerUnsupported},
		{[]string{AnswerUnsupported, AnswerUnsupported}, AnswerUnsupported},
		{[]string{AnswerUnsupported, AnswerPassed}, AnswerPassed},
		{[]string{AnswerPassed, AnswerUnanswered, AnswerPassed}, AnswerUnanswered},
		{[]string{AnswerUnanswered, AnswerCanceled}, AnswerCanceled},
		{[]string{AnswerCanceled, AnswerInfrastructure}, AnswerInfrastructure},
		{[]string{AnswerPassed, AnswerInfrastructure, AnswerFailed, AnswerUnsupported}, AnswerFailed},
		{[]string{"skipped"}, AnswerUnsupported},
	} {
		if got := FoldVerdictAnswers(tc.answers...); got != tc.want {
			t.Errorf("FoldVerdictAnswers(%v) = %q, want %q", tc.answers, got, tc.want)
		}
	}
	// Every pair folds to its more severe answer, in either order.
	order := []string{AnswerFailed, AnswerInfrastructure, AnswerCanceled, AnswerUnanswered, AnswerPassed, AnswerUnsupported}
	for i, severe := range order {
		for _, mild := range order[i:] {
			if FoldVerdictAnswers(severe, mild) != severe || FoldVerdictAnswers(mild, severe) != severe {
				t.Errorf("%q and %q do not fold to %q", severe, mild, severe)
			}
		}
	}
}

func TestFoldRequiredChecksIgnoresAdvisoryChecks(t *testing.T) {
	required := func(answer string) VerdictCheck {
		return VerdictCheck{Check: "test", Answer: answer, Level: LevelRequired}
	}
	advisory := func(answer string) VerdictCheck {
		return VerdictCheck{Check: "e2e", Answer: answer, Level: LevelAdvisory}
	}
	for _, tc := range []struct {
		checks []VerdictCheck
		want   string
	}{
		{nil, AnswerUnsupported},
		{[]VerdictCheck{advisory(AnswerFailed)}, AnswerUnsupported},
		{[]VerdictCheck{required(AnswerPassed), advisory(AnswerFailed)}, AnswerPassed},
		{[]VerdictCheck{required(AnswerUnsupported), advisory(AnswerInfrastructure)}, AnswerUnsupported},
		{[]VerdictCheck{required(AnswerPassed), required(AnswerUnsupported)}, AnswerPassed},
		{[]VerdictCheck{required(AnswerPassed), required(AnswerCanceled)}, AnswerCanceled},
	} {
		if got := FoldRequiredChecks(tc.checks); got != tc.want {
			t.Errorf("FoldRequiredChecks(%+v) = %q, want %q", tc.checks, got, tc.want)
		}
	}
}

func TestVerdictMarkFollowsTheAnswerAndTheGaps(t *testing.T) {
	gaps := []VerdictGap{{Kind: GapNoRequirement, Project: "/shop"}}
	for _, tc := range []struct {
		answer string
		gaps   []VerdictGap
		want   string
	}{
		{AnswerPassed, nil, MarkVerified},
		{AnswerPassed, []VerdictGap{}, MarkVerified},
		{AnswerPassed, gaps, MarkChecked},
		{AnswerFailed, nil, ""},
		{AnswerFailed, gaps, ""},
		{AnswerInfrastructure, gaps, ""},
		{AnswerCanceled, nil, ""},
		{AnswerUnanswered, nil, ""},
		{AnswerUnsupported, nil, ""},
	} {
		if got := VerdictMarkFor(tc.answer, tc.gaps); got != tc.want {
			t.Errorf("VerdictMarkFor(%q, %d gaps) = %q, want %q", tc.answer, len(tc.gaps), got, tc.want)
		}
	}
}

// A declaration change needs its approval whether it weakens the declaration
// or not: without one, the verdict records a gap and stays checked.
func TestEveryUnapprovedDeclarationIsAGap(t *testing.T) {
	verdict := readVerdictFixture(t, "passed-verified.json")
	declaration := &verdict.Predicate.Declarations[1]
	if declaration.Weakening {
		t.Fatal("the fixture's second declaration must not weaken")
	}
	declaration.Approval = nil
	verdict.Predicate.Gaps = []VerdictGap{{Kind: GapUnapprovedDeclaration, Path: declaration.Path}}
	verdict.Predicate.Mark = MarkChecked
	if err := ValidateVerdict(verdict); err != nil {
		t.Fatalf("ValidateVerdict: %v", err)
	}
}

// A producer that does not know the impact or the declarations says so with a
// gap, so a passed verdict cannot read verified on evidence nobody recorded.
func TestUnknownEvidenceKeepsAPassedVerdictChecked(t *testing.T) {
	verdict := readVerdictFixture(t, "passed-verified.json")
	p := &verdict.Predicate
	p.Impact.Projects = []VerdictProject{}
	p.Requirements = VerdictRequirements{}
	p.Declarations = []VerdictDeclaration{}
	if err := ValidateVerdict(verdict); err != nil {
		t.Fatalf("a verdict that knows the change reaches nothing: %v", err)
	}
	for _, gaps := range [][]VerdictGap{
		{{Kind: GapUnknownImpact}},
		{{Kind: GapUnknownDeclarations}},
		{{Kind: GapUnknownDeclarations}, {Kind: GapUnknownImpact}},
	} {
		p.Gaps, p.Mark = gaps, MarkVerified
		if err := ValidateVerdict(verdict); err == nil || err.Error() != "verdict mark does not follow its answer and gaps" {
			t.Errorf("verified with %+v: ValidateVerdict = %v", gaps, err)
		}
		p.Mark = MarkChecked
		if err := ValidateVerdict(verdict); err != nil {
			t.Errorf("checked with %+v: %v", gaps, err)
		}
	}
}

// A project without counts is a project whose requirements the producer does
// not know: it adds nothing to the sums but ProjectsUnknown, and it carries
// exactly one gap, which is not the gap of a project that states no
// requirement.
func TestAProjectWithoutCountsIsUnknownNotEmpty(t *testing.T) {
	verdict := readVerdictFixture(t, "passed-checked.json")
	p := &verdict.Predicate
	p.Impact.Projects[1].Requirements = nil
	p.Requirements = VerdictRequirements{Total: 4, Proven: 2, Missing: 2, ProjectsUnknown: 1}
	if err := ValidateVerdict(verdict); err == nil || err.Error() != "verdict gaps do not match its impact and declarations" {
		t.Fatalf("a no-requirement gap for a project without counts: ValidateVerdict = %v", err)
	}
	p.Gaps = []VerdictGap{
		{Kind: GapUnapprovedDeclaration, Path: "libs/money/specs/money.json"},
		{Kind: GapUnknownRequirements, Project: "/services/cart"},
		{Kind: GapUnprovenRequirement, Project: "/libs/money", Requirement: "money/rounding#half-even"},
		{Kind: GapUnprovenRequirement, Project: "/libs/money", Requirement: "money/rounding#no-float"},
	}
	if err := ValidateVerdict(verdict); err != nil {
		t.Fatalf("ValidateVerdict: %v", err)
	}
}

// verdictCase is one change to a valid verdict and the refusal it causes.
type verdictCase struct {
	name    string
	base    string
	mutate  func(*Verdict)
	refusal string
}

func TestValidateVerdictRefusals(t *testing.T) {
	const (
		lists        = "verdict omits a list or writes it as null"
		subject      = "verdict subject is not the change's repository at its head commit"
		revisions    = "verdict revisions must be full lowercase object IDs"
		run          = "verdict run identity is malformed"
		checks       = "verdict checks are not canonical"
		projects     = "verdict impacted projects are not canonical"
		counts       = "verdict project requirement counts do not add up"
		approval     = "verdict approval is incomplete"
		declarations = "verdict declarations are not canonical"
		gaps         = "verdict gaps are not canonical"
		gapMatch     = "verdict gaps do not match its impact and declarations"
		sums         = "verdict requirements are not the sums of its impacted projects"
		mark         = "verdict mark does not follow its answer and gaps"
		channels     = "verdict deploy channels are not canonical"
		deployCount  = "verdict deploy names no channel or more than 64"
		computedAt   = "verdict computedAt is not an RFC 3339 time"
		revision     = "verdict revision is below 1"
	)
	const verified, checked = "passed-verified.json", "passed-checked.json"
	cases := []verdictCase{
		{"statement type", verified, func(v *Verdict) { v.Type = "https://in-toto.io/Statement/v0.1" },
			"verdict is not an in-toto Statement v1"},
		{"predicate type", verified, func(v *Verdict) { v.PredicateType = "https://putnami.dev/verdict" },
			`unsupported verdict predicate type "https://putnami.dev/verdict"`},
		{"null subject", verified, func(v *Verdict) { v.Subject = nil }, lists},
		{"null checks", verified, func(v *Verdict) { v.Predicate.Checks = nil }, lists},
		{"null projects", verified, func(v *Verdict) { v.Predicate.Impact.Projects = nil }, lists},
		{"null declarations", verified, func(v *Verdict) { v.Predicate.Declarations = nil }, lists},
		{"null gaps", verified, func(v *Verdict) { v.Predicate.Gaps = nil }, lists},
		{"null channels", verified, func(v *Verdict) { v.Predicate.Deploy.Channels = nil }, lists},
		{"no subject", verified, func(v *Verdict) { v.Subject = []VerdictSubject{} }, subject},
		{"two subjects", verified, func(v *Verdict) { v.Subject = append(v.Subject, v.Subject[0]) }, subject},
		{"subject names another repository", verified, func(v *Verdict) {
			v.Subject[0].Name = "https://example.test/acme/other.git"
		}, subject},
		{"subject commit is not the change's head", verified, func(v *Verdict) {
			v.Subject[0].Digest.GitCommit = v.Predicate.Change.Base
		}, subject},
		{"subject commit is the tested tree", verified, func(v *Verdict) {
			v.Subject[0].Digest.GitCommit = v.Predicate.Change.Tree
		}, subject},
		{"uppercase head", verified, func(v *Verdict) { v.Predicate.Change.Head = strings.ToUpper(v.Predicate.Change.Head) },
			revisions},
		{"long head", verified, func(v *Verdict) { v.Predicate.Change.Head += "0" }, revisions},
		{"short tree", verified, func(v *Verdict) { v.Predicate.Change.Tree = v.Predicate.Change.Tree[:39] }, revisions},
		{"short base", verified, func(v *Verdict) { v.Predicate.Change.Base = "1a2b3c4" }, revisions},
		{"no tested checkout", verified, func(v *Verdict) { v.Predicate.Change.TestedCheckout = "" },
			`unknown verdict tested checkout ""`},
		{"unknown tested checkout", checked, func(v *Verdict) { v.Predicate.Change.TestedCheckout = "rebase" },
			`unknown verdict tested checkout "rebase"`},
		{"tested checkout is a commit", checked, func(v *Verdict) {
			v.Predicate.Change.TestedCheckout = v.Predicate.Change.Head
		}, `unknown verdict tested checkout "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"`},
		{"merge without a base", checked, func(v *Verdict) { v.Predicate.Change.Base = "" },
			"verdict tested a merge without a base"},
		{"empty run ID", verified, func(v *Verdict) { v.Predicate.Run.ID = "" }, run},
		{"run ID with a space", verified, func(v *Verdict) { v.Predicate.Run.ID = "run 1" }, run},
		{"long run ID", verified, func(v *Verdict) { v.Predicate.Run.ID = strings.Repeat("r", 129) }, run},
		{"attempt zero", verified, func(v *Verdict) { v.Predicate.Run.Attempt = 0 }, run},
		{"empty conclusion", verified, func(v *Verdict) { v.Predicate.Run.Conclusion = "" }, run},
		{"conclusion is not a machine code", verified, func(v *Verdict) { v.Predicate.Run.Conclusion = "in-progress" }, run},
		{"too many checks", verified, func(v *Verdict) { v.Predicate.Checks = passingChecks(MaxVerdictChecks + 1) },
			"verdict has more than 512 checks"},
		{"unknown check answer", verified, func(v *Verdict) { v.Predicate.Checks[0].Answer = "skipped" },
			`unknown verdict answer "skipped"`},
		{"unknown check level", verified, func(v *Verdict) { v.Predicate.Checks[0].Level = "optional" },
			`unknown verdict check level "optional"`},
		{"unsorted checks", verified, func(v *Verdict) {
			v.Predicate.Checks[0], v.Predicate.Checks[1] = v.Predicate.Checks[1], v.Predicate.Checks[0]
		}, checks},
		{"duplicate check", verified, func(v *Verdict) { v.Predicate.Checks[1] = v.Predicate.Checks[0] }, checks},
		{"malformed check name", verified, func(v *Verdict) { v.Predicate.Checks[0].Check = "-build" }, checks},
		{"malformed phase", verified, func(v *Verdict) { v.Predicate.Checks[0].Phase = "1build" }, checks},
		{"malformed reason", verified, func(v *Verdict) { v.Predicate.Checks[1].Reason = "Below floor" }, checks},
		{"reason on a passed check", verified, func(v *Verdict) { v.Predicate.Checks[0].Reason = "cached" }, checks},
		{"answer is not the fold", verified, func(v *Verdict) { v.Predicate.Answer = AnswerFailed },
			"verdict answer is not the fold of its required checks"},
		{"answer folds an advisory check", verified, func(v *Verdict) {
			v.Predicate.Answer, v.Predicate.Mark = AnswerFailed, ""
		}, "verdict answer is not the fold of its required checks"},
		{"too many projects", verified, func(v *Verdict) {
			v.Predicate.Impact.Projects = make([]VerdictProject, MaxVerdictProjects+1)
		}, "verdict has more than 4096 impacted projects"},
		{"unknown relation", verified, func(v *Verdict) { v.Predicate.Impact.Projects[0].Relation = "indirect" },
			`unknown verdict project relation "indirect"`},
		{"relative project ID", verified, func(v *Verdict) { v.Predicate.Impact.Projects[0].Project = "services/cart" },
			projects},
		{"project ID with a dot segment", verified, func(v *Verdict) {
			v.Predicate.Impact.Projects[0].Project = "/services/../cart"
		}, projects},
		{"unsorted projects", verified, func(v *Verdict) {
			list := v.Predicate.Impact.Projects
			list[0], list[1] = list[1], list[0]
		}, projects},
		{"via on a touched project", verified, func(v *Verdict) {
			v.Predicate.Impact.Projects[0].Via = "/services/checkout"
		}, projects},
		{"via names its own project", verified, func(v *Verdict) {
			v.Predicate.Impact.Projects[1].Via = "/services/checkout"
		}, projects},
		{"via names an unlisted project", verified, func(v *Verdict) {
			v.Predicate.Impact.Projects[1].Via = "/services/payments"
		}, projects},
		{"negative proven count", verified, func(v *Verdict) {
			v.Predicate.Impact.Projects[0].Requirements = &VerdictRequirementCounts{Total: 4, Proven: -1, Missing: 5}
		}, counts},
		{"missing is not total minus proven", verified, func(v *Verdict) {
			v.Predicate.Impact.Projects[0].Requirements.Missing = 1
		}, counts},
		{"projectsWithout is not the count", verified, func(v *Verdict) { v.Predicate.Requirements.ProjectsWithout = 1 }, sums},
		{"projectsUnknown is not the count", verified, func(v *Verdict) { v.Predicate.Requirements.ProjectsUnknown = 1 }, sums},
		{"a project without counts is not counted", verified, func(v *Verdict) {
			v.Predicate.Impact.Projects[1].Requirements = nil
		}, sums},
		{"a project without counts adds to the sums", checked, func(v *Verdict) {
			v.Predicate.Impact.Projects[0].Requirements = nil
			v.Predicate.Requirements.ProjectsUnknown = 1
		}, sums},
		{"too many declarations", verified, func(v *Verdict) {
			v.Predicate.Declarations = make([]VerdictDeclaration, MaxVerdictDeclarations+1)
		}, "verdict has more than 1024 declarations"},
		{"unknown declaration kind", verified, func(v *Verdict) { v.Predicate.Declarations[1].Kind = "readme" },
			`unknown verdict declaration kind "readme"`},
		{"approval without approver", verified, func(v *Verdict) { v.Predicate.Declarations[0].Approval.By = "" },
			approval},
		{"approval through an unknown place", verified, func(v *Verdict) {
			v.Predicate.Declarations[0].Approval.Via = "email"
		}, approval},
		{"approval without a time", verified, func(v *Verdict) { v.Predicate.Declarations[0].Approval.At = "yesterday" },
			approval},
		{"absolute declaration path", verified, func(v *Verdict) {
			v.Predicate.Declarations[1].Path = "/services/cart/specs/cart.json"
		}, declarations},
		{"declaration path with a backslash", verified, func(v *Verdict) {
			v.Predicate.Declarations[1].Path = `services\cart\specs\cart.json`
		}, declarations},
		{"declaration path with a dot segment", verified, func(v *Verdict) {
			v.Predicate.Declarations[1].Path = "services/cart/./specs/cart.json"
		}, declarations},
		{"declaration path with an empty segment", verified, func(v *Verdict) {
			v.Predicate.Declarations[1].Path = "services/cart//cart.json"
		}, declarations},
		{"duplicate declaration", verified, func(v *Verdict) {
			v.Predicate.Declarations[1].Path = v.Predicate.Declarations[0].Path
		}, declarations},
		{"declarations unsorted by path", verified, func(v *Verdict) {
			list := v.Predicate.Declarations
			list[0].Weakening = false
			list[0], list[1] = list[1], list[0]
		}, declarations},
		{"too many gaps", checked, func(v *Verdict) { v.Predicate.Gaps = make([]VerdictGap, MaxVerdictGaps+1) },
			"verdict has more than 16384 gaps"},
		{"unknown gap kind", checked, func(v *Verdict) { v.Predicate.Gaps[0].Kind = "flaky" },
			`unknown verdict gap kind "flaky"`},
		{"no-requirement gap names a requirement", checked, func(v *Verdict) {
			v.Predicate.Gaps[0].Requirement = "money/rounding#half-even"
		}, gaps},
		{"no-requirement gap names a path", checked, func(v *Verdict) { v.Predicate.Gaps[0].Path = "putnami.ci.json" },
			gaps},
		{"unapproved-declaration gap names a project", checked, func(v *Verdict) {
			v.Predicate.Gaps[1].Project = "/libs/money"
		}, gaps},
		{"unapproved-declaration gap with an absolute path", checked, func(v *Verdict) {
			v.Predicate.Gaps[1].Path = "/libs/money/specs/money.json"
		}, gaps},
		{"unproven-requirement gap without a feature", checked, func(v *Verdict) {
			v.Predicate.Gaps[2].Requirement = "half-even"
		}, gaps},
		{"unproven-requirement gap with a long requirement", checked, func(v *Verdict) {
			v.Predicate.Gaps[3].Requirement = "money/rounding#" + strings.Repeat("a", 512)
		}, gaps},
		{"unproven-requirement gap names a path", checked, func(v *Verdict) {
			v.Predicate.Gaps[2].Path = "libs/money/specs/money.json"
		}, gaps},
		{"unknown-requirements gap names a requirement", checked, func(v *Verdict) {
			v.Predicate.Gaps[0] = VerdictGap{Kind: GapUnknownRequirements, Project: "/services/cart",
				Requirement: "cart/totals#sum"}
		}, gaps},
		{"unknown-requirements gap without a project", checked, func(v *Verdict) {
			v.Predicate.Gaps[0] = VerdictGap{Kind: GapUnknownRequirements}
		}, gaps},
		{"unknown-impact gap names a project", checked, func(v *Verdict) {
			v.Predicate.Gaps = []VerdictGap{{Kind: GapUnknownImpact, Project: "/libs/money"}}
		}, gaps},
		{"unknown-declarations gap names a path", checked, func(v *Verdict) {
			v.Predicate.Gaps = []VerdictGap{{Kind: GapUnknownDeclarations, Path: "putnami.ci.json"}}
		}, gaps},
		{"duplicate unknown-impact gap", checked, func(v *Verdict) {
			v.Predicate.Gaps = []VerdictGap{{Kind: GapUnknownImpact}, {Kind: GapUnknownImpact}}
		}, gaps},
		{"unsorted gaps", checked, func(v *Verdict) {
			list := v.Predicate.Gaps
			list[2], list[3] = list[3], list[2]
		}, gaps},
		{"duplicate gap", checked, func(v *Verdict) { v.Predicate.Gaps[3] = v.Predicate.Gaps[2] }, gaps},
		{"gap for a proven project", checked, func(v *Verdict) {
			v.Predicate.Gaps = append(v.Predicate.Gaps, VerdictGap{
				Kind: GapUnprovenRequirement, Project: "/services/checkout", Requirement: "checkout/pay#card",
			})
		}, gapMatch},
		{"fewer unproven gaps than missing requirements", checked, func(v *Verdict) {
			v.Predicate.Gaps = v.Predicate.Gaps[:3]
		}, gapMatch},
		{"gap for an approved declaration", checked, func(v *Verdict) {
			v.Predicate.Declarations[0].Approval = &VerdictApproval{By: "octo-reviewer", Via: ApprovalViaConsole,
				At: "2026-10-13T09:10:00Z"}
		}, gapMatch},
		{"no gap for an unapproved declaration that does not weaken", verified, func(v *Verdict) {
			v.Predicate.Declarations[1].Approval = nil
		}, gapMatch},
		{"no gap for a project without requirements", checked, func(v *Verdict) {
			v.Predicate.Gaps = v.Predicate.Gaps[1:]
		}, gapMatch},
		{"no gap for a project without counts", checked, func(v *Verdict) {
			v.Predicate.Impact.Projects[2].Requirements = nil
			v.Predicate.Requirements = VerdictRequirements{Total: 3, Proven: 1, Missing: 2, ProjectsWithout: 1,
				ProjectsUnknown: 1}
		}, gapMatch},
		{"unknown-requirements gap for a project with counts", checked, func(v *Verdict) {
			v.Predicate.Gaps = slices.Insert(v.Predicate.Gaps, 2,
				VerdictGap{Kind: GapUnknownRequirements, Project: "/services/checkout"})
		}, gapMatch},
		{"unknown-requirements gap for an unlisted project", checked, func(v *Verdict) {
			v.Predicate.Gaps = slices.Insert(v.Predicate.Gaps, 2,
				VerdictGap{Kind: GapUnknownRequirements, Project: "/services/payments"})
		}, gapMatch},
		{"impacted projects beside an unknown impact", checked, func(v *Verdict) {
			v.Predicate.Gaps = slices.Insert(v.Predicate.Gaps, 2, VerdictGap{Kind: GapUnknownImpact})
		}, "verdict lists impacted projects beside an unknown impact"},
		{"declarations beside unknown declarations", checked, func(v *Verdict) {
			v.Predicate.Gaps = slices.Insert(v.Predicate.Gaps, 2, VerdictGap{Kind: GapUnknownDeclarations})
		}, "verdict lists declarations beside unknown declarations"},
		{"verified with gaps", checked, func(v *Verdict) { v.Predicate.Mark = MarkVerified }, mark},
		{"passed without a mark", verified, func(v *Verdict) { v.Predicate.Mark = "" }, mark},
		{"checked without gaps", verified, func(v *Verdict) { v.Predicate.Mark = MarkChecked }, mark},
		{"deploy without channels", verified, func(v *Verdict) { v.Predicate.Deploy.Channels = []VerdictChannel{} },
			deployCount},
		{"too many channels", verified, func(v *Verdict) { v.Predicate.Deploy.Channels = passedChannels(65) },
			deployCount},
		{"unknown channel answer", verified, func(v *Verdict) { v.Predicate.Deploy.Channels[0].Answer = "skipped" },
			`unknown verdict answer "skipped"`},
		{"uppercase channel", verified, func(v *Verdict) { v.Predicate.Deploy.Channels[0].Channel = "Canary" }, channels},
		{"channel placeholder", verified, func(v *Verdict) { v.Predicate.Deploy.Channels[1].Channel = "pr-{number}" },
			channels},
		{"unsorted channels", verified, func(v *Verdict) {
			list := v.Predicate.Deploy.Channels
			list[0], list[1] = list[1], list[0]
		}, channels},
		{"duplicate channel", verified, func(v *Verdict) { v.Predicate.Deploy.Channels[1] = v.Predicate.Deploy.Channels[0] },
			channels},
		{"unknown deploy answer", verified, func(v *Verdict) { v.Predicate.Deploy.Answer = "maybe" },
			`unknown verdict answer "maybe"`},
		{"deploy answer is not the fold", verified, func(v *Verdict) { v.Predicate.Deploy.Answer = AnswerUnsupported },
			"verdict deploy answer is not the fold of its channels"},
		{"computedAt without a zone", verified, func(v *Verdict) { v.Predicate.ComputedAt = "2026-10-13T09:00:00" },
			computedAt},
		{"computedAt with a space", verified, func(v *Verdict) { v.Predicate.ComputedAt = "2026-10-13 09:00:00Z" },
			computedAt},
		{"revision zero", verified, func(v *Verdict) { v.Predicate.Revision = 0 }, revision},
		{"negative revision", verified, func(v *Verdict) { v.Predicate.Revision = -1 }, revision},
	}
	bases := map[string]Verdict{verified: readVerdictFixture(t, verified), checked: readVerdictFixture(t, checked)}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := cloneVerdict(t, bases[tc.base])
			tc.mutate(&verdict)
			if err := ValidateVerdict(verdict); err == nil || err.Error() != tc.refusal {
				t.Fatalf("ValidateVerdict = %v, want %q", err, tc.refusal)
			}
		})
	}
}

// passingChecks returns count required checks that passed, sorted by name.
func passingChecks(count int) []VerdictCheck {
	checks := make([]VerdictCheck, count)
	for i := range checks {
		checks[i] = VerdictCheck{Check: fmt.Sprintf("check-%04d", i), Answer: AnswerPassed, Level: LevelRequired}
	}
	return checks
}

// passedChannels returns count channels whose deploys passed, sorted by name.
func passedChannels(count int) []VerdictChannel {
	channels := make([]VerdictChannel, count)
	for i := range channels {
		channels[i] = VerdictChannel{Channel: fmt.Sprintf("channel-%03d", i), Answer: AnswerPassed}
	}
	return channels
}

func TestValidateVerdictAcceptsItsBounds(t *testing.T) {
	verdict := readVerdictFixture(t, "passed-verified.json")
	verdict.Predicate.Checks = passingChecks(MaxVerdictChecks)
	verdict.Predicate.Deploy.Channels = passedChannels(MaxVerdictChannels)
	verdict.Predicate.Deploy.Answer = AnswerPassed
	verdict.Predicate.Run = VerdictRun{ID: strings.Repeat("r", 128), Attempt: 3, Conclusion: strings.Repeat("c", 64)}
	if err := ValidateVerdict(verdict); err != nil {
		t.Fatalf("ValidateVerdict at its bounds: %v", err)
	}
}

func TestVerdictRepositoryIsACredentialFreeURL(t *testing.T) {
	base := readVerdictFixture(t, "passed-verified.json")
	withRepository := func(repository string) Verdict {
		verdict := cloneVerdict(t, base)
		verdict.Predicate.Change.Repository = repository
		verdict.Subject[0].Name = repository
		return verdict
	}
	for _, repository := range []string{
		"https://example.test/acme/shop.git",
		"http://git.internal.example.test/acme/api",
		"ssh://git.example.test/acme/legacy.git",
		"https://example.test:8443/acme/shop",
	} {
		if err := ValidateVerdict(withRepository(repository)); err != nil {
			t.Errorf("repository %q refused: %v", repository, err)
		}
	}
	for _, repository := range []string{
		"",
		" https://example.test/acme/shop.git",
		"https://example.test/acme/shop.git\n",
		"ftp://example.test/acme/shop.git",
		"HTTPS://example.test/acme/shop.git",
		"https://token@example.test/acme/shop.git",
		"https://user:secret@example.test/acme/shop.git",
		"ssh://git@example.test/acme/shop.git",
		"git@example.test:acme/shop.git",
		"https://example.test/acme/shop.git?ref=main",
		"https://example.test/acme/shop.git#main",
		"https://example.test",
		"https://example.test/",
		"https://example.test/acme/shop/",
		"https:///acme/shop.git",
		"https:example.test/acme/shop.git",
		`https://example.test/acme\shop.git`,
		"https://example.test/acme/my shop.git",
		"https://example.test/" + strings.Repeat("a", 2048),
	} {
		err := ValidateVerdict(withRepository(repository))
		if err == nil || err.Error() != "verdict repository is not a credential-free https, http or ssh URL" {
			t.Errorf("repository %q: ValidateVerdict = %v", repository, err)
		}
	}
}

func TestVerdictReferenceIsAFullGitReference(t *testing.T) {
	base := readVerdictFixture(t, "passed-verified.json")
	withReference := func(reference string) Verdict {
		verdict := cloneVerdict(t, base)
		verdict.Predicate.Change.Reference = reference
		return verdict
	}
	for _, reference := range []string{
		"refs/heads/main", "refs/heads/feature/orders", "refs/merge-requests/42/head", "refs/tags/v1.4.0",
		"refs/heads/release-2026.10",
	} {
		if err := ValidateVerdict(withReference(reference)); err != nil {
			t.Errorf("reference %q refused: %v", reference, err)
		}
	}
	for _, reference := range []string{
		"", "main", "heads/main", "refs/", "refs/heads/", "refs//main", "refs/heads/.hidden",
		"refs/heads/main.lock", "refs/heads/main.", "refs/heads/a..b", "refs/heads/a@{1}", "refs/heads/a b",
		"refs/heads/a~1", "refs/heads/a^", "refs/heads/a:b", "refs/heads/a?", "refs/heads/a*", "refs/heads/a[",
		`refs/heads/a\b`, "refs/heads/a\x7f", "refs/heads/" + strings.Repeat("a", 1024),
	} {
		err := ValidateVerdict(withReference(reference))
		if err == nil || err.Error() != "verdict reference is not a full Git reference" {
			t.Errorf("reference %q: ValidateVerdict = %v", reference, err)
		}
	}
}

func TestParseVerdictIsStrictAndBounded(t *testing.T) {
	valid := readFixture(t, filepath.Join("verdict", "valid"), "passed-verified.json")
	padded := append(append([]byte(nil), valid...), bytes.Repeat([]byte(" "), MaxVerdictBytes-len(valid))...)
	if _, err := ParseVerdict(padded); err != nil {
		t.Fatalf("a verdict of exactly MaxVerdictBytes is refused: %v", err)
	}
	_, err := ParseVerdict(append(padded, ' '))
	if want := fmt.Sprintf("verdict is %d bytes; maximum is %d", MaxVerdictBytes+1, MaxVerdictBytes); err == nil ||
		err.Error() != want {
		t.Fatalf("ParseVerdict above the bound = %v, want %q", err, want)
	}
	for name, data := range map[string][]byte{
		"trailing value": append(append([]byte(nil), valid...), "{}"...),
		"truncated":      valid[:len(valid)/2],
		"wrong type":     bytes.Replace(valid, []byte(`"revision": 2`), []byte(`"revision": "2"`), 1),
		"not an object":  []byte(`[]`),
	} {
		if _, err := ParseVerdict(data); err == nil || !strings.HasPrefix(err.Error(), "verdict is not strict JSON: ") {
			t.Errorf("%s: ParseVerdict = %v", name, err)
		}
	}
}

func TestVerdictSchemaIsEmbeddedAndClosed(t *testing.T) {
	schema := verdictSchema(t)
	if schema["$id"] != VerdictSchemaURL {
		t.Fatalf("schema $id = %v, want %s", schema["$id"], VerdictSchemaURL)
	}
	closed := 0
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch value := node.(type) {
		case map[string]any:
			if value["type"] == "object" {
				closed++
				if value["additionalProperties"] != false {
					t.Errorf("%s is an object schema that is not closed", path)
				}
			}
			for key, child := range value {
				walk(path+"/"+key, child)
			}
		case []any:
			for i, child := range value {
				walk(fmt.Sprintf("%s/%d", path, i), child)
			}
		}
	}
	walk("#", schema)
	// The root, the subject's digest, and fourteen object definitions.
	if closed != 16 {
		t.Fatalf("schema has %d object schemas, want 16", closed)
	}
	copied := VerdictSchema()
	copied[0] = 'x'
	if VerdictSchema()[0] != '{' {
		t.Fatal("VerdictSchema shares its bytes with the caller")
	}
}

// TestVerdictSchemaNamesTheGoMembers pins each schema object to the Go type it
// describes: the same members, and required exactly when Go always writes
// them.
func TestVerdictSchemaNamesTheGoMembers(t *testing.T) {
	schema := verdictSchema(t)
	for path, value := range map[string]any{
		"":                                Verdict{},
		"$defs/subject":                   VerdictSubject{},
		"$defs/subject/properties/digest": VerdictDigest{},
		"$defs/predicate":                 VerdictPredicate{},
		"$defs/change":                    VerdictChange{},
		"$defs/run":                       VerdictRun{},
		"$defs/check":                     VerdictCheck{},
		"$defs/impact":                    VerdictImpact{},
		"$defs/project":                   VerdictProject{},
		"$defs/requirementCounts":         VerdictRequirementCounts{},
		"$defs/requirements":              VerdictRequirements{},
		"$defs/declaration":               VerdictDeclaration{},
		"$defs/approval":                  VerdictApproval{},
		"$defs/gap":                       VerdictGap{},
		"$defs/deploy":                    VerdictDeploy{},
		"$defs/channel":                   VerdictChannel{},
	} {
		node := schemaNode(t, schema, path)
		members, required := jsonMembers(reflect.TypeOf(value))
		properties := sortedMapKeys(node["properties"].(map[string]any))
		if !slices.Equal(properties, members) {
			t.Errorf("%s: schema properties %v, Go members %v", path, properties, members)
		}
		var listed []string
		for _, name := range node["required"].([]any) {
			listed = append(listed, name.(string))
		}
		slices.Sort(listed)
		if !slices.Equal(listed, required) {
			t.Errorf("%s: schema requires %v, Go always writes %v", path, listed, required)
		}
	}
}

func TestVerdictSchemaMatchesTheGoVocabulary(t *testing.T) {
	schema := verdictSchema(t)
	if !strings.HasSuffix(VerdictPredicateType, fmt.Sprintf("/v%d", VerdictVersion)) {
		t.Fatalf("predicate type %s does not carry version %d", VerdictPredicateType, VerdictVersion)
	}
	for path, want := range map[string]any{
		"properties/_type/const":                           VerdictStatementType,
		"properties/predicateType/const":                   VerdictPredicateType,
		"$defs/predicate/properties/checks/maxItems":       float64(MaxVerdictChecks),
		"$defs/predicate/properties/declarations/maxItems": float64(MaxVerdictDeclarations),
		"$defs/predicate/properties/gaps/maxItems":         float64(MaxVerdictGaps),
		"$defs/impact/properties/projects/maxItems":        float64(MaxVerdictProjects),
		"$defs/deploy/properties/channels/maxItems":        float64(MaxVerdictChannels),
		"$defs/run/properties/id/pattern":                  verdictRunIDPattern.String(),
		"$defs/run/properties/conclusion/pattern":          verdictCodePattern.String(),
		"$defs/check/properties/check/pattern":             verdictCheckPattern.String(),
		"$defs/check/properties/phase/pattern":             commandPattern.String(),
		"$defs/check/properties/reason/pattern":            verdictReasonPattern.String(),
		"$defs/gap/properties/requirement/pattern":         verdictRequirementPattern.String(),
		"$defs/channel/properties/channel/pattern":         channelPattern.String(),
	} {
		if got := schemaValue(t, schema, path); got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	for path, want := range map[string][]string{
		"$defs/answer/enum":                           append([]string{AnswerUnsupported}, verdictAnswerSeverity...),
		"$defs/predicate/properties/mark/enum":        {MarkVerified, MarkChecked},
		"$defs/check/properties/level/enum":           {LevelRequired, LevelAdvisory},
		"$defs/project/properties/relation/enum":      {RelationTouched, RelationReached},
		"$defs/declaration/properties/kind/enum":      verdictDeclarationKinds,
		"$defs/approval/properties/via/enum":          {ApprovalViaCodeHost, ApprovalViaConsole},
		"$defs/change/properties/testedCheckout/enum": {TestedCheckoutHead, TestedCheckoutMerge},
		"$defs/gap/properties/kind/enum": {GapNoRequirement, GapUnprovenRequirement, GapUnapprovedDeclaration,
			GapUnknownImpact, GapUnknownRequirements, GapUnknownDeclarations},
	} {
		var got []string
		for _, value := range schemaValue(t, schema, path).([]any) {
			got = append(got, value.(string))
		}
		slices.Sort(got)
		sorted := slices.Sorted(slices.Values(want))
		if !slices.Equal(got, sorted) {
			t.Errorf("%s = %v, want %v", path, got, sorted)
		}
	}
}

func verdictSchema(t *testing.T) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(VerdictSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// schemaValue returns the value at a slash-separated path under schema.
func schemaValue(t *testing.T, schema map[string]any, path string) any {
	t.Helper()
	var node any = schema
	for _, key := range strings.Split(path, "/") {
		object, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("schema has no %s", path)
		}
		if node, ok = object[key]; !ok {
			t.Fatalf("schema has no %s", path)
		}
	}
	return node
}

// schemaNode returns the object at path, the root when path is empty.
func schemaNode(t *testing.T, schema map[string]any, path string) map[string]any {
	t.Helper()
	if path == "" {
		return schema
	}
	return schemaValue(t, schema, path).(map[string]any)
}

// jsonMembers returns the sorted wire names of a struct's members, and the
// sorted names of those it always writes.
func jsonMembers(typ reflect.Type) (members, required []string) {
	for i := range typ.NumField() {
		name, options, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		members = append(members, name)
		if options != "omitempty" {
			required = append(required, name)
		}
	}
	slices.Sort(members)
	slices.Sort(required)
	return members, required
}
