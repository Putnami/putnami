package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/github-collaboration/internal/github"
	"go.putnami.dev/tooling/github-collaboration/internal/provider"
)

const feature = "tooling/github-collaboration"

func readOffer(t *testing.T) collab.ProviderOffer {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", extension.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := extension.ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("the manifest does not parse strictly: %v", diags)
	}
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
	return offer
}

// TestTheManifestPassesBothGates holds the shipped manifest to the extension
// protocol's strict validation and to the collaboration offer the
// orchestrator reads: the provider offers exactly the operations this runtime
// answers, through the runtime's provider bridge.
func TestTheManifestPassesBothGates(t *testing.T) {
	spectest.Proves(t, feature, "truthful-manifest", "the-manifest-passes-both-gates")
	offer := readOffer(t)
	if !slices.Equal(offer.Versions(collab.ContractTasks), []int{1}) || !slices.Equal(offer.Versions(collab.ContractProposals), []int{1}) ||
		len(offer.Versions(collab.ContractMemory)) != 0 {
		t.Fatalf("offer %v", offer)
	}
	answered := map[collab.OperationKey]bool{}
	for key := range provider.New().Handlers() {
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
			if provided.Spec.Preconditions == collab.PreconditionRuleDeclared && provided.Declaration.Preconditions != collab.PreconditionsChecked {
				t.Errorf("%s declares preconditions %q; GitHub has no conditional write, so it compares then writes", provided.Tool, provided.Declaration.Preconditions)
			}
		}
	}
	if len(answered) != 0 {
		t.Errorf("the runtime answers operations the manifest does not offer: %v", answered)
	}
	proposals, _ := offer.Operations(collab.ContractProposals, 1)
	if !strings.Contains(proposals[collab.OperationMerge].Definition.Description, "settings.allowMerge") {
		t.Error("the merge tool must say that the binding has to allow it")
	}
}

// TestTheLinkToolDeclaresTheParentOutsideTheRevision: the tasks contract lets
// a provider leave the parent outside a task's revision only when its link
// operation says so, where capabilities reports it.
func TestTheLinkToolDeclaresTheParentOutsideTheRevision(t *testing.T) {
	spectest.Proves(t, feature, "truthful-manifest", "the-parent-is-outside-the-revision-and-link-says-so")
	tasks, _ := readOffer(t).Operations(collab.ContractTasks, 1)
	description := tasks[collab.OperationLink].Definition.Description
	if !strings.Contains(description, "outside the issue's revision") || !strings.Contains(description, "expectedRevision does not detect a concurrent link") {
		t.Fatalf("the link tool does not declare that the parent is outside the revision: %q", description)
	}
}

func TestNoClaimIsOffered(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "no-claim-is-offered")
	tasks, _ := readOffer(t).Operations(collab.ContractTasks, 1)
	if _, offered := tasks[collab.OperationClaim]; offered {
		t.Fatal("GitHub cannot hold a task exclusively; claim must not be offered")
	}
	if !strings.Contains(tasks[collab.OperationAssign].Definition.Description, "never an exclusive claim") {
		t.Error("the assign tool must say an assignee is not a claim")
	}
}

func TestEveryToolReachesOutsideTheMachineAndSaysSo(t *testing.T) {
	spectest.Proves(t, feature, "truthful-manifest", "every-tool-reaches-outside-the-machine-and-says-so")
	offer := readOffer(t)
	for _, contract := range []string{collab.ContractTasks, collab.ContractProposals} {
		operations, _ := offer.Operations(contract, 1)
		for _, provided := range operations {
			if !*provided.Definition.Annotations.OpenWorldHint {
				t.Errorf("%s reaches GitHub and must say openWorldHint: true", provided.Tool)
			}
			if provided.Definition.TimeoutMs <= 100000 {
				t.Errorf("%s times out after %dms, before the provider's own 100s budget", provided.Tool, provided.Definition.TimeoutMs)
			}
		}
	}
}

func TestTheRuntimeHandshake(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"__putnami", "runtime-info"}, provider.New(), nil, &out, &errOut); code != 0 {
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

// TestTheProviderBridgeRedactsItsAnswer runs one routed call through the
// runtime against a loopback stand-in whose answer carries the credential.
func TestTheProviderBridgeRedactsItsAnswer(t *testing.T) {
	const token = "ghp_bridgeTOKENbridgeTOKENbridgeTOKEN00"
	var agent atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent.Store(r.Header.Get("User-Agent"))
		if r.Header.Get("Authorization") != "Bearer "+token || r.URL.Path != "/repos/acme/app/issues/3" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"id":1,"number":3,"title":"Echo `+token+`","state":"open","labels":[],"assignees":[],`+
			`"html_url":"https://github.com/acme/app/issues/3","updated_at":"2026-09-24T08:00:00Z"}`)
	}))
	defer server.Close()
	env := map[string]string{github.OverrideVariable: server.URL, "GH_TOKEN": token}
	p := &provider.Provider{Getenv: func(name string) string { return env[name] }}
	request, _ := json.Marshal(extension.ToolCallRequest{
		Name:      "github-collaboration.tasks.get",
		Arguments: json.RawMessage(`{"ref":{"source":"github:acme/app","id":"3"}}`),
		Provider: &extension.ToolProviderCall{Contract: "tasks", Version: 1, Operation: "get",
			Settings: json.RawMessage(`{"repository":"acme/app"}`)},
	})
	var out, errOut bytes.Buffer
	if code := run([]string{providerToolArg}, p, bytes.NewReader(request), &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	if strings.Contains(out.String(), token) || !strings.Contains(out.String(), github.RedactedMarker) {
		t.Fatalf("the answer carries the credential: %s", out.String())
	}
	if seen, _ := agent.Load().(string); !strings.HasPrefix(seen, "putnami-github-collaboration/") {
		t.Errorf("user agent %q", seen)
	}
	var result extension.ToolCallResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	response, diags := collab.ParseResponse([]byte(result.Content[0].Text))
	if diags != nil || response.Outcome != collab.OutcomeOK || result.IsError {
		t.Fatalf("response %s: %v", result.Content[0].Text, diags)
	}
	if code := run([]string{providerToolArg}, p, strings.NewReader("{"), &out, &errOut); code != 1 {
		t.Errorf("an undecodable request exited %d", code)
	}
}

func TestAnUnknownInvocationIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"tasks", "find"}, provider.New(), nil, &out, &errOut); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), providerToolArg) {
		t.Fatalf("code %d, out %q, err %q", code, out.String(), errOut.String())
	}
}
