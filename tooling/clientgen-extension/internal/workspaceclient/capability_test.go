package workspaceclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// cachedAnalyzerFixture is a provider whose one operation declares a response
// cache, and the committed Go target the check judges. mutate edits the
// manifest before it is committed.
func cachedAnalyzerFixture(t *testing.T, mutate func(*clientcontract.GeneratedClientManifestV1)) (provider, memoryReader) {
	t.Helper()
	staleMs := 300000
	policy := &clientcontract.CachePolicy{FreshMs: 5000, StaleMs: &staleMs}
	operations := []clientcontract.GeneratedOperation{{
		OperationID: "effectiveAccess", Service: "identity", MethodSymbol: "EffectiveAccess", Stream: clientcontract.StreamUnary,
		Transports: []clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: "/internal/authorization/effective-access", Encoding: clientcontract.EncodingJSON}},
		Cache:      policy,
	}}
	config := &clientGenConfig{Targets: []string{"go"}}
	config.Go.Output = "clients/go"
	item := provider{
		rel: "services/identity", root: "/workspace/services/identity", config: config,
		classification: ClassificationFirstParty,
		document: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "identity", Audience: "identity"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		contractHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		operations:   operations,
	}
	content := []byte("// fresh generator output\n")
	sum := sha256.Sum256(content)
	manifest := clientcontract.GeneratedClientManifestV1{
		ProtocolVersion: clientcontract.ProtocolVersion,
		GeneratedBy:     clientcontract.GeneratedBy,
		Language:        clientcontract.GeneratedLanguageGo,
		Service:         item.document.Service,
		Binding: clientcontract.GeneratedBinding{ImportPath: "example.dev/identity/client", Clients: []clientcontract.GeneratedBindingClient{{
			Service: "identity", ClientSymbol: "IdentityClient", BindingSymbol: "RegisterIdentityClient",
		}}},
		ContractSHA256:      item.contractHash,
		Operations:          []clientcontract.GeneratedOperation{operations[0]},
		RuntimeCapabilities: []clientcontract.RuntimeCapability{clientcontract.RuntimeCapabilityResponseCache},
		Files:               []clientcontract.GeneratedFile{{Path: "client.gen.go", SHA256: hex.EncodeToString(sum[:])}},
	}
	if mutate != nil {
		mutate(&manifest)
	}
	data, err := json.Marshal(&manifest)
	if err != nil {
		t.Fatal(err)
	}
	return item, memoryReader{
		"services/identity/clients/go/client.putnami.json": data,
		"services/identity/clients/go/client.gen.go":       content,
	}
}

func TestACommittedManifestThatDeclaresACachePolicyItsRuntimeDoesNotImplementFailsTheCheck(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "runtime-capability-gate",
		"a-committed-manifest-that-declares-a-cache-policy-its-runtime-does-not-implement-fails-the-check")
	item, committed := cachedAnalyzerFixture(t, nil)
	if report := inspectWithReader("/workspace", ModeCheck, []provider{item}, nil, committed, committed); len(report.Findings) != 0 {
		t.Fatalf("a consistent cached target produced findings: %+v", report.Findings)
	}

	item, committed = cachedAnalyzerFixture(t, func(manifest *clientcontract.GeneratedClientManifestV1) {
		manifest.RuntimeCapabilities = nil
	})
	report := inspectWithReader("/workspace", ModeCheck, []provider{item}, nil, committed, committed)
	assertFindingCodes(t, report, "clientgen.unsupported-runtime-capability")

	item, committed = cachedAnalyzerFixture(t, func(manifest *clientcontract.GeneratedClientManifestV1) {
		manifest.RuntimeCapabilities = []clientcontract.RuntimeCapability{"response-cache-v2"}
	})
	report = inspectWithReader("/workspace", ModeCheck, []provider{item}, nil, committed, committed)
	found := false
	for _, finding := range report.Findings {
		found = found || finding.Code == "clientgen.unsupported-runtime-capability"
	}
	if !found || report.Clean() {
		t.Fatalf("a capability no runtime implements passed the check: %+v", report.Findings)
	}
}

func TestACommittedCachePolicyThatDriftsFromTheProviderContractFailsTheCheck(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "runtime-capability-gate",
		"a-committed-cache-policy-that-drifts-from-the-provider-contract-fails-the-check")
	item, committed := cachedAnalyzerFixture(t, func(manifest *clientcontract.GeneratedClientManifestV1) {
		policy := *manifest.Operations[0].Cache
		policy.FreshMs = 60000
		manifest.Operations[0].Cache = &policy
	})
	report := inspectWithReader("/workspace", ModeCheck, []provider{item}, nil, committed, committed)
	assertFindingCodes(t, report, "clientgen.operation-coverage-drift")
}
