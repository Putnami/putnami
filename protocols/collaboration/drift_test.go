package collaboration

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
)

type schemaNode struct {
	Properties map[string]*schemaNode `json:"properties"`
	Items      *schemaNode            `json:"items"`
	Defs       map[string]*schemaNode `json:"$defs"`
	Ref        string                 `json:"$ref"`
	Enum       []string               `json:"enum"`
	Required   []string               `json:"required"`
	Minimum    *int                   `json:"minimum"`
	Maximum    *int                   `json:"maximum"`
	MaxLength  *int                   `json:"maxLength"`
	MaxItems   *int                   `json:"maxItems"`
	Pattern    string                 `json:"pattern"`
}

func readSchemaNode(t *testing.T, name string) *schemaNode {
	t.Helper()
	data, err := SchemaFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var node schemaNode
	if err := json.Unmarshal(data, &node); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return &node
}

// resolve follows $ref chains inside one contract file and into the common
// schema.
func resolve(t *testing.T, node *schemaNode, local, common *schemaNode) *schemaNode {
	t.Helper()
	for depth := 0; node != nil && node.Ref != ""; depth++ {
		if depth > 8 {
			t.Fatalf("reference chain too deep at %s", node.Ref)
		}
		file, fragment, _ := strings.Cut(node.Ref, "#")
		owner := local
		if file == CommonSchemaFile {
			owner = common
		}
		node = owner.Defs[strings.TrimPrefix(fragment, "/$defs/")]
		if node == nil {
			t.Fatalf("dangling reference")
		}
	}
	return node
}

func sortedProperties(node *schemaNode) []string {
	names := make([]string, 0, len(node.Properties))
	for name := range node.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func goMembers(typ reflect.Type) []string {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	fields := jsonFields(typ)
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestDrift_OperationSchemasMatchGoTypes holds every operation's request and
// result schema on the member set of its Go type, in both directions.
func TestDrift_OperationSchemasMatchGoTypes(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "schemas-track-the-go-types", "operation-schemas-match-the-go-types")
	common := readSchemaNode(t, CommonSchemaFile)
	for _, spec := range Catalog() {
		document := readSchemaNode(t, ContractSchemaFile(spec.Name, spec.Version))
		for _, op := range spec.Operations {
			for _, side := range []struct {
				def   string
				value any
			}{
				{op.Name + "Input", op.NewInput()},
				{op.Name + "Result", op.NewResult()},
			} {
				node := resolve(t, document.Defs[side.def], document, common)
				if node == nil {
					t.Errorf("%s v%d: schema has no $defs.%s", spec.Name, spec.Version, side.def)
					continue
				}
				if got, want := sortedProperties(node), goMembers(reflect.TypeOf(side.value)); !slices.Equal(got, want) {
					t.Errorf("%s v%d %s: schema members %v, Go members %v", spec.Name, spec.Version, side.def, got, want)
				}
			}
		}
	}
}

// TestDrift_EntitySchemasMatchGoTypes pins the shared entities and the
// envelope documents.
func TestDrift_EntitySchemasMatchGoTypes(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "schemas-track-the-go-types", "entity-schemas-match-the-go-types")
	common := readSchemaNode(t, CommonSchemaFile)
	tasks := readSchemaNode(t, ContractSchemaFile(ContractTasks, 1))
	proposals := readSchemaNode(t, ContractSchemaFile(ContractProposals, 1))
	memory := readSchemaNode(t, ContractSchemaFile(ContractMemory, 1))
	missions := readSchemaNode(t, ContractSchemaFile(ContractMissions, 1))
	cases := []struct {
		name   string
		node   *schemaNode
		goType any
	}{
		{"Envelope", common, Envelope{}},
		{"Error", common.Defs["error"], Error{}},
		{"Response", common.Defs["response"], Response{}},
		{"ProviderIdentity", common.Defs["providerIdentity"], ProviderIdentity{}},
		{"Ref", common.Defs["ref"], Ref{}},
		{"PageRequest", common.Defs["pageRequest"], PageRequest{}},
		{"Page", common.Defs["page"], Page{}},
		{"Binding", common.Defs["binding"], Binding{}},
		{"ProviderRequest", common.Defs["providerRequest"], ProviderRequest{}},
		{"ProviderCall", common.Defs["providerCall"], ProviderCall{}},
		{"ProjectRef", common.Defs["projectRef"], ProjectRef{}},
		{"Selection", common.Defs["selection"], Selection{}},
		{"Capabilities", common.Defs["capabilities"], Capabilities{}},
		{"OperationCapability", common.Defs["operationCapability"], OperationCapability{}},
		{"CapabilityIssue", common.Defs["capabilityIssue"], CapabilityIssue{}},
		{"CapabilitiesInput", common.Defs["capabilitiesInput"], CapabilitiesInput{}},
		{"Task", tasks.Defs["task"], Task{}},
		{"Change", proposals.Defs["change"], Change{}},
		{"Proposal", proposals.Defs["proposal"], Proposal{}},
		{"Checks", proposals.Defs["checks"], Checks{}},
		{"Check", proposals.Defs["check"], Check{}},
		{"Review", proposals.Defs["review"], Review{}},
		{"ProposalComment", proposals.Defs["comment"], ProposalComment{}},
		{"MemoryIdentity", memory.Defs["identity"], MemoryIdentity{}},
		{"Evidence", memory.Defs["evidence"], Evidence{}},
		{"MemoryRecord", memory.Defs["record"], MemoryRecord{}},
		{"Provenance", memory.Defs["record"].Properties["provenance"], Provenance{}},
		{"Freshness", memory.Defs["record"].Properties["freshness"], Freshness{}},
		{"Precondition", memory.Defs["precondition"], Precondition{}},
		{"MissionName", missions.Defs["missionName"], MissionName{}},
		{"SessionRef", missions.Defs["session"], SessionRef{}},
		{"PlanStep", missions.Defs["planStep"], PlanStep{}},
		{"MissionPlan", missions.Defs["plan"], MissionPlan{}},
		{"MessageReply", missions.Defs["messageReply"], MessageReply{}},
		{"SessionUsage", missions.Defs["sessionUsage"], SessionUsage{}},
		{"SessionStatus", missions.Defs["syncResult"].Properties["session"], SessionStatus{}},
		{"OpenDecision", missions.Defs["openDecision"], OpenDecision{}},
		{"MissionView", missions.Defs["missionView"], MissionView{}},
		{"InboxDecision", missions.Defs["inboxDecision"], InboxDecision{}},
		{"InboxMessage", missions.Defs["inboxMessage"], InboxMessage{}},
		{"DecisionStatus", missions.Defs["decisionStatus"], DecisionStatus{}},
		{"ReplyStatus", missions.Defs["replyStatus"], ReplyStatus{}},
		{"DecisionOption", missions.Defs["decisionOption"], DecisionOption{}},
		{"DecisionRecommendation", missions.Defs["askInput"].Properties["recommendation"], DecisionRecommendation{}},
		{"MissionDecision", missions.Defs["askResult"].Properties["decision"], MissionDecision{}},
		{"RepliedMessage", missions.Defs["replyResult"].Properties["message"], RepliedMessage{}},
		{"MissionSession", missions.Defs["missionSession"], MissionSession{}},
	}
	for _, tc := range cases {
		if tc.node == nil {
			t.Errorf("%s: schema definition missing", tc.name)
			continue
		}
		if got, want := sortedProperties(tc.node), goMembers(reflect.TypeOf(tc.goType)); !slices.Equal(got, want) {
			t.Errorf("%s: schema members %v, Go members %v", tc.name, got, want)
		}
	}
	bindings := common.Defs["bindings"]
	if got := sortedProperties(bindings); !slices.Equal(got, ContractNames) {
		t.Errorf("bindings members %v, want ContractNames %v", got, ContractNames)
	}
}

// TestDrift_EnumsMatchGoVocabularies keeps every closed vocabulary on one
// member set.
func TestDrift_EnumsMatchGoVocabularies(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "schemas-track-the-go-types", "enums-match-the-go-vocabularies")
	common := readSchemaNode(t, CommonSchemaFile)
	tasks := readSchemaNode(t, ContractSchemaFile(ContractTasks, 1))
	proposals := readSchemaNode(t, ContractSchemaFile(ContractProposals, 1))
	memory := readSchemaNode(t, ContractSchemaFile(ContractMemory, 1))
	missions := readSchemaNode(t, ContractSchemaFile(ContractMissions, 1))
	cases := []struct {
		name string
		enum []string
		want []string
	}{
		{"contract", common.Defs["contract"].Enum, ContractNames},
		{"outcome", common.Defs["outcome"].Enum, stringsOf(ValidOutcomes)},
		{"preconditions", common.Defs["preconditions"].Enum, stringsOf(ValidPreconditions)},
		{"manifest preconditions", stringsOf(ValidPreconditions), extension.ProviderPreconditions},
		{"selection.mode", common.Defs["selection"].Properties["mode"].Enum, SelectionModes},
		{"capabilities.status", common.Defs["capabilities"].Properties["status"].Enum, stringsOf(ValidBindingStatuses)},
		{"taskState", tasks.Defs["taskState"].Enum, stringsOf(ValidTaskStates)},
		{"proposalState", proposals.Defs["proposalState"].Enum, stringsOf(ValidProposalStates)},
		{"checks.state", proposals.Defs["checks"].Properties["state"].Enum, stringsOf(ValidChecksStates)},
		{"check.state", proposals.Defs["check"].Properties["state"].Enum, stringsOf(ValidCheckStates)},
		{"reviewVerdict", proposals.Defs["reviewVerdict"].Enum, stringsOf(ValidReviewVerdicts)},
		{"mergeInput.method", proposals.Defs["mergeInput"].Properties["method"].Enum, stringsOf(ValidMergeMethods)},
		{"memory kind", memory.Defs["kind"].Enum, stringsOf(ValidMemoryKinds)},
		{"evidence kind", memory.Defs["evidence"].Properties["kind"].Enum, stringsOf(ValidEvidenceKinds)},
		{"sessionState", missions.Defs["sessionState"].Enum, stringsOf(ValidSessionStates)},
		{"loopStage", missions.Defs["loopStage"].Enum, stringsOf(ValidLoopStages)},
		{"planStep status", missions.Defs["planStep"].Properties["status"].Enum, stringsOf(ValidPlanStepStatuses)},
		{"inbox kind", missions.Defs["inboxMessage"].Properties["kind"].Enum, stringsOf(ValidInboxKinds)},
		{"decisionState", missions.Defs["decisionState"].Enum, stringsOf(ValidDecisionStates)},
		{"replyState", missions.Defs["replyState"].Enum, stringsOf(ValidReplyStates)},
		{"replyResult message state", missions.Defs["replyResult"].Properties["message"].Properties["state"].Enum, []string{string(ReplyStateReplied)}},
		{"replyStatus reason", missions.Defs["replyStatus"].Properties["reason"].Enum, ValidReplyRefusals},
	}
	for _, tc := range cases {
		if !slices.Equal(tc.enum, tc.want) {
			t.Errorf("%s enum %v, Go vocabulary %v", tc.name, tc.enum, tc.want)
		}
		if !slices.IsSorted(tc.want) {
			t.Errorf("%s vocabulary %v is not in canonical order", tc.name, tc.want)
		}
	}
}

// TestDrift_MissionsPatternsMatchTheValidator holds every pattern the missions
// schema publishes on the rule the Go validator enforces, and every member
// that names an option or a plan step on the shared definition, so a client
// that validates against the schema accepts exactly what the orchestrator
// accepts.
func TestDrift_MissionsPatternsMatchTheValidator(t *testing.T) {
	missions := readSchemaNode(t, ContractSchemaFile(ContractMissions, 1))
	defs := missions.Defs
	for _, tc := range []struct {
		name      string
		schema    string
		validator *regexp.Regexp
	}{
		{"id", defs["id"].Pattern, missionIDPattern},
		{"optionId", defs["optionId"].Pattern, optionIDPattern},
		{"session.engine", defs["session"].Properties["engine"].Pattern, enginePattern},
		{"session.id", defs["session"].Properties["id"].Pattern, sessionIDPattern},
		{"syncInput.event", defs["syncInput"].Properties["event"].Pattern, eventPattern},
	} {
		if tc.schema != tc.validator.String() {
			t.Errorf("%s pattern is %q in the schema, %q in Go", tc.name, tc.schema, tc.validator)
		}
	}
	for _, tc := range []struct {
		name string
		node *schemaNode
		ref  string
	}{
		{"decisionOption.id", defs["decisionOption"].Properties["id"], "#/$defs/optionId"},
		{"recommendation.option", defs["askInput"].Properties["recommendation"].Properties["option"], "#/$defs/optionId"},
		{"inboxDecision.option", defs["inboxDecision"].Properties["option"], "#/$defs/optionId"},
		{"askResult decision.option", defs["askResult"].Properties["decision"].Properties["option"], "#/$defs/optionId"},
		{"planStep.key", defs["planStep"].Properties["key"], CommonSchemaFile + "#/$defs/token"},
		{"missionName.repository", defs["missionName"].Properties["repository"], CommonSchemaFile + "#/$defs/token"},
	} {
		if tc.node == nil || tc.node.Ref != tc.ref {
			t.Errorf("%s does not reference %s", tc.name, tc.ref)
		}
	}
	repository := regexp.MustCompile(defs["missionName"].Properties["repository"].Pattern)
	for _, value := range []string{"putnami/putnami", "acme/app-2", "Putnami/putnami", "acme/App", "a b", "é/ü"} {
		input := &MissionsSyncInput{
			Session: SessionRef{Engine: "host-a", ID: "s-1"}, Mission: MissionName{Repository: value, Key: "k"},
			Event: "stop", State: SessionStateWorking, ObservedAt: "2026-10-26T09:00:00Z",
		}
		if schema, validator := repository.MatchString(value), ValidateInput(input) == nil; schema != validator {
			t.Errorf("repository %q: the schema accepts it %v, the validator %v", value, schema, validator)
		}
	}
}

// TestDrift_MissionsBoundsMatchTheValidator holds every size and range the
// missions schema publishes on the bound the Go validator enforces, and checks
// the validator at the result's decisions bound: a sync names at most
// MaxSyncDecisions decisions, so its result reports at most as many.
func TestDrift_MissionsBoundsMatchTheValidator(t *testing.T) {
	defs := readSchemaNode(t, ContractSchemaFile(ContractMissions, 1)).Defs
	input, result := defs["syncInput"].Properties, defs["syncResult"].Properties
	for _, tc := range []struct {
		name string
		got  *int
		want int
	}{
		{"syncResult.minIntervalMs minimum", result["minIntervalMs"].Minimum, MinSyncIntervalMs},
		{"syncResult.minIntervalMs maximum", result["minIntervalMs"].Maximum, MaxSyncIntervalMs},
		{"syncResult.inbox maxItems", result["inbox"].MaxItems, MaxInboxMessages},
		{"syncResult.decisions maxItems", result["decisions"].MaxItems, MaxSyncDecisions},
		{"syncResult.replies maxItems", result["replies"].MaxItems, MaxSyncReplies},
		{"missionView.openDecisions maxItems", defs["missionView"].Properties["openDecisions"].MaxItems, MaxOpenDecisions},
		{"statusResult.sessions maxItems", defs["statusResult"].Properties["sessions"].MaxItems, MaxListMembers},
		{"missionSession.branch maxLength", defs["missionSession"].Properties["branch"].MaxLength, MaxBranchLength},
		{"missionSession.error maxLength", defs["missionSession"].Properties["error"].MaxLength, MaxErrorLength},
		{"plan.steps maxItems", defs["plan"].Properties["steps"].MaxItems, MaxListMembers},
		{"syncInput.delivered maxItems", input["delivered"].MaxItems, MaxListMembers},
		{"syncInput.decisions maxItems", input["decisions"].MaxItems, MaxSyncDecisions},
		{"syncInput.replies maxItems", input["replies"].MaxItems, MaxSyncReplies},
	} {
		if tc.got == nil || *tc.got != tc.want {
			t.Errorf("%s is %v in the schema, %d in Go", tc.name, tc.got, tc.want)
		}
	}
	if ref := input["usage"]; ref == nil || ref.Ref != "#/$defs/sessionUsage" {
		t.Errorf("syncInput.usage does not reference #/$defs/sessionUsage")
	}
	if ref := input["stage"]; ref == nil || ref.Ref != "#/$defs/loopStage" {
		t.Errorf("syncInput.stage does not reference #/$defs/loopStage")
	}
	session := defs["missionSession"].Properties
	if ref := session["usage"]; ref == nil || ref.Ref != "#/$defs/sessionUsage" {
		t.Errorf("missionSession.usage does not reference #/$defs/sessionUsage")
	}
	if ref := session["stage"]; ref == nil || ref.Ref != "#/$defs/loopStage" {
		t.Errorf("missionSession.stage does not reference #/$defs/loopStage")
	}
	for name, node := range map[string]*schemaNode{"syncInput": input["waitUntil"], "missionSession": session["waitUntil"]} {
		if node == nil || node.Ref != "collaboration.json#/$defs/timestamp" {
			t.Errorf("%s.waitUntil does not reference the shared timestamp", name)
		}
	}
	usage := defs["sessionUsage"].Properties
	for _, field := range []string{"inputTokens", "outputTokens", "cacheWriteTokens", "cacheReadTokens"} {
		if node := usage[field]; node == nil || node.Minimum == nil || *node.Minimum != 0 || node.Maximum == nil || *node.Maximum != MaxUsageTokens {
			t.Errorf("sessionUsage.%s is not bounded 0 to %d in the schema", field, int64(MaxUsageTokens))
		}
	}
	for _, tc := range []struct {
		count int64
		valid bool
	}{{-1, false}, {0, true}, {MaxUsageTokens, true}, {MaxUsageTokens + 1, false}} {
		in := &MissionsSyncInput{
			Session: SessionRef{Engine: "host-a", ID: "s-1"}, Mission: MissionName{Repository: "putnami/putnami", Key: "k"},
			Event: "stop", State: SessionStateWorking, ObservedAt: "2026-10-26T09:00:00Z",
			Usage: &SessionUsage{InputTokens: tc.count, OutputTokens: tc.count, CacheWriteTokens: tc.count, CacheReadTokens: tc.count},
		}
		if diags := ValidateInput(in); (diags == nil) != tc.valid {
			t.Errorf("usage of %d tokens: valid %v, want %v (%v)", tc.count, diags == nil, tc.valid, diags)
		}
	}

	syncResult := func(decisions int) *MissionsSyncResult {
		r := &MissionsSyncResult{
			Session: SessionStatus{State: SessionStateWorking},
			Mission: MissionView{Key: "k", Plan: MissionPlan{Steps: []PlanStep{}}, OpenDecisions: []OpenDecision{}},
			Inbox:   []InboxMessage{}, Decisions: []DecisionStatus{}, Replies: []ReplyStatus{},
			MinIntervalMs: MinSyncIntervalMs, Now: "2026-10-26T09:00:00Z",
		}
		for i := range decisions {
			r.Decisions = append(r.Decisions, DecisionStatus{ID: fmt.Sprintf("D-%d", i+1), State: DecisionStateOpen})
		}
		return r
	}
	if diags := ValidateResult(syncResult(MaxSyncDecisions)); diags != nil {
		t.Errorf("a result with %d decisions is refused: %v", MaxSyncDecisions, diags)
	}
	if diags := ValidateResult(syncResult(MaxSyncDecisions + 1)); diags == nil {
		t.Errorf("a result with %d decisions is accepted; a sync names at most %d", MaxSyncDecisions+1, MaxSyncDecisions)
	}
	for _, tc := range []struct {
		interval int
		valid    bool
	}{{MinSyncIntervalMs - 1, false}, {MinSyncIntervalMs, true}, {MaxSyncIntervalMs, true}, {MaxSyncIntervalMs + 1, false}} {
		r := syncResult(0)
		r.MinIntervalMs = tc.interval
		if diags := ValidateResult(r); (diags == nil) != tc.valid {
			t.Errorf("minIntervalMs %d: valid %v, want %v (%v)", tc.interval, diags == nil, tc.valid, diags)
		}
	}
}

// TestDrift_PageBoundsMatchTheValidator holds the published page bounds on the
// ones the Go validator enforces, so a client that validates a request against
// the schema accepts exactly what the orchestrator accepts.
func TestDrift_PageBoundsMatchTheValidator(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "bounded-documents", "page-bounds-match-the-schema")
	if MaxPageSize != 100 {
		t.Errorf("MaxPageSize is %d, want 100", MaxPageSize)
	}
	common := readSchemaNode(t, CommonSchemaFile)
	size := common.Defs["pageRequest"].Properties["size"]
	bound := func(name string, got *int, want int) {
		t.Helper()
		if got == nil || *got != want {
			t.Errorf("%s is %v in the schema, %d in Go", name, got, want)
		}
	}
	bound("pageRequest.size minimum", size.Minimum, 0)
	bound("pageRequest.size maximum", size.Maximum, MaxPageSize)
	bound("pageRequest.cursor maxLength", common.Defs["pageRequest"].Properties["cursor"].MaxLength, MaxCursorLength)
	bound("page.next maxLength", common.Defs["page"].Properties["next"].MaxLength, MaxCursorLength)
	for _, tc := range []struct {
		size  int
		valid bool
	}{{-1, false}, {0, true}, {1, true}, {MaxPageSize, true}, {MaxPageSize + 1, false}} {
		document := fmt.Sprintf(`{"page":{"size":%d}}`, tc.size)
		if _, diags := ParseRequest(ContractTasks, 1, OperationFind, []byte(document)); (diags == nil) != tc.valid {
			t.Errorf("page size %d: valid %v, want %v (%v)", tc.size, diags == nil, tc.valid, diags)
		}
	}
}

func stringsOf[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

// TestDrift_SelectionMarshalsLikeTheJobSelection is the anti-drift pin on the
// other wire that carries a resolved selection.
//
// A provider call has no job context, so this contract declares its own
// Selection. Three types describe one resolved answer: the CLI's
// ResolvedSelection, protocols/job's Selection and this one. What holds them
// together is the bytes. A member added to one and forgotten on another would
// let the same narrowing read differently depending on which surface answered,
// which is the one failure a shared resolver exists to prevent. The comparison
// is over the marshaled document (member names, member order and omitempty
// behavior), because that is the whole contract.
func TestDrift_SelectionMarshalsLikeTheJobSelection(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "schemas-track-the-go-types", "entity-schemas-match-the-go-types")
	for _, sample := range []struct {
		name      string
		job       job.Selection
		selection Selection
	}{
		{
			name:      "the unscoped whole-workspace projection",
			job:       job.Selection{Mode: job.SelectionModeAll, ProjectIDs: []string{"/a", "/b"}},
			selection: Selection{Mode: SelectionModeAll, ProjectIDs: []string{"/a", "/b"}},
		},
		{
			name:      "an explicit selector",
			job:       job.Selection{Mode: job.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/a"}},
			selection: Selection{Mode: SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/a"}},
		},
		{
			name: "impacted, with the baseline it resolved to",
			job: job.Selection{
				Mode: job.SelectionModeImpacted, Scoped: true,
				Baseline: "origin/main", BaselineSource: "trunk",
				ProjectIDs: []string{}, EmptyImpact: true,
			},
			selection: Selection{
				Mode: SelectionModeImpacted, Scoped: true,
				Baseline: "origin/main", BaselineSource: "trunk",
				ProjectIDs: []string{}, EmptyImpact: true,
			},
		},
	} {
		t.Run(sample.name, func(t *testing.T) {
			jobBytes, err := json.Marshal(sample.job)
			if err != nil {
				t.Fatalf("marshal job selection: %v", err)
			}
			selectionBytes, err := json.Marshal(sample.selection)
			if err != nil {
				t.Fatalf("marshal provider selection: %v", err)
			}
			if string(jobBytes) != string(selectionBytes) {
				t.Errorf("the two selection contracts no longer marshal identically:\n  job:      %s\n  provider: %s",
					jobBytes, selectionBytes)
			}
		})
	}

	// The vocabularies are the same closed set, in the same canonical order, so
	// a consumer of either reads one mode table.
	if !slices.Equal(SelectionModes, job.SelectionModes) {
		t.Errorf("selection modes = %v, job selection modes = %v", SelectionModes, job.SelectionModes)
	}
}
