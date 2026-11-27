package extension

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// A reader before rung 31 refuses the member and would key a selection-free
// task on the run's selection. A manifest with a task that declares
// selection none therefore needs rung 31, and a lower stamp is refused.
func TestASelectionFreeTaskRequiresItsContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "later-rungs-raise-the-contract-floor", "selection-free-tasks-require-contract-31")
	spectest.Proves(t, "extension/manifest-contract", "later-rungs-raise-the-contract-floor", "selection-free-tasks-below-contract-31-do-not-load")
	manifest := []byte(`{"name":"@test/sdd","cliContract":30,"commands":{"validate-workspace":{"activation":"workspace-once","run":[{"id":"features","task":"features"}]}},"tasks":{"features":{"kind":"command","command":"echo","selection":"none"}}}`)
	if _, err := NegotiateManifest("sdd.json", manifest); err == nil {
		t.Fatal("contract-30 stamp accepted a selection-free task")
	}
	m, err := NegotiateManifest("sdd.json", []byte(strings.Replace(string(manifest), `"cliContract":30`, `"cliContract":31`, 1)))
	if err != nil {
		t.Fatalf("contract-31 stamp refused: %v", err)
	}
	if !m.Tasks["features"].ReadsNoSelection() {
		t.Fatal("selection none did not parse")
	}
	if got := RequiredLoadContract(m); got != protocolcli.SelectionFreeTaskContract {
		t.Fatalf("RequiredLoadContract = %d, want %d: the rung binds every origin", got, protocolcli.SelectionFreeTaskContract)
	}
	// A task that leaves the member out keeps the floor its other vocabulary
	// puts.
	plain := &Manifest{Name: "@test/plain", Tasks: map[string]TaskDefinition{"check": {Kind: "command", Command: "echo"}}}
	if got := RequiredLoadContract(plain); got == protocolcli.SelectionFreeTaskContract {
		t.Fatalf("RequiredLoadContract = %d for a manifest that does not declare the member", got)
	}
}

// The member has one value, and a batchable task cannot declare it.
func TestTheSelectionMemberIsValidated(t *testing.T) {
	for name, want := range map[string][]string{
		"task-selection-unknown.json":   {"invalid-enum tasks.check-exec.selection"},
		"task-selection-batchable.json": {"invalid-value tasks.lint-exec.selection"},
	} {
		m := loadFixtureManifest(t, filepath.Join("fixtures/invalid", name))
		var got []string
		for _, d := range ValidateManifest(m) {
			if strings.HasSuffix(d.Field, ".selection") {
				got = append(got, d.Code+" "+d.Field)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: selection diagnostics = %v, want %v", name, got, want)
		}
	}
	valid, diags := ParseManifest([]byte(`{"name":"@test/extension","commands":{"check":{"activation":"workspace-once","run":[{"id":"check","task":"check-exec"}]}},"tasks":{"check-exec":{"kind":"command","command":"echo","selection":"none"}}}`))
	if len(diags) != 0 {
		t.Fatalf("parse: %v", diags)
	}
	if diags := FullValidateManifest(valid); len(diags) != 0 {
		t.Fatalf("a workspace-once task that reads no selection is refused: %v", diags)
	}
}
