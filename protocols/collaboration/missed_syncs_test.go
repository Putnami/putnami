package collaboration

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// TestOnlyDeclaredOperationsListMembers: tasks.get lists the context members,
// missions.sync and missions.status list missedSyncs, every proposals
// operation that answers a proposal lists pageUrl, each in canonical order,
// and no other operation of any contract lists a member.
func TestOnlyDeclaredOperationsListMembers(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "declared-task-context", "only-tasks-get-declares-the-context-members")
	spectest.Proves(t, "collaboration/provider-contract", "declared-missed-syncs", "only-missions-sync-and-status-declare-missed-syncs")
	spectest.Proves(t, "collaboration/provider-contract", "proposal-page-url", "page-url-is-an-optional-display-link")
	if !slices.IsSorted(TaskContextMembers) || !slices.IsSorted(MissionMembers) || !slices.IsSorted(ProposalMembers) {
		t.Errorf("member vocabularies %v, %v and %v are not in canonical order", TaskContextMembers, MissionMembers, ProposalMembers)
	}
	answersAProposal := []string{OperationAutoMerge, OperationFind, OperationMerge, OperationStatus, OperationUpsert}
	for _, spec := range Catalog() {
		for _, op := range spec.Operations {
			var want []string
			switch {
			case spec.Name == ContractTasks && op.Name == OperationGet:
				want = TaskContextMembers
			case spec.Name == ContractMissions && (op.Name == OperationSync || op.Name == OperationStatus):
				want = MissionMembers
			case spec.Name == ContractProposals && slices.Contains(answersAProposal, op.Name):
				want = ProposalMembers
			}
			if !slices.Equal(op.Members, want) {
				t.Errorf("%s.%s lists members %v, want %v", spec.Name, op.Name, op.Members, want)
			}
		}
	}
}

// TestReadProviderOffer_DeclaredMissedSyncs: a missions provider offers
// missedSyncs on sync and status, and the member on another operation
// withdraws the contract version.
func TestReadProviderOffer_DeclaredMissedSyncs(t *testing.T) {
	declare := func(members map[string][]string) map[string]extension.ProviderDeclaration {
		missions := fullProvider(ContractMissions)
		missions.Operations[OperationStatus] = providerOperation(ContractMissions, OperationStatus)
		for operation, list := range members {
			declared := missions.Operations[operation]
			declared.Members = list
			missions.Operations[operation] = declared
		}
		return map[string]extension.ProviderDeclaration{ContractMissions: missions}
	}
	offer, diags := ReadProviderOffer(declare(map[string][]string{
		OperationSync: {MissionMemberMissedSyncs}, OperationStatus: {MissionMemberMissedSyncs},
	}))
	if diag.HasErrors(diags) {
		t.Fatalf("declared missedSyncs was refused: %v", diags)
	}
	ops, _ := offer.Operations(ContractMissions, 1)
	for _, operation := range []string{OperationSync, OperationStatus} {
		if got := ops[operation].Members(); !slices.Equal(got, []string{MissionMemberMissedSyncs}) {
			t.Errorf("%s offers members %v, want [missedSyncs]", operation, got)
		}
	}
	misplaced, _ := ReadProviderOffer(declare(map[string][]string{OperationAsk: {MissionMemberMissedSyncs}}))
	if len(misplaced.Versions(ContractMissions)) != 0 {
		t.Fatal("missedSyncs declared on missions.ask is still offered")
	}
}

// TestMissedSyncs_Bounds: a report counts 1 to MaxMissedSyncs syncs, names
// the last reason on one line within its bound, and the UTC time of the last
// one, in a sync and in a status session alike.
func TestMissedSyncs_Bounds(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "declared-missed-syncs", "missed-syncs-bounds-are-validated")
	valid := MissedSyncs{Count: 3, Reason: "auth.unavailable", LastAt: "2026-11-18T09:00:00Z"}
	cases := []struct {
		name  string
		edit  func(*MissedSyncs)
		codes []string
	}{
		{"a report", func(*MissedSyncs) {}, nil},
		{"the largest count", func(m *MissedSyncs) { m.Count = MaxMissedSyncs }, nil},
		{"no syncs", func(m *MissedSyncs) { m.Count = 0 }, []string{ErrorCodeInvalidValue}},
		{"a count above the bound", func(m *MissedSyncs) { m.Count = MaxMissedSyncs + 1 }, []string{ErrorCodeInvalidValue}},
		{"no reason", func(m *MissedSyncs) { m.Reason = " " }, []string{ErrorCodeRequired}},
		{"a reason on two lines", func(m *MissedSyncs) { m.Reason = "denied\nagain" }, []string{ErrorCodeInvalidValue}},
		{"a long reason", func(m *MissedSyncs) { m.Reason = fill(MaxMissedSyncReasonLength + 1) }, []string{ErrorCodeTooLarge}},
		{"a time with an offset", func(m *MissedSyncs) { m.LastAt = "2026-11-18T10:00:00+01:00" }, []string{ErrorCodeInvalidTimestamp}},
		{"no time", func(m *MissedSyncs) { m.LastAt = "" }, []string{ErrorCodeRequired}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			missed := valid
			tc.edit(&missed)
			in := syncInput()
			in.MissedSyncs = &missed
			if got := distinctCodes(ValidateInput(in)); !slices.Equal(got, tc.codes) {
				t.Fatalf("sync codes = %v, want %v", got, tc.codes)
			}
			status := &MissionsStatusResult{
				Mission:  MissionView{Key: "issue-596", Plan: MissionPlan{Steps: []PlanStep{}}, OpenDecisions: []OpenDecision{}},
				Sessions: []MissionSession{{Session: SessionRef{Engine: "host-a", ID: "s-1"}, State: SessionStateWorking, ObservedAt: "2026-11-18T09:01:00Z", MissedSyncs: &missed}},
			}
			if got := distinctCodes(ValidateResult(status)); !slices.Equal(got, tc.codes) {
				t.Fatalf("status codes = %v, want %v", got, tc.codes)
			}
		})
	}
}

// TestReadProviderOffer_PageURLAllOrNone: a proposals provider offers pageUrl
// when it declares it on every operation it offers that answers a proposal,
// and a declaration on some of them withdraws its proposals version.
func TestReadProviderOffer_PageURLAllOrNone(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "proposal-page-url", "a-partial-page-url-declaration-withdraws-the-version")
	declare := func(on ...string) map[string]extension.ProviderDeclaration {
		proposals := fullProvider(ContractProposals)
		for _, operation := range on {
			declared := proposals.Operations[operation]
			declared.Members = []string{ProposalMemberPageURL}
			proposals.Operations[operation] = declared
		}
		return map[string]extension.ProviderDeclaration{ContractProposals: proposals}
	}
	var answering []string
	full := fullProvider(ContractProposals)
	spec, _ := Lookup(ContractProposals, 1)
	for _, op := range spec.Operations {
		if _, offered := full.Operations[op.Name]; offered && slices.Contains(op.Members, ProposalMemberPageURL) {
			answering = append(answering, op.Name)
		}
	}
	if len(answering) < 2 {
		t.Fatalf("the full provider offers %v; the test needs two operations that answer a proposal", answering)
	}
	offer, diags := ReadProviderOffer(declare(answering...))
	if diag.HasErrors(diags) {
		t.Fatalf("pageUrl declared on %v was refused: %v", answering, diags)
	}
	ops, _ := offer.Operations(ContractProposals, 1)
	for _, operation := range answering {
		if got := ops[operation].Members(); !slices.Equal(got, ProposalMembers) {
			t.Errorf("%s offers members %v, want %v", operation, got, ProposalMembers)
		}
	}
	if none, diags := ReadProviderOffer(declare()); diag.HasErrors(diags) || len(none.Versions(ContractProposals)) != 1 {
		t.Fatalf("a provider without pageUrl lost its proposals version: %v", diags)
	}
	partial, diags := ReadProviderOffer(declare(answering[0]))
	if len(partial.Versions(ContractProposals)) != 0 || !strings.Contains(fmt.Sprint(diags), "on every operation that answers a proposal") {
		t.Fatalf("pageUrl declared on %s only is still offered: %v", answering[0], diags)
	}
}
