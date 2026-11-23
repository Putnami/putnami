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

// TestReadProviderOffer_DeclaredSessionURL: a missions provider offers
// sessionUrl on sync, and the member on status withdraws the contract
// version, because status does not declare it.
func TestReadProviderOffer_DeclaredSessionURL(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "declared-session-url", "only-missions-sync-declares-session-url")
	declare := func(operation string, members []string) map[string]extension.ProviderDeclaration {
		missions := fullProvider(ContractMissions)
		missions.Operations[OperationStatus] = providerOperation(ContractMissions, OperationStatus)
		declared := missions.Operations[operation]
		declared.Members = members
		missions.Operations[operation] = declared
		return map[string]extension.ProviderDeclaration{ContractMissions: missions}
	}
	offer, diags := ReadProviderOffer(declare(OperationSync, MissionMembers))
	if diag.HasErrors(diags) {
		t.Fatalf("declared sessionUrl was refused: %v", diags)
	}
	ops, _ := offer.Operations(ContractMissions, 1)
	if got := ops[OperationSync].Members(); !slices.Equal(got, []string{MissionMemberMissedSyncs, MissionMemberSessionURL}) {
		t.Errorf("sync offers members %v, want [missedSyncs sessionUrl]", got)
	}
	misplaced, _ := ReadProviderOffer(declare(OperationStatus, []string{MissionMemberSessionURL}))
	if len(misplaced.Versions(ContractMissions)) != 0 {
		t.Fatal("sessionUrl declared on missions.status is still offered")
	}
}

// TestSessionURL_Bounds: a sync accepts no sessionUrl, an empty one that
// clears the address, and an absolute https address of at most
// MaxSessionURLLength characters; it refuses any other address.
func TestSessionURL_Bounds(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "declared-session-url", "a-session-url-is-an-https-address")
	longest := "https://claude.ai/code/" + strings.Repeat("a", MaxSessionURLLength-len("https://claude.ai/code/"))
	cases := []struct {
		name  string
		value *string
		codes []string
	}{
		{"no address", nil, nil},
		{"an empty address", ptr(""), nil},
		{"a cloud session", ptr("https://claude.ai/code/session_01AbC"), nil},
		{"the longest address", ptr(longest), nil},
		{"a longer address", ptr(longest + "a"), []string{ErrorCodeInvalidURL}},
		{"an http address", ptr("http://claude.ai/code/session_01"), []string{ErrorCodeInvalidURL}},
		{"an uppercase scheme", ptr("HTTPS://claude.ai/code/session_01"), []string{ErrorCodeInvalidURL}},
		{"a relative address", ptr("/code/session_01"), []string{ErrorCodeInvalidURL}},
		{"an address with no host", ptr("https:///code/session_01"), []string{ErrorCodeInvalidURL}},
		{"an address with credentials", ptr("https://user:secret@claude.ai/code/session_01"), []string{ErrorCodeInvalidURL}},
		{"an address with a space", ptr("https://claude.ai/code/session 01"), []string{ErrorCodeInvalidURL}},
		{"an address on two lines", ptr("https://claude.ai/code/session_01\nx"), []string{ErrorCodeInvalidURL}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := syncInput()
			in.SessionURL = tc.value
			if got := distinctCodes(ValidateInput(in)); !slices.Equal(got, tc.codes) {
				t.Fatalf("sync codes = %v, want %v", got, tc.codes)
			}
		})
	}
}

// TestSessionURL_EmptyIsNotAbsent: an empty sessionUrl survives a parse and
// an encoding, because it clears the address while an absent one keeps it.
func TestSessionURL_EmptyIsNotAbsent(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "declared-session-url", "a-session-url-is-an-https-address")
	in := syncInput()
	in.SessionURL = ptr("")
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"sessionUrl":""`) {
		t.Fatalf("an empty sessionUrl was not encoded: %s", encoded)
	}
	parsed, diags := ParseRequest(ContractMissions, 1, OperationSync, encoded)
	if diags != nil {
		t.Fatalf("an empty sessionUrl was refused: %v", diags)
	}
	sync := parsed.(*MissionsSyncInput)
	if sync.SessionURL == nil || *sync.SessionURL != "" {
		t.Fatalf("an empty sessionUrl read back as %v", sync.SessionURL)
	}
	in.SessionURL = nil
	if encoded, _ = json.Marshal(in); strings.Contains(string(encoded), "sessionUrl") {
		t.Fatalf("an absent sessionUrl was encoded: %s", encoded)
	}
}

func ptr(value string) *string { return &value }
