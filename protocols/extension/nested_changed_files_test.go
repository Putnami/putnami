package extension

import (
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// A changedFiles gate inside anyOf selects files no binding reads: strict
// validation and loading refuse it, at any depth, and a top-level one stays.
func TestContextActivation_ANestedChangeGateIsRefused(t *testing.T) {
	for name, gate := range map[string]StepActivation{
		"an alternative":       {AnyOf: []StepActivation{{ChangedFiles: []string{"**/*.go"}}, {Files: []string{"go.mod"}}}},
		"a nested alternative": {AnyOf: []StepActivation{{AnyOf: []StepActivation{{ChangedFiles: []string{"**/*.go"}}}}}},
	} {
		m := contextActivationFixture(t, gate)
		diags := FullValidateManifest(m)
		if !hasCode(diags, "nested-changed-files") {
			t.Errorf("%s: strict validation = %v, want nested-changed-files", name, diags)
		}
		if _, err := NegotiateManifest("x.json", marshalManifest(t, m)); err == nil || !strings.Contains(err.Error(), "inside anyOf") {
			t.Errorf("%s: load error = %v, want the nested change gate refused", name, err)
		}
	}
	top := contextActivationFixture(t, StepActivation{ChangedFiles: []string{"**/*.go"}, AnyOf: []StepActivation{{Files: []string{"go.mod"}}}})
	top.CLIContract = protocolcli.ContextActivationContract
	if diags := FullValidateManifest(top); hasCode(diags, "nested-changed-files") {
		t.Errorf("a top-level change gate was refused: %v", diags)
	}
}
