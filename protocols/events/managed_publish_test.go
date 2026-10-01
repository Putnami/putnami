package events

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestManagedPublishV1_ValidFixtures(t *testing.T) {
	fixtures := ManagedPublishV1Fixtures()
	entries, err := fs.ReadDir(fixtures, "valid")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("managed publish valid fixture corpus is empty")
	}

	required := map[string]bool{
		"minimal.json":         false,
		"full.json":            false,
		"retry-attempt-1.json": false,
		"retry-attempt-2.json": false,
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		required[entry.Name()] = true
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := fs.ReadFile(fixtures, "valid/"+entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			if _, diags := ParseAndValidateManagedPublishFrame(data); diag.HasErrors(diags) {
				t.Fatalf("valid fixture produced errors: %v", diags)
			}
		})
	}
	for name, found := range required {
		if !found {
			t.Errorf("managed publish corpus missing required valid fixture %q", name)
		}
	}
}

func TestManagedPublishV1_InvalidFixtures(t *testing.T) {
	fixtures := ManagedPublishV1Fixtures()
	entries, err := fs.ReadDir(fixtures, "invalid")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("managed publish invalid fixture corpus is empty")
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := fs.ReadFile(fixtures, "invalid/"+entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			if _, diags := ParseAndValidateManagedPublishFrame(data); !diag.HasErrors(diags) {
				t.Fatalf("invalid fixture should produce errors")
			}
		})
	}
}

func TestManagedPublishV1_InvalidFixtureCoverage(t *testing.T) {
	required := []string{
		"bad-protocol.json",
		"bad-type.json",
		"missing-protocol.json",
		"missing-type.json",
		"missing-id.json",
		"missing-dedupe-key.json",
		"missing-topic.json",
		"missing-topic-version.json",
		"missing-payload.json",
		"channel.json",
		"reserved-attribute-workspace-id.json",
		"reserved-attribute-environment.json",
		"reserved-attribute-workload.json",
		"reserved-attribute-service.json",
		"reserved-attribute-channel.json",
		"reserved-attribute-topology-generation-id.json",
		"reserved-attribute-traceparent.json",
		"reserved-attribute-tracestate.json",
		"reserved-attribute-auth-prefix.json",
		"reserved-attribute-putnami-prefix.json",
	}
	for _, name := range required {
		if _, err := ReadManagedPublishV1Fixture("invalid/" + name); err != nil {
			t.Errorf("managed publish corpus missing required invalid fixture %q: %v", name, err)
		}
	}
}

func TestManagedPublishV1_ByteStableRetry(t *testing.T) {
	first, err := ReadManagedPublishV1Fixture("valid/retry-attempt-1.json")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := ReadManagedPublishV1Fixture("valid/retry-attempt-2.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, retry) {
		t.Fatal("retry fixtures must be byte-for-byte identical")
	}
	if diags := ValidateManagedPublishRetry(first, retry); diag.HasErrors(diags) {
		t.Fatalf("byte-stable retry produced errors: %v", diags)
	}

	mutated := bytes.Replace(retry, []byte(`"revision": 7`), []byte(`"revision": 8`), 1)
	if diags := ValidateManagedPublishRetry(first, mutated); !hasEventsCode(diags, "retry-request-mutated") {
		t.Fatalf("mutated retry should trigger retry-request-mutated, got %v", diags)
	}
}

func TestManagedPublishV1_OutcomeFixtures(t *testing.T) {
	fixtures := ManagedPublishV1Fixtures()
	entries, err := fs.ReadDir(fixtures, "outcomes")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("managed publish outcome fixture corpus is empty")
	}

	required := map[string]bool{
		"accepted-200.json":               false,
		"permanent-400.json":              false,
		"permanent-401.json":              false,
		"permanent-403.json":              false,
		"retryable-429.json":              false,
		"retryable-503.json":              false,
		"ambiguous-timeout.json":          false,
		"ambiguous-unstructured-502.json": false,
	}
	classes := map[ManagedPublishOutcomeClass]int{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		required[entry.Name()] = true
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := fs.ReadFile(fixtures, "outcomes/"+entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			fixture, diags := ParseAndValidateManagedPublishOutcomeFixture(data)
			if diag.HasErrors(diags) {
				t.Fatalf("outcome fixture produced errors: %v", diags)
			}
			classes[fixture.Expected.Class]++
		})
	}
	for name, found := range required {
		if !found {
			t.Errorf("managed publish corpus missing required outcome fixture %q", name)
		}
	}
	for _, class := range []ManagedPublishOutcomeClass{
		ManagedPublishAccepted,
		ManagedPublishPermanent,
		ManagedPublishRetryable,
		ManagedPublishAmbiguous,
	} {
		if classes[class] == 0 {
			t.Errorf("managed publish corpus has no %q outcome", class)
		}
	}
}

func TestManagedPublishV1_RejectsKnownOutcomesClassifiedAsAmbiguous(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "accepted",
			status: 200,
			body:   `{"protocol":"putnami.events.v1","id":"evt-outcome-1","topic":"orders.created","timestamp":"2026-07-22T08:00:00Z"}`,
		},
		{
			name:   "permanent",
			status: 400,
			body:   `{"protocol":"putnami.events.v1","code":"invalid_frame","message":"invalid","retryable":false}`,
		},
		{
			name:   "retryable",
			status: 503,
			body:   `{"protocol":"putnami.events.v1","code":"upstream_unavailable","message":"unavailable","retryable":true}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := &ManagedPublishOutcomeFixture{
				Name:    test.name,
				Request: ManagedPublishRequestRef{ID: "evt-outcome-1", Topic: "orders.created"},
				Input:   ManagedPublishOutcomeInput{HTTP: &ManagedPublishHTTPResult{Status: test.status, Body: json.RawMessage(test.body)}},
				Expected: ManagedPublishOutcomeExpectation{
					Class:            ManagedPublishAmbiguous,
					Action:           ManagedPublishRetry,
					SameRoute:        true,
					SameRequestBytes: true,
				},
			}
			if diags := ValidateManagedPublishOutcomeFixture(fixture); !hasEventsCode(diags, "invalid-outcome-class") {
				t.Fatalf("known outcome classified as ambiguous should fail, got %v", diags)
			}
		})
	}
}

func TestManagedPublishV1_SchemasAreValidJSON(t *testing.T) {
	for _, name := range []string{"managed-publish-frame-v1.json", "managed-publish-outcome-v1.json"} {
		data, err := os.ReadFile(filepath.Join("schemas", name))
		if err != nil {
			t.Fatal(err)
		}
		var schema any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Errorf("schema %s is not valid JSON: %v", name, err)
		}
	}
}
