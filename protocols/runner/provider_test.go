package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProviderEnvelopesAreStrict(t *testing.T) {
	t.Parallel()
	request, err := ParseProviderRequest([]byte(`{"protocolVersion":1,"id":1,"op":"submit","payload":{"a":1}}`))
	if err != nil || request.Op != OpSubmit || request.ID != 1 {
		t.Fatalf("request = %+v, %v", request, err)
	}
	for name, line := range map[string]string{
		"unknown op":      `{"protocolVersion":1,"id":1,"op":"restore"}`,
		"other version":   `{"protocolVersion":2,"id":1,"op":"submit"}`,
		"zero id":         `{"protocolVersion":1,"id":0,"op":"submit"}`,
		"extra member":    `{"protocolVersion":1,"id":1,"op":"submit","argv":["x"]}`,
		"duplicate":       `{"protocolVersion":1,"id":1,"id":2,"op":"submit"}`,
		"null payload":    `{"protocolVersion":1,"id":1,"op":"submit","payload":null}`,
		"trailing":        `{"protocolVersion":1,"id":1,"op":"submit"}{}`,
		"missing version": `{"id":1,"op":"submit"}`,
	} {
		if _, err := ParseProviderRequest([]byte(line)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	response, err := ParseProviderResponse([]byte(`{"protocolVersion":1,"id":3,"ok":false,"error":{"code":"unavailable","message":"no capacity"}}`))
	if err != nil || response.Error == nil || response.Error.Code != "unavailable" {
		t.Fatalf("response = %+v, %v", response, err)
	}
	for name, line := range map[string]string{
		"failure without error": `{"protocolVersion":1,"id":3,"ok":false}`,
		"other version":         `{"protocolVersion":9,"id":3,"ok":true}`,
		"unknown member":        `{"protocolVersion":1,"id":3,"ok":true,"trusted":true}`,
	} {
		if _, err := ParseProviderResponse([]byte(line)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, op := range []ProviderOp{OpInitialize, OpPrepare, OpSubmit, OpLookup, OpFollow, OpCancel, OpFetch, OpShutdown} {
		if !op.Valid() {
			t.Errorf("%s reported invalid", op)
		}
	}
	for _, op := range []ProviderOp{"inspect", "restart", "resubmit", ""} {
		if op.Valid() {
			t.Errorf("%q is not part of this protocol version", op)
		}
	}
}

// The lifecycle ops round-trip through the envelope with their own shapes: a
// lookup miss carries no member at all, a cancel acknowledgement carries the
// state observed once the request was applied.
func TestLifecycleOpPayloadsRoundTrip(t *testing.T) {
	t.Parallel()
	payload, err := MarshalPayload(LookupParams{IdempotencyKey: strings.Repeat("ab", 16)})
	if err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(ProviderRequest{ProtocolVersion: ProviderProtocolVersion, ID: 4, Op: OpLookup, Payload: payload})
	request, err := ParseProviderRequest(line)
	if err != nil || request.Op != OpLookup {
		t.Fatalf("lookup envelope: %+v, %v", request, err)
	}
	miss, _ := MarshalPayload(LookupResult{})
	if string(miss) != "{}" {
		t.Fatalf("a lookup miss must carry no member, got %s", miss)
	}
	hit, _ := MarshalPayload(LookupResult{Attempt: "a-1", State: StateRunning, ExecutionInputDigest: BlobDigest(nil)})
	var decoded LookupResult
	if err := json.Unmarshal(hit, &decoded); err != nil || decoded.Attempt != "a-1" || decoded.State != StateRunning || decoded.ExecutionInputDigest != BlobDigest(nil) {
		t.Fatalf("lookup hit = %+v, %v", decoded, err)
	}
	ack, _ := MarshalPayload(CancelResult{State: StateCompleted})
	response, err := ParseProviderResponse([]byte(`{"protocolVersion":1,"id":5,"ok":true,"payload":` + string(ack) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	var cancel CancelResult
	if err := json.Unmarshal(response.Payload, &cancel); err != nil || cancel.State != StateCompleted {
		t.Fatalf("cancel acknowledgement = %+v, %v", cancel, err)
	}
	if MaxFollowRecords <= 0 || MaxFollowRecords > MaxBundleSessions*MaxBundleFiles {
		t.Fatalf("follow answers must be bounded: %d", MaxFollowRecords)
	}
}

// A submit envelope wraps the request two levels deeper than the bare
// document, so a task with declared resources reaches depth 9 inside it; the
// envelope budget admits it while the document budget stays at 8.
func TestSubmitEnvelopeCarriesDeclaredResources(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("fixtures", "execution-request", "valid", "resources.json"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := ParseExecutionRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	task := request.Plan.Tasks[0]
	if len(task.Resources.Reads) == 0 || len(task.Resources.Writes) == 0 {
		t.Fatalf("fixture declares no resources: %+v", task.Resources)
	}
	manifest := SourceManifest{Version: SourceManifestVersion, Entries: []SourceEntry{}}
	payload, err := MarshalPayload(SubmitParams{Request: request, Manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(ProviderRequest{ProtocolVersion: ProviderProtocolVersion, ID: 3, Op: OpSubmit, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseProviderRequest(line)
	if err != nil {
		t.Fatalf("submit envelope with declared resources refused: %v", err)
	}
	var params SubmitParams
	if err := json.Unmarshal(parsed.Payload, &params); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(params.Request, request) {
		t.Fatalf("request changed across the envelope:\n%+v\n%+v", params.Request, request)
	}
	response := ProviderResponse{ProtocolVersion: ProviderProtocolVersion, ID: 3, OK: true}
	response.Payload, _ = MarshalPayload(FetchResult{Bundle: SessionBundle{Attempt: "a", SessionID: "20260916-101500-0a1b2c", Sessions: []BundleSession{{ID: "20260916-101500-0a1b2c", Files: []BundleFile{{Name: BundleSessionFile, Digest: BlobDigest(nil), Size: 0}}}}}})
	line, _ = json.Marshal(response)
	if _, err := ParseProviderResponse(line); err != nil {
		t.Fatalf("fetch envelope refused: %v", err)
	}
	// The bare document keeps its own budget: nine levels are still refused.
	deep := []byte(`{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":{"i":1}}}}}}}}}`)
	if err := strictJSON(deep); err == nil {
		t.Fatal("document budget admitted nine levels")
	}
	if err := strictJSONDepth(deep, MaxEnvelopeDepth); err != nil {
		t.Fatalf("envelope budget refused nine levels: %v", err)
	}
	tooDeep := []byte(`{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":{"i":{"j":{"k":{"l":{"m":1}}}}}}}}}}}}}`)
	if err := strictJSONDepth(tooDeep, MaxEnvelopeDepth); err == nil {
		t.Fatal("envelope budget is unbounded")
	}
}

func TestSessionBundleFixtures(t *testing.T) {
	t.Parallel()
	for _, validity := range []string{"valid", "invalid"} {
		paths, err := filepath.Glob(filepath.Join("fixtures", "session-bundle", validity, "*.json"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("fixture corpus: %v, %v", paths, err)
		}
		for _, path := range paths {
			t.Run(validity+"/"+filepath.Base(path), func(t *testing.T) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := strictJSON(data); err != nil {
					t.Fatal(err)
				}
				var bundle SessionBundle
				if err := json.Unmarshal(data, &bundle); err != nil {
					t.Fatal(err)
				}
				err = ValidateSessionBundle(bundle)
				if (err == nil) != (validity == "valid") {
					t.Fatalf("validity %s, err %v", validity, err)
				}
			})
		}
	}
}

func TestTerminalStatesAndSessionIDs(t *testing.T) {
	t.Parallel()
	for _, state := range []string{StateQueued, StatePreparing, StateRunning} {
		if TerminalState(state) {
			t.Errorf("%s is terminal", state)
		}
	}
	for _, state := range []string{StateCompleted, StateFailed, StateCanceled} {
		if !TerminalState(state) {
			t.Errorf("%s is not terminal", state)
		}
	}
	if !ValidSessionID("20260916-101500-0a1b2c") || ValidSessionID("20260916-101500-0A1B2C") || ValidSessionID("latest") || ValidSessionID("../20260916-101500-0a1b2c") {
		t.Error("session id rule diverged")
	}
	path, ok := ExchangeBlobPath("/exchange", "sha256:"+strings.Repeat("ab", 32))
	if !ok || path != filepath.Join("/exchange", "ab", strings.Repeat("ab", 32)) {
		t.Errorf("exchange path = %s, %v", path, ok)
	}
	if _, ok := ExchangeBlobPath("/exchange", "sha256:../x"); ok {
		t.Error("traversal digest accepted")
	}
	if BundleFileAdmitted("reporting.json") || !BundleFileAdmitted(BundleSessionFile) {
		t.Error("bundle file allowlist diverged")
	}
}
