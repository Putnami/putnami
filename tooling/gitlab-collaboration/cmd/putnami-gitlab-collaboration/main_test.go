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
	"go.putnami.dev/sdk/extension/collabhelper"
	"go.putnami.dev/tooling/gitlab-collaboration/internal/gitlab"
	"go.putnami.dev/tooling/gitlab-collaboration/internal/provider"
)

const feature = "tooling/gitlab-collaboration"

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
	offer, diags := collab.ReadProviderOffer(manifest.Providers)
	if len(diags) != 0 {
		t.Fatalf("the provider declarations draw diagnostics: %v", diags)
	}
	return offer
}

// TestTheManifestPassesBothGates holds the shipped manifest to the extension
// protocol's strict validation and to the collaboration offer the
// orchestrator reads: the provider offers exactly the operations this runtime
// answers, through the runtime's provider entry point.
func TestTheManifestPassesBothGates(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "truthful-provider-declarations", "the-gitlab-provider-manifest-draws-no-diagnostic")
	spectest.Proves(t, feature, "truthful-manifest", "the-manifest-passes-both-gates")
	offer := readOffer(t)
	if !slices.Equal(offer.Versions(collab.ContractTasks), []int{1}) || !slices.Equal(offer.Versions(collab.ContractProposals), []int{1}) ||
		len(offer.Versions(collab.ContractMemory)) != 0 || len(offer.Versions(collab.ContractMissions)) != 0 {
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
			if !slices.Equal(provided.Provider.Args, []string{providerArg}) || provided.Provider.Command != "{extensionRuntime}" {
				t.Errorf("%s does not route to the runtime's provider entry point", key)
			}
			if provided.Spec.Preconditions == collab.PreconditionRuleDeclared && provided.Preconditions() != collab.PreconditionsChecked {
				t.Errorf("%s declares preconditions %q; GitLab has no conditional write, so it compares then writes", key, provided.Preconditions())
			}
		}
	}
	if len(answered) != 0 {
		t.Errorf("the runtime answers operations the manifest does not offer: %v", answered)
	}
	operations, _ := offer.Operations(collab.ContractTasks, 1)
	for _, absent := range []string{collab.OperationLink, collab.OperationClaim} {
		if _, offered := operations[absent]; offered {
			t.Errorf("%s is offered; ADR 0001 leaves it out", absent)
		}
	}
}

func TestEveryOperationReachesOutsideTheMachineAndSaysSo(t *testing.T) {
	spectest.Proves(t, feature, "truthful-manifest", "every-operation-reaches-outside-the-machine-and-says-so")
	offer := readOffer(t)
	for _, contract := range []string{collab.ContractTasks, collab.ContractProposals} {
		operations, _ := offer.Operations(contract, 1)
		for name, provided := range operations {
			if provided.Operation.OpenWorld == nil || !*provided.Operation.OpenWorld {
				t.Errorf("%s.%s reaches GitLab and must say openWorld: true", contract, name)
			}
			if provided.Provider.TimeoutMs <= 100000 {
				t.Errorf("%s.%s times out after %dms, before the provider's own 100s budget", contract, name, provided.Provider.TimeoutMs)
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

// TestTheProviderRedactsItsAnswer runs one routed call through the runtime
// against a loopback stand-in whose answer carries the credential.
func TestTheProviderRedactsItsAnswer(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "a-credential-never-reaches-an-answer")
	const token = "glpat-BRIDGEtokenBRIDGEtoken00"
	var agent atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent.Store(r.Header.Get("User-Agent"))
		if r.Header.Get("Authorization") != "Bearer "+token || r.URL.EscapedPath() != "/api/v4/projects/acme%2Fapp/issues/1" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"id":100,"iid":1,"project_id":3,"title":"Echo `+token+`","description":null,"state":"opened",`+
			`"labels":[],"assignees":[],"author":{"id":1,"username":"bot"},"created_at":"2026-09-24T08:00:00.000Z",`+
			`"updated_at":"2026-09-24T08:00:00.000Z","_links":{}}`)
	}))
	defer server.Close()
	env := map[string]string{gitlab.OverrideVariable: server.URL, gitlab.TokenVariable: token}
	p := &provider.Provider{Getenv: func(name string) string { return env[name] }}
	request, _ := json.Marshal(collab.ProviderRequest{
		Arguments: json.RawMessage(`{"ref":{"source":"gitlab:gitlab.com/acme/app","id":"1"}}`),
		Provider: &collab.ProviderCall{Contract: "tasks", Version: 1, Operation: "get",
			Settings: json.RawMessage(`{"project":"acme/app"}`)},
	})
	var out, errOut bytes.Buffer
	if code := run([]string{providerArg}, p, bytes.NewReader(request), &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	if strings.Contains(out.String(), token) || !strings.Contains(out.String(), collabhelper.RedactedMarker) {
		t.Fatalf("the answer carries the credential: %s", out.String())
	}
	if seen, _ := agent.Load().(string); !strings.HasPrefix(seen, "putnami-gitlab-collaboration/") {
		t.Errorf("user agent %q", seen)
	}
	response, diags := collab.ParseResponse(out.Bytes())
	if diags != nil || response.Outcome != collab.OutcomeOK {
		t.Fatalf("response %s: %v", out.String(), diags)
	}
	if code := run([]string{providerArg}, p, strings.NewReader("{"), &out, &errOut); code != 1 {
		t.Errorf("an undecodable request exited %d", code)
	}
}

func TestAnUnknownInvocationIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"tasks", "find"}, provider.New(), nil, &out, &errOut); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), providerArg) {
		t.Fatalf("code %d, out %q, err %q", code, out.String(), errOut.String())
	}
}
