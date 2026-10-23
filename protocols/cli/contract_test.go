package cli

import "testing"

// TestCurrentContract_Pinned keeps the contract version intentional: bumping it
// requires updating this test, the changelog block on the constant, and — since
// contract 3 replaced adaptation with rejection — shipping the migration that
// moves an extension onto the new contract.
func TestCurrentContract_Pinned(t *testing.T) {
	if CurrentContract != 4 {
		t.Fatalf("CurrentContract = %d, want 4 — a bump must ship the migration for contract N-1 and update the changelog on the constant", CurrentContract)
	}
}

// TestCurrentContract_OnlyMovesForward pins the floor: contract 0 is reserved
// for manifests without a cliContract field (the pre-registry world), so the
// constant can never regress to it.
func TestCurrentContract_OnlyMovesForward(t *testing.T) {
	if CurrentContract < 1 {
		t.Fatalf("CurrentContract = %d; the contract version only moves forward (>= 1)", CurrentContract)
	}
}

// The additive rungs leave the base stamp unchanged; a reader before each
// rung rejects only manifests that declare that rung's vocabulary.
func TestAdditiveContractsAreOrdered(t *testing.T) {
	if AgentContentContract != 5 {
		t.Fatalf("AgentContentContract = %d, want 5", AgentContentContract)
	}
	if AgentContentContract != CurrentContract+1 {
		t.Fatalf("AgentContentContract = %d, want CurrentContract+1 (%d): a base-contract reader must see it as newer",
			AgentContentContract, CurrentContract+1)
	}
	// Rung 6 named the go-embed selectors the CLI expanded itself; rung 14
	// retires it, and its number stays taken.
	if ReleaseBaselineInputContract != AgentContentContract+2 ||
		ExtensionDependenciesContract != ReleaseBaselineInputContract+1 ||
		VerbVocabularyContract != ExtensionDependenciesContract+1 ||
		SourceBoundCacheContract != VerbVocabularyContract+1 ||
		CommandKindContract != SourceBoundCacheContract+1 ||
		PublishChannelsContract != CommandKindContract+1 ||
		ProvisionedToolchainContract != PublishChannelsContract+1 ||
		InputSelectorsContract != ProvisionedToolchainContract+1 || LatestContract != InputSelectorsContract {
		t.Fatalf("additive contract ladder = base %d, agent %d, release baseline %d, dependencies %d, verbs %d, source-bound cache %d, command kinds %d, publish channels %d, provisioned toolchain %d, input selectors %d, latest %d",
			CurrentContract, AgentContentContract, ReleaseBaselineInputContract,
			ExtensionDependenciesContract, VerbVocabularyContract, SourceBoundCacheContract, CommandKindContract,
			PublishChannelsContract, ProvisionedToolchainContract, InputSelectorsContract, LatestContract)
	}
	// Rung 7 names two meanings that one CLI release added together.
	if GitInputModeContract != ReleaseBaselineInputContract {
		t.Fatalf("GitInputModeContract = %d, want rung %d with the release baseline", GitInputModeContract, ReleaseBaselineInputContract)
	}
}
