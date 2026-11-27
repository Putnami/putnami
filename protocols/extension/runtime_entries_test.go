package extension

import (
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// runtimeEntriesFixture is the emptied-output fixture with a prepared runtime
// that declares a codegen entry and compiles cmd/** and internal/**, and its
// task running that entry, stamped at the entries rung.
func runtimeEntriesFixture(t *testing.T) *Manifest {
	t.Helper()
	m := loadFixtureManifest(t, "fixtures/valid/declared-output-emptied.json")
	m.CLIContract = protocolcli.RuntimeEntriesContract
	m.Runtime = &RuntimeDefinition{
		Executable: "compiled/putnami-sample",
		Entries:    map[string]RuntimeEntry{"codegen": {Executable: "compiled/putnami-sample-codegen"}},
		Prepare: &RuntimePrepare{
			Command: "{extensionRoot}/bin/prepare",
			Args:    []string{"--output", "{runtimeOutput}"},
			Inputs:  []string{"bin/prepare", "cmd/**", "config/**", "internal/**"},
		},
		CompiledInputs: []string{"cmd/**", "internal/**"},
	}
	task := m.Tasks["build-describe"]
	task.Command = "{extensionRuntime}"
	task.Args = []string{"build-describe"}
	task.RuntimeEntry = "codegen"
	m.Tasks["build-describe"] = task
	return m
}

func TestRuntimeEntries_AValidDeclarationValidates(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "lifecycles-validate-and-fail-in-closed-vocabularies", "the-runtime-section-validates")
	m := runtimeEntriesFixture(t)
	if diags := ValidateRuntime(m); len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got %v", diags)
	}
	if diags := FullValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("a manifest with runtime entries must validate: %v", diags)
	}
}

// Each rule reports its own code on its own field, so a reader keys on the
// code rather than on the message.
func TestRuntimeEntries_Rules(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "lifecycles-validate-and-fail-in-closed-vocabularies", "the-runtime-section-validates")
	cases := []struct {
		name  string
		edit  func(*Manifest)
		code  string
		field string
	}{
		{"entry name with an uppercase letter", func(m *Manifest) {
			m.Runtime.Entries = map[string]RuntimeEntry{"Codegen": {Executable: "compiled/x"}}
			task := m.Tasks["build-describe"]
			task.RuntimeEntry = ""
			m.Tasks["build-describe"] = task
		}, "invalid-value", "runtime.entries.Codegen"},
		{"entry without an executable", func(m *Manifest) {
			m.Runtime.Entries["codegen"] = RuntimeEntry{}
		}, "invalid-runtime-path", "runtime.entries.codegen.executable"},
		{"entry executable escaping the root", func(m *Manifest) {
			m.Runtime.Entries["codegen"] = RuntimeEntry{Executable: "../codegen"}
		}, "invalid-runtime-path", "runtime.entries.codegen.executable"},
		{"entry executable equal to the main executable", func(m *Manifest) {
			m.Runtime.Entries["codegen"] = RuntimeEntry{Executable: "compiled/putnami-sample"}
		}, "duplicate-value", "runtime.entries.codegen.executable"},
		{"task naming an undeclared entry", func(m *Manifest) {
			task := m.Tasks["build-describe"]
			task.RuntimeEntry = "lint"
			m.Tasks["build-describe"] = task
		}, "unknown-reference", "tasks.build-describe.runtimeEntry"},
		{"compiled input that is not a prepare input", func(m *Manifest) {
			m.Runtime.CompiledInputs = []string{"tools/**"}
		}, "invalid-value", "runtime.compiledInputs[0]"},
		{"compiled input declared twice", func(m *Manifest) {
			m.Runtime.CompiledInputs = []string{"cmd/**", "cmd/**"}
		}, "duplicate-value", "runtime.compiledInputs[1]"},
		{"compiled inputs without a prepare", func(m *Manifest) {
			m.Runtime.Prepare = nil
		}, "invalid-value", "runtime.compiledInputs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := runtimeEntriesFixture(t)
			tc.edit(m)
			diags := ValidateRuntime(m)
			for _, d := range diags {
				if d.Code == tc.code && d.Field == tc.field {
					return
				}
			}
			t.Fatalf("want %s on %s, got %v", tc.code, tc.field, diags)
		})
	}
}

// Two spellings of one runtime normalize to one declaration, so they digest
// to one runtime.
func TestRuntimeEntries_Normalize(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "lifecycles-validate-and-fail-in-closed-vocabularies", "the-runtime-section-validates")
	m := runtimeEntriesFixture(t)
	m.Runtime.Entries["codegen"] = RuntimeEntry{Executable: "./compiled//putnami-sample-codegen"}
	m.Runtime.CompiledInputs = []string{"internal/**", "./cmd/**"}
	NormalizeRuntime(m)
	if got := m.Runtime.Entries["codegen"].Executable; got != "compiled/putnami-sample-codegen" {
		t.Errorf("entry executable = %q", got)
	}
	if got := strings.Join(m.Runtime.CompiledInputs, ","); got != "cmd/**,internal/**" {
		t.Errorf("compiled inputs = %q", got)
	}
}

// Naming a runtime entry is task contract: an entry written under one entry
// never serves a task that runs another.
func TestRuntimeEntries_TheEntryIsTaskContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "task-digests-move-only-with-the-contract", "only-contract-members-move-the-digest")
	m := runtimeEntriesFixture(t)
	task := m.Tasks["build-describe"]
	withEntry := TaskContractDigest(task)
	task.RuntimeEntry = ""
	if TaskContractDigest(task) == withEntry {
		t.Fatal("removing the runtime entry kept the task contract digest")
	}
}

// Entries, compiled inputs and a task's runtime entry each require rung 29,
// and a manifest stamped there loads with them.
func TestRuntimeEntries_RequireTheirContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "later-rungs-raise-the-contract-floor", "runtime-entries-require-contract-29")
	edits := map[string]func(*Manifest){
		"entries and a task entry": func(*Manifest) {},
		"compiled inputs alone": func(m *Manifest) {
			m.Runtime.Entries = nil
			task := m.Tasks["build-describe"]
			task.RuntimeEntry = ""
			m.Tasks["build-describe"] = task
		},
		"entries alone": func(m *Manifest) {
			m.Runtime.CompiledInputs = nil
			task := m.Tasks["build-describe"]
			task.RuntimeEntry = ""
			m.Tasks["build-describe"] = task
		},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			m := runtimeEntriesFixture(t)
			edit(m)
			if !DeclaresRuntimeEntries(m) {
				t.Fatal("DeclaresRuntimeEntries = false")
			}
			if got := RequiredCLIContract(m); got != protocolcli.RuntimeEntriesContract {
				t.Errorf("RequiredCLIContract = %d, want %d", got, protocolcli.RuntimeEntriesContract)
			}
			if got := RequiredLoadContract(m); got != protocolcli.RuntimeEntriesContract {
				t.Errorf("RequiredLoadContract = %d, want %d", got, protocolcli.RuntimeEntriesContract)
			}
		})
	}
	loaded, err := NegotiateManifest("x.json", marshalManifest(t, runtimeEntriesFixture(t)))
	if err != nil {
		t.Fatalf("a manifest stamped 29 must load: %v", err)
	}
	if loaded.Tasks["build-describe"].RuntimeEntry != "codegen" || loaded.Runtime.Entries["codegen"].Executable == "" {
		t.Fatalf("loaded runtime = %+v, task = %+v", loaded.Runtime, loaded.Tasks["build-describe"])
	}

	plain := runtimeEntriesFixture(t)
	plain.Runtime.Entries, plain.Runtime.CompiledInputs = nil, nil
	task := plain.Tasks["build-describe"]
	task.RuntimeEntry = ""
	plain.Tasks["build-describe"] = task
	if DeclaresRuntimeEntries(plain) {
		t.Fatal("DeclaresRuntimeEntries = true for a runtime without entries")
	}
}

// Stamped below 29, a manifest with runtime entries is refused for them, with
// the remedy that moves it.
func TestRuntimeEntries_BelowTheirContractAreRefused(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "later-rungs-raise-the-contract-floor", "runtime-entries-below-contract-29-do-not-load")
	for _, stamp := range []int{protocolcli.CurrentContract, protocolcli.RuntimeEntriesContract - 1} {
		m := runtimeEntriesFixture(t)
		m.CLIContract = stamp
		_, err := NegotiateManifest("x.json", marshalManifest(t, m))
		if err == nil {
			t.Fatalf("stamp %d loaded a manifest with runtime entries", stamp)
		}
		for _, want := range []string{"requires 29 for its runtime entries", "putnami extensions update"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("stamp %d: error %q does not say %q", stamp, err, want)
			}
		}
	}
}

func TestDrift_RuntimeEntry(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "the-schema-and-the-go-types-agree", "every-wire-struct-matches-the-schema")
	assertFieldParity(t, "RuntimeEntry", schemaDefProperties(t, "runtimeEntry"), goTypeJSONFields(t, RuntimeEntry{}))
}
