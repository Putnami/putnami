package cache

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func validTestKey() string    { return strings.Repeat("a", KeyLength) }
func validTestDigest() string { return DigestAlgorithm + digestSep + strings.Repeat("b", KeyLength) }

// TestProviderOp_Valid pins the recognized op set.
func TestProviderOp_Valid(t *testing.T) {
	for _, op := range []ProviderOp{
		OpInitialize, OpAuthenticate, OpPrefetch, OpRestore, OpUpload,
		OpMarkerLookup, OpMarkerWrite, OpObjectGet, OpObjectPut,
		OpSummary, OpShutdown,
	} {
		if !op.Valid() {
			t.Errorf("op %q should be valid", op)
		}
	}
	for _, op := range []ProviderOp{"", "frobnicate", "Restore"} {
		if op.Valid() {
			t.Errorf("op %q should be invalid", op)
		}
	}
}

// TestProviderOp_ValidOnObjectCacheSocket pins the socket's op set. The socket
// is reachable by any job subprocess, so admitting a session op there would let
// a task shut the provider down, publish a run marker, or restore a task entry
// it never declared.
func TestProviderOp_ValidOnObjectCacheSocket(t *testing.T) {
	for _, op := range []ProviderOp{OpObjectGet, OpObjectPut} {
		if !op.ValidOnObjectCacheSocket() {
			t.Errorf("op %q should be valid on the object-cache socket", op)
		}
	}
	for _, op := range []ProviderOp{
		OpInitialize, OpAuthenticate, OpPrefetch, OpRestore, OpUpload,
		OpMarkerLookup, OpMarkerWrite, OpSummary, OpShutdown, "frobnicate",
	} {
		if op.ValidOnObjectCacheSocket() {
			t.Errorf("op %q must not be valid on the object-cache socket", op)
		}
	}
}

// TestObjectCacheContract pins the negotiation tokens and the two job-facing
// environment variable names. All four are read by a second implementation (the
// provider extension and the language extension that calls the socket), so a
// rename here is a wire break, not a refactor.
func TestObjectCacheContract(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{CapabilityObjectCache, "object-cache"},
		{string(OpObjectGet), "object-get"},
		{string(OpObjectPut), "object-put"},
		{ObjectCacheSocketEnv, "PUTNAMI_CACHE_OBJECT_SOCKET"},
		{CacheTrustEnv, "PUTNAMI_CACHE_TRUST"},
	} {
		if tc.got != tc.want {
			t.Errorf("wire token = %q, want %q", tc.got, tc.want)
		}
	}
}

// TestRunCredentialContract pins the run-credential tokens a provider in
// another repository reads, and the bound it shares with protocol/registry.
func TestRunCredentialContract(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{CapabilityRunCredential, "run-credential"},
		{string(OpAuthenticate), "authenticate"},
		{CodeInvalidCredential, "invalid-credential"},
	} {
		if tc.got != tc.want {
			t.Errorf("wire token = %q, want %q", tc.got, tc.want)
		}
	}
	if MaxRunCredentialBytes != 16384 {
		t.Errorf("MaxRunCredentialBytes = %d, want 16384", MaxRunCredentialBytes)
	}
	for value, want := range map[string]bool{
		"prc_x":      true,
		"prc_\u00e9": true,
		strings.Repeat("a", MaxRunCredentialBytes): true,
		"":           false,
		"prc x":      false,
		"prc\u00a0x": false,
		"prc\u3000x": false,
		"prc_\xff":   false,
		strings.Repeat("a", MaxRunCredentialBytes+1):        false,
		strings.Repeat("\u00e9", MaxRunCredentialBytes/2+1): false,
	} {
		if got := ValidRunCredential(value); got != want {
			t.Errorf("ValidRunCredential(%.24q) = %v, want %v", value, got, want)
		}
	}
}

// TestAuthenticateNeverFormatsTheCredential keeps the run credential out of
// every formatted value and diagnostic, while the wire still carries it.
func TestAuthenticateNeverFormatsTheCredential(t *testing.T) {
	const secret = "prc_secret_run_credential"
	params := AuthenticateParams{Credential: secret}
	payload, err := MarshalPayload(params)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"credential":"`+secret+`"}` {
		t.Fatalf("the wire encoding lost the credential: %s", payload)
	}
	request := ProviderRequest{ProtocolVersion: 2, ID: 2, Op: OpAuthenticate, Payload: payload}
	for _, value := range []any{params, &params, request, &request} {
		for _, format := range []string{"%v", "%+v", "%s", "%#v"} {
			if out := fmt.Sprintf(format, value); strings.Contains(out, secret) {
				t.Errorf("%s of %T printed the credential: %s", format, value, out)
			}
		}
	}
	if out := fmt.Sprint(params); out != "{credential:<redacted>}" {
		t.Errorf("formatted params = %s", out)
	}
	_, diags := ParseAndValidateAuthenticateParams([]byte(`{"credential":"prc secret"}`))
	if !diag.HasErrors(diags) || strings.Contains(fmt.Sprint(diags), "prc secret") {
		t.Errorf("a malformed credential: %v", diags)
	}
}

// TestObjectCacheIsAdditiveToV2 pins the compatibility claim the design rests
// on: the object cache ships WITHOUT a provider protocol bump, so a v1/v2
// provider that ignores the capability keeps working unchanged.
func TestObjectCacheIsAdditiveToV2(t *testing.T) {
	if ProviderProtocolVersion != 2 {
		t.Fatalf("ProviderProtocolVersion = %d, want 2 — the object cache is additive to v2", ProviderProtocolVersion)
	}
	// A v1 initialize result — the shape a channel-less provider answers with —
	// stays valid, and carries no socket.
	legacy := []byte(`{"protocolVersion":1,"providerVersion":"1.0.0","ready":true}`)
	got, diags := ParseAndValidateInitializeResult(legacy)
	if got == nil || diag.HasErrors(diags) {
		t.Fatalf("a legacy initialize result must stay valid: %+v / %v", got, diags)
	}
	if got.ObjectCacheSocket != "" {
		t.Fatalf("ObjectCacheSocket = %q, want empty for a provider that never advertised the capability", got.ObjectCacheSocket)
	}
}

// TestObjectGetParams_RoundTrip exercises the socket's request path: marshal
// typed params, wrap in the SAME envelope stdin/stdout uses, strict-parse, and
// decode the payload back.
func TestObjectGetParams_RoundTrip(t *testing.T) {
	params := &ObjectGetParams{
		Namespace:      "go-build",
		IDs:            []string{validTestKey(), strings.Repeat("c", 32)},
		AcceptChannels: []Channel{ChannelTrusted},
	}
	raw, err := MarshalPayload(params)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := &ProviderRequest{ProtocolVersion: ProviderProtocolVersion, ID: 3, Op: OpObjectGet, Payload: raw}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	got, diags := ParseAndValidateProviderRequest(b)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if got.Op != OpObjectGet || got.ID != 3 {
		t.Fatalf("envelope mismatch: op=%q id=%d", got.Op, got.ID)
	}
	p, pdiags := ParseAndValidateObjectGetParams(got.Payload)
	if diag.HasErrors(pdiags) {
		t.Fatalf("unexpected payload errors: %v", pdiags)
	}
	if p.Namespace != "go-build" || len(p.IDs) != 2 {
		t.Fatalf("payload mismatch: %+v", p)
	}
}

// TestObjectGetParams_Validation covers the closed alphabets a provider relies
// on to use a namespace and an id as storage path segments, and the channel
// filter's closed enum.
func TestObjectGetParams_Validation(t *testing.T) {
	id := validTestKey()
	for _, tc := range []struct {
		name string
		data []byte
		bad  bool
	}{
		{name: "empty ids", data: []byte(`{"namespace":"go-build","ids":[]}`)},
		{name: "no channel filter accepts everything", data: []byte(`{"namespace":"go-build","ids":["` + id + `"]}`)},
		{name: "trusted filter", data: []byte(`{"namespace":"go-build","ids":["` + id + `"],"acceptChannels":["trusted","hint"]}`)},
		{name: "uppercase namespace", data: []byte(`{"namespace":"Go-Build","ids":[]}`), bad: true},
		{name: "namespace with a separator", data: []byte(`{"namespace":"go/build","ids":[]}`), bad: true},
		{name: "empty namespace", data: []byte(`{"namespace":"","ids":[]}`), bad: true},
		{name: "uppercase id", data: []byte(`{"namespace":"go-build","ids":["ABCD"]}`), bad: true},
		{name: "non-hex id", data: []byte(`{"namespace":"go-build","ids":["../escape"]}`), bad: true},
		{name: "empty id", data: []byte(`{"namespace":"go-build","ids":[""]}`), bad: true},
		{name: "unknown channel", data: []byte(`{"namespace":"go-build","ids":[],"acceptChannels":["anything"]}`), bad: true},
		{name: "unknown field", data: []byte(`{"namespace":"go-build","ids":[],"priority":2}`), bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, diags := ParseAndValidateObjectGetParams(tc.data)
			if tc.bad {
				if got != nil || !diag.HasErrors(diags) {
					t.Fatalf("expected a validation error, got %+v / %v", got, diags)
				}
				return
			}
			if got == nil || diag.HasErrors(diags) {
				t.Fatalf("expected acceptance, got %+v / %v", got, diags)
			}
		})
	}
}

// TestObjectGetResult_Validation pins what a caller may trust about a hit
// before it reads bytes off the filesystem: a well-formed digest it can resolve
// to a blob-exchange path, a non-negative size, and closed provenance enums. A
// channel-less hit stays valid — it is the legacy signal, treated as an
// untrusted hint by an authoritative policy.
func TestObjectGetResult_Validation(t *testing.T) {
	digest := validTestDigest()
	id := validTestKey()
	valid := []byte(`{"objects":[{"id":"` + id + `","digest":"` + digest + `","size":12,"meta":"out","producer":"ci","channel":"trusted"}]}`)
	got, diags := ParseAndValidateObjectGetResult(valid)
	if got == nil || diag.HasErrors(diags) {
		t.Fatalf("a complete hit should be accepted: %+v / %v", got, diags)
	}
	if got.Objects[0].Meta != "out" {
		t.Fatalf("opaque meta was not preserved: %+v", got.Objects[0])
	}

	legacy := []byte(`{"objects":[{"id":"` + id + `","digest":"` + digest + `","size":12}]}`)
	if got, diags := ParseAndValidateObjectGetResult(legacy); got == nil || diag.HasErrors(diags) {
		t.Fatalf("a channel-less hit should be accepted: %+v / %v", got, diags)
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"bad digest", []byte(`{"objects":[{"id":"` + id + `","digest":"nope","size":1}]}`)},
		{"bad id", []byte(`{"objects":[{"id":"zz","digest":"` + digest + `","size":1}]}`)},
		{"negative size", []byte(`{"objects":[{"id":"` + id + `","digest":"` + digest + `","size":-1}]}`)},
		{"bad channel", []byte(`{"objects":[{"id":"` + id + `","digest":"` + digest + `","size":1,"channel":"unsafe"}]}`)},
		{"bad producer", []byte(`{"objects":[{"id":"` + id + `","digest":"` + digest + `","size":1,"producer":"robot"}]}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, diags := ParseAndValidateObjectGetResult(tc.data); got != nil || !diag.HasErrors(diags) {
				t.Fatalf("expected a validation error, got %+v / %v", got, diags)
			}
		})
	}
}

// TestObjectPut_RoundTrip covers the write path, including the fire-and-forget
// acknowledgement shape.
func TestObjectPut_RoundTrip(t *testing.T) {
	params := &ObjectPutParams{
		Namespace: "go-build",
		Objects: []ObjectPut{
			{ID: strings.Repeat("b", 40), Digest: validTestDigest(), Size: 17, Meta: "output-id"},
			{ID: strings.Repeat("a", 40), Digest: validTestDigest(), Size: 3},
		},
	}
	raw, err := MarshalPayload(params)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	got, diags := ParseAndValidateObjectPutParams(raw)
	if got == nil || diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %+v / %v", got, diags)
	}
	// Normalization is what makes one batch's canonical form independent of the
	// order the caller happened to queue its objects in.
	if got.Objects[0].ID != strings.Repeat("a", 40) {
		t.Fatalf("objects not sorted by id: %+v", got.Objects)
	}

	result, rdiags := ParseAndValidateObjectPutResult([]byte(`{"accepted":2}`))
	if result == nil || diag.HasErrors(rdiags) || result.Accepted != 2 {
		t.Fatalf("put result = %+v / %v", result, rdiags)
	}
	if _, rdiags := ParseAndValidateObjectPutResult([]byte(`{"accepted":-1}`)); !diag.HasErrors(rdiags) {
		t.Fatal("a negative accepted count should be rejected")
	}
}

// TestObjectPutParams_CarriesNoTrustAssertion pins the security shape of the
// write path: an ObjectPut has no channel or producer field at all, so a job
// process cannot even claim one. The provider derives trust from its own
// credential, exactly as it overwrites UploadParams provenance.
func TestObjectPutParams_CarriesNoTrustAssertion(t *testing.T) {
	claimed := []byte(`{"namespace":"go-build","objects":[{"id":"` + validTestKey() +
		`","digest":"` + validTestDigest() + `","size":1,"channel":"trusted"}]}`)
	if got, diags := ParseAndValidateObjectPutParams(claimed); got != nil || !diag.HasErrors(diags) {
		t.Fatalf("a caller-asserted channel must be rejected as an unknown field, got %+v / %v", got, diags)
	}
}

// TestNormalizeObjectGetParams_Deterministic verifies normalization makes one
// lookup's canonical form stable whatever order the caller batched it in.
func TestNormalizeObjectGetParams_Deterministic(t *testing.T) {
	p := &ObjectGetParams{
		Namespace:      " go-build ",
		IDs:            []string{"bb", "aa"},
		AcceptChannels: []Channel{ChannelTrusted, ChannelHint},
	}
	NormalizeObjectGetParams(p)
	if p.Namespace != "go-build" {
		t.Errorf("namespace not trimmed: %q", p.Namespace)
	}
	if p.IDs[0] != "aa" {
		t.Errorf("ids not sorted: %v", p.IDs)
	}
	if p.AcceptChannels[0] != ChannelHint {
		t.Errorf("channels not sorted: %v", p.AcceptChannels)
	}
}

// TestRestoreStatus_Valid pins the restore status set, including the distinct
// error (restore-failure) signal.
func TestRestoreStatus_Valid(t *testing.T) {
	for _, s := range []RestoreStatus{RestoreHit, RestoreMiss, RestoreError} {
		if !s.Valid() {
			t.Errorf("status %q should be valid", s)
		}
	}
	if RestoreStatus("partial").Valid() {
		t.Error(`status "partial" should be invalid`)
	}
}

// TestProviderRequest_RoundTrip exercises the full envelope path the harness
// uses: marshal a typed payload, wrap in a request, encode, strict-parse, then
// decode the payload back.
func TestProviderRequest_RoundTrip(t *testing.T) {
	params := &RestoreParams{Key: validTestKey()}
	raw, err := MarshalPayload(params)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := &ProviderRequest{ProtocolVersion: ProviderProtocolVersion, ID: 42, Op: OpRestore, Payload: raw}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	got, diags := ParseAndValidateProviderRequest(b)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if got.Op != OpRestore || got.ID != 42 {
		t.Fatalf("envelope mismatch: op=%q id=%d", got.Op, got.ID)
	}

	p, pdiags := ParseAndValidateRestoreParams(got.Payload)
	if diag.HasErrors(pdiags) {
		t.Fatalf("unexpected payload errors: %v", pdiags)
	}
	if p.Key != params.Key {
		t.Fatalf("payload key mismatch: got %q want %q", p.Key, params.Key)
	}
}

// TestProviderResponse_RoundTrip round-trips a hit response carrying a restore
// result payload.
func TestProviderResponse_RoundTrip(t *testing.T) {
	result := &RestoreResult{
		Status:   RestoreHit,
		Result:   &ActionResult{Status: "success"},
		Manifest: &Manifest{Files: []FileEntry{{Path: "dist/a.js", Digest: validTestDigest(), Mode: 0o644, Size: 12}}},
	}
	raw, err := MarshalPayload(result)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	resp := &ProviderResponse{ProtocolVersion: ProviderProtocolVersion, ID: 7, OK: true, Payload: raw}
	b, _ := json.Marshal(resp)

	got, diags := ParseAndValidateProviderResponse(b)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if !got.OK || got.ID != 7 {
		t.Fatalf("envelope mismatch: ok=%v id=%d", got.OK, got.ID)
	}
	rr, rdiags := ParseAndValidateRestoreResult(got.Payload)
	if diag.HasErrors(rdiags) {
		t.Fatalf("unexpected payload errors: %v", rdiags)
	}
	if rr.Status != RestoreHit || rr.Manifest == nil || len(rr.Manifest.Files) != 1 {
		t.Fatalf("restore result mismatch: %+v", rr)
	}
}

// TestProviderRequest_VersionMismatch verifies a mismatched provider protocol
// version is rejected against ProviderProtocolVersion, not the HTTP
// ProtocolVersion.
func TestProviderRequest_VersionMismatch(t *testing.T) {
	b := []byte(`{"protocolVersion": 999, "id": 1, "op": "restore"}`)
	got, diags := ParseAndValidateProviderRequest(b)
	if got != nil || !diag.HasErrors(diags) {
		t.Fatalf("expected version-mismatch error, got %+v / %v", got, diags)
	}
}

// TestProviderProtocolVersion pins the v2 provenance migration and its v1
// compatibility floor so a future bump requires an explicit migration story.
func TestProviderProtocolVersion(t *testing.T) {
	if ProviderProtocolVersion != 2 {
		t.Fatalf("ProviderProtocolVersion = %d, want 2 — bumping requires a migration story", ProviderProtocolVersion)
	}
	if ProviderProtocolMinVersion != 1 {
		t.Fatalf("ProviderProtocolMinVersion = %d, want 1 — v1 providers must remain tolerated", ProviderProtocolMinVersion)
	}
	if ProviderProtocolProvenanceVersion != 2 {
		t.Fatalf("ProviderProtocolProvenanceVersion = %d, want 2", ProviderProtocolProvenanceVersion)
	}
	if CapabilityProviderProtocolV2 != "provider-protocol-v2" {
		t.Fatalf("CapabilityProviderProtocolV2 = %q, want stable v1-bootstrap wire token", CapabilityProviderProtocolV2)
	}
}

// TestProviderProtocolVersion_ToleratesLegacy pins the v1/v2 negotiation
// window: v1 envelopes remain valid for channel-less providers while v2 carries
// the provenance extension. Versions outside that window are rejected.
func TestProviderProtocolVersion_ToleratesLegacy(t *testing.T) {
	for _, version := range []int{ProviderProtocolMinVersion, ProviderProtocolVersion} {
		data := []byte(`{"protocolVersion": ` + strconv.Itoa(version) + `, "id": 1, "op": "restore"}`)
		got, diags := ParseAndValidateProviderRequest(data)
		if got == nil || diag.HasErrors(diags) {
			t.Errorf("protocol version %d should be accepted: got %+v / %v", version, got, diags)
		}
	}

	for _, version := range []int{ProviderProtocolMinVersion - 1, ProviderProtocolVersion + 1} {
		data := []byte(`{"protocolVersion": ` + strconv.Itoa(version) + `, "id": 1, "op": "restore"}`)
		got, diags := ParseAndValidateProviderRequest(data)
		if got != nil || !diag.HasErrors(diags) {
			t.Errorf("protocol version %d should be rejected: got %+v / %v", version, got, diags)
		}
	}

	if ProviderProtocolHasProvenance(ProviderProtocolMinVersion) {
		t.Errorf("v%d should not carry provenance", ProviderProtocolMinVersion)
	}
	if !ProviderProtocolHasProvenance(ProviderProtocolVersion) {
		t.Errorf("v%d should carry provenance", ProviderProtocolVersion)
	}
}

// TestEntryProvenance_Validation covers both the compatibility case (all fields
// omitted by a v1/channel-less provider) and the complete, closed v2 triple.
func TestEntryProvenance_Validation(t *testing.T) {
	key := validTestKey()
	digest := validTestDigest()

	legacyRestore := []byte(`{"status":"hit","result":{"status":"success"},"manifest":{"files":[{"path":"dist/a.js","digest":"` + digest + `","mode":420,"size":12}]}}`)
	if got, diags := ParseAndValidateRestoreResult(legacyRestore); got == nil || diag.HasErrors(diags) {
		t.Fatalf("legacy channel-less restore should be accepted: got %+v / %v", got, diags)
	}

	provenancedUpload := []byte(`{"key":"` + key + `","result":{"status":"success"},"manifest":{"files":[]},"producer":"ci","producerIdentity":"oidc:repo:putnami/putnami","channel":"trusted"}`)
	if got, diags := ParseAndValidateUploadParams(provenancedUpload); got == nil || diag.HasErrors(diags) {
		t.Fatalf("complete provenance should be accepted: got %+v / %v", got, diags)
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{
			name: "bad producer",
			data: []byte(`{"key":"` + key + `","result":{"status":"success"},"manifest":{"files":[]},"producer":"robot","producerIdentity":"subject","channel":"hint"}`),
		},
		{
			name: "bad channel",
			data: []byte(`{"status":"hit","result":{"status":"success"},"manifest":{"files":[{"path":"dist/a.js","digest":"` + digest + `","mode":420,"size":12}]},"producer":"developer","producerIdentity":"subject","channel":"unsafe"}`),
		},
		{
			name: "partial provenance",
			data: []byte(`{"key":"` + key + `","result":{"status":"success"},"manifest":{"files":[]},"producer":"ci"}`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags []diag.Diagnostic
			switch tc.name {
			case "bad channel":
				_, diags = ParseAndValidateRestoreResult(tc.data)
			default:
				_, diags = ParseAndValidateUploadParams(tc.data)
			}
			if !diag.HasErrors(diags) {
				t.Fatalf("expected provenance validation error, got %v", diags)
			}
		})
	}
}

// TestProviderResponse_FailedNeedsCode verifies a failed response without an
// error code is rejected (the harness relies on a populated error).
func TestProviderResponse_FailedNeedsCode(t *testing.T) {
	b := []byte(`{"protocolVersion": 1, "id": 1, "ok": false}`)
	_, diags := ParseAndValidateProviderResponse(b)
	if !diag.HasErrors(diags) {
		t.Fatal("expected required-field error for missing error code")
	}
}

// TestBlobExchangePath checks the shared blob-exchange layout helper both repos use.
func TestBlobExchangePath(t *testing.T) {
	digest := validTestDigest()
	hex := strings.TrimPrefix(digest, DigestAlgorithm+digestSep)
	want := filepath.Join("/exchange", hex[:2], hex)
	got, ok := BlobExchangePath("/exchange", digest)
	if !ok || got != want {
		t.Fatalf("BlobExchangePath = %q, %v; want %q, true", got, ok, want)
	}
	if _, ok := BlobExchangePath("/exchange", "not-a-digest"); ok {
		t.Fatal("malformed digest should report ok=false")
	}
}

// TestNormalizeInitializeParams_Deterministic verifies normalization sorts the
// digest/capability sets so the canonical form is stable.
func TestNormalizeInitializeParams_Deterministic(t *testing.T) {
	d1 := DigestAlgorithm + digestSep + strings.Repeat("c", KeyLength)
	d2 := DigestAlgorithm + digestSep + strings.Repeat("a", KeyLength)
	p := &InitializeParams{
		ProtocolVersion: ProviderProtocolVersion,
		BlobExchangeDir: "/x",
		Capabilities:    []string{"upload-batch", "find-missing"},
		KnownDigests:    []string{d1, d2},
	}
	NormalizeInitializeParams(p)
	if p.Mode != DefaultMode {
		t.Errorf("mode not defaulted: %q", p.Mode)
	}
	if p.Capabilities[0] != "find-missing" {
		t.Errorf("capabilities not sorted: %v", p.Capabilities)
	}
	if p.KnownDigests[0] != d2 {
		t.Errorf("known digests not sorted: %v", p.KnownDigests)
	}
}

// TestMarshalPayload_Nil confirms a nil body yields an absent payload.
func TestMarshalPayload_Nil(t *testing.T) {
	raw, err := MarshalPayload(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if raw != nil {
		t.Fatalf("expected nil payload, got %q", raw)
	}
}
