package releaseset

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
)

func TestFromContextStrictlyReadsOneImmutablePlan(t *testing.T) {
	plan := testPlan(t)
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &job.Context{Params: job.Params{ContextParamName: raw}}
	parsed, err := FromContext(ctx)
	if err != nil {
		t.Fatalf("FromContext: %v", err)
	}
	member, ok := parsed.Member("npm", "@putnami/downstream")
	if !ok || !member.Selected || member.Version != "1.1.0-next" {
		t.Fatalf("selected member = %+v, %v", member, ok)
	}

	parsed.Baseline().ReleaseSet.Members[0].Version = "mutated"
	if plan.Baseline().ReleaseSet.Members[0].Version == "mutated" {
		t.Fatal("FromContext returned an alias into the supplied plan")
	}
}

func TestParseParamsAbsentSupportsFullPublish(t *testing.T) {
	parsed, err := ParseParams(nil)
	if err != nil || parsed != nil {
		t.Fatalf("ParseParams(nil) = %+v, %v; want nil, nil", parsed, err)
	}
}

func TestParseParamsAcceptsAdditiveFieldsAtSupportedVersion(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "release-plan-compatibility", "supported-plans-accept-additive-fields")
	plan := testPlan(t)
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		raw  string
	}{
		{"plan", string(raw[:len(raw)-1]) + `,"sdkUnknownPlanField":true}`},
		{"member", strings.Replace(string(raw), `"selected":`, `"sdkUnknownMemberField":true,"selected":`, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := ParseParams(job.Params{ContextParamName: json.RawMessage(tc.raw)})
			if err != nil {
				t.Fatalf("ParseParams rejected additive field: %v", err)
			}
			if !reflect.DeepEqual(parsed, plan) {
				t.Fatalf("ParseParams changed known plan fields: got %+v, want %+v", parsed, plan)
			}
		})
	}
}

func TestParseParamsRejectsFutureProtocolVersion(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "release-plan-compatibility", "future-plans-name-received-and-supported-versions")
	plan := testPlan(t)
	plan.ProtocolVersion = distribution.ProtocolVersion + 1
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseParams(job.Params{ContextParamName: raw})
	want := fmt.Sprintf("protocolVersion %d is unsupported; the SDK supports %d", plan.ProtocolVersion, distribution.ProtocolVersion)
	if parsed != nil || err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("ParseParams = %+v, %v; want nil and error containing %q", parsed, err, want)
	}
}

func TestParseParamsRejectsTrailingAndOversizedDocuments(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "release-plan-compatibility", "plan-framing-and-size-limits-remain-enforced")
	plan := testPlan(t)
	raw, _ := json.Marshal(plan)
	trailing := append([]byte(nil), raw...)
	trailing = append(trailing, []byte(` {}`)...)
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"trailing", trailing, "trailing"},
		{"null", []byte("null"), "empty"},
		{"oversized", make([]byte, distribution.MaxJSONBytes+1), "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseParams(job.Params{ContextParamName: tc.raw})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestValidatePlanRejectsMutatedUnchangedAndIncompleteMembers(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Plan)
		want string
	}{
		{"unchanged version", func(plan *Plan) {
			for i := range plan.Members {
				if !plan.Members[i].Selected {
					plan.Members[i].Version = "9.9.9"
				}
			}
		}, "exactly inherit"},
		{"unchanged provenance", func(plan *Plan) {
			for i := range plan.Members {
				if !plan.Members[i].Selected {
					plan.Members[i].SelectionFingerprint = testDigest('9')
				}
			}
		}, "exactly inherit"},
		{"unchanged member absent from head", func(plan *Plan) {
			for i := range plan.Members {
				if !plan.Members[i].Selected {
					plan.Members[i].Coordinate = "@putnami/foreign"
				}
			}
		}, "absent from the baseline head"},
		{"selected without provenance", func(plan *Plan) {
			for i := range plan.Members {
				if plan.Members[i].Selected {
					plan.Members[i].SourceRevision = ""
				}
			}
		}, "invalid_source_revision"},
		{"selected platforms", func(plan *Plan) {
			for i := range plan.Members {
				if plan.Members[i].Selected {
					plan.Members[i].Platforms = map[string]string{"linux/amd64": testDigest('c')}
				}
			}
		}, "platform digests"},
		{"v1 plan", func(plan *Plan) { plan.ProtocolVersion = 1 }, "unsupported"},
		{"head not answered", func(plan *Plan) { plan.Heads = map[string]*distribution.ChannelHead{} }, "channels are invalid"},
		{"unrequested head", func(plan *Plan) {
			plan.Heads["stable"] = plan.Heads["canary"]
		}, "channels are invalid"},
		{"head with generation zero", func(plan *Plan) { plan.Heads["canary"].Generation = 0 }, "channels are invalid"},
		{"unselected without head", func(plan *Plan) {
			plan.Heads = map[string]*distribution.ChannelHead{"canary": nil}
		}, "no head to inherit"},
		{"selected digest", func(plan *Plan) {
			for i := range plan.Members {
				if plan.Members[i].Selected {
					plan.Members[i].ArtifactDigest = testDigest('c')
				}
			}
		}, "already carries"},
		{"selected project", func(plan *Plan) {
			for i := range plan.Members {
				if plan.Members[i].Selected {
					plan.Members[i].ProjectID = ""
				}
			}
		}, "project id"},
		{"non-canonical channel", func(plan *Plan) {
			plan.Channels = []string{"Canary"}
			plan.Heads = map[string]*distribution.ChannelHead{"Canary": plan.Heads["canary"]}
		}, "invalid_channel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := testPlan(t)
			tc.edit(plan)
			if err := ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidatePlan error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// D14: one publication names several channels, every head is resolved before
// planning, and the FIRST is the baseline unchanged members inherit from. The
// other heads are carried only as their own compare-and-swap expectations, so a
// head that differs from the baseline must not change what is inherited.
func TestValidatePlanCarriesEveryResolvedHeadAndBaselinesTheFirst(t *testing.T) {
	plan := testPlan(t)
	other := *plan.Heads["canary"]
	plan.Channels = []string{"canary", "next"}
	plan.Heads["next"] = &other
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("multi-channel plan rejected: %v", err)
	}
	if plan.Baseline() != plan.Heads["canary"] {
		t.Fatal("baseline is not the head of the first listed channel")
	}

	// The baseline follows the channel ORDER, not the map.
	reordered := testPlan(t)
	reordered.Channels = []string{"next", "canary"}
	reordered.Heads = map[string]*distribution.ChannelHead{"canary": plan.Heads["canary"], "next": nil}
	if reordered.Baseline() != nil {
		t.Fatal("baseline must be the first listed channel's head, empty here")
	}
	// With an empty baseline nothing can be inherited, so an unselected member
	// is now a defect even though the other channel does have a head.
	if err := ValidatePlan(reordered); err == nil || !strings.Contains(err.Error(), "no head to inherit") {
		t.Fatalf("inheriting from a non-baseline head accepted: %v", err)
	}
}

// D13: the member key is (ecosystem, coordinate) and the project is provenance,
// so one project contributes several selected members — including two in one
// ecosystem — and the plan must not reject it.
func TestValidatePlanAdmitsSeveralSelectedMembersFromOneProject(t *testing.T) {
	plan := testFullPlanOverHead(t)
	for i := range plan.Members {
		plan.Members[i].ProjectID = "/one"
	}
	plan.Members = append(plan.Members, PlannedMember{
		Ecosystem: "oci", Coordinate: "putnami/one", Version: "1.1.0-next",
		Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision,
		SelectionFingerprint: testDigest('5'), Selected: true, ProjectID: "/one",
	})
	plan = NormalizePlan(plan)
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("several members from one project rejected: %v", err)
	}
}

const testRevision = "8d5edb7513d93b9165ba2a7cb48466d022fc3f63"

func testPlan(t *testing.T) *Plan {
	t.Helper()
	base := distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Members: []distribution.ReleaseSetMember{
			{Ecosystem: "npm", Coordinate: "@putnami/upstream", Version: "1.0.0-old", ArtifactDigest: testDigest('a'), Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision, SelectionFingerprint: testDigest('1')},
			{Ecosystem: "npm", Coordinate: "@putnami/downstream", Version: "1.0.0-old", ArtifactDigest: testDigest('b'), Dependencies: []distribution.ReleaseSetDependency{{Ecosystem: "npm", Coordinate: "@putnami/upstream", Version: "1.0.0-old"}}, SourceRevision: testRevision, SelectionFingerprint: testDigest('2')},
		},
	}
	base = *distribution.NormalizeReleaseSet(&base)
	ref, diagnostics := distribution.DeriveReleaseSetRef(&base)
	if hasDiagnosticErrors(diagnostics) {
		t.Fatalf("derive base ref: %v", diagnostics)
	}
	plan := &Plan{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Channels:        []string{"canary"},
		Heads: map[string]*distribution.ChannelHead{
			"canary": {Ref: ref, Generation: 7, ReleaseSet: &base},
		},
		Members: []PlannedMember{
			{Ecosystem: "npm", Coordinate: "@putnami/upstream", Version: "1.0.0-old", ArtifactDigest: testDigest('a'), Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision, SelectionFingerprint: testDigest('1')},
			{Ecosystem: "npm", Coordinate: "@putnami/downstream", Version: "1.1.0-next", Dependencies: []distribution.ReleaseSetDependency{{Ecosystem: "npm", Coordinate: "@putnami/upstream", Version: "1.0.0-old"}}, SourceRevision: testRevision, SelectionFingerprint: testDigest('3'), Selected: true, ProjectID: "/downstream"},
		},
	}
	return NormalizePlan(plan)
}

func testDigest(char byte) string { return "sha256:" + strings.Repeat(string(char), 64) }

func TestValidatePlanFullPublishOverExistingHeadMayChangeMembership(t *testing.T) {
	extra := PlannedMember{
		Ecosystem: "npm", Coordinate: "@putnami/added", Version: "1.1.0-next",
		Dependencies: []distribution.ReleaseSetDependency{}, Selected: true, ProjectID: "/added",
		SourceRevision: testRevision, SelectionFingerprint: testDigest('4'),
	}
	cases := []struct {
		name string
		edit func(*Plan)
	}{
		{"every base member reselected", func(*Plan) {}},
		{"member added since the head", func(plan *Plan) { plan.Members = append(plan.Members, extra) }},
		{"member dropped since the head", func(plan *Plan) {
			kept := plan.Members[:0]
			for _, member := range plan.Members {
				if member.Coordinate == "@putnami/upstream" {
					member.Dependencies = []distribution.ReleaseSetDependency{}
					kept = append(kept, member)
				}
			}
			plan.Members = kept
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := testFullPlanOverHead(t)
			tc.edit(plan)
			plan = NormalizePlan(plan)
			if !plan.SelectsEveryMember() {
				t.Fatalf("fixture must select every member: %+v", plan.Members)
			}
			if err := ValidatePlan(plan); err != nil {
				t.Fatalf("ValidatePlan error = %v, want a valid full plan over the resolved head", err)
			}
			if plan.Baseline() == nil {
				t.Fatalf("full plan lost its CAS baseline: %+v", plan)
			}
		})
	}
}

func TestValidatePlanWithoutHeadSelectsEveryMember(t *testing.T) {
	// An empty channel: a null head, every member selected.
	plan := testFullPlanOverHead(t)
	plan.Heads = map[string]*distribution.ChannelHead{"canary": nil}
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("empty-channel plan rejected: %v", err)
	}
	plan.Members[0].Selected = false
	plan.Members[0].ArtifactDigest = testDigest('a')
	if err := ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), "no head to inherit") {
		t.Fatalf("unselected member without head accepted: %v", err)
	}
}

func TestValidatePlanConfirmsHeadWhenNothingIsSelected(t *testing.T) {
	plan := testPlan(t)
	baseMembers := plan.Baseline().ReleaseSet.Members
	for i := range plan.Members {
		plan.Members[i] = inheritedMember(baseMembers[i], "/"+strings.TrimPrefix(baseMembers[i].Coordinate, "@putnami/"))
	}
	plan = NormalizePlan(plan)
	if plan.SelectsEveryMember() || len(plan.SelectedMembers()) != 0 {
		t.Fatalf("fixture must select nothing: %+v", plan.Members)
	}
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("no-impact plan rejected: %v", err)
	}
	plan.Heads = map[string]*distribution.ChannelHead{"canary": nil}
	if err := ValidatePlan(plan); err == nil {
		t.Fatal("no-impact plan without a head accepted")
	}
	if (&Plan{}).SelectsEveryMember() {
		t.Fatal("an empty plan must not count as a full publication")
	}
}

func TestClonePlanDoesNotAliasPlatformsOrHeads(t *testing.T) {
	plan := testPlan(t)
	plan.Members[0].Platforms = map[string]string{"linux/amd64": testDigest('c')}
	clone := ClonePlan(plan)
	clone.Members[0].Platforms["darwin/arm64"] = testDigest('d')
	if len(plan.Members[0].Platforms) != 1 {
		t.Fatal("ClonePlan aliased the platform map")
	}
	clone.Heads["canary"].Generation = 99
	if plan.Heads["canary"].Generation == 99 {
		t.Fatal("ClonePlan aliased a resolved head")
	}
}

func inheritedMember(member distribution.ReleaseSetMember, projectID string) PlannedMember {
	return PlannedMember{
		Ecosystem: member.Ecosystem, Coordinate: member.Coordinate, Version: member.Version,
		ArtifactDigest: member.ArtifactDigest, Dependencies: append([]distribution.ReleaseSetDependency(nil), member.Dependencies...),
		SourceRevision: member.SourceRevision, SelectionFingerprint: member.SelectionFingerprint,
		Project: member.Project, Kind: member.Kind, SourceTree: member.SourceTree,
		ProjectID: projectID,
	}
}

// testFullPlanOverHead is the --all publish over a channel that already has a
// head: the same base as testPlan, every member selected at a new version.
func testFullPlanOverHead(t *testing.T) *Plan {
	t.Helper()
	plan := testPlan(t)
	for i := range plan.Members {
		plan.Members[i].Selected = true
		plan.Members[i].Version = "1.1.0-next"
		plan.Members[i].ArtifactDigest = ""
		for j := range plan.Members[i].Dependencies {
			plan.Members[i].Dependencies[j].Version = "1.1.0-next"
		}
	}
	plan.Members[0].ProjectID = "/" + strings.TrimPrefix(plan.Members[0].Coordinate, "@putnami/")
	plan.Members[1].ProjectID = "/" + strings.TrimPrefix(plan.Members[1].Coordinate, "@putnami/")
	return NormalizePlan(plan)
}

// memberProject is the attribution the fixtures below give a member: the
// canonical logical project id a publisher records for it.
func memberProject(coordinate string) string {
	return "typescript/framework/" + strings.TrimPrefix(coordinate, "@putnami/")
}

// attributedTestPlan is testPlan over a baseline whose members already carry
// their source project and artifact kind: the state every head reaches once a
// publisher records attribution.
func attributedTestPlan(t *testing.T) *Plan {
	t.Helper()
	plan := testPlan(t)
	base := plan.Baseline().ReleaseSet
	for i := range base.Members {
		base.Members[i].Project = memberProject(base.Members[i].Coordinate)
		base.Members[i].Kind = distribution.KindLibrary
	}
	ref, diagnostics := distribution.DeriveReleaseSetRef(base)
	if hasDiagnosticErrors(diagnostics) {
		t.Fatalf("derive attributed base ref: %v", diagnostics)
	}
	plan.Heads["canary"].Ref = ref
	for i := range plan.Members {
		plan.Members[i].Project = memberProject(plan.Members[i].Coordinate)
		plan.Members[i].Kind = distribution.KindLibrary
	}
	return NormalizePlan(plan)
}

// TestValidatePlanOwnsAttributionForEveryMember pins the half of attribution
// the plan owns: every member states the project and kind the coordinator
// declares for it, and an unchanged member may differ from the baseline's
// record in exactly those two fields and nothing else. An attributed plan over
// a head that predates the opt-in is the ordinary way a channel becomes
// attributed: its unchanged members gain their values without being
// republished. The vocabulary and the project grammar apply to every member,
// unchanged ones included.
func TestValidatePlanOwnsAttributionForEveryMember(t *testing.T) {
	if err := ValidatePlan(attributedTestPlan(t)); err != nil {
		t.Fatalf("attributed plan rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*Plan)
	}{
		{"unchanged member drops its project", func(plan *Plan) {
			for i := range plan.Members {
				if !plan.Members[i].Selected {
					plan.Members[i].Project = ""
				}
			}
		}},
		{"unchanged member is re-attributed", func(plan *Plan) {
			for i := range plan.Members {
				if !plan.Members[i].Selected {
					plan.Members[i].Project = "go/framework/other"
				}
			}
		}},
		{"unchanged member is reclassified", func(plan *Plan) {
			for i := range plan.Members {
				if !plan.Members[i].Selected {
					plan.Members[i].Kind = distribution.KindImage
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := attributedTestPlan(t)
			tc.edit(plan)
			if err := ValidatePlan(plan); err != nil {
				t.Fatalf("attribution is the plan's statement, not the head's: %v", err)
			}
		})
	}

	cases := []struct {
		name string
		edit func(*Plan)
		want string
	}{
		{"unchanged member states a kind outside the vocabulary", func(plan *Plan) {
			for i := range plan.Members {
				if !plan.Members[i].Selected {
					plan.Members[i].Kind = "container"
				}
			}
		}, "invalid_kind"},
		{"unchanged member states an unrepresentable project", func(plan *Plan) {
			for i := range plan.Members {
				if !plan.Members[i].Selected {
					plan.Members[i].Project = "/leading/slash"
				}
			}
		}, "invalid_project"},
		{"selected member states a kind outside the vocabulary", func(plan *Plan) {
			for i := range plan.Members {
				if plan.Members[i].Selected {
					plan.Members[i].Kind = "container"
				}
			}
		}, "invalid_kind"},
		{"selected member states an unrepresentable project", func(plan *Plan) {
			for i := range plan.Members {
				if plan.Members[i].Selected {
					plan.Members[i].Project = "/leading/slash"
				}
			}
		}, "invalid_project"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := attributedTestPlan(t)
			tc.edit(plan)
			if err := ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidatePlan error = %v, want containing %q", err, tc.want)
			}
		})
	}

	// A baseline that predates attribution keeps validating, whether the plan
	// leaves its unchanged members bare or attributes them: the first is a
	// plan built without the opt-in, the second is the opt-in reaching a head
	// published before it.
	for _, attributeUnchanged := range []bool{false, true} {
		legacy := testPlan(t)
		for i := range legacy.Members {
			if legacy.Members[i].Selected || attributeUnchanged {
				legacy.Members[i].Project = memberProject(legacy.Members[i].Coordinate)
				legacy.Members[i].Kind = distribution.KindLibrary
			}
		}
		if err := ValidatePlan(legacy); err != nil {
			t.Fatalf("plan over a baseline without attribution (unchanged attributed: %v) rejected: %v", attributeUnchanged, err)
		}
	}
}

// TestAttributionSurvivesTheJobContextRoundTrip pins the transport: the plan an
// extension reads carries the same attribution the coordinator wrote, so a
// publish job and the final snapshot never disagree about it.
func TestAttributionSurvivesTheJobContextRoundTrip(t *testing.T) {
	plan := attributedTestPlan(t)
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseParams(job.Params{ContextParamName: raw})
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	for _, member := range parsed.Members {
		if member.Project != memberProject(member.Coordinate) || member.Kind != distribution.KindLibrary {
			t.Fatalf("round-tripped member lost its attribution: %+v", member)
		}
	}

	// A member with no attribution omits both keys rather than writing empty
	// strings, so an unattributed plan is byte-identical to the one this SDK
	// produced before the fields existed.
	bare, err := json.Marshal(testPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), `"project"`) || strings.Contains(string(bare), `"kind"`) {
		t.Fatalf("an unattributed plan emits empty attribution keys: %s", bare)
	}
}

// D14, amended by protocols/distribution ADR 0006: a plan may measure against a
// channel it does not advance. The named baseline's head rides in Heads beside
// the advanced channels, because the publication resolved all of them in ONE
// exchange, and it is never a channel this plan releases.
func TestValidatePlanMeasuresAgainstANamedBaselineChannel(t *testing.T) {
	plan := testPlan(t)
	canary := plan.Heads["canary"]
	// pr-7 is empty and advanced; canary has the head and is only read.
	plan.Channels = []string{"pr-7"}
	plan.BaselineChannel = "canary"
	plan.Heads = map[string]*distribution.ChannelHead{"pr-7": nil, "canary": canary}
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("plan over a named baseline channel rejected: %v", err)
	}
	if plan.Baseline() != canary || plan.BaselineChannelName() != "canary" {
		t.Fatalf("baseline = %+v / %q, want canary's head", plan.Baseline(), plan.BaselineChannelName())
	}

	// The head of the first ADVANCED channel wins whenever it exists, so a
	// publication that already has one behaves exactly as before.
	own := testPlan(t)
	ownHead := *own.Heads["canary"]
	own.Channels = []string{"pr-7"}
	own.BaselineChannel = "canary"
	own.Heads = map[string]*distribution.ChannelHead{"pr-7": &ownHead, "canary": own.Heads["canary"]}
	if own.Baseline() != &ownHead || own.BaselineChannelName() != "pr-7" {
		t.Fatalf("baseline = %+v / %q, want pr-7's own head", own.Baseline(), own.BaselineChannelName())
	}

	// Neither head: nothing to inherit, so an unselected member is a defect and
	// the report names no baseline channel.
	empty := testFullPlanOverHead(t)
	empty.Channels = []string{"pr-7"}
	empty.BaselineChannel = "canary"
	empty.Heads = map[string]*distribution.ChannelHead{"pr-7": nil, "canary": nil}
	if empty.Baseline() != nil || empty.BaselineChannelName() != "" {
		t.Fatalf("baseline = %+v / %q, want none", empty.Baseline(), empty.BaselineChannelName())
	}
	if err := ValidatePlan(empty); err != nil {
		t.Fatalf("plan over two empty channels rejected: %v", err)
	}
	empty.Members[0].Selected = false
	empty.Members[0].ArtifactDigest = testDigest('a')
	if err := ValidatePlan(empty); err == nil || !strings.Contains(err.Error(), "no head to inherit") {
		t.Fatalf("unselected member without any head accepted: %v", err)
	}
}

// A baseline is READ and never released. A channel that is both would state two
// different things about one head, so the plan is refused before its heads are
// replayed.
func TestValidatePlanRefusesABaselineChannelItAdvances(t *testing.T) {
	plan := testPlan(t)
	plan.BaselineChannel = "canary"
	if err := ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), "is also advanced by the plan") {
		t.Fatalf("baseline channel among the advanced channels accepted: %v", err)
	}

	// "" names no baseline, so a malformed plan carrying it among its CHANNELS
	// is a channel fault. The diagnostic has to name that member: sending a
	// reader to the baseline field, which is empty and correct, is the one
	// answer that cannot be acted on.
	unnamed := testPlan(t)
	unnamed.Channels = append(unnamed.Channels, "")
	err := ValidatePlan(unnamed)
	if err == nil || strings.Contains(err.Error(), "baseline") {
		t.Fatalf("empty channel reported as a baseline fault: %v", err)
	}
}

// The plan's heads replay the publication's ONE resolve, so the baseline's head
// is a REQUESTED channel: an extension built before this member existed refuses
// the same plan with "which was not requested", which is the SDK floor ADR 0006
// records.
func TestPlanHeadsReplayTheResolveThatIncludedTheBaseline(t *testing.T) {
	plan := testPlan(t)
	canary := plan.Heads["canary"]
	plan.Channels = []string{"pr-7"}
	plan.Heads = map[string]*distribution.ChannelHead{"pr-7": nil, "canary": canary}
	if err := ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), "which was not requested") {
		t.Fatalf("an unrequested head was accepted without a baseline channel: %v", err)
	}
	plan.BaselineChannel = "canary"
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("the same heads with the baseline named were rejected: %v", err)
	}
}

// testTree is the git tree a fixture member was built from.
const testTree = "c0ffee5f1e2d3c4b5a69788796a5b4c3d2e1f0a9"

// sourceTreeTestPlan is testPlan over a baseline whose members record the tree
// they were built from, with the selected member built from the same tree.
func sourceTreeTestPlan(t *testing.T) *Plan {
	t.Helper()
	plan := testPlan(t)
	base := plan.Baseline().ReleaseSet
	for i := range base.Members {
		base.Members[i].SourceTree = testTree
	}
	ref, diagnostics := distribution.DeriveReleaseSetRef(base)
	if hasDiagnosticErrors(diagnostics) {
		t.Fatalf("derive base ref: %v", diagnostics)
	}
	plan.Heads["canary"].Ref = ref
	for i := range plan.Members {
		plan.Members[i].SourceTree = testTree
	}
	return NormalizePlan(plan)
}

// TestValidatePlanHoldsTheSourceTreeToTheBaselineRecord pins that the source
// tree is provenance, not attribution: it belongs to the publication that built
// the artifact, so an unchanged member inherits it from the head verbatim like
// its revision, and a present value is held to the protocol's grammar.
func TestValidatePlanHoldsTheSourceTreeToTheBaselineRecord(t *testing.T) {
	if err := ValidatePlan(sourceTreeTestPlan(t)); err != nil {
		t.Fatalf("plan recording source trees rejected: %v", err)
	}
	// A baseline that predates the field keeps validating, whether or not the
	// member this plan republishes records a tree.
	for _, record := range []bool{false, true} {
		legacy := testPlan(t)
		for i := range legacy.Members {
			if record && legacy.Members[i].Selected {
				legacy.Members[i].SourceTree = testTree
			}
		}
		if err := ValidatePlan(legacy); err != nil {
			t.Fatalf("plan over a baseline without source trees (selected records one: %v) rejected: %v", record, err)
		}
	}
	cases := []struct {
		name string
		edit func(*Plan)
		want string
	}{
		{"unchanged member drops its source tree", func(plan *Plan) {
			for i := range plan.Members {
				if !plan.Members[i].Selected {
					plan.Members[i].SourceTree = ""
				}
			}
		}, "does not exactly inherit its baseline record"},
		{"unchanged member states another source tree", func(plan *Plan) {
			for i := range plan.Members {
				if !plan.Members[i].Selected {
					plan.Members[i].SourceTree = strings.Repeat("e", 40)
				}
			}
		}, "does not exactly inherit its baseline record"},
		{"selected member states an abbreviated source tree", func(plan *Plan) {
			for i := range plan.Members {
				if plan.Members[i].Selected {
					plan.Members[i].SourceTree = "c0ffee5"
				}
			}
		}, "invalid_source_tree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := sourceTreeTestPlan(t)
			tc.edit(plan)
			if err := ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidatePlan error = %v, want containing %q", err, tc.want)
			}
		})
	}

	// The job context carries the tree to the extension, and a plan without
	// one is byte-identical to what this SDK produced before the field.
	raw, err := json.Marshal(sourceTreeTestPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseParams(job.Params{ContextParamName: raw})
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	for _, member := range parsed.Members {
		if member.SourceTree != testTree {
			t.Fatalf("round-tripped member lost its source tree: %+v", member)
		}
	}
	bare, err := json.Marshal(testPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), `"sourceTree"`) {
		t.Fatalf("a plan without source trees emits the key: %s", bare)
	}
}
