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
	"go.putnami.dev/protocol/features/spectest"
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
// reads when it resolves a binding: the provider offers exactly the tasks,
// proposals and missions operations this runtime answers, each saying that it
// stays on the machine.
func TestTheManifestPassesBothGates(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "truthful-provider-declarations", "the-local-provider-manifest-draws-no-diagnostic")
	manifest := readManifest(t)
	if manifest.Name != extensionName {
		t.Fatalf("manifest name %q, runtime answers as %q", manifest.Name, extensionName)
	}
	if diags := extension.ValidateManifest(manifest); diag.HasErrors(diags) {
		t.Fatalf("the manifest fails extension validation: %v", diags)
	}
	offer, diags := collab.ReadProviderOffer(manifest.Providers)
	if len(diags) != 0 {
		t.Fatalf("the provider declarations draw diagnostics: %v", diags)
	}
	if !slices.Equal(offer.Versions(collab.ContractTasks), []int{1}) || !slices.Equal(offer.Versions(collab.ContractProposals), []int{1}) ||
		!slices.Equal(offer.Versions(collab.ContractMissions), []int{1}) || len(offer.Versions(collab.ContractMemory)) != 0 {
		t.Fatalf("offer %v", offer)
	}
	answered := map[collab.OperationKey]bool{}
	for key := range newHandlers() {
		answered[key] = true
	}
	for _, contract := range []string{collab.ContractTasks, collab.ContractProposals, collab.ContractMissions} {
		operations, _ := offer.Operations(contract, 1)
		for name, provided := range operations {
			key := collab.OperationKey{Contract: contract, Version: 1, Operation: name}
			if !answered[key] {
				t.Errorf("the manifest offers %s, which the runtime does not answer", key)
			}
			delete(answered, key)
			if !slices.Equal(provided.Provider.Args, []string{providerArg}) || provided.Provider.Command != "{extensionRuntime}" {
				t.Errorf("%s does not route to the runtime's provider entry point", key)
			}
			if provided.Operation.OpenWorld == nil || *provided.Operation.OpenWorld {
				t.Errorf("%s does not say that it stays on the machine", key)
			}
		}
	}
	if len(answered) != 0 {
		t.Errorf("the runtime answers operations the manifest does not offer: %v", answered)
	}
	status, _ := offer.Operations(collab.ContractProposals, 1)
	if !strings.Contains(status[collab.OperationStatus].Operation.Description, "no hosted checks") ||
		!strings.Contains(status[collab.OperationUpsert].Operation.Description, "merge is not offered") {
		t.Error("the proposals operations must disclose the absence of hosted checks and merge")
	}
	missions, _ := offer.Operations(collab.ContractMissions, 1)
	if !strings.Contains(missions[collab.OperationAsk].Operation.Description, "answers/<decision>.json") ||
		!strings.Contains(missions[collab.OperationSync].Operation.Description, "files in the store directory") {
		t.Error("the missions operations must disclose that the owner answers and writes messages as files")
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

func TestTheProviderEntryPoint(t *testing.T) {
	workspace := t.TempDir()
	request, _ := json.Marshal(collab.ProviderRequest{
		Arguments:     json.RawMessage(`{"title":"from the provider entry point","idempotencyKey":"k"}`),
		WorkspaceRoot: workspace,
		Provider:      &collab.ProviderCall{Contract: "tasks", Version: 1, Operation: "create"},
	})
	var out, errOut bytes.Buffer
	if code := run([]string{providerArg}, bytes.NewReader(request), &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	response, diags := collab.ParseResponse(out.Bytes())
	if diags != nil || response.Outcome != collab.OutcomeOK {
		t.Fatalf("response %s: %v", out.String(), diags)
	}
	if code := run([]string{providerArg}, strings.NewReader("{"), &out, &errOut); code != 1 {
		t.Errorf("an undecodable request exited %d", code)
	}
}

func TestAnUnknownInvocationIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"tasks", "find"}, nil, &out, &errOut); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), providerArg) {
		t.Fatalf("code %d, out %q, err %q", code, out.String(), errOut.String())
	}
}
