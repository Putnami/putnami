package collaboration

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// TestReadProviderOffer_DeclaredLastPrompt: a missions provider offers
// lastPrompt on sync, and the member on status withdraws the contract
// version, because status does not declare it.
func TestReadProviderOffer_DeclaredLastPrompt(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "declared-last-prompt", "only-missions-sync-declares-last-prompt")
	declare := func(operation string, members []string) map[string]extension.ProviderDeclaration {
		missions := fullProvider(ContractMissions)
		missions.Operations[OperationStatus] = providerOperation(ContractMissions, OperationStatus)
		declared := missions.Operations[operation]
		declared.Members = members
		missions.Operations[operation] = declared
		return map[string]extension.ProviderDeclaration{ContractMissions: missions}
	}
	offer, diags := ReadProviderOffer(declare(OperationSync, []string{MissionMemberLastPrompt}))
	if diag.HasErrors(diags) {
		t.Fatalf("declared lastPrompt was refused: %v", diags)
	}
	ops, _ := offer.Operations(ContractMissions, 1)
	if got := ops[OperationSync].Members(); !slices.Equal(got, []string{MissionMemberLastPrompt}) {
		t.Errorf("sync offers members %v, want [lastPrompt]", got)
	}
	if slices.Contains(MissionStatusMembers, MissionMemberLastPrompt) {
		t.Fatal("missions.status declares lastPrompt")
	}
	misplaced, _ := ReadProviderOffer(declare(OperationStatus, []string{MissionMemberLastPrompt}))
	if len(misplaced.Versions(ContractMissions)) != 0 {
		t.Fatal("lastPrompt declared on missions.status is still offered")
	}
}

// TestLastPrompt_Bounds: a sync accepts no lastPrompt and a request of at
// most MaxLastPromptLength characters, on several lines and in any script;
// it refuses a longer one and a NUL character.
func TestLastPrompt_Bounds(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "declared-last-prompt", "a-last-prompt-is-bounded-text")
	cases := []struct {
		name  string
		value string
		codes []string
	}{
		{"no request", "", nil},
		{"a request on two lines", "Fix the build.\nThen open the PR.", nil},
		{"the longest request", strings.Repeat("é", MaxLastPromptLength), nil},
		{"a longer request", strings.Repeat("a", MaxLastPromptLength+1), []string{ErrorCodeTooLarge}},
		{"a request with a NUL", "a\x00b", []string{ErrorCodeInvalidValue}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := syncInput()
			in.LastPrompt = tc.value
			if got := distinctCodes(ValidateInput(in)); !slices.Equal(got, tc.codes) {
				t.Fatalf("sync codes = %v, want %v", got, tc.codes)
			}
		})
	}
	in := syncInput()
	in.LastPrompt = "Fix the build."
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	parsed, diags := ParseRequest(ContractMissions, 1, OperationSync, encoded)
	if diags != nil {
		t.Fatalf("a lastPrompt was refused: %v", diags)
	}
	if got := parsed.(*MissionsSyncInput).LastPrompt; got != "Fix the build." {
		t.Fatalf("lastPrompt read back as %q", got)
	}
	in.LastPrompt = ""
	if encoded, _ = json.Marshal(in); strings.Contains(string(encoded), "lastPrompt") {
		t.Fatalf("an absent lastPrompt was encoded: %s", encoded)
	}
}
