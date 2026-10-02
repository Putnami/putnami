package registry

import "testing"

// TestConformance_ProtocolVersion pins the current protocol version so any bump
// is intentional and surfaces in code review.
func TestConformance_ProtocolVersion(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — bumping requires a migration story", ProtocolVersion)
	}
}

// TestConformance_Seam pins the credential-seam invocation. The framework shells
// exactly `putnami cloud registry-token --host <host>`; the cloud repo must
// implement that subcommand. Changing any token here is a cross-repo break.
func TestConformance_Seam(t *testing.T) {
	if CLIExecutableEnv != "PUTNAMI_CLI_EXECUTABLE" {
		t.Errorf("CLIExecutableEnv = %q, want PUTNAMI_CLI_EXECUTABLE", CLIExecutableEnv)
	}
	if SeamParentCommand != "cloud" {
		t.Errorf("SeamParentCommand = %q, want cloud", SeamParentCommand)
	}
	if SeamSubcommand != "registry-token" {
		t.Errorf("SeamSubcommand = %q, want registry-token", SeamSubcommand)
	}
	if SeamHostFlag != "host" {
		t.Errorf("SeamHostFlag = %q, want host", SeamHostFlag)
	}
	if SeamMaterializeFlag != "materialize" {
		t.Errorf("SeamMaterializeFlag = %q, want materialize", SeamMaterializeFlag)
	}
}

// TestConformance_PublishProviderMarker pins the feature-detection command name
// shared with @putnami/cloud.
func TestConformance_PublishProviderMarker(t *testing.T) {
	if PublishProviderCommandName != "publish-provider" {
		t.Errorf("PublishProviderCommandName = %q, want publish-provider", PublishProviderCommandName)
	}
}

// TestConformance_ValidBearer pins the bare-bearer-on-stdout contract: a token is
// valid only when non-empty and free of internal whitespace.
func TestConformance_ValidBearer(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"plain token", "abc123.def456", true},
		{"jwt-ish", "eyJhbG.eyJzdW. sig", false},
		{"empty", "", false},
		{"leading space", " abc", false},
		{"trailing newline", "abc\n", false},
		{"internal tab", "ab\tcd", false},
		{"status line", "resolved token for host", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ValidBearer(c.in); got != c.want {
				t.Errorf("ValidBearer(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestConformance_CredentialProvider pins the purpose-keyed seam: the reserved
// command, the RPC version, the capability, the purposes and the ops. Every
// provider and engine depends on these tokens.
func TestConformance_CredentialProvider(t *testing.T) {
	if CredentialProviderCommand != "credential-provider" {
		t.Errorf("CredentialProviderCommand = %q, want credential-provider", CredentialProviderCommand)
	}
	if CredentialProviderCommand == PublishProviderCommandName {
		t.Error("the credential provider must not reuse the reserved publish-provider name")
	}
	if CredentialProtocolVersion != 1 {
		t.Errorf("CredentialProtocolVersion = %d, want 1", CredentialProtocolVersion)
	}
	if CapabilityCredentialV1 != "credential-v1" {
		t.Errorf("CapabilityCredentialV1 = %q, want credential-v1", CapabilityCredentialV1)
	}
	if PurposeRead != "read" || PurposePublish != "publish" {
		t.Errorf("purposes = %q, %q; want read, publish", PurposeRead, PurposePublish)
	}
	if CredentialOpInitialize != "initialize" || CredentialOpCredential != "credential" || CredentialOpShutdown != "shutdown" {
		t.Error("credential-provider op names changed")
	}
	if !ValidPurpose(PurposeRead) || !ValidPurpose(PurposePublish) || ValidPurpose("deploy") || ValidPurpose("") {
		t.Error("ValidPurpose drifted from the two purposes")
	}
}

// TestConformance_PublicationProvider pins the publication-v1 tokens and
// bounds: the capability, the ops, the refusal codes, the plan version and
// the line bounds. The engine and every provider that echoes publication-v1
// depend on them.
func TestConformance_PublicationProvider(t *testing.T) {
	if CapabilityPublicationV1 != "publication-v1" {
		t.Errorf("CapabilityPublicationV1 = %q, want publication-v1", CapabilityPublicationV1)
	}
	if CredentialOpResolve != "resolve" || CredentialOpOpen != "open" || CredentialOpRelease != "release" {
		t.Error("publication-v1 op names changed")
	}
	for _, op := range []CredentialOp{CredentialOpResolve, CredentialOpOpen, CredentialOpRelease} {
		if op.Capability() != CapabilityPublicationV1 || op.MaxLineBytes() != 8<<20 {
			t.Errorf("%s: capability %q, line bound %d; want publication-v1, 8 MiB", op, op.Capability(), op.MaxLineBytes())
		}
	}
	for _, op := range []CredentialOp{CredentialOpInitialize, CredentialOpCredential, CredentialOpShutdown} {
		if op.Capability() != CapabilityCredentialV1 || op.MaxLineBytes() != 64<<10 {
			t.Errorf("%s: capability %q, line bound %d; want credential-v1, 64 KiB", op, op.Capability(), op.MaxLineBytes())
		}
	}
	if unknown := CredentialOp("token"); unknown.Valid() || unknown.Capability() != "" || unknown.MaxLineBytes() != 0 || unknown.Allowed([]string{CapabilityCredentialV1, CapabilityPublicationV1}) {
		t.Error("an unknown op is valid, defined by a capability, bounded or allowed")
	}
	if maxCredentialDepth != 5 || maxPublicationDepth != 12 {
		t.Errorf("nesting bounds = %d, %d; want 5, 12", maxCredentialDepth, maxPublicationDepth)
	}
	want := []string{"plan_not_open", "plan_already_open", "plan_mismatch", "artifact_missing", "artifact_digest_mismatch", "not_forward", "conflict", "channel_immutable", "namespace_forbidden"}
	if len(PublicationRefusalCodes) != len(want) {
		t.Fatalf("PublicationRefusalCodes = %v, want %v", PublicationRefusalCodes, want)
	}
	for index, code := range want {
		if PublicationRefusalCodes[index] != code {
			t.Errorf("PublicationRefusalCodes[%d] = %q, want %q", index, PublicationRefusalCodes[index], code)
		}
		if err := ValidateRefusal(CredentialRefusal{Code: code}); err != nil {
			t.Errorf("refusal code %q: %v", code, err)
		}
	}
	if PublicationPlanProtocolVersion != 1 || MaxPublicationPlanBytes != 1<<20 || MaxAncestrySnapshotCommits != 1_000_000 {
		t.Error("plan version, plan bound or ancestry snapshot bound changed")
	}
}
