package agentartifact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// Both packagers keep the user-scope skills, stamp contract 20, and refuse a
// name the built content does not ship as a skill.
func TestPackagersKeepAndCheckUserScopeSkills(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "user-scope-skills-are-named-by-the-author", "the-package-step-refuses-an-unshipped-user-scope-skill")
	const declared = `{"name":"` + contentExtensionName + `","agentContent":{"path":"agent-content","source":"agent-src","userScope":["acme-review"]}}`

	pkg, err := PackageExtension(newContentExtension(t, declared), "1.0.0")
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	loaded, err := extproto.NegotiateManifest(extproto.ManifestFilename, pkg.Manifest)
	if err != nil {
		t.Fatalf("the packaged manifest must load: %v", err)
	}
	if got, want := loaded.AgentContent.UserScope, []string{"acme-review"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("userScope = %v, want %v", got, want)
	}
	if loaded.CLIContract != protocolcli.UserScopeSkillsContract {
		t.Fatalf("cliContract = %d, want %d", loaded.CLIContract, protocolcli.UserScopeSkillsContract)
	}

	root := newContentExtension(t, declared)
	stageDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stageDir, extproto.ManifestFilename), []byte(declared), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := StageExtensionContent(root, stageDir, contentExtensionName, "1.0.0"); err != nil {
		t.Fatalf("stage: %v", err)
	}
	staged, err := os.ReadFile(filepath.Join(stageDir, extproto.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		AgentContent extproto.AgentContentContribution `json:"agentContent"`
	}
	if err := json.Unmarshal(staged, &raw); err != nil {
		t.Fatal(err)
	}
	if got := raw.AgentContent.UserScope; !reflect.DeepEqual(got, []string{"acme-review"}) {
		t.Fatalf("staged userScope = %v", got)
	}

	unshipped := `{"name":"` + contentExtensionName + `","agentContent":{"path":"agent-content","source":"agent-src","userScope":["acme-review","acme-reviewer"]}}`
	if _, err := PackageExtension(newContentExtension(t, unshipped), "1.0.0"); err == nil || !strings.Contains(err.Error(), "does not ship (acme-reviewer)") {
		t.Fatalf("a worker profile named as a user-scope skill must be refused: %v", err)
	}
	stageDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(stageDir, extproto.ManifestFilename), []byte(unshipped), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := StageExtensionContent(newContentExtension(t, unshipped), stageDir, contentExtensionName, "1.0.0"); err == nil || !strings.Contains(err.Error(), "does not ship (acme-reviewer)") {
		t.Fatalf("the staging packager must refuse it too: %v", err)
	}
}
