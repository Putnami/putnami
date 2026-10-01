package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestReleaseSetPublishOutcomeConformance(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		events := readReleaseSetFixture(t, "success.jsonl")
		outcome, diagnostics := RequireReleaseSetPublishOutcome(events)
		if diag.HasErrors(diagnostics) {
			t.Fatalf("successful fixture produced errors: %v", diagnostics)
		}
		if len(outcome) == 0 || !json.Valid(outcome) {
			t.Fatalf("outcome = %#v", outcome)
		}
		optional, optionalDiagnostics := ReleaseSetPublishOutcome(events)
		if diag.HasErrors(optionalDiagnostics) || !bytes.Equal(optional, outcome) {
			t.Fatalf("optional extraction = %#v, diagnostics=%v", optional, optionalDiagnostics)
		}
		assertRuntimeDiagnostic(t, RejectReleaseSetPublishOutcome(events), "unexpected-release-set-outcome")
	})

	t.Run("dry run emits no successful outcome", func(t *testing.T) {
		diagnostics := RejectReleaseSetPublishOutcome(readReleaseSetFixture(t, "dry-run.jsonl"))
		if diag.HasErrors(diagnostics) {
			t.Fatalf("dry-run fixture produced errors: %v", diagnostics)
		}
	})

	t.Run("managed success requires outcome", func(t *testing.T) {
		_, diagnostics := RequireReleaseSetPublishOutcome(readReleaseSetFixture(t, "missing-outcome.jsonl"))
		assertRuntimeDiagnostic(t, diagnostics, "missing-release-set-outcome")
	})

	for _, name := range []string{"duplicate-outcome.jsonl", "duplicate-member.jsonl"} {
		t.Run(name, func(t *testing.T) {
			_, diagnostics := RequireReleaseSetPublishOutcome(readReleaseSetFixture(t, name))
			assertRuntimeDiagnostic(t, diagnostics, "duplicate-release-set-outcome")
		})
	}

	t.Run("CAS conflict cannot claim success", func(t *testing.T) {
		events := readReleaseSetFixture(t, "cas-conflict.jsonl")
		if diagnostics := RejectReleaseSetPublishOutcome(events); diag.HasErrors(diagnostics) {
			t.Fatalf("failed CAS fixture claimed an outcome: %v", diagnostics)
		}
		_, diagnostics := RequireReleaseSetPublishOutcome(events)
		assertRuntimeDiagnostic(t, diagnostics, "missing-release-set-outcome")
	})

	t.Run("channel move fixture retains published ref", func(t *testing.T) {
		outcome, diagnostics := RequireReleaseSetPublishOutcome(readReleaseSetFixture(t, "channel-move.jsonl"))
		if diag.HasErrors(diagnostics) {
			t.Fatalf("channel-move fixture produced errors: %v", diagnostics)
		}
		published, publishedDiagnostics := RequireReleaseSetPublishOutcome(readReleaseSetFixture(t, "success.jsonl"))
		if diag.HasErrors(publishedDiagnostics) || !bytes.Equal(outcome, published) {
			t.Fatalf("published outcome changed after channel move: before=%s after=%s diagnostics=%v", published, outcome, publishedDiagnostics)
		}
	})
}

func TestReleaseSetPublishOutcomeRejectsInvalidResultLocations(t *testing.T) {
	failed := decodeReleaseSetStream(t, strings.Replace(
		string(readReleaseSetFixtureBytes(t, "success.jsonl")), `"status":"OK"`, `"status":"FAILED"`, 1))
	_, diagnostics := ReleaseSetPublishOutcome(failed)
	assertRuntimeDiagnostic(t, diagnostics, "release-set-on-unsuccessful-result")

	wrongDataType := decodeReleaseSetStream(t, `{"v":1,"type":"result","data":{"status":"OK","data":[]}}`)
	_, diagnostics = ReleaseSetPublishOutcome(wrongDataType)
	assertRuntimeDiagnostic(t, diagnostics, "invalid-result-data")

	malformedResult := []*Event{{V: 1, Type: EventResult, Data: []byte(`{"status":`)}}
	_, diagnostics = ReleaseSetPublishOutcome(malformedResult)
	assertRuntimeDiagnostic(t, diagnostics, "invalid-result-data")
}

func TestReleaseSetPublishOutcomeKeepsDistributionPayloadOpaque(t *testing.T) {
	if outcome, diagnostics := ExtractReleaseSetPublishOutcome(nil); outcome != nil || len(diagnostics) != 0 {
		t.Fatalf("nil result data = %#v, %v", outcome, diagnostics)
	}
	if outcome, diagnostics := ExtractReleaseSetPublishOutcome(map[string]any{"other": true}); outcome != nil || len(diagnostics) != 0 {
		t.Fatalf("unrelated result data = %#v, %v", outcome, diagnostics)
	}
	if outcome, diagnostics := ExtractReleaseSetPublishOutcome(map[string]any{ReleaseSetResultDataKey: func() {}}); outcome != nil {
		t.Fatalf("unencodable outcome decoded: %#v", outcome)
	} else {
		assertRuntimeDiagnostic(t, diagnostics, "invalid-release-set-candidate")
	}

	data := map[string]any{ReleaseSetResultDataKey: map[string]any{
		"protocolVersion": 1,
		"namespace":       "putnami",
		"channel":         "canary",
		"ref": map[string]any{
			"id":     "rs_" + strings.Repeat("a", 64),
			"digest": "sha256:" + strings.Repeat("b", 64),
		},
	}}
	outcome, diagnostics := ExtractReleaseSetPublishOutcome(data)
	if diag.HasErrors(diagnostics) || len(outcome) == 0 || !json.Valid(outcome) {
		t.Fatalf("runtime interpreted Distribution-owned fields: outcome=%s diagnostics=%v", outcome, diagnostics)
	}
}

func TestReleaseSetSchemaReferencesDistributionAuthority(t *testing.T) {
	const authority = "https://putnami.dev/schemas/distribution-release-set-publish-outcome-v2.json"
	for _, name := range []string{"event.json", "event-v2.json", "payloads.json"} {
		data, err := os.ReadFile(filepath.Join("schemas", name))
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		var releaseSet map[string]any
		if name == "payloads.json" {
			releaseSet = schemaObject(t, schema, "$defs", "ReleaseSetPublishOutcome")
		} else {
			releaseSet = schemaObject(t, schema, "$defs", "ResultEvent", "properties", "data", "properties", "data", "properties", ReleaseSetResultDataKey)
		}
		if releaseSet["$ref"] != authority {
			t.Fatalf("%s releaseSet $ref = %#v, want %q", name, releaseSet["$ref"], authority)
		}
		if _, redeclared := releaseSet["properties"]; redeclared {
			t.Fatalf("%s redeclares Distribution release-set fields", name)
		}
	}
}

func schemaObject(t *testing.T, root map[string]any, path ...string) map[string]any {
	t.Helper()
	current := root
	for _, member := range path {
		value, ok := current[member]
		if !ok {
			t.Fatalf("schema path %s is missing %q", strings.Join(path, "."), member)
		}
		current, ok = value.(map[string]any)
		if !ok {
			t.Fatalf("schema path %s member %q is %T, want object", strings.Join(path, "."), member, value)
		}
	}
	return current
}

func readReleaseSetFixture(t *testing.T, name string) []*Event {
	t.Helper()
	return decodeReleaseSetStream(t, string(readReleaseSetFixtureBytes(t, name)))
}

func readReleaseSetFixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", "release-set", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeReleaseSetStream(t *testing.T, data string) []*Event {
	t.Helper()
	events, err := DecodeAll(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func assertRuntimeDiagnostic(t *testing.T, diagnostics []diag.Diagnostic, code string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return
		}
	}
	t.Fatalf("missing diagnostic %q in %#v", code, diagnostics)
}
