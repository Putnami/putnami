package collaboration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// frozenV1Digests pins every document of fixtures/v1, written on 2026-11-07
// when version 1 became stable (ADR 0006). A version 1 document never changes:
// when this map or a file under fixtures/v1 needs an edit, the change opens a
// new contract version instead.
var frozenV1Digests = map[string]string{
	"bindings.full.json":                               "d5f3c71756bd3a62959c5338940d0d4041f114dbc1d4eff44cc67a1f511188c2",
	"envelope.tasks-find-ok.json":                      "5d20ddf3c9c0f0b0a49645e766f570b35bc386a2cda6be73c824847b48c5ba57",
	"envelope.unresolved.json":                         "a2bc64d7c44755baf17c4c8f579108866ee30624531c250d7c0478a937ec76c0",
	"providers.full.json":                              "c220e088b451a1c6d95350774813a6146aeae208395613d7e9d239ebc86ff36b",
	"request.memory.checkpoint.expected-revision.json": "0b7d2822a8275770bf6557f3bdc532b0624b4c6ad52abf65b3f558816070d935",
	"request.missions.ask.options.json":                "78998c247762e5dc56a0771fb9c9acdb21234ffe83bad6047cde395f8baed5c0",
	"request.missions.sync.minimal.json":               "bb09e91bf533ec4414503790ca365b1de162a5e0bc8eaca1e6115ea69e1f010d",
	"request.proposals.review.approve.json":            "9bb0cee9debc51ee876edd95919b4b937607d750f4c4146f9ab0e7e555e81f77",
	"request.proposals.upsert.draft.json":              "9cdf9141e1a650bd94a7913c774ea17a6d49c35c1f4d31139b8c6d2110cf3725",
	"request.tasks.create.minimal.json":                "0296475d8f1135e2bc000a2627e670d56996aa49627def1db4f17f5570ad5d22",
	"request.tasks.find.paged.json":                    "6c776a36b852711428b1fe9c0e1fc9ed0a88ffbfb77dc45e9701bf2d4b8b37cf",
	"request.tasks.transition.done.json":               "13c51da2263428cf4e5d0f7b22c52efc72034dbb9d319943c02efab1768403b4",
	"response.not-found.json":                          "b1f439dd9fe84a72706c5dce951e0bc6e29361521b70c25452c023190c9d62eb",
	"response.ok.json":                                 "7d65550b45937fbbd8d3f31e9b5c45b9d08c3d16003c817371a3886ac8969188",
	"result.memory.mission.current.json":               "4ae5296e56bff7b8ae47bf4a3628763a4196503426a828b1d1db0aa2ff5f154d",
	"result.missions.ask.answered.json":                "1f7c47799bf494a2dc55118c5c1d2f142d2ac01a027d73b1753356858aa6f219",
	"result.proposals.status.hosted.json":              "03b0e04deaada638fe9a34ad0697bfd7e5e87daeafbb5204931ded08126e6f7f",
	"result.proposals.upsert.created.json":             "8fcb8bdf7d0d34730a4109ea715be5a40e2c54292377eded0c2bf1707d132476",
	"result.tasks.find.two.json":                       "8d60ad43a9db2f58f0b0e24dca6de354e882b538634d49eda890bd4d8939346d",
}

func frozenV1Files(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("fixtures", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func TestFrozenV1_EveryDocumentParses(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "version-one-documents-never-change", "every-frozen-v1-document-parses")
	kinds := map[string]bool{}
	for _, name := range frozenV1Files(t) {
		data, err := os.ReadFile(filepath.Join("fixtures", "v1", name))
		if err != nil {
			t.Fatal(err)
		}
		kind, _, _ := bytes.Cut([]byte(name), []byte("."))
		kinds[string(kind)] = true
		t.Run(name, func(t *testing.T) {
			if string(kind) == "providers" {
				readFrozenProviders(t, data)
				return
			}
			if diags := parseFixture(t, name, data); diag.HasErrors(diags) {
				t.Fatalf("a frozen version 1 document is rejected: %v", diags)
			}
		})
	}
	for _, kind := range []string{"bindings", "providers", "request", "result", "response", "envelope"} {
		if !kinds[kind] {
			t.Errorf("fixtures/v1 holds no %s document", kind)
		}
	}
}

// readFrozenProviders reads the providers member of an extension manifest, the
// declaration a provider author writes, and checks it against the catalog.
func readFrozenProviders(t *testing.T, data []byte) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var providers map[string]extension.ProviderDeclaration
	if err := decoder.Decode(&providers); err != nil {
		t.Fatalf("a frozen provider declaration does not decode: %v", err)
	}
	offer, diags := ReadProviderOffer(providers)
	if diag.HasErrors(diags) {
		t.Fatalf("a frozen provider declaration is refused: %v", diags)
	}
	for _, contract := range []string{ContractTasks, ContractProposals, ContractMemory, ContractMissions} {
		if _, ok := offer[contract]; !ok {
			t.Errorf("the frozen provider declaration offers no %s contract", contract)
		}
	}
}

func TestFrozenV1_EveryDocumentKeepsItsDigest(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "version-one-documents-never-change", "every-frozen-v1-document-keeps-its-digest")
	files := frozenV1Files(t)
	pinned := make([]string, 0, len(frozenV1Digests))
	for name := range frozenV1Digests {
		pinned = append(pinned, name)
	}
	sort.Strings(pinned)
	if !slices.Equal(files, pinned) {
		t.Fatalf("fixtures/v1 holds %v, the pin lists %v: a version 1 document is never added, removed or renamed (ADR 0006)", files, pinned)
	}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join("fixtures", "v1", name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != frozenV1Digests[name] {
			t.Errorf("fixtures/v1/%s changed (sha256 %s): a version 1 document never changes; open a new contract version instead (ADR 0006)", name, got)
		}
	}
}
