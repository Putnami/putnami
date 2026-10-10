package remotecache

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

// objectWireWitness is the canonical JSON of one object lookup request, one
// lookup response, one store request, and one store response.
//
// It is the PAIRING PIN for a contract that has two declarations in this
// repository: these client types and cache-server's internal/api/object types.
// The CLI extension must not link the server's storage library, so the shapes
// cannot be shared — the same JSON literal appears in the server package's
// TestObjectWireShape, and a change to either side that is not made to the other
// fails one of the two tests.
const (
	objectLookupRequestJSON  = `{"protocolVersion":1,"namespace":"go-build","ids":["aa","bb"]}`
	objectLookupResponseJSON = `{"protocolVersion":1,"objects":[{"id":"aa","digest":"sha256:` +
		`0000000000000000000000000000000000000000000000000000000000000000","sizeBytes":3,"meta":"out-1",` +
		`"producer":"ci","channel":"trusted","blob":{"digest":"sha256:` +
		`0000000000000000000000000000000000000000000000000000000000000000","data":"YWJj"}}]}`
	objectStoreRequestJSON = `{"protocolVersion":1,"namespace":"go-build","objects":[{"id":"aa","digest":"sha256:` +
		`0000000000000000000000000000000000000000000000000000000000000000","sizeBytes":3,"meta":"out-1"}]}`
	objectStoreResponseJSON = `{"protocolVersion":1,"results":[{"id":"aa","stored":true}]}`
)

const zeroDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// TestObjectWireShape pins the four object payloads byte for byte, in both
// directions: what this client MARSHALS and what it PARSES.
func TestObjectWireShape(t *testing.T) {
	lookupReq := &ObjectLookupRequest{ProtocolVersion: cache.ProtocolVersion, Namespace: "go-build", IDs: []string{"aa", "bb"}}
	assertJSON(t, "lookup request", lookupReq, objectLookupRequestJSON)

	lookupResp := &ObjectLookupResponse{
		ProtocolVersion: cache.ProtocolVersion,
		Objects: []ObjectRecord{{
			ID: "aa", Digest: zeroDigest, SizeBytes: 3, Meta: "out-1",
			Producer: "ci", Channel: "trusted",
			Blob: &cache.InlineBlob{Digest: zeroDigest, Data: []byte("abc")},
		}},
	}
	assertJSON(t, "lookup response", lookupResp, objectLookupResponseJSON)

	storeReq := &ObjectStoreRequest{
		ProtocolVersion: cache.ProtocolVersion,
		Namespace:       "go-build",
		Objects:         []ObjectOffer{{ID: "aa", Digest: zeroDigest, SizeBytes: 3, Meta: "out-1"}},
	}
	assertJSON(t, "store request", storeReq, objectStoreRequestJSON)

	storeResp := &ObjectStoreResponse{
		ProtocolVersion: cache.ProtocolVersion,
		Results:         []ObjectStoreResult{{ID: "aa", Stored: true}},
	}
	assertJSON(t, "store response", storeResp, objectStoreResponseJSON)

	// And both responses parse back from the canonical bytes.
	if _, err := parseStrictJSON[ObjectLookupResponse]([]byte(objectLookupResponseJSON), "lookup"); err != nil {
		t.Errorf("parse canonical lookup response: %v", err)
	}
	if _, err := parseStrictJSON[ObjectStoreResponse]([]byte(objectStoreResponseJSON), "store"); err != nil {
		t.Errorf("parse canonical store response: %v", err)
	}
}

func assertJSON(t *testing.T, what string, v any, want string) {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", what, err)
	}
	if string(got) != want {
		t.Errorf("%s JSON =\n  %s\nwant\n  %s", what, got, want)
	}
}

// TestObjectOfferCarriesNoProvenance is the trust rule at the type level: the
// wire has no field a caller could use to assert a channel, so the server's
// stamp is the only source of trust.
func TestObjectOfferCarriesNoProvenance(t *testing.T) {
	data, err := json.Marshal(ObjectOffer{ID: "aa", Digest: zeroDigest, SizeBytes: 1, Meta: "m"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"channel", "producer", "producerIdentity"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("ObjectOffer JSON %s carries %q", data, forbidden)
		}
	}
}

// TestParseStrictJSONRejectsUnknownFields proves the response parsers are
// strict: these routes are versioned by cache.ProtocolVersion, so a reshaped
// response is a version bump, never a silent change under a reader.
func TestParseStrictJSONRejectsUnknownFields(t *testing.T) {
	_, err := parseStrictJSON[ObjectStoreResponse]([]byte(`{"protocolVersion":1,"surprise":true}`), "store")
	if err == nil {
		t.Fatal("an unknown field must be rejected")
	}
}

// TestObjectWritePathReadsTheServerCapabilities proves the provider's off switch
// and its upload-mechanism selection both come from the discovery probe.
func TestObjectWritePathReadsTheServerCapabilities(t *testing.T) {
	if _, supported := ObjectWritePath(nil); supported {
		t.Error("a nil capabilities response must not enable the object cache")
	}
	if _, supported := ObjectWritePath(&cache.CapabilitiesResponse{
		Capabilities: []string{cache.CapabilityFindMissing},
	}); supported {
		t.Error("a server that does not advertise object-cache must not enable it")
	}
	wp, supported := ObjectWritePath(&cache.CapabilitiesResponse{
		Capabilities: []string{cache.CapabilityObjectCache},
	})
	if !supported {
		t.Error("object-cache must enable the feature")
	}
	if wp.CoalesceSmall {
		t.Error("upload-batch coalescing must stay off until the server advertises it")
	}
	if wp, _ = ObjectWritePath(&cache.CapabilitiesResponse{
		Capabilities: []string{cache.CapabilityObjectCache, cache.CapabilityUploadBatch},
	}); !wp.CoalesceSmall {
		t.Error("upload-batch must select inline coalescing for the object write path")
	}
}

// TestLookupObjectsRejectsAnUnrequestedID keeps a server from steering a caller
// into writing bytes under a key it never computed.
func TestLookupObjectsRejectsAnUnrequestedID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ObjectLookupResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Objects:         []ObjectRecord{{ID: "zz", Digest: zeroDigest, SizeBytes: 1}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	if _, err := c.LookupObjects(context.Background(), "go-build", []string{"aa"}); err == nil {
		t.Fatal("a record outside the requested id set must be rejected")
	}
}

// TestLookupObjectsRejectsADuplicateRecord proves one requested id yields at
// most one record.
func TestLookupObjectsRejectsADuplicateRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ObjectLookupResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Objects: []ObjectRecord{
				{ID: "aa", Digest: zeroDigest, SizeBytes: 1},
				{ID: "aa", Digest: zeroDigest, SizeBytes: 1},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	if _, err := c.LookupObjects(context.Background(), "go-build", []string{"aa"}); err == nil {
		t.Fatal("a duplicate record must be rejected")
	}
}

// TestLookupObjectsRejectsAMalformedDigest proves a record the caller could not
// address is refused rather than staged.
func TestLookupObjectsRejectsAMalformedDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ObjectLookupResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Objects:         []ObjectRecord{{ID: "aa", Digest: "not-a-digest", SizeBytes: 1}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	if _, err := c.LookupObjects(context.Background(), "go-build", []string{"aa"}); err == nil {
		t.Fatal("a malformed digest must be rejected")
	}
}

// TestObjectRoutesOn404AreAColdCache proves a cache server that predates the
// object routes reads as a cold cache rather than an error: the feature is an
// accelerator, and every failure mode of it must be a local build.
func TestObjectRoutesOn404AreAColdCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == cache.FindMissingPath {
			// The blob halves exist on any server that reaches here; only the
			// object index is missing.
			_ = json.NewEncoder(w).Encode(cache.FindMissingResponse{ProtocolVersion: cache.ProtocolVersion})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	records, err := c.LookupObjects(context.Background(), "go-build", []string{"aa"})
	if err != nil || len(records) != 0 {
		t.Fatalf("records=%+v err=%v want no hits and no error", records, err)
	}
	stored, err := c.StoreObjects(context.Background(), "go-build",
		[]ObjectOffer{{ID: "aa", Digest: zeroDigest, SizeBytes: 0}}, emptySource{}, WritePath{})
	if err != nil || len(stored) != 0 {
		t.Fatalf("stored=%+v err=%v want nothing stored and no error", stored, err)
	}
}

// TestLookupObjectsSkipsTheRoundTripForAnEmptyBatch proves a no-op lookup costs
// nothing.
func TestLookupObjectsSkipsTheRoundTripForAnEmptyBatch(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	if _, err := c.LookupObjects(context.Background(), "go-build", nil); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if calls != 0 {
		t.Errorf("calls=%d want 0 for an empty batch", calls)
	}
}

// emptySource yields empty blobs, enough for the control-plane assertions above.
type emptySource struct{}

func (emptySource) OpenBlob(string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}
