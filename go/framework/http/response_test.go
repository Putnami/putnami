package http

import (
	"net/http/httptest"
	"testing"
)

func TestJSONBytes(t *testing.T) {
	payload := []byte(`{"hello":"world"}`)
	resp := JSONBytes(payload)

	if resp.Status != 200 {
		t.Errorf("status = %d, want 200", resp.Status)
	}
	if got := resp.Headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	// BodyBytes returns the pre-encoded bytes verbatim (no re-encoding).
	body, err := resp.BodyBytes()
	if err != nil {
		t.Fatalf("BodyBytes: %v", err)
	}
	if string(body) != string(payload) {
		t.Errorf("BodyBytes = %q, want %q", body, payload)
	}

	// WriteTo streams the exact precomputed bytes with the JSON content type.
	rec := httptest.NewRecorder()
	if err := resp.WriteTo(rec); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if rec.Body.String() != string(payload) {
		t.Errorf("written body = %q, want %q", rec.Body.String(), payload)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("written Content-Type = %q, want application/json", ct)
	}
}
