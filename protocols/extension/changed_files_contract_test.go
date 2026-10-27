package extension

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
)

// changedFilesFixture is a manifest stamped 16 whose `test-race` step gates on
// the changed Go files and binds them.
var changedFilesFixture = filepath.Join("testdata", "changed-files", ManifestFilename)

// readChangedFilesFixture parses the fixture without negotiating it, so a test
// can restamp it.
func readChangedFilesFixture(t *testing.T) *Manifest {
	t.Helper()
	data, err := os.ReadFile(changedFilesFixture)
	if err != nil {
		t.Fatal(err)
	}
	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("fixture fails strict validation: %v", diags)
	}
	return m
}

// A change gate and a context binding each require rung 16 from every origin,
// and the stamped fixture loads with both forms intact.
func TestChangedFilesRequireTheirContract(t *testing.T) {
	m := readChangedFilesFixture(t)
	if !DeclaresChangedFiles(m) {
		t.Fatal("DeclaresChangedFiles = false for a step that gates on and binds the changed files")
	}
	if got := RequiredLoadContract(m); got != protocolcli.ChangedFilesContract {
		t.Errorf("RequiredLoadContract = %d, want %d", got, protocolcli.ChangedFilesContract)
	}
	if got := RequiredCLIContract(m); got != protocolcli.ChangedFilesContract {
		t.Errorf("RequiredCLIContract = %d, want %d", got, protocolcli.ChangedFilesContract)
	}
	loaded, err := LoadManifest(changedFilesFixture)
	if err != nil {
		t.Fatalf("a manifest stamped 16 must load: %v", err)
	}
	step := loaded.Commands["test"].Run[1]
	if step.Activation == nil || len(step.Activation.ChangedFiles) != 2 {
		t.Fatalf("loaded activation = %+v, want two changed-file globs", step.Activation)
	}
	binding := step.With["changed-files"]
	if binding.From != InputBindingFromContext || binding.Path != ContextPathChangedFiles {
		t.Errorf("loaded binding = %+v, want from %q path %q", binding, InputBindingFromContext, ContextPathChangedFiles)
	}

	for name, mutate := range map[string]func(*Manifest){
		"gate only": func(m *Manifest) {
			step := m.Commands["test"].Run[1]
			step.With = nil
			m.Commands["test"].Run[1] = step
		},
		"binding only": func(m *Manifest) {
			step := m.Commands["test"].Run[1]
			step.Activation = nil
			m.Commands["test"].Run[1] = step
		},
	} {
		m := readChangedFilesFixture(t)
		mutate(m)
		if got := RequiredLoadContract(m); got != protocolcli.ChangedFilesContract {
			t.Errorf("%s: RequiredLoadContract = %d, want %d", name, got, protocolcli.ChangedFilesContract)
		}
	}
}

// Stamped below 16, the manifest is refused for the step that reads the
// changed files, with the remedy that moves it: an older reader would plan the
// step on every run and key it without the files.
func TestChangedFilesBelowTheirContractAreRefused(t *testing.T) {
	for _, stamp := range []int{protocolcli.CurrentContract, protocolcli.ChangedFilesContract - 1} {
		m := readChangedFilesFixture(t)
		m.CLIContract = stamp
		_, err := NegotiateManifest("x.json", marshalManifest(t, m))
		if err == nil {
			t.Fatalf("stamp %d loaded a manifest whose step reads the changed files", stamp)
		}
		for _, want := range []string{"requires 16 for a step that reads the run's changed files", "putnami extensions update"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("stamp %d: error %q does not say %q", stamp, err, want)
			}
		}
	}
}

// A gate without globs and a literal or step binding declare nothing: the
// manifest keeps the base stamp.
func TestNoChangedFilesFormKeepsTheStamp(t *testing.T) {
	m := &Manifest{
		Name: "@test/x",
		Commands: map[string]CommandDefinition{"test": {Run: []PipelineStep{
			{ID: "a", Task: "t", Activation: &StepActivation{Files: []string{"go.mod"}}},
			{ID: "b", Task: "t", DependsOn: []string{"a"}, With: map[string]InputBinding{
				"mode": {Value: "test", HasValue: true},
				"tree": {FromStep: "a", Output: "tree"},
			}},
		}}},
		Tasks: map[string]TaskDefinition{"t": {Kind: "command", Command: "echo"}},
	}
	if DeclaresChangedFiles(m) {
		t.Fatal("DeclaresChangedFiles = true for a manifest without either form")
	}
	if got := RequiredCLIContract(m); got != protocolcli.CurrentContract {
		t.Errorf("RequiredCLIContract = %d, want the base %d", got, protocolcli.CurrentContract)
	}
	if DeclaresChangedFiles(nil) {
		t.Error("DeclaresChangedFiles(nil) = true")
	}
}

// bindingManifest is a manifest stamped `stamp` with one step that binds
// `binding` and no change gate.
func bindingManifest(stamp int, binding InputBinding) *Manifest {
	return &Manifest{
		Name:        "@test/x",
		Version:     "1.0.0",
		CLIContract: stamp,
		Commands: map[string]CommandDefinition{"test": {Run: []PipelineStep{
			{ID: "a", Task: "t", With: map[string]InputBinding{"config": binding}},
		}}},
		Tasks: map[string]TaskDefinition{"t": {Kind: "command", Command: "echo"}},
	}
}

// No reader below 16 resolved a `from` binding, so a manifest stamped below it
// that spells one loads as before and binds nothing: only the changed-files
// pair is the new form. Strict validation, which gates packaging, refuses each
// spelling, and the packager therefore never stamps one.
func TestUnresolvedBindingsBelowTheContractLoadAsBefore(t *testing.T) {
	for name, binding := range map[string]InputBinding{
		"command source":     {From: "command", Path: "params.target"},
		"other context path": {From: InputBindingFromContext, Path: "workspace.name"},
	} {
		m := bindingManifest(protocolcli.ChangedFilesContract-1, binding)
		if DeclaresChangedFiles(m) {
			t.Errorf("%s: DeclaresChangedFiles = true for a binding that is not the changed files", name)
		}
		loaded, err := NegotiateManifest("x.json", marshalManifest(t, m))
		if err != nil {
			t.Fatalf("%s: a manifest stamped %d must load as before: %v", name, m.CLIContract, err)
		}
		if got := loaded.Commands["test"].Run[0].With["config"]; got.From != binding.From || got.Path != binding.Path {
			t.Errorf("%s: loaded binding = %+v, want %+v", name, got, binding)
		}
		_, diags := ParseAndValidateManifest(marshalManifest(t, m))
		if !hasCode(diags, "unresolvable-binding") {
			t.Errorf("%s: strict validation = %v, want unresolvable-binding", name, diags)
		}
	}
}

// Stamped 16, a manifest speaks the binding vocabulary of that rung, so a
// source the planner does not resolve is refused at load rather than keying
// the step without the input its author meant to bind.
func TestUnresolvedBindingsAtTheContractAreRefused(t *testing.T) {
	for name, binding := range map[string]InputBinding{
		"command source": {From: "command", Path: "params.target"},
		"mistyped path":  {From: InputBindingFromContext, Path: "selection.changedFile"},
		"path only":      {Path: ContextPathChangedFiles},
	} {
		_, err := NegotiateManifest("x.json", marshalManifest(t, bindingManifest(protocolcli.ChangedFilesContract, binding)))
		if err == nil {
			t.Fatalf("%s: a manifest stamped 16 loaded a binding the planner does not resolve", name)
		}
		if !strings.Contains(err.Error(), "commands.test.run[0].with.config") {
			t.Errorf("%s: error %q does not name the binding", name, err)
		}
	}
	if _, err := NegotiateManifest("x.json", marshalManifest(t, bindingManifest(protocolcli.ChangedFilesContract,
		InputBinding{From: InputBindingFromContext, Path: ContextPathChangedFiles}))); err != nil {
		t.Fatalf("the changed-files binding stamped 16 must load: %v", err)
	}
}
