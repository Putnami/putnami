package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// userScopeManifest is a packaged contribution that names user-scope skills,
// stamped with the contract that vocabulary requires. It lives outside
// fixtures/valid, whose manifests every additive-rung test reads as using no
// rung above agent content.
const userScopeManifest = "testdata/user-scope.extension.json"

// The user-scope list names skills once: the invalid fixture repeats one and
// spells another as prose, and each defect is reported at its entry.
func TestUserScopeSkills_NameSkillsOnce(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "user-scope-skills-are-named-by-the-author", "a-user-scope-list-names-skills-once")
	m := loadFixtureManifest(t, userScopeManifest)
	if got, want := m.AgentContent.UserScope, []string{"spec", "context"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("userScope = %v, want %v", got, want)
	}
	if diags := FullValidateManifest(m); len(diagCodes(diags)) != 0 {
		t.Fatalf("the valid user-scope fixture reports %v", diags)
	}
	invalid := loadFixtureManifest(t, filepath.Join("fixtures/invalid", "agent-content-user-scope.json"))
	fields := []string{}
	for _, d := range ValidateAgentContent(invalid) {
		fields = append(fields, d.Field)
	}
	if want := []string{"agentContent.userScope.1", "agentContent.userScope.2"}; !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields = %v, want %v", fields, want)
	}
	// The authored form carries the list too.
	authored := loadFixtureManifest(t, "fixtures/valid/agent-content-authored.json")
	authored.AgentContent.UserScope = []string{"spec"}
	if diags := ValidateAgentContent(authored); len(diagCodes(diags)) != 0 {
		t.Fatalf("an authored contribution may name user-scope skills: %v", diags)
	}
}

// A non-empty list raises the floor to contract 20, an empty one does not,
// and a manifest stamped below 20 is refused with the reason.
func TestUserScopeSkills_RequireContract20(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "user-scope-skills-are-named-by-the-author", "a-user-scope-list-requires-contract-20")
	path := userScopeManifest
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := RequiredCLIContract(m); got != protocolcli.UserScopeSkillsContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.UserScopeSkillsContract)
	}
	if protocolcli.LatestContract < protocolcli.UserScopeSkillsContract {
		t.Fatalf("LatestContract %d does not read contract %d", protocolcli.LatestContract, protocolcli.UserScopeSkillsContract)
	}
	m.AgentContent.UserScope = nil
	if got := RequiredCLIContract(m); got != protocolcli.AgentContentContract {
		t.Fatalf("without user-scope skills RequiredCLIContract = %d, want %d", got, protocolcli.AgentContentContract)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["cliContract"] = protocolcli.ProviderContract
	below, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := NegotiateManifest(path, below); err == nil || m != nil {
		t.Fatal("a manifest with user-scope skills stamped 19 loaded")
	} else if !strings.Contains(err.Error(), "requires 20 for its user-scope skills") {
		t.Fatalf("the refusal must name the user-scope requirement: %v", err)
	}
}
