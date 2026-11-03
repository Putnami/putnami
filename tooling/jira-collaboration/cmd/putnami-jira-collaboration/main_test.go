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
	"go.putnami.dev/tooling/jira-collaboration/internal/jira"
	"go.putnami.dev/tooling/jira-collaboration/internal/provider"
)

const feature = "tooling/jira-collaboration"

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
	spectest.Proves(t, feature, "truthful-manifest", "the-manifest-passes-both-gates")
	offer := readOffer(t)
	if !slices.Equal(offer.Versions(collab.ContractTasks), []int{1}) || len(offer.Versions(collab.ContractProposals)) != 0 ||
		len(offer.Versions(collab.ContractMemory)) != 0 {
		t.Fatalf("offer %v", offer)
	}
	answered := map[collab.OperationKey]bool{}
	for key := range provider.New().Handlers() {
		answered[key] = true
	}
	operations, _ := offer.Operations(collab.ContractTasks, 1)
	for name, provided := range operations {
		key := collab.OperationKey{Contract: collab.ContractTasks, Version: 1, Operation: name}
		if !answered[key] {
			t.Errorf("the manifest offers %s, which the runtime does not answer", key)
		}
		delete(answered, key)
		if !slices.Equal(provided.Provider.Args, []string{providerArg}) || provided.Provider.Command != "{extensionRuntime}" {
			t.Errorf("%s does not route to the runtime's provider entry point", key)
		}
		if provided.Spec.Preconditions == collab.PreconditionRuleDeclared && provided.Preconditions() != collab.PreconditionsChecked {
			t.Errorf("%s declares preconditions %q; Jira has no conditional write, so it compares then writes", key, provided.Preconditions())
		}
	}
	if len(answered) != 0 {
		t.Errorf("the runtime answers operations the manifest does not offer: %v", answered)
	}
	for _, absent := range []string{collab.OperationAssign, collab.OperationLink, collab.OperationClaim} {
		if _, offered := operations[absent]; offered {
			t.Errorf("%s is offered; ADR 0001 leaves it out", absent)
		}
	}
}

func TestEveryOperationReachesOutsideTheMachineAndSaysSo(t *testing.T) {
	spectest.Proves(t, feature, "truthful-manifest", "every-operation-reaches-outside-the-machine-and-says-so")
	operations, _ := readOffer(t).Operations(collab.ContractTasks, 1)
	for name, provided := range operations {
		if provided.Operation.OpenWorld == nil || !*provided.Operation.OpenWorld {
			t.Errorf("tasks.%s reaches Jira and must say openWorld: true", name)
		}
		if provided.Provider.TimeoutMs <= 100000 {
			t.Errorf("tasks.%s times out after %dms, before the provider's own 100s budget", name, provided.Provider.TimeoutMs)
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
	const email, token = "bot@example.com", "ATATT3xBRIDGEtokenBRIDGEtokenBRIDGE00"
	var agent atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent.Store(r.Header.Get("User-Agent"))
		user, password, _ := r.BasicAuth()
		if user != email || password != token || r.URL.Path != "/rest/api/2/issue/10000" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"id":"10000","key":"ENG-1","fields":{"summary":"Echo `+token+`","description":null,`+
			`"status":{"name":"To Do","statusCategory":{"key":"new"}},"labels":[],"created":"2026-09-24T08:00:00.000+0000",`+
			`"updated":"2026-09-24T08:00:00.000+0000"}}`)
	}))
	defer server.Close()
	env := map[string]string{jira.OverrideVariable: server.URL, jira.EmailVariable: email, jira.TokenVariable: token}
	p := &provider.Provider{Getenv: func(name string) string { return env[name] }}
	request, _ := json.Marshal(collab.ProviderRequest{
		Arguments: json.RawMessage(`{"ref":{"source":"jira:acme.atlassian.net","id":"10000"}}`),
		Provider: &collab.ProviderCall{Contract: "tasks", Version: 1, Operation: "get",
			Settings: json.RawMessage(`{"site":"acme.atlassian.net","project":"ENG"}`)},
	})
	var out, errOut bytes.Buffer
	if code := run([]string{providerArg}, p, bytes.NewReader(request), &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	if strings.Contains(out.String(), token) || !strings.Contains(out.String(), jira.RedactedMarker) {
		t.Fatalf("the answer carries the credential: %s", out.String())
	}
	if seen, _ := agent.Load().(string); !strings.HasPrefix(seen, "putnami-jira-collaboration/") {
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
