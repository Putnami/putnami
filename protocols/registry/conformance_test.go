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
