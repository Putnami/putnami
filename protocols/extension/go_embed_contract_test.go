package extension

import (
	"encoding/json"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
)

func TestGoEmbedSelectorRequiresAdditiveCLIContract(t *testing.T) {
	manifest := []byte(`{"name":"@test/go","cliContract":5,"commands":{"build":{"run":[{"id":"b","task":"build"}]}},"tasks":{"build":{"kind":"command","command":"echo","inputs":{"sources":{"from":"project","files":["**/*.go","go-embed:build"]}}}}}`)
	if _, err := NegotiateManifest("go.json", manifest); err == nil || !strings.Contains(err.Error(), "requires 6") {
		t.Fatalf("old CLI stamp accepted selector: %v", err)
	}
	manifest = []byte(strings.Replace(string(manifest), `"cliContract":5`, `"cliContract":6`, 1))
	m, err := NegotiateManifest("go.json", manifest)
	if err != nil || RequiredCLIContract(m) != protocolcli.GoEmbedInputsContract {
		t.Fatalf("selector contract = %v, %v", m, err)
	}
	m.AgentContent = &AgentContentContribution{}
	if got := RequiredCLIContract(m); got != protocolcli.GoEmbedInputsContract {
		t.Fatalf("combined additive floor = %d", got)
	}
	for _, bad := range []string{
		strings.Replace(string(manifest), `"from":"project"`, `"from":"workspace"`, 1),
		strings.Replace(string(manifest), `go-embed:build`, `go-embed:unknown`, 1),
	} {
		if _, err := NegotiateManifest("go.json", []byte(bad)); err == nil {
			t.Fatalf("unsupported selector accepted: %s", bad)
		}
	}
}

func TestFullValidateManifestSharesGoEmbedSelectorRulesWithLoader(t *testing.T) {
	base := &Manifest{
		Commands: map[string]CommandDefinition{"build": {Run: []PipelineStep{{ID: "build", Task: "build"}}}},
		Tasks: map[string]TaskDefinition{"build": {Kind: "command", Command: "echo", Inputs: map[string]TaskInputPort{
			"sources": {From: TaskInputFromProject, Files: []string{"**/*.go", "go-embed:build"}},
		}}},
	}
	if diags := FullValidateManifest(base); diag.HasErrors(diags) {
		t.Fatalf("valid project selector rejected: %v", diags)
	}
	for _, test := range []struct {
		name, origin, selector, want string
	}{
		{"workspace origin", TaskInputFromWorkspace, "go-embed:test", "only valid in project task inputs"},
		{"closure origin", TaskInputFromClosure, "go-embed:build", "only valid in project task inputs"},
		{"unknown selector", TaskInputFromProject, "go-embed:unknown", "unsupported go embed selector"},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := *base
			copy.Tasks = map[string]TaskDefinition{"build": base.Tasks["build"]}
			task := copy.Tasks["build"]
			task.Inputs = map[string]TaskInputPort{"sources": {From: test.origin, Files: []string{test.selector}}}
			copy.Tasks["build"] = task
			diags := FullValidateManifest(&copy)
			if !diag.HasErrors(diags) || !strings.Contains(formatDiagnostics(diags), test.want) {
				t.Fatalf("FullValidateManifest accepted %s: %v", test.name, diags)
			}
			data, err := json.Marshal(&copy)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NegotiateManifest("go.json", data); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loader disagreed on %s: %v", test.name, err)
			}
		})
	}
}
