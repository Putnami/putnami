package extension

import (
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// emptiedFileFixture is the emptied-output fixture with a manifest file only
// its task writes, declared emptied, stamped 27.
func emptiedFileFixture(t *testing.T) *Manifest {
	t.Helper()
	m := loadFixtureManifest(t, "fixtures/valid/declared-output-emptied.json")
	m.CLIContract = protocolcli.EmptiedFileOutputContract
	task := m.Tasks["build-describe"]
	task.Declares.Outputs["manifest"] = DeclaredOutput{
		Kind: OutputKindFile, Root: OutputRootProject, Path: ".gen/manifest.json",
		OptionalEmpty: true, Emptied: true, Description: "A manifest absent for a project that declares nothing.",
	}
	m.Tasks["build-describe"] = task
	return m
}

// An emptied file output requires rung 27 from every origin, and the stamped
// manifest validates and loads with the flag. An emptied directory keeps its
// rung.
func TestEmptiedFileOutput_RequiresItsContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "later-rungs-raise-the-contract-floor", "emptied-file-outputs-require-contract-27")
	m := emptiedFileFixture(t)
	if diags := FullValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("an emptied file output must validate: %v", diags)
	}
	if !DeclaresEmptiedFileOutput(m) {
		t.Fatal("DeclaresEmptiedFileOutput = false for an emptied file output")
	}
	if got := RequiredLoadContract(m); got != protocolcli.EmptiedFileOutputContract {
		t.Errorf("RequiredLoadContract = %d, want %d", got, protocolcli.EmptiedFileOutputContract)
	}
	if got := RequiredCLIContract(m); got != protocolcli.EmptiedFileOutputContract {
		t.Errorf("RequiredCLIContract = %d, want %d", got, protocolcli.EmptiedFileOutputContract)
	}
	loaded, err := NegotiateManifest("x.json", marshalManifest(t, m))
	if err != nil {
		t.Fatalf("a manifest stamped 27 must load: %v", err)
	}
	if output := loaded.Tasks["build-describe"].Declares.Outputs["manifest"]; !output.Emptied || output.Kind != OutputKindFile {
		t.Fatalf("loaded output = %+v", output)
	}

	// Emptied directories and a file without the flag keep their rung.
	directories := loadFixtureManifest(t, "fixtures/valid/declared-output-emptied.json")
	if DeclaresEmptiedFileOutput(directories) {
		t.Fatal("DeclaresEmptiedFileOutput = true for emptied directories")
	}
	unflagged := emptiedFileFixture(t)
	output := unflagged.Tasks["build-describe"].Declares.Outputs["manifest"]
	output.Emptied = false
	unflagged.Tasks["build-describe"].Declares.Outputs["manifest"] = output
	if DeclaresEmptiedFileOutput(unflagged) {
		t.Fatal("DeclaresEmptiedFileOutput = true for a file without the flag")
	}
}

// Stamped below 27, a manifest with an emptied file output is refused for it,
// with the remedy that moves it.
func TestEmptiedFileOutput_BelowItsContractIsRefused(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "later-rungs-raise-the-contract-floor", "emptied-file-outputs-below-contract-27-do-not-load")
	for _, stamp := range []int{protocolcli.CurrentContract, protocolcli.EmptiedFileOutputContract - 1} {
		m := emptiedFileFixture(t)
		m.CLIContract = stamp
		_, err := NegotiateManifest("x.json", marshalManifest(t, m))
		if err == nil {
			t.Fatalf("stamp %d loaded a manifest with an emptied file output", stamp)
		}
		for _, want := range []string{"requires 27 for an emptied file output", "putnami extensions update"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("stamp %d: error %q does not say %q", stamp, err, want)
			}
		}
	}
}
