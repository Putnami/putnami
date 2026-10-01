package cache

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

var (
	keyA = strings.Repeat("a", KeyLength)
	keyB = strings.Repeat("b", KeyLength)
	keyC = strings.Repeat("c", KeyLength)
	digX = DigestAlgorithm + ":" + strings.Repeat("d", KeyLength)
	digY = DigestAlgorithm + ":" + strings.Repeat("e", KeyLength)
)

func TestValidKey(t *testing.T) {
	cases := map[string]bool{
		keyA:                           true,
		strings.Repeat("a", 63):        false, // too short
		strings.Repeat("a", 65):        false, // too long
		strings.Repeat("A", KeyLength): false, // uppercase rejected
		strings.Repeat("g", KeyLength): false, // non-hex
		"":                             false,
	}
	for in, want := range cases {
		if got := ValidKey(in); got != want {
			t.Errorf("ValidKey(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDigestRoundTrip(t *testing.T) {
	d := DigestOf([]byte("hello world"))
	if !ValidDigest(d) {
		t.Fatalf("DigestOf produced an invalid digest: %q", d)
	}
	if !strings.HasPrefix(d, DigestAlgorithm+":") {
		t.Errorf("digest %q missing algorithm prefix", d)
	}
	// Deterministic.
	if DigestOf([]byte("hello world")) != d {
		t.Error("DigestOf is not deterministic")
	}
	// Content-addressed: different bytes, different digest.
	if DigestOf([]byte("hello mars")) == d {
		t.Error("DigestOf collided on different content")
	}
}

func TestValidDigest(t *testing.T) {
	cases := map[string]bool{
		digX:                             true,
		strings.Repeat("d", 64):          false, // no prefix
		"md5:" + strings.Repeat("d", 64): false, // wrong algorithm
		"sha256:xyz":                     false, // not hex / wrong length
		"":                               false,
	}
	for in, want := range cases {
		if got := ValidDigest(in); got != want {
			t.Errorf("ValidDigest(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBearerToken(t *testing.T) {
	tok, ok := BearerToken(AuthorizationValue("secret-123"))
	if !ok || tok != "secret-123" {
		t.Errorf("round-trip = (%q, %v), want (secret-123, true)", tok, ok)
	}
	if _, ok := BearerToken("Basic abc"); ok {
		t.Error("expected non-bearer scheme to be rejected")
	}
	if _, ok := BearerToken("Bearer "); ok {
		t.Error("expected empty token to be rejected")
	}
}

func TestModeValid(t *testing.T) {
	for _, m := range []Mode{ModeMinimal, ModeToplevel, ModeFull} {
		if !m.Valid() {
			t.Errorf("Mode %q should be valid", m)
		}
	}
	if Mode("eager").Valid() {
		t.Error("unknown mode should be invalid")
	}
}

func TestValidateRequest_Errors(t *testing.T) {
	r := &NegotiateRequest{
		ProtocolVersion: 99,
		Mode:            Mode("eager"),
		Keys: []KeyRequest{
			{Key: "not-a-key"},
		},
	}
	diags := ValidateRequest(r)
	if !diag.HasErrors(diags) {
		t.Fatal("expected validation errors")
	}
	codes := codeSet(diags)
	for _, want := range []string{CodeVersionMismatch, CodeInvalidMode, CodeInvalidKey} {
		if !codes[want] {
			t.Errorf("expected diagnostic %q, got %v", want, codes)
		}
	}
}

func TestValidateRequest_DuplicateKeyWarns(t *testing.T) {
	r := &NegotiateRequest{
		ProtocolVersion: ProtocolVersion,
		Keys:            []KeyRequest{{Key: keyA}, {Key: keyA}},
	}
	diags := ValidateRequest(r)
	if diag.HasErrors(diags) {
		t.Errorf("duplicate key should warn, not error: %v", diags)
	}
	if !codeSet(diags)[CodeDuplicateKey] {
		t.Errorf("expected duplicate-key warning, got %v", diags)
	}
}

func TestValidateResponse_HitRequiresResult(t *testing.T) {
	resp := &NegotiateResponse{
		ProtocolVersion: ProtocolVersion,
		Results:         []KeyResult{{Key: keyA, Hit: true}},
	}
	if !diag.HasErrors(ValidateResponse(resp)) {
		t.Error("hit without a result should be an error")
	}
}

func TestValidateResponse_TransferMethodChecked(t *testing.T) {
	resp := &NegotiateResponse{
		ProtocolVersion: ProtocolVersion,
		Results: []KeyResult{{
			Key:       keyA,
			Hit:       true,
			Result:    &ActionResult{Status: "success"},
			Downloads: []BlobTransfer{{Digest: digX, URL: "https://cas/x", Method: TransferPut}},
		}},
	}
	if !codeSet(ValidateResponse(resp))[CodeInvalidMethod] {
		t.Error("a download with PUT method should be flagged")
	}
}

func TestNormalizeRequest_DefaultsAndOrder(t *testing.T) {
	r := NormalizeRequest(&NegotiateRequest{
		Keys: []KeyRequest{{Key: keyC}, {Key: keyA}, {Key: keyB}},
	})
	if r.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion default = %d, want %d", r.ProtocolVersion, ProtocolVersion)
	}
	if r.Mode != DefaultMode {
		t.Errorf("Mode default = %q, want %q", r.Mode, DefaultMode)
	}
	if r.Keys[0].Key != keyA || r.Keys[1].Key != keyB || r.Keys[2].Key != keyC {
		t.Errorf("keys not sorted: %v", r.Keys)
	}
}

func TestNormalizeRequest_Deterministic(t *testing.T) {
	build := func() *NegotiateRequest {
		return &NegotiateRequest{
			Keys: []KeyRequest{{Key: keyC}, {Key: keyA}, {Key: keyB}},
		}
	}
	canonical := NormalizeRequest(build())
	for i := 0; i < 100; i++ {
		if got := NormalizeRequest(build()); !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: non-deterministic NormalizeRequest\ncanonical: %#v\ngot: %#v", i, canonical, got)
		}
	}
}

func TestNormalizeResponse_Deterministic(t *testing.T) {
	build := func() *NegotiateResponse {
		return &NegotiateResponse{
			Results: []KeyResult{
				{Key: keyB, Hit: true, Result: &ActionResult{Status: "success"}, Manifest: &Manifest{
					Files: []FileEntry{{Path: "z.js", Digest: digY}, {Path: "a.js", Digest: digX}},
				}},
				{Key: keyA, Hit: false, Downloads: []BlobTransfer{
					{Digest: digY, URL: "https://cas/y", Method: TransferGet},
					{Digest: digX, URL: "https://cas/x", Method: TransferGet},
				}},
			},
		}
	}
	canonical := NormalizeResponse(build())
	if canonical.Results[0].Key != keyA {
		t.Fatalf("results not sorted by key: %v", canonical.Results)
	}
	if canonical.Results[1].Manifest.Files[0].Path != "a.js" {
		t.Fatalf("manifest files not sorted: %v", canonical.Results[1].Manifest.Files)
	}
	if canonical.Results[0].Downloads[0].Digest != digX {
		t.Fatalf("downloads not sorted: %v", canonical.Results[0].Downloads)
	}
	for i := 0; i < 100; i++ {
		if got := NormalizeResponse(build()); !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: non-deterministic NormalizeResponse", i)
		}
	}
}

func TestRunMarkerRequest_NormalizeAndValidate(t *testing.T) {
	req := NormalizeRunMarkerRequest(&RunMarkerRequest{
		ProtocolVersion: ProtocolVersion,
		Workspace:       "  repo:abc  ",
		Branch:          " main ",
		Commands:        []string{" build ", "", "test"},
		Selection:       RunMarkerSelectionAll,
	})
	if req.Workspace != "repo:abc" || req.Branch != "main" {
		t.Fatalf("fields not trimmed: %+v", req)
	}
	if !reflect.DeepEqual(req.Commands, []string{"build", "test"}) {
		t.Fatalf("commands = %v, want [build test]", req.Commands)
	}
	if diags := ValidateRunMarkerRequest(req); diag.HasErrors(diags) {
		t.Fatalf("valid run marker request produced diagnostics: %v", diags)
	}
}

func TestRunMarkerRequest_SelectionRequired(t *testing.T) {
	req := &RunMarkerRequest{
		ProtocolVersion: ProtocolVersion,
		Workspace:       "repo:abc",
		Branch:          "main",
		Commands:        []string{"build"},
		Selection:       RunMarkerSelection("partial"),
	}
	if !codeSet(ValidateRunMarkerRequest(req))[CodeInvalidSelection] {
		t.Fatalf("expected invalid-selection diagnostic")
	}
}

func TestPublishRunMarkerRequest_SHARequired(t *testing.T) {
	req := &PublishRunMarkerRequest{
		ProtocolVersion: ProtocolVersion,
		Workspace:       "repo:abc",
		Branch:          "main",
		Commands:        []string{"build"},
		Selection:       RunMarkerSelectionAll,
	}
	if !codeSet(ValidatePublishRunMarkerRequest(req))[CodeRequiredField] {
		t.Fatalf("expected required-field diagnostic for missing sha")
	}
}

func TestValidateStoreRequest_Errors(t *testing.T) {
	r := &StoreRequest{
		ProtocolVersion: 99,
		Key:             "not-a-key",
		Manifest:        &Manifest{Files: []FileEntry{{Path: "", Digest: "bad"}}},
	}
	codes := codeSet(ValidateStoreRequest(r))
	for _, want := range []string{CodeVersionMismatch, CodeInvalidKey, CodeRequiredField, CodeInvalidDigest} {
		if !codes[want] {
			t.Errorf("expected diagnostic %q, got %v", want, codes)
		}
	}
}

func TestValidateStoreRequest_ManifestRequired(t *testing.T) {
	r := &StoreRequest{ProtocolVersion: ProtocolVersion, Key: keyA}
	if !codeSet(ValidateStoreRequest(r))[CodeRequiredField] {
		t.Error("a store request without a manifest should be a required-field error")
	}
}

func TestValidateStoreRequest_Valid(t *testing.T) {
	r := &StoreRequest{
		ProtocolVersion: ProtocolVersion,
		Key:             keyA,
		Manifest:        &Manifest{Files: []FileEntry{{Path: "dist/index.js", Digest: digX, Mode: 0o644}}},
	}
	if diags := ValidateStoreRequest(r); diag.HasErrors(diags) {
		t.Errorf("expected a valid store request, got %v", diags)
	}
}

func TestValidateStoreResponse_TransferMethodChecked(t *testing.T) {
	resp := &StoreResponse{
		ProtocolVersion: ProtocolVersion,
		Key:             keyA,
		Uploads:         []BlobTransfer{{Digest: digX, URL: "https://cas/x", Method: TransferGet}},
	}
	if !codeSet(ValidateStoreResponse(resp))[CodeInvalidMethod] {
		t.Error("an upload with GET method should be flagged")
	}
}

func TestValidateStoreResponse_EmptyUploadsValid(t *testing.T) {
	resp := &StoreResponse{ProtocolVersion: ProtocolVersion, Key: keyA}
	if diag.HasErrors(ValidateStoreResponse(resp)) {
		t.Error("an empty upload set (full dedup) should be valid")
	}
}

func TestValidateCommitRequest_RequiresResult(t *testing.T) {
	r := &CommitRequest{
		ProtocolVersion: ProtocolVersion,
		Key:             keyA,
		Manifest:        &Manifest{Files: []FileEntry{{Path: "x", Digest: digX}}},
	}
	if !codeSet(ValidateCommitRequest(r))[CodeRequiredField] {
		t.Error("a commit without a result should be a required-field error")
	}
}

func TestValidateActionResult_AllowsCachedEvents(t *testing.T) {
	r := &CommitRequest{
		ProtocolVersion: ProtocolVersion,
		Key:             keyA,
		Result: &ActionResult{
			Status: "success",
			Events: []ActionEvent{
				{Version: 1, Type: "metric", Data: map[string]any{"name": "coverage", "value": float64(84), "unit": "percent"}},
				{Version: 1, Type: "summary", Data: map[string]any{"message": "51/51 passed, 84.0% coverage"}},
			},
		},
		Manifest: &Manifest{Files: []FileEntry{}},
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	got, diags := ParseAndValidateCommitRequest(b)
	if got == nil || diag.HasErrors(diags) {
		t.Fatalf("commit request with cached events should validate: %v", diags)
	}
	if len(got.Result.Events) != 2 || got.Result.Events[0].Type != "metric" {
		t.Errorf("cached events not preserved: %+v", got.Result.Events)
	}
}

func TestValidateActionResult_RejectsCachedEventWithoutType(t *testing.T) {
	r := &CommitRequest{
		ProtocolVersion: ProtocolVersion,
		Key:             keyA,
		Result:          &ActionResult{Status: "success", Events: []ActionEvent{{Version: 1}}},
		Manifest:        &Manifest{Files: []FileEntry{}},
	}
	if !codeSet(ValidateCommitRequest(r))[CodeRequiredField] {
		t.Error("a cached event without a type should be a required-field error")
	}
}

func TestValidateCommitResponse_Errors(t *testing.T) {
	resp := &CommitResponse{ProtocolVersion: 99, Key: "bad"}
	codes := codeSet(ValidateCommitResponse(resp))
	for _, want := range []string{CodeVersionMismatch, CodeInvalidKey} {
		if !codes[want] {
			t.Errorf("expected diagnostic %q, got %v", want, codes)
		}
	}
}

func TestNormalizeStoreRequest_DefaultsAndOrder(t *testing.T) {
	r := NormalizeStoreRequest(&StoreRequest{
		Key: keyA,
		Manifest: &Manifest{Files: []FileEntry{
			{Path: "z.js", Digest: digY}, {Path: "a.js", Digest: digX},
		}},
	})
	if r.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion default = %d, want %d", r.ProtocolVersion, ProtocolVersion)
	}
	if r.Manifest.Files[0].Path != "a.js" {
		t.Errorf("manifest files not sorted: %v", r.Manifest.Files)
	}
}

func TestNormalizeStoreResponse_Deterministic(t *testing.T) {
	build := func() *StoreResponse {
		return &StoreResponse{
			Key: keyA,
			Uploads: []BlobTransfer{
				{Digest: digY, URL: "https://cas/y", Method: TransferPut},
				{Digest: digX, URL: "https://cas/x", Method: TransferPut},
			},
		}
	}
	canonical := NormalizeStoreResponse(build())
	if canonical.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion default = %d, want %d", canonical.ProtocolVersion, ProtocolVersion)
	}
	if canonical.Uploads[0].Digest != digX {
		t.Fatalf("uploads not sorted: %v", canonical.Uploads)
	}
	for i := 0; i < 100; i++ {
		if got := NormalizeStoreResponse(build()); !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: non-deterministic NormalizeStoreResponse", i)
		}
	}
}

func TestParseAndValidate_StoreCommit_RoundTrip(t *testing.T) {
	storeReq := &StoreRequest{
		ProtocolVersion: ProtocolVersion,
		Key:             keyA,
		Manifest:        &Manifest{Files: []FileEntry{{Path: "dist/index.js", Digest: digX, Mode: 0o644, Size: 10}}},
	}
	b, err := json.Marshal(storeReq)
	if err != nil {
		t.Fatal(err)
	}
	if got, diags := ParseAndValidateStoreRequest(b); got == nil || diag.HasErrors(diags) {
		t.Fatalf("round-trip store request failed: %v", diags)
	}

	commitReq := &CommitRequest{
		ProtocolVersion: ProtocolVersion,
		Key:             keyA,
		Result:          &ActionResult{Status: "success"},
		Manifest:        &Manifest{Files: []FileEntry{{Path: "dist/index.js", Digest: digX, Mode: 0o644}}},
	}
	b, err = json.Marshal(commitReq)
	if err != nil {
		t.Fatal(err)
	}
	if got, diags := ParseAndValidateCommitRequest(b); got == nil || diag.HasErrors(diags) {
		t.Fatalf("round-trip commit request failed: %v", diags)
	}
}

func TestValidateFindMissingRequest_TooManyBlobs(t *testing.T) {
	blobs := make([]BlobRef, MaxKeysPerRequest+1)
	for i := range blobs {
		blobs[i] = BlobRef{Digest: digX}
	}
	r := &FindMissingRequest{ProtocolVersion: ProtocolVersion, Blobs: blobs}
	if !codeSet(ValidateFindMissingRequest(r))[CodeTooManyKeys] {
		t.Error("a blob set over the per-request limit should be flagged")
	}
}

func TestValidateFindMissingResponse_TransferMethodChecked(t *testing.T) {
	resp := &FindMissingResponse{
		ProtocolVersion: ProtocolVersion,
		Uploads:         []BlobTransfer{{Digest: digX, URL: "https://cas/x", Method: TransferGet}},
	}
	if !codeSet(ValidateFindMissingResponse(resp))[CodeInvalidMethod] {
		t.Error("an upload with GET method should be flagged")
	}
}

func TestNormalizeFindMissingRequest_DefaultsAndOrder(t *testing.T) {
	r := &FindMissingRequest{Blobs: []BlobRef{{Digest: digY}, {Digest: digX}}}
	NormalizeFindMissingRequest(r)
	if r.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", r.ProtocolVersion, ProtocolVersion)
	}
	if r.Blobs[0].Digest != digX || r.Blobs[1].Digest != digY {
		t.Errorf("blobs not sorted by digest: %+v", r.Blobs)
	}
}

func TestValidateCommitBatchRequest_DuplicateKeyWarns(t *testing.T) {
	r := &CommitBatchRequest{
		ProtocolVersion: ProtocolVersion,
		Entries: []CommitEntry{
			{Key: keyA, Result: &ActionResult{Status: "success"}, Manifest: &Manifest{}},
			{Key: keyA, Result: &ActionResult{Status: "success"}, Manifest: &Manifest{}},
		},
	}
	diags := ValidateCommitBatchRequest(r)
	if diag.HasErrors(diags) {
		t.Errorf("duplicate key should warn, not error: %v", diags)
	}
	if !codeSet(diags)[CodeDuplicateKey] {
		t.Errorf("expected duplicate-key warning, got %v", diags)
	}
}

func TestValidateCommitBatchRequest_EntryRequiresResultAndManifest(t *testing.T) {
	r := &CommitBatchRequest{
		ProtocolVersion: ProtocolVersion,
		Entries:         []CommitEntry{{Key: keyA}},
	}
	if !diag.HasErrors(ValidateCommitBatchRequest(r)) {
		t.Error("an entry missing result and manifest should error")
	}
}

func TestNormalizeCommitBatchRequest_DefaultsAndOrder(t *testing.T) {
	r := &CommitBatchRequest{
		Entries: []CommitEntry{
			{Key: keyB, Result: &ActionResult{Status: "success"}, Manifest: &Manifest{Files: []FileEntry{
				{Path: "b.js", Digest: digY}, {Path: "a.js", Digest: digX},
			}}},
			{Key: keyA, Result: &ActionResult{Status: "success"}, Manifest: &Manifest{}},
		},
	}
	NormalizeCommitBatchRequest(r)
	if r.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", r.ProtocolVersion, ProtocolVersion)
	}
	if r.Entries[0].Key != keyA || r.Entries[1].Key != keyB {
		t.Errorf("entries not sorted by key: %s, %s", r.Entries[0].Key, r.Entries[1].Key)
	}
	if files := r.Entries[1].Manifest.Files; files[0].Path != "a.js" || files[1].Path != "b.js" {
		t.Errorf("manifest files not sorted by path: %+v", files)
	}
}

func TestNormalizeCapabilitiesResponse_SortsCapabilities(t *testing.T) {
	resp := &CapabilitiesResponse{Capabilities: []string{CapabilityCommitBatch, CapabilityFindMissing}}
	NormalizeCapabilitiesResponse(resp)
	if resp.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", resp.ProtocolVersion, ProtocolVersion)
	}
	if resp.Capabilities[0] != CapabilityCommitBatch || resp.Capabilities[1] != CapabilityFindMissing {
		t.Errorf("capabilities not sorted: %v", resp.Capabilities)
	}
}

func TestParseAndValidate_BatchWritePath_RoundTrip(t *testing.T) {
	roundTrip := func(name string, v any, parse func([]byte) (any, []diag.Diagnostic)) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		got, diags := parse(b)
		if got == nil || diag.HasErrors(diags) {
			t.Fatalf("%s: round-trip failed: %v", name, diags)
		}
	}

	roundTrip("capabilities", &CapabilitiesResponse{ProtocolVersion: ProtocolVersion, Capabilities: []string{CapabilityFindMissing}},
		func(b []byte) (any, []diag.Diagnostic) { return ParseAndValidateCapabilitiesResponse(b) })
	roundTrip("find-missing request", &FindMissingRequest{ProtocolVersion: ProtocolVersion, Blobs: []BlobRef{{Digest: digX, SizeBytes: 10}}},
		func(b []byte) (any, []diag.Diagnostic) { return ParseAndValidateFindMissingRequest(b) })
	roundTrip("find-missing response", &FindMissingResponse{ProtocolVersion: ProtocolVersion, Uploads: []BlobTransfer{{Digest: digX, URL: "https://cas/x", Method: TransferPut}}},
		func(b []byte) (any, []diag.Diagnostic) { return ParseAndValidateFindMissingResponse(b) })
	roundTrip("commit-batch request", &CommitBatchRequest{ProtocolVersion: ProtocolVersion, Entries: []CommitEntry{{Key: keyA, Result: &ActionResult{Status: "success"}, Manifest: &Manifest{}}}},
		func(b []byte) (any, []diag.Diagnostic) { return ParseAndValidateCommitBatchRequest(b) })
	roundTrip("commit-batch response", &CommitBatchResponse{ProtocolVersion: ProtocolVersion, Results: []CommitResult{{Key: keyA, Committed: true}}},
		func(b []byte) (any, []diag.Diagnostic) { return ParseAndValidateCommitBatchResponse(b) })
}

func TestNormalizeUploadGrant_DefaultsMethodAndVersion(t *testing.T) {
	g := &UploadGrant{URLTemplate: "https://cas.example/blob/" + DigestPlaceholder}
	NormalizeUploadGrant(g)
	if g.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", g.ProtocolVersion, ProtocolVersion)
	}
	if g.Method != TransferPut {
		t.Errorf("Method = %q, want %q", g.Method, TransferPut)
	}
	if diag.HasErrors(ValidateUploadGrant(g)) {
		t.Error("a normalized grant with a placeholder template should validate")
	}
}

func TestValidateUploadGrant_RequiresPlaceholder(t *testing.T) {
	g := &UploadGrant{ProtocolVersion: ProtocolVersion, URLTemplate: "https://cas.example/blob/fixed", Method: TransferPut}
	if !codeSet(ValidateUploadGrant(g))[CodeRequiredField] {
		t.Error("a template without the digest placeholder should be flagged")
	}
}

func TestValidateUploadGrant_MethodChecked(t *testing.T) {
	g := &UploadGrant{ProtocolVersion: ProtocolVersion, URLTemplate: "x/" + DigestPlaceholder, Method: TransferGet}
	if !codeSet(ValidateUploadGrant(g))[CodeInvalidMethod] {
		t.Error("a non-PUT grant method should be flagged")
	}
}

func TestParseAndValidateUploadGrant_RoundTrip(t *testing.T) {
	g := &UploadGrant{
		ProtocolVersion:         ProtocolVersion,
		URLTemplate:             "https://cas.example/blob/" + DigestPlaceholder,
		Method:                  TransferPut,
		Headers:                 map[string]string{"x-goog-if-generation-match": "0"},
		ConditionalExistsStatus: 412,
	}
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	got, diags := ParseAndValidateUploadGrant(b)
	if got == nil || diag.HasErrors(diags) {
		t.Fatalf("round-trip upload grant failed: %v", diags)
	}
	if got.ConditionalExistsStatus != 412 {
		t.Errorf("ConditionalExistsStatus = %d, want 412", got.ConditionalExistsStatus)
	}
}

func TestValidateUploadBatchRequest_RejectsOversizeBlob(t *testing.T) {
	r := &UploadBatchRequest{
		ProtocolVersion: ProtocolVersion,
		Blobs: []InlineBlob{
			{Digest: digX, Data: make([]byte, MaxInlineBlobBytes+1)},
		},
	}
	if !codeSet(ValidateUploadBatchRequest(r))[CodeBlobTooLarge] {
		t.Error("an inline blob over the size limit should be flagged")
	}
}

func TestValidateUploadBatchRequest_DuplicateDigestWarns(t *testing.T) {
	r := &UploadBatchRequest{
		ProtocolVersion: ProtocolVersion,
		Blobs:           []InlineBlob{{Digest: digX, Data: []byte("a")}, {Digest: digX, Data: []byte("a")}},
	}
	diags := ValidateUploadBatchRequest(r)
	if diag.HasErrors(diags) {
		t.Errorf("a duplicate digest should warn, not error: %v", diags)
	}
	if !codeSet(diags)[CodeDuplicateKey] {
		t.Errorf("expected a duplicate-digest warning, got %v", diags)
	}
}

func TestValidateUploadBatchResponse_DigestChecked(t *testing.T) {
	resp := &UploadBatchResponse{ProtocolVersion: ProtocolVersion, Stored: []string{"not-a-digest"}}
	if !codeSet(ValidateUploadBatchResponse(resp))[CodeInvalidDigest] {
		t.Error("a malformed stored digest should be flagged")
	}
}

func TestNormalizeUploadBatch_DefaultsAndOrder(t *testing.T) {
	req := &UploadBatchRequest{Blobs: []InlineBlob{{Digest: digY, Data: []byte("y")}, {Digest: digX, Data: []byte("x")}}}
	NormalizeUploadBatchRequest(req)
	if req.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", req.ProtocolVersion, ProtocolVersion)
	}
	if req.Blobs[0].Digest != digX || req.Blobs[1].Digest != digY {
		t.Errorf("blobs not sorted by digest: %+v", req.Blobs)
	}

	resp := &UploadBatchResponse{Stored: []string{digY, digX}}
	NormalizeUploadBatchResponse(resp)
	if resp.Stored[0] != digX || resp.Stored[1] != digY {
		t.Errorf("stored not sorted: %v", resp.Stored)
	}
}

func TestParseAndValidate_UploadBatch_RoundTrip(t *testing.T) {
	req := &UploadBatchRequest{
		ProtocolVersion: ProtocolVersion,
		Blobs:           []InlineBlob{{Digest: digX, Data: []byte("compiled")}},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if got, diags := ParseAndValidateUploadBatchRequest(b); got == nil || diag.HasErrors(diags) {
		t.Fatalf("round-trip upload-batch request failed: %v", diags)
	}

	resp := &UploadBatchResponse{ProtocolVersion: ProtocolVersion, Stored: []string{digX}}
	b, err = json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got, diags := ParseAndValidateUploadBatchResponse(b); got == nil || diag.HasErrors(diags) {
		t.Fatalf("round-trip upload-batch response failed: %v", diags)
	}
}

func codeSet(diags []diag.Diagnostic) map[string]bool {
	out := make(map[string]bool, len(diags))
	for _, d := range diags {
		out[d.Code] = true
	}
	return out
}
