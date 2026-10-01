package store

import (
	"errors"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
)

func sample() *Document {
	return &Document{
		Format: FormatVersion, ID: "m-1", Kind: collab.MemoryKindMission, Sequence: 3,
		Identity: collab.MemoryIdentity{Workspace: "w", Mission: "m"}, Content: "<state> & more",
		Provenance: collab.Provenance{RecordedAt: "2026-09-24T08:00:00Z"}, UpdatedAt: "2026-09-24T08:00:00Z",
		Write: &Write{Key: "sha256:k", Request: "sha256:r"},
	}
}

func mustEncode(t *testing.T, doc *Document) []byte {
	t.Helper()
	encoded, err := Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestARevisionIsDerivedFromTheWholeDocument(t *testing.T) {
	doc := sample()
	encoded := mustEncode(t, doc)
	if !strings.Contains(string(encoded), "<state> & more") || !strings.HasSuffix(string(encoded), "}\n") {
		t.Fatalf("encoding %s", encoded)
	}
	decoded, err := Decode("m-1", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Revision() != doc.Revision() || !strings.HasPrefix(doc.Revision(), "3.") {
		t.Fatalf("revision %s after a round trip, %s before", decoded.Revision(), doc.Revision())
	}
	changed := sample()
	changed.Write.Key = "sha256:other"
	if changed.Revision() == doc.Revision() {
		t.Fatal("two documents that differ share a revision")
	}
}

func TestDecodeRefusesWhatItCannotRead(t *testing.T) {
	good := string(mustEncode(t, sample()))
	for name, data := range map[string]string{
		"another id":      good,
		"another format":  strings.Replace(good, `"format": 1`, `"format": 2`, 1),
		"an unknown key":  strings.Replace(good, `"format": 1`, `"format": 1, "verdict": "passed"`, 1),
		"two values":      good + good,
		"not a document":  "[]",
		"a negative seq":  strings.Replace(good, `"sequence": 3`, `"sequence": -1`, 1),
		"an oversize one": strings.Repeat(" ", MaxDocumentBytes+1),
	} {
		id := "m-1"
		if name == "another id" {
			id = "m-2"
		}
		if _, err := Decode(id, []byte(data)); err == nil {
			t.Errorf("%s decoded", name)
		}
	}
}

// TestEncodeRefusesWhatDecodeWouldRefuse: the bound is checked where a
// record is written, not only where it is read.
func TestEncodeRefusesWhatDecodeWouldRefuse(t *testing.T) {
	doc := sample()
	doc.Content = strings.Repeat("x", MaxDocumentBytes)
	_, err := Encode(doc)
	var failure *Error
	if !errors.As(err, &failure) || failure.Outcome != Invalid || failure.Reason != "record.too_large" || failure.Retryable ||
		!strings.Contains(failure.Error(), "m-1") {
		t.Fatalf("a document above the bound encoded: %v", err)
	}
	doc.Content = strings.Repeat("x", MaxDocumentBytes/2)
	encoded := mustEncode(t, doc)
	if _, err := Decode("m-1", encoded); err != nil {
		t.Fatalf("a document Encode accepted does not decode: %v", err)
	}
}

func TestTheStoreIdentity(t *testing.T) {
	meta, err := NewMeta()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeMeta(EncodeMeta(meta))
	if err != nil || decoded != meta {
		t.Fatalf("identity %+v → %+v (%v)", meta, decoded, err)
	}
	for _, data := range []string{`{"format":2,"id":"0123456789abcdef"}`, `{"format":1,"id":"short"}`, `{"format":1}`, `x`} {
		if _, err := DecodeMeta([]byte(data)); err == nil {
			t.Errorf("%s decoded", data)
		}
	}
}

func TestRecordNames(t *testing.T) {
	for name, want := range map[string]string{"m-0a.json": "m-0a", "release-policy.json": "release-policy"} {
		if id, ok := IDFromFileName(name); !ok || id != want || RecordFileName(id) != name || RecordPath(id) != "records/"+name {
			t.Errorf("%s: %q %v", name, id, ok)
		}
	}
	for _, name := range []string{".a.json.1.tmp", "README.md", "Upper.json", ".json", "a b.json"} {
		if _, ok := IDFromFileName(name); ok {
			t.Errorf("%s is a record name", name)
		}
	}
	failure := Failf(Unavailable, "store.busy", true, "busy %d", 1)
	if failure.Error() != "busy 1" || failure.Unwrap() == nil {
		t.Fatalf("failure %v", failure)
	}
}
