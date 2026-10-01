package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

func readManifest(t *testing.T) *extension.Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", extension.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := extension.ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("the manifest does not parse strictly: %v", diags)
	}
	return manifest
}

// TestTheManifestPassesBothGates holds the shipped manifest to the extension
// protocol's strict validation and to the collaboration offer the orchestrator
// reads when it resolves a binding: the provider offers exactly the tasks and
// proposals operations this runtime answers, with truthful annotations.
func TestTheManifestPassesBothGates(t *testing.T) {
	manifest := readManifest(t)
	if manifest.Name != extensionName {
		t.Fatalf("manifest name %q, runtime answers as %q", manifest.Name, extensionName)
	}
	if diags := extension.ValidateManifest(manifest); diag.HasErrors(diags) {
		t.Fatalf("the manifest fails extension validation: %v", diags)
	}
	offer, diags := collab.ReadProviderOffer(manifest.Tools)
	if len(diags) != 0 {
		t.Fatalf("the provider declarations draw diagnostics: %v", diags)
	}
	if !slices.Equal(offer.Versions(collab.ContractTasks), []int{1}) || !slices.Equal(offer.Versions(collab.ContractProposals), []int{1}) ||
		len(offer.Versions(collab.ContractMemory)) != 0 {
		t.Fatalf("offer %v", offer)
	}
	answered := map[collab.OperationKey]bool{}
	for key := range newHandlers() {
		answered[key] = true
	}
	for _, contract := range []string{collab.ContractTasks, collab.ContractProposals} {
		operations, _ := offer.Operations(contract, 1)
		for name, provided := range operations {
			key := collab.OperationKey{Contract: contract, Version: 1, Operation: name}
			if !answered[key] {
				t.Errorf("the manifest offers %s, which the runtime does not answer", key)
			}
			delete(answered, key)
			if !slices.Equal(provided.Definition.Args, []string{providerToolArg}) || provided.Definition.Command != "{extensionRuntime}" {
				t.Errorf("%s does not route to the runtime's provider bridge", provided.Tool)
			}
			if *provided.Definition.Annotations.OpenWorldHint {
				t.Errorf("%s claims to reach outside the machine", provided.Tool)
			}
		}
	}
	if len(answered) != 0 {
		t.Errorf("the runtime answers operations the manifest does not offer: %v", answered)
	}
	status, _ := offer.Operations(collab.ContractProposals, 1)
	if !strings.Contains(status[collab.OperationStatus].Definition.Description, "no hosted checks") ||
		!strings.Contains(status[collab.OperationUpsert].Definition.Description, "merge is not offered") {
		t.Error("the proposals tools must disclose the absence of hosted checks and merge")
	}
}

func TestTheRuntimeHandshake(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"__putnami", "runtime-info"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	var info runtimeproto.Info
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Extension != extensionName || info.RuntimeABI != runtimeproto.RuntimeABIVersion {
		t.Fatalf("info %+v", info)
	}
}

func TestTheProviderBridge(t *testing.T) {
	workspace := t.TempDir()
	request, _ := json.Marshal(extension.ToolCallRequest{
		Name:          "local-collaboration.tasks.create",
		Arguments:     json.RawMessage(`{"title":"from the bridge","idempotencyKey":"k"}`),
		WorkspaceRoot: workspace,
		Provider:      &extension.ToolProviderCall{Contract: "tasks", Version: 1, Operation: "create"},
	})
	var out, errOut bytes.Buffer
	if code := run([]string{providerToolArg}, bytes.NewReader(request), &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	var result extension.ToolCallResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	response, diags := collab.ParseResponse([]byte(result.Content[0].Text))
	if diags != nil || response.Outcome != collab.OutcomeOK || result.IsError {
		t.Fatalf("response %s: %v", result.Content[0].Text, diags)
	}
	if code := run([]string{providerToolArg}, strings.NewReader("{"), &out, &errOut); code != 1 {
		t.Errorf("an undecodable request exited %d", code)
	}
}

func TestAnUnknownInvocationIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"tasks", "find"}, nil, &out, &errOut); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), providerToolArg) {
		t.Fatalf("code %d, out %q, err %q", code, out.String(), errOut.String())
	}
}
