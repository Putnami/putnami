package runtime

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// decodeLine unmarshals a single emitted JSONL line into a generic object so a
// test can inspect the extra top-level fields emitWithExtra merges in — fields
// the typed Event struct does not model and DecodeAll would otherwise drop.
func decodeLine(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		t.Fatalf("decode emitted line: %v\nline: %s", err, line)
	}
	return obj
}

// TestEmitWithExtra_TypedFieldsWinAndExtrasSurvive covers the documented
// emitWithExtra invariant: a typed Event field wins over a colliding extra key,
// and non-colliding extras are preserved. Exercised through both ArtifactData
// and SummaryData, the two public entry points into emitWithExtra.
func TestEmitWithExtra_TypedFieldsWinAndExtrasSurvive(t *testing.T) {
	t.Run("ArtifactData", func(t *testing.T) {
		var buf bytes.Buffer
		em := NewEmitter(&buf)
		// "path" collides with the typed Event.Path; "registry" is a new key.
		if err := em.ArtifactData("bin", "app", "published", "dist/app", map[string]any{
			"path":     "SHOULD-NOT-WIN",
			"registry": "npm",
		}); err != nil {
			t.Fatal(err)
		}

		obj := decodeLine(t, bytes.TrimSpace(buf.Bytes()))
		if obj["path"] != "dist/app" {
			t.Errorf("typed path lost to extra: got %v, want %q", obj["path"], "dist/app")
		}
		if obj["registry"] != "npm" {
			t.Errorf("non-colliding extra dropped: registry = %v, want %q", obj["registry"], "npm")
		}
		if obj["kind"] != "published" {
			t.Errorf("kind = %v, want published", obj["kind"])
		}
	})

	t.Run("SummaryData", func(t *testing.T) {
		var buf bytes.Buffer
		em := NewEmitter(&buf)
		// "message" collides with the typed Event.Message; "counts" is new.
		if err := em.SummaryData("done", map[string]any{
			"message": "SHOULD-NOT-WIN",
			"counts":  map[string]any{"passed": float64(3)},
		}); err != nil {
			t.Fatal(err)
		}

		obj := decodeLine(t, bytes.TrimSpace(buf.Bytes()))
		if obj["message"] != "done" {
			t.Errorf("typed message lost to extra: got %v, want %q", obj["message"], "done")
		}
		if !reflect.DeepEqual(obj["counts"], map[string]any{"passed": float64(3)}) {
			t.Errorf("non-colliding extra dropped: counts = %v", obj["counts"])
		}
	})

	t.Run("EmptyExtraFallsBackToPlainEmit", func(t *testing.T) {
		var buf bytes.Buffer
		em := NewEmitter(&buf)
		if err := em.SummaryData("plain", nil); err != nil {
			t.Fatal(err)
		}
		events, err := DecodeAll(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Type != EventSummary || events[0].Message != "plain" {
			t.Fatalf("empty-extra path did not emit a plain summary: %+v", events)
		}
	})
}

// TestLogContext covers LogContext, which forwards structured context and
// optional error details from child processes.
func TestLogContext(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf)
	if err := em.LogContext(LevelError, "child failed",
		map[string]any{"pid": float64(42), "stage": "compile"},
		&ErrorInfo{Message: "boom", Stack: "at main"},
	); err != nil {
		t.Fatal(err)
	}

	events, err := DecodeAll(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Type != EventLog || e.Level != string(LevelError) || e.Message != "child failed" {
		t.Errorf("unexpected log event: %+v", e)
	}
	if e.Context == nil {
		t.Fatal("log context not carried")
	}
	if got := (*e.Context)["stage"]; got != "compile" {
		t.Errorf("context[stage] = %v, want compile", got)
	}
	if e.Error == nil || e.Error.Message != "boom" || e.Error.Stack != "at main" {
		t.Errorf("error info not carried: %+v", e.Error)
	}
}

// TestPublishRecordRoundTrip covers the PublishRecord wire shape: it is carried
// in the extra fields of a kind="published" artifact event and consumed by the
// CLI's published-artifacts renderer. This asserts the fields survive an
// emit → decode round trip.
func TestPublishRecordRoundTrip(t *testing.T) {
	rec := PublishRecord{
		Registry:     "docker",
		Name:         "oci.putnami.dev/team/thing",
		ImageDigest:  "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ImmutableRef: "oci.putnami.dev/team/thing@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Version:      "1.2.3",
		Tags:         []string{"latest", "next"},
		DryRun:       true,
	}

	// The publish record travels as the artifact event's extra fields.
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var extra map[string]any
	if err := json.Unmarshal(raw, &extra); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	em := NewEmitter(&buf)
	if err := em.ArtifactData("pub-1", rec.Name, ArtifactKindPublished, "", extra); err != nil {
		t.Fatal(err)
	}

	// Decode the raw line and reconstruct the PublishRecord from the top-level
	// fields, then assert it matches what was emitted.
	line := bytes.TrimSpace(buf.Bytes())
	var got PublishRecord
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("decode publish record: %v\nline: %s", err, line)
	}
	if !reflect.DeepEqual(got, rec) {
		t.Errorf("publish record round-trip mismatch\n got: %+v\nwant: %+v", got, rec)
	}

	// The artifact envelope fields must coexist with the record fields.
	obj := decodeLine(t, line)
	if obj["kind"] != ArtifactKindPublished || obj["id"] != "pub-1" {
		t.Errorf("artifact envelope fields lost: kind=%v id=%v", obj["kind"], obj["id"])
	}
}
