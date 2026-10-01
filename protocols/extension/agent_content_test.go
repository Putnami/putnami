package extension

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
)

// agentContentValidFixtures are the valid fixtures that declare an
// agent-content contribution, one per form.
var agentContentValidFixtures = []string{
	"agent-content.json",          // packaged, content-only: no runtime, no command, no tool
	"agent-content-authored.json", // authored, built from its source
}

// agentContentInvalidFixtureCodes maps each agent-content counter-example to
// the exact diagnostic codes FullValidateManifest reports, in order. Exact
// rather than contains: a fixture that started failing for an unrelated reason
// would stop certifying the rule it was written for.
var agentContentInvalidFixtureCodes = map[string][]string{
	// An empty section names nothing; it must not validate a manifest that
	// otherwise declares nothing either.
	"agent-content-empty.json": {"required-field", "empty-agent-content"},
	// Both directories are extension-relative and stay inside the extension.
	"agent-content-paths.json": {"invalid-agent-content-path", "invalid-agent-content-path"},
	// The build writes path, so it can neither contain nor sit inside source.
	"agent-content-overlap.json": {"agent-content-overlap"},
	// A contribution is authored or packaged, never both.
	"agent-content-ambiguous.json": {"ambiguous-agent-content"},
	// Paths are canonical and the digest is bare lowercase hex.
	"agent-content-digest.json": {"invalid-agent-content-path", "invalid-agent-content-digest"},
	// A superseded identity is a registry name, is not the extension itself,
	// and appears once.
	"agent-content-supersedes.json": {"invalid-agent-content-supersedes", "invalid-agent-content-supersedes", "invalid-agent-content-supersedes"},
}

// The superseded identities are part of both forms: the authored fixture
// carries them, and they survive the strict parse unchanged and in order.
func TestAgentContent_SupersedesIsReadInBothForms(t *testing.T) {
	m := loadFixtureManifest(t, "fixtures/valid/agent-content-authored.json")
	if got, want := m.AgentContent.Supersedes, []string{"@acme/review-workflows", "maintainer-workflows"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("supersedes = %v, want %v", got, want)
	}
	packaged := loadFixtureManifest(t, "fixtures/valid/agent-content.json")
	packaged.AgentContent.Supersedes = []string{"@acme/workflows"}
	if diags := ValidateAgentContent(packaged); diag.HasErrors(diags) {
		t.Fatalf("a packaged contribution may name what it supersedes: %v", diags)
	}
}

func TestAgentContent_InvalidFixturesReportExactCodes(t *testing.T) {
	for name, codes := range agentContentInvalidFixtureCodes {
		t.Run(name, func(t *testing.T) {
			m := loadFixtureManifest(t, filepath.Join("fixtures/invalid", name))
			diags := FullValidateManifest(m)
			if got := diagCodes(diags); !reflect.DeepEqual(got, codes) {
				t.Fatalf("codes = %v, want %v (%v)", got, codes, diags)
			}
		})
	}
}

// A content-only extension is a complete manifest: no runtime, no command and
// no tool are needed, and the packaged fixture proves it under the same entry
// points every other manifest goes through.
func TestAgentContent_ContentOnlyManifestNeedsNoRuntimeCommandOrTool(t *testing.T) {
	m := loadFixtureManifest(t, "fixtures/valid/agent-content.json")
	if m.DeclaresRuntime() || len(m.Commands) != 0 || len(m.Tools) != 0 {
		t.Fatal("the content-only fixture must declare no runtime, command or tool")
	}
	if diags := FullValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("a content-only manifest must validate: %v", diags)
	}
	if !DeclaresContractSurface(m) {
		t.Fatal("agent content is a contract surface: an unstamped content-only manifest must not load as hook-only")
	}
}

// An empty contribution must never stand in for a real one: removing the
// section yields the ordinary "declares nothing" error, and keeping it empty
// still fails.
func TestAgentContent_EmptyContributionDoesNotValidateAnEmptyManifest(t *testing.T) {
	bare, diags := ParseManifest([]byte(`{"name":"@acme/empty"}`))
	if diag.HasErrors(diags) {
		t.Fatal(diags)
	}
	if codes := diagCodes(diag.Errors(ValidateManifest(bare))); !reflect.DeepEqual(codes, []string{"required-field"}) {
		t.Fatalf("an empty manifest reports %v, want required-field", codes)
	}

	empty, diags := ParseManifest([]byte(`{"name":"@acme/empty","agentContent":{}}`))
	if diag.HasErrors(diags) {
		t.Fatal(diags)
	}
	errs := diag.Errors(ValidateManifest(empty))
	if len(errs) == 0 {
		t.Fatal("an empty agent-content section validated an otherwise empty manifest")
	}
	if !hasCode(errs, "empty-agent-content") {
		t.Fatalf("errors = %v, want empty-agent-content", errs)
	}
}

// Every manifest written before the section keeps its verdict and its stamp:
// no diagnostic from the new validator, and the base contract as the required
// stamp. The mutation is the control that proves the validator is wired.
func TestAgentContent_AdditiveForEveryOtherFixture(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	declaring := make(map[string]bool, len(agentContentValidFixtures))
	for _, name := range agentContentValidFixtures {
		declaring[name] = true
	}
	checked := 0
	for _, path := range files {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			m := loadFixtureManifest(t, path)
			if declaring[name] {
				if !m.DeclaresAgentContent() || RequiredCLIContract(m) != protocolcli.AgentContentContract {
					t.Fatalf("%s must declare agent content and require contract %d", name, protocolcli.AgentContentContract)
				}
				return
			}
			checked++
			if m.DeclaresAgentContent() {
				t.Fatalf("%s unexpectedly declares agent content", name)
			}
			if diags := ValidateAgentContent(m); diags != nil {
				t.Fatalf("agent-content validation spoke about a manifest without the section: %v", diags)
			}
			if got := RequiredCLIContract(m); got != protocolcli.CurrentContract {
				t.Fatalf("RequiredCLIContract = %d, want the base contract %d", got, protocolcli.CurrentContract)
			}
			m.AgentContent = &AgentContentContribution{Path: "../escape", ManifestSHA256: strings.Repeat("a", 64)}
			if !hasCode(ValidateAgentContent(m), "invalid-agent-content-path") {
				t.Fatal("control: a defective agent-content section must be reported")
			}
		})
	}
	if checked == 0 {
		t.Fatal("no fixture without agent content was checked")
	}
}

// The valid content fixtures carry the stamp their vocabulary requires, so the
// negotiating loader reads them.
func TestAgentContent_ValidFixturesLoad(t *testing.T) {
	for _, name := range agentContentValidFixtures {
		t.Run(name, func(t *testing.T) {
			m, err := LoadManifest(filepath.Join("fixtures/valid", name))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if m.CLIContract != protocolcli.AgentContentContract {
				t.Fatalf("cliContract = %d, want %d", m.CLIContract, protocolcli.AgentContentContract)
			}
			if !m.AgentContent.Packaged() && m.AgentContent.Source == "" {
				t.Fatal("a loaded contribution names neither a source nor a digest")
			}
		})
	}
}

// contract4Manifest is the manifest shape a CLI released at contract 4 decodes
// into: it has no agentContent member.
type contract4Manifest struct {
	CLIContract   int                               `json:"cliContract,omitempty"`
	Commands      map[string]CommandDefinition      `json:"commands,omitempty"`
	CommandGroups map[string]CommandGroupDefinition `json:"commandGroups,omitempty"`
	Tools         map[string]ToolDefinition         `json:"tools,omitempty"`
}

// contract4Load is the negotiation a CLI released at contract 4 runs, restated
// here so the compatibility claim is executable: it decodes permissively and
// ladders on the stamp alone, with 4 as its current and latest contract.
func contract4Load(path string, data []byte) (*contract4Manifest, error) {
	const readerContract = 4
	var m contract4Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse extension manifest %s: %w", path, err)
	}
	surface := len(m.Commands) > 0 || len(m.CommandGroups) > 0 || len(m.Tools) > 0
	switch {
	case m.CLIContract > readerContract:
		return nil, fmt.Errorf("extension manifest %s requires a newer putnami (contract %d > %d)", path, m.CLIContract, readerContract)
	case m.CLIContract == readerContract:
		return &m, nil
	case !surface:
		return &m, nil
	default:
		return nil, errors.New("declares an older contract")
	}
}

// TestAgentContent_OlderReaderRefusesThePackage is the compatibility decision,
// executed. A contract-4 reader decodes permissively, so the agentContent member
// vanishes without an error — schema validation alone could never stop that.
// What stops it is the stamp: a content-bearing manifest is stamped 5, and the
// released ladder refuses any stamp above its own contract. A content-only
// manifest that a packager left at 4 or unstamped would load there as a
// content-free extension, which is exactly why this build never produces one:
// its own loader refuses both.
func TestAgentContent_OlderReaderRefusesThePackage(t *testing.T) {
	path := filepath.Join("fixtures/valid", "agent-content.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The permissive decode drops the section without complaint.
	var decoded contract4Manifest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("a permissive decode must not fail on an unknown member: %v", err)
	}

	loaded, err := contract4Load(path, data)
	if err == nil || loaded != nil {
		t.Fatal("a contract-4 reader loaded a content-bearing package; it would silently drop the agent content")
	}
	if !strings.Contains(err.Error(), "requires a newer putnami") {
		t.Fatalf("the refusal must name the remedy: %v", err)
	}

	// The two stamps that WOULD fool that reader never load here, so they can
	// never be published by a packager whose postcondition is this loader.
	for _, stamp := range []int{0, protocolcli.CurrentContract} {
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		if stamp == 0 {
			delete(raw, "cliContract")
		} else {
			raw["cliContract"] = stamp
		}
		rewritten, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if old, err := contract4Load(path, rewritten); err != nil || old == nil {
			t.Fatalf("control: a contract-4 reader loads the content-only manifest stamped %d (err %v)", stamp, err)
		}
		if m, err := NegotiateManifest(path, rewritten); err == nil || m != nil {
			t.Fatalf("this loader accepted a content-bearing manifest stamped %d", stamp)
		} else if !strings.Contains(err.Error(), "requires 5 for its agent-content contribution") {
			t.Fatalf("stamp %d: the refusal must name the agent-content requirement: %v", stamp, err)
		}
	}
}

// An extension that supplies commands ships its matching instructions under
// the same manifest and the same version: the two surfaces coexist, and the
// stamp the manifest needs is the additive one.
func TestAgentContent_CoexistsWithACommandSurface(t *testing.T) {
	path := writeMatrixManifest(t, matrixManifest(protocolcli.AgentContentContract, commandsSurface+",\n  "+agentContentSurface))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("commands and agent content must validate together: %v", diags)
	}
	if len(m.Commands) != 1 || !m.DeclaresAgentContent() {
		t.Fatal("both surfaces must survive parsing")
	}
	if got := RequiredCLIContract(m); got != protocolcli.AgentContentContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.AgentContentContract)
	}
	if _, err := LoadManifest(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	below := writeMatrixManifest(t, matrixManifest(protocolcli.CurrentContract, commandsSurface+",\n  "+agentContentSurface))
	if _, err := LoadManifest(below); err == nil {
		t.Fatal("a command-bearing manifest with agent content stamped at the base contract must not load")
	}
}

// A content-free manifest stamped at the additive contract still loads: the
// stamp covers more than the manifest uses, which a contract-5 reader
// represents completely.
func TestAgentContent_ContentFreeManifestAtTheAdditiveContractLoads(t *testing.T) {
	path := writeMatrixManifest(t, matrixManifest(protocolcli.AgentContentContract, commandsSurface))
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.DeclaresAgentContent() {
		t.Fatal("the manifest declares no agent content")
	}
}
