package openapiutil

import (
	"bytes"
	"encoding/json"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestCanonicalizeSortsParametersAndRequired(t *testing.T) {
	body := []byte(`{
  "openapi": "3.0.3",
  "info": {"title":"T","version":"1"},
  "paths": {
    "/items/{id}": {
      "get": {
        "parameters": [
          {"name":"z","in":"query","required":false},
          {"name":"id","in":"path","required":true}
        ],
        "responses": {"200":{"description":"OK"}}
      }
    }
  },
  "components": {
    "schemas": {
      "Item": {
        "type": "object",
        "required": ["z", "a"]
      }
    }
  }
}`)

	out, err := Canonicalize(body)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal canonical body: %v", err)
	}
	params := doc["paths"].(map[string]any)["/items/{id}"].(map[string]any)["get"].(map[string]any)["parameters"].([]any)
	if got := params[0].(map[string]any)["name"]; got != "id" {
		t.Fatalf("first parameter = %v, want id", got)
	}
	required := doc["components"].(map[string]any)["schemas"].(map[string]any)["Item"].(map[string]any)["required"].([]any)
	if required[0] != "a" || required[1] != "z" {
		t.Fatalf("required = %v, want [a z]", required)
	}
}

func TestMergeKeepsPathsFromBothDocuments(t *testing.T) {
	prev := []byte(`{"openapi":"3.0.3","info":{"title":"T","version":"1"},"paths":{"/v1/users":{"get":{"responses":{"200":{"description":"OK"}}}}}}`)
	next := []byte(`{"openapi":"3.0.3","info":{"title":"T","version":"1"},"paths":{"/api/configs":{"post":{"responses":{"200":{"description":"OK"}}}}}}`)

	out, err := Merge(prev, next, "generate", "describe")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var doc struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal merged body: %v", err)
	}
	for _, path := range []string{"/v1/users", "/api/configs"} {
		if _, ok := doc.Paths[path]; !ok {
			t.Fatalf("missing merged path %s in %v", path, doc.Paths)
		}
	}
}

func TestCanonicalizePreservesProviderDeclaredValues(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "contract-canonicalization-preserves-declared-order", "a-provider-declared-value-survives-canonicalization")
	body := []byte(`{
  "openapi": "3.0.3",
  "info": {"title":"T","version":"1"},
  "paths": {
    "/rules": {
      "post": {
        "requestBody": {
          "content": {
            "application/json": {
              "schema": {"$ref":"#/components/schemas/Rule"},
              "example": {"parameters":["second","first"],"required":["z","a"]}
            }
          }
        },
        "responses": {"200":{"description":"OK"}}
      }
    }
  },
  "components": {
    "schemas": {
      "Rule": {
        "type": "object",
        "required": ["z","a"],
        "properties": {
          "stages": {
            "type": "array",
            "default": ["validate","render","publish"],
            "enum": [["validate","render"],["render","validate"]]
          },
          "policy": {
            "type": "object",
            "default": {"parameters":["second","first"],"required":["z","a"]}
          }
        }
      }
    }
  }
}`)

	out, err := Canonicalize(body)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal canonical body: %v", err)
	}

	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	rule := schemas["Rule"].(map[string]any)
	properties := rule["properties"].(map[string]any)

	stages := properties["stages"].(map[string]any)
	if got := stages["default"].([]any); got[0] != "validate" || got[1] != "render" || got[2] != "publish" {
		t.Fatalf("default = %v, want the declared order [validate render publish]", got)
	}
	firstEnumMember := stages["enum"].([]any)[0].([]any)
	if firstEnumMember[0] != "validate" || firstEnumMember[1] != "render" {
		t.Fatalf("enum[0] = %v, want the declared order [validate render]", firstEnumMember)
	}

	policyDefault := properties["policy"].(map[string]any)["default"].(map[string]any)
	if got := policyDefault["required"].([]any); got[0] != "z" || got[1] != "a" {
		t.Fatalf("default.required = %v, want the declared order [z a]", got)
	}
	if got := policyDefault["parameters"].([]any); got[0] != "second" || got[1] != "first" {
		t.Fatalf("default.parameters = %v, want the declared order [second first]", got)
	}

	example := doc["paths"].(map[string]any)["/rules"].(map[string]any)["post"].(map[string]any)["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["example"].(map[string]any)
	if got := example["required"].([]any); got[0] != "z" || got[1] != "a" {
		t.Fatalf("example.required = %v, want the declared order [z a]", got)
	}
	if got := example["parameters"].([]any); got[0] != "second" || got[1] != "first" {
		t.Fatalf("example.parameters = %v, want the declared order [second first]", got)
	}

	// The schema keyword one level up is a set, so it stays sorted: the fix
	// narrows normalization to structure, it does not remove it.
	if got := rule["required"].([]any); got[0] != "a" || got[1] != "z" {
		t.Fatalf("schema required = %v, want the sorted keyword [a z]", got)
	}
}

func TestCanonicalizePreservesPutnamiExtensionPayloads(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "contract-canonicalization-preserves-declared-order", "a-putnami-extension-payload-survives-canonicalization")
	body := []byte(`{
  "openapi": "3.0.3",
  "info": {"title":"T","version":"1"},
  "paths": {
    "/items": {
      "get": {
        "x-putnami-client": {
          "security": [{"serviceToken":[]},{"apiKey":[]}],
          "required": ["z","a"],
          "parameters": ["second","first"]
        },
        "responses": {"200":{"description":"OK"}}
      }
    }
  }
}`)

	out, err := Canonicalize(body)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal canonical body: %v", err)
	}
	contract := doc["paths"].(map[string]any)["/items"].(map[string]any)["get"].(map[string]any)["x-putnami-client"].(map[string]any)
	alternatives := contract["security"].([]any)
	if _, ok := alternatives[0].(map[string]any)["serviceToken"]; !ok {
		t.Fatalf("first security alternative = %v, want the declared serviceToken first", alternatives[0])
	}
	if got := contract["required"].([]any); got[0] != "z" || got[1] != "a" {
		t.Fatalf("x-putnami-client.required = %v, want the declared order [z a]", got)
	}
	if got := contract["parameters"].([]any); got[0] != "second" || got[1] != "first" {
		t.Fatalf("x-putnami-client.parameters = %v, want the declared order [second first]", got)
	}
}

func TestMergePreservesDeclaredServerAndAlternativeOrder(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "contract-canonicalization-preserves-declared-order", "declared-server-and-alternative-order-survives-a-merge")
	prev := []byte(`{
  "openapi":"3.0.3",
  "info":{"title":"T","version":"1"},
  "servers":[{"url":"https://primary.example"},{"url":"https://fallback.example"}],
  "security":[{"serviceToken":[]},{"apiKey":[]}],
  "tags":[{"name":"zeta"},{"name":"alpha"}],
  "paths":{"/items":{"get":{"responses":{"200":{"description":"OK"}}}}}
}`)
	next := []byte(`{
  "openapi":"3.0.3",
  "info":{"title":"T","version":"1"},
  "servers":[{"url":"https://primary.example"},{"url":"https://regional.example"}],
  "security":[{"serviceToken":[]},{"oauth":["read"]}],
  "tags":[{"name":"zeta"}],
  "paths":{"/items":{"get":{"responses":{"200":{"description":"OK"}}}}}
}`)

	out, err := Merge(prev, next, "build-generate", "build-describe")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal merged body: %v", err)
	}

	servers := doc["servers"].([]any)
	wantServers := []string{"https://primary.example", "https://fallback.example", "https://regional.example"}
	if len(servers) != len(wantServers) {
		t.Fatalf("servers = %v, want %v", servers, wantServers)
	}
	for i, want := range wantServers {
		if got := servers[i].(map[string]any)["url"]; got != want {
			t.Fatalf("servers[%d] = %v, want %s", i, got, want)
		}
	}

	security := doc["security"].([]any)
	wantAlternatives := []string{"serviceToken", "apiKey", "oauth"}
	if len(security) != len(wantAlternatives) {
		t.Fatalf("security = %v, want %v", security, wantAlternatives)
	}
	for i, want := range wantAlternatives {
		if _, ok := security[i].(map[string]any)[want]; !ok {
			t.Fatalf("security[%d] = %v, want the %s alternative", i, security[i], want)
		}
	}

	tags := doc["tags"].([]any)
	wantTags := []string{"zeta", "alpha"}
	if len(tags) != len(wantTags) {
		t.Fatalf("tags = %v, want %v", tags, wantTags)
	}
	for i, want := range wantTags {
		if got := tags[i].(map[string]any)["name"]; got != want {
			t.Fatalf("tags[%d] = %v, want %s", i, got, want)
		}
	}
}

func TestMergeTakesTheDescribedValueWholeAndCanonicalizeIsIdempotent(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "contract-canonicalization-preserves-declared-order", "a-canonical-contract-is-stable-under-a-second-pass")
	prev := []byte(`{
  "openapi":"3.0.3",
  "info":{"title":"T","version":"1"},
  "paths":{"/items":{"get":{"responses":{"200":{"description":"OK","content":{"application/json":{"schema":{"type":"object"},"example":{"id":"guessed","stale":true}}}}}}}}
}`)
	next := []byte(`{
  "openapi":"3.0.3",
  "info":{"title":"T","version":"1"},
  "paths":{"/items":{"get":{"responses":{"200":{"description":"OK","content":{"application/json":{"schema":{"type":"object"},"example":{"id":"declared"}}}}}}}}
}`)

	out, err := Merge(prev, next, "build-generate", "build-describe")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal merged body: %v", err)
	}
	example := doc["paths"].(map[string]any)["/items"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["example"].(map[string]any)
	if example["id"] != "declared" {
		t.Fatalf("example.id = %v, want the described value declared", example["id"])
	}
	if _, ok := example["stale"]; ok {
		t.Fatalf("example = %v, want the guessed field dropped with the value it belonged to", example)
	}

	again, err := Canonicalize(out)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if !bytes.Equal(out, again) {
		t.Fatalf("a second canonical pass changed the bytes:\nfirst:\n%s\nsecond:\n%s", out, again)
	}
}

// A first-party provider enumerates every route it serves, so publication must
// not resurrect one it stopped serving. The merged bytes equal the canonical
// bytes of the described document exactly: same route set, same operations, no
// residue from the previously published surface.
func TestMergeDropsRoutesTheFirstPartyProviderNoLongerDeclares(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "contract-canonicalization-preserves-declared-order", "a-route-a-first-party-provider-removed-does-not-survive-publication")

	contract := `"x-putnami-client":{"protocolVersion":1,"service":{"id":"widgets","audience":"urn:widgets"},"credentials":{}}`
	operation := `{"operationId":"getWidgets","responses":{"200":{"description":"OK"}},"x-putnami-client":{"stream":"unary","transports":[{"protocol":"rest-json","path":"/widgets","encoding":"json"}],"security":{"alternatives":[{"allOf":[]}]},"idempotency":{"kind":"safe"}}}`
	retired := `{"operationId":"getRetired","responses":{"200":{"description":"OK"}}}`

	// The previously published document still carries the retired route, plus a
	// stale second method on a path the provider still serves.
	prev := []byte(`{"openapi":"3.0.3","info":{"title":"T","version":"1"},` + contract + `,"paths":{` +
		`"/widgets":{"get":` + operation + `,"delete":` + retired + `},` +
		`"/retired":{"get":` + retired + `}}}`)
	next := []byte(`{"openapi":"3.0.3","info":{"title":"T","version":"1"},` + contract + `,"paths":{"/widgets":{"get":` + operation + `}}}`)

	merged, err := Merge(prev, next, "generate", "describe")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	canonical, err := Canonicalize(next)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if !bytes.Equal(merged, canonical) {
		t.Fatalf("publication kept a route the provider removed\nmerged:\n%s\ndescribed:\n%s", merged, canonical)
	}
	if bytes.Contains(merged, []byte("/retired")) || bytes.Contains(merged, []byte("getRetired")) {
		t.Fatalf("retired route survived publication:\n%s", merged)
	}
}

// The same route retention still holds for a document that is not first-party:
// a static visitor's surface is all the publication has, so nothing is dropped.
func TestMergeKeepsRoutesWhenTheProviderIsNotFirstParty(t *testing.T) {
	prev := []byte(`{"openapi":"3.0.3","info":{"title":"T","version":"1"},"paths":{"/legacy":{"get":{"responses":{"200":{"description":"OK"}}}}}}`)
	next := []byte(`{"openapi":"3.0.3","info":{"title":"T","version":"1"},"paths":{"/current":{"get":{"responses":{"200":{"description":"OK"}}}}}}`)
	merged, err := Merge(prev, next, "generate", "describe")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !bytes.Contains(merged, []byte("/legacy")) {
		t.Fatalf("non first-party publication lost a statically discovered route:\n%s", merged)
	}
}
