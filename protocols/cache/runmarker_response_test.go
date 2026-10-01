package cache

import (
	"encoding/json"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// The run-marker RESPONSE strict parsers/validators are wire-contract parsers
// for live CLI-consumed routes (RunMarkerLookupPath / RunMarkerPublishPath).
// Only the request side was covered; these tests exercise the response side.

func TestRunMarkerResponse_RoundTrip(t *testing.T) {
	// A hit carries a marker.
	hit := RunMarkerResponse{ProtocolVersion: ProtocolVersion, Marker: &RunMarker{SHA: "abc123", UpdatedAt: "2026-07-03T00:00:00Z"}}
	data, err := json.Marshal(hit)
	if err != nil {
		t.Fatal(err)
	}
	got, diags := ParseAndValidateRunMarkerResponse(data)
	if diag.HasErrors(diags) {
		t.Fatalf("valid hit response produced diagnostics: %v", diags)
	}
	if got.Marker == nil || got.Marker.SHA != "abc123" {
		t.Fatalf("marker round-trip mismatch: %+v", got.Marker)
	}

	// A nil marker is a cache miss, not an error.
	miss, diags := ParseAndValidateRunMarkerResponse([]byte(`{"protocolVersion":1}`))
	if diag.HasErrors(diags) {
		t.Fatalf("miss response produced diagnostics: %v", diags)
	}
	if miss.Marker != nil {
		t.Fatalf("expected nil marker on miss, got %+v", miss.Marker)
	}
}

func TestRunMarkerResponse_VersionMismatch(t *testing.T) {
	_, diags := ParseAndValidateRunMarkerResponse([]byte(`{"protocolVersion":2,"marker":{"sha":"abc"}}`))
	if !codeSet(diags)[CodeVersionMismatch] {
		t.Fatalf("expected version-mismatch diagnostic, got %v", diags)
	}
}

func TestRunMarkerResponse_MarkerSHARequired(t *testing.T) {
	if diags := ValidateRunMarkerResponse(&RunMarkerResponse{ProtocolVersion: ProtocolVersion, Marker: &RunMarker{}}); !codeSet(diags)[CodeRequiredField] {
		t.Fatalf("expected required-field diagnostic for empty marker sha, got %v", diags)
	}
}

func TestRunMarkerResponse_UnknownFieldRejected(t *testing.T) {
	if _, diags := ParseRunMarkerResponseStrict([]byte(`{"protocolVersion":1,"nope":true}`)); !codeSet(diags)[CodeParseError] {
		t.Fatalf("expected parse-error on unknown field, got %v", diags)
	}
}

func TestPublishRunMarkerResponse_RoundTrip(t *testing.T) {
	resp := PublishRunMarkerResponse{ProtocolVersion: ProtocolVersion, Published: true, Marker: &RunMarker{SHA: "def456"}}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	got, diags := ParseAndValidatePublishRunMarkerResponse(data)
	if diag.HasErrors(diags) {
		t.Fatalf("valid publish response produced diagnostics: %v", diags)
	}
	if !got.Published || got.Marker == nil || got.Marker.SHA != "def456" {
		t.Fatalf("publish response round-trip mismatch: %+v", got)
	}

	// published=false with no marker (benign race) is still valid.
	notPub, diags := ParseAndValidatePublishRunMarkerResponse([]byte(`{"protocolVersion":1,"published":false}`))
	if diag.HasErrors(diags) {
		t.Fatalf("published=false response produced diagnostics: %v", diags)
	}
	if notPub.Published {
		t.Fatalf("expected published=false")
	}
}

func TestPublishRunMarkerResponse_VersionMismatch(t *testing.T) {
	_, diags := ParseAndValidatePublishRunMarkerResponse([]byte(`{"protocolVersion":99,"published":true}`))
	if !codeSet(diags)[CodeVersionMismatch] {
		t.Fatalf("expected version-mismatch diagnostic, got %v", diags)
	}
}

func TestPublishRunMarkerResponse_MarkerSHARequired(t *testing.T) {
	if diags := ValidatePublishRunMarkerResponse(&PublishRunMarkerResponse{ProtocolVersion: ProtocolVersion, Published: true, Marker: &RunMarker{}}); !codeSet(diags)[CodeRequiredField] {
		t.Fatalf("expected required-field diagnostic for empty marker sha, got %v", diags)
	}
}
