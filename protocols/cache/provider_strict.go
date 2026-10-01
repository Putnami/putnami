package cache

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Diagnostic codes specific to the provider RPC, in addition to the shared
// codes declared in strict.go (CodeParseError, CodeVersionMismatch, …).
const (
	CodeInvalidOp         = "invalid-op"
	CodeInvalidStatus     = "invalid-status"
	CodeInvalidProducer   = "invalid-producer"
	CodeInvalidChannel    = "invalid-channel"
	CodeInvalidNamespace  = "invalid-namespace"
	CodeInvalidObjectID   = "invalid-object-id"
	CodeInvalidSize       = "invalid-size"
	CodeInvalidCredential = "invalid-credential" // #nosec G101 -- diagnostic code, not a credential
)

// providerVersionDiag reports an unsupported provider RPC version (distinct
// from the HTTP ProtocolVersion) on the "protocolVersion" field. The current
// parser accepts legacy v1 envelopes as well as the v2 provenance shape.
func providerVersionDiag(got int) []diag.Diagnostic {
	if got < ProviderProtocolMinVersion || got > ProviderProtocolVersion {
		return []diag.Diagnostic{diag.Errorf(CodeVersionMismatch, "protocolVersion",
			"unsupported provider protocol version %d (supported: %d-%d)", got,
			ProviderProtocolMinVersion, ProviderProtocolVersion)}
	}
	return nil
}

// --- provider request envelope ---

// ParseProviderRequestStrict decodes JSON into a ProviderRequest, rejecting
// unknown fields and returning structured diagnostics on a decode error.
func ParseProviderRequestStrict(data []byte) (*ProviderRequest, []diag.Diagnostic) {
	return parseStrict[ProviderRequest](data, "provider request")
}

// ValidateProviderRequest checks the framing invariants: a supported provider
// protocol version and a recognized op. The op-specific Payload is validated
// separately after the envelope is dispatched by Op.
func ValidateProviderRequest(r *ProviderRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "request is nil")}
	}
	diags := providerVersionDiag(r.ProtocolVersion)
	if !r.Op.Valid() {
		diags = append(diags, diag.Errorf(CodeInvalidOp, "op", "unknown provider op %q", r.Op))
	}
	return diags
}

// NormalizeProviderRequest applies canonical defaults in place.
func NormalizeProviderRequest(r *ProviderRequest) *ProviderRequest {
	if r == nil {
		return nil
	}
	if r.ProtocolVersion == 0 {
		r.ProtocolVersion = ProviderProtocolVersion
	}
	return r
}

// ParseAndValidateProviderRequest combines strict parsing, validation, and
// normalization. The request is nil only when parsing or a hard validation
// error fails.
func ParseAndValidateProviderRequest(data []byte) (*ProviderRequest, []diag.Diagnostic) {
	return parseAndValidate(data, ParseProviderRequestStrict, ValidateProviderRequest, NormalizeProviderRequest)
}

// --- provider response envelope ---

// ParseProviderResponseStrict decodes JSON into a ProviderResponse, rejecting
// unknown fields and returning structured diagnostics on a decode error.
func ParseProviderResponseStrict(data []byte) (*ProviderResponse, []diag.Diagnostic) {
	return parseStrict[ProviderResponse](data, "provider response")
}

// ValidateProviderResponse checks the framing invariants: a supported provider
// protocol version and a present error code on a failed response. The
// op-specific Payload is validated separately by the caller.
func ValidateProviderResponse(r *ProviderResponse) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "response is nil")}
	}
	diags := providerVersionDiag(r.ProtocolVersion)
	if !r.OK {
		if r.Error == nil || strings.TrimSpace(r.Error.Code) == "" {
			diags = append(diags, diag.Errorf(CodeRequiredField, "error.code",
				"a failed response (ok=false) must carry an error code"))
		}
	}
	return diags
}

// NormalizeProviderResponse applies canonical defaults in place.
func NormalizeProviderResponse(r *ProviderResponse) *ProviderResponse {
	if r == nil {
		return nil
	}
	if r.ProtocolVersion == 0 {
		r.ProtocolVersion = ProviderProtocolVersion
	}
	return r
}

// ParseAndValidateProviderResponse combines strict parsing, validation, and
// normalization.
func ParseAndValidateProviderResponse(data []byte) (*ProviderResponse, []diag.Diagnostic) {
	return parseAndValidate(data, ParseProviderResponseStrict, ValidateProviderResponse, NormalizeProviderResponse)
}

// --- initialize params ---

// ParseInitializeParamsStrict decodes JSON into InitializeParams.
func ParseInitializeParamsStrict(data []byte) (*InitializeParams, []diag.Diagnostic) {
	return parseStrict[InitializeParams](data, "initialize params")
}

// ValidateInitializeParams checks a supported provider protocol version, a
// blob-exchange directory, a recognized mode (when set), and well-formed known
// digests.
func ValidateInitializeParams(p *InitializeParams) []diag.Diagnostic {
	if p == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "params are nil")}
	}
	diags := providerVersionDiag(p.ProtocolVersion)
	if strings.TrimSpace(p.BlobExchangeDir) == "" {
		diags = append(diags, diag.Errorf(CodeRequiredField, "blobExchangeDir", "a blob-exchange directory is required"))
	}
	if p.Mode != "" && !p.Mode.Valid() {
		diags = append(diags, diag.Errorf(CodeInvalidMode, "mode", "unknown materialization mode %q", p.Mode))
	}
	diags = append(diags, validateDigestList("knownDigests", p.KnownDigests)...)
	return diags
}

// NormalizeInitializeParams applies canonical defaults and ordering in place.
func NormalizeInitializeParams(p *InitializeParams) *InitializeParams {
	if p == nil {
		return nil
	}
	if p.ProtocolVersion == 0 {
		p.ProtocolVersion = ProviderProtocolVersion
	}
	if p.Mode == "" {
		p.Mode = DefaultMode
	}
	sort.Strings(p.Capabilities)
	sort.Strings(p.KnownDigests)
	return p
}

// ParseAndValidateInitializeParams combines strict parsing, validation, and
// normalization.
func ParseAndValidateInitializeParams(data []byte) (*InitializeParams, []diag.Diagnostic) {
	return parseAndValidate(data, ParseInitializeParamsStrict, ValidateInitializeParams, NormalizeInitializeParams)
}

// --- initialize result ---

// ParseInitializeResultStrict decodes JSON into InitializeResult.
func ParseInitializeResultStrict(data []byte) (*InitializeResult, []diag.Diagnostic) {
	return parseStrict[InitializeResult](data, "initialize result")
}

// ValidateInitializeResult checks a supported provider protocol version and a
// present provider version (the version gate depends on it).
func ValidateInitializeResult(r *InitializeResult) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "result is nil")}
	}
	diags := providerVersionDiag(r.ProtocolVersion)
	if strings.TrimSpace(r.ProviderVersion) == "" {
		diags = append(diags, diag.Errorf(CodeRequiredField, "providerVersion",
			"a provider version is required for the version gate"))
	}
	return diags
}

// NormalizeInitializeResult applies canonical defaults and ordering in place.
func NormalizeInitializeResult(r *InitializeResult) *InitializeResult {
	if r == nil {
		return nil
	}
	if r.ProtocolVersion == 0 {
		r.ProtocolVersion = ProviderProtocolVersion
	}
	sort.Strings(r.Capabilities)
	return r
}

// ParseAndValidateInitializeResult combines strict parsing, validation, and
// normalization.
func ParseAndValidateInitializeResult(data []byte) (*InitializeResult, []diag.Diagnostic) {
	return parseAndValidate(data, ParseInitializeResultStrict, ValidateInitializeResult, NormalizeInitializeResult)
}

// --- authenticate params ---

// ParseAuthenticateParamsStrict decodes JSON into AuthenticateParams.
func ParseAuthenticateParamsStrict(data []byte) (*AuthenticateParams, []diag.Diagnostic) {
	if d := exactMemberNames(data, "authenticate params", "credential"); d != nil {
		return nil, d
	}
	return parseStrict[AuthenticateParams](data, "authenticate params")
}

// ValidateAuthenticateParams checks a well-formed run credential. Its
// diagnostic never quotes the value.
func ValidateAuthenticateParams(p *AuthenticateParams) []diag.Diagnostic {
	if p == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "params are nil")}
	}
	if !ValidRunCredential(p.Credential) {
		return []diag.Diagnostic{diag.Errorf(CodeInvalidCredential, "credential",
			"the run credential must be 1 to %d bytes of UTF-8 with no whitespace", MaxRunCredentialBytes)}
	}
	return nil
}

// ParseAndValidateAuthenticateParams combines strict parsing and validation.
// The credential is opaque, so nothing is normalized.
func ParseAndValidateAuthenticateParams(data []byte) (*AuthenticateParams, []diag.Diagnostic) {
	return parseAndValidate(data, ParseAuthenticateParamsStrict, ValidateAuthenticateParams, func(p *AuthenticateParams) *AuthenticateParams { return p })
}

// --- authenticate result ---

// ParseAuthenticateResultStrict decodes JSON into AuthenticateResult.
func ParseAuthenticateResultStrict(data []byte) (*AuthenticateResult, []diag.Diagnostic) {
	if d := exactMemberNames(data, "authenticate result"); d != nil {
		return nil, d
	}
	return parseStrict[AuthenticateResult](data, "authenticate result")
}

// exactMemberNames refuses a member of the JSON object data that is not one of
// names, compared with case. encoding/json matches member names without
// regard to case, so a "Credential" member would otherwise decode. It leaves
// data that is not an object to parseStrict.
func exactMemberNames(data []byte, what string, names ...string) []diag.Diagnostic {
	var members map[string]json.RawMessage
	if json.Unmarshal(data, &members) != nil {
		return nil
	}
	for member := range members {
		if !slices.Contains(names, member) {
			return []diag.Diagnostic{diag.Errorf(CodeParseError, "", "failed to parse %s: unknown member %q", what, member)}
		}
	}
	return nil
}

// ParseAndValidateAuthenticateResult strictly parses the empty result.
func ParseAndValidateAuthenticateResult(data []byte) (*AuthenticateResult, []diag.Diagnostic) {
	return parseAndValidate(data, ParseAuthenticateResultStrict,
		func(*AuthenticateResult) []diag.Diagnostic { return nil },
		func(r *AuthenticateResult) *AuthenticateResult { return r })
}

// --- prefetch params ---

// ParsePrefetchParamsStrict decodes JSON into PrefetchParams.
func ParsePrefetchParamsStrict(data []byte) (*PrefetchParams, []diag.Diagnostic) {
	return parseStrict[PrefetchParams](data, "prefetch params")
}

// ValidatePrefetchParams checks a bounded, well-formed key set (an empty set is
// valid — a no-op prefetch).
func ValidatePrefetchParams(p *PrefetchParams) []diag.Diagnostic {
	if p == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "params are nil")}
	}
	var diags []diag.Diagnostic
	if len(p.Keys) > MaxKeysPerRequest {
		diags = append(diags, diag.Errorf(CodeTooManyKeys, "keys",
			"%d keys exceeds the per-request limit of %d", len(p.Keys), MaxKeysPerRequest))
	}
	for i, k := range p.Keys {
		if !ValidKey(k) {
			diags = append(diags, diag.Errorf(CodeInvalidKey, fmt.Sprintf("keys[%d]", i),
				"malformed cache key %q", k))
		}
	}
	return diags
}

// NormalizePrefetchParams sorts the key set for deterministic output.
func NormalizePrefetchParams(p *PrefetchParams) *PrefetchParams {
	if p == nil {
		return nil
	}
	sort.Strings(p.Keys)
	return p
}

// ParseAndValidatePrefetchParams combines strict parsing, validation, and
// normalization.
func ParseAndValidatePrefetchParams(data []byte) (*PrefetchParams, []diag.Diagnostic) {
	return parseAndValidate(data, ParsePrefetchParamsStrict, ValidatePrefetchParams, NormalizePrefetchParams)
}

// --- restore params ---

// ParseRestoreParamsStrict decodes JSON into RestoreParams.
func ParseRestoreParamsStrict(data []byte) (*RestoreParams, []diag.Diagnostic) {
	return parseStrict[RestoreParams](data, "restore params")
}

// ValidateRestoreParams checks a well-formed cache key.
func ValidateRestoreParams(p *RestoreParams) []diag.Diagnostic {
	if p == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "params are nil")}
	}
	if !ValidKey(p.Key) {
		return []diag.Diagnostic{diag.Errorf(CodeInvalidKey, "key", "malformed cache key %q", p.Key)}
	}
	return nil
}

// ParseAndValidateRestoreParams combines strict parsing and validation.
func ParseAndValidateRestoreParams(data []byte) (*RestoreParams, []diag.Diagnostic) {
	return parseAndValidate(data, ParseRestoreParamsStrict, ValidateRestoreParams, func(p *RestoreParams) *RestoreParams { return p })
}

// --- restore result ---

// ParseRestoreResultStrict decodes JSON into RestoreResult.
func ParseRestoreResultStrict(data []byte) (*RestoreResult, []diag.Diagnostic) {
	return parseStrict[RestoreResult](data, "restore result")
}

// ValidateRestoreResult checks a recognized status and, on a hit, a result
// carrying a status and a well-formed manifest. When entry provenance is
// present, all three fields must be complete and use their closed enums; absent
// provenance remains valid for legacy v1/channel-less providers.
func ValidateRestoreResult(r *RestoreResult) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "result is nil")}
	}
	diags := validateEntryProvenance("", r.Producer, r.ProducerIdentity, r.Channel)
	if !r.Status.Valid() {
		return append(diags, diag.Errorf(CodeInvalidStatus, "status", "unknown restore status %q", r.Status))
	}
	if r.Status != RestoreHit {
		return diags
	}
	diags = append(diags, validateActionResult("result", r.Result)...)
	if r.Manifest == nil {
		diags = append(diags, diag.Errorf(CodeRequiredField, "manifest", "a manifest is required on a hit"))
	} else {
		diags = append(diags, validateManifestFiles("manifest", r.Manifest)...)
	}
	return diags
}

// NormalizeRestoreResult sorts the manifest files for deterministic output.
func NormalizeRestoreResult(r *RestoreResult) *RestoreResult {
	if r == nil {
		return nil
	}
	NormalizeManifestFiles(r.Manifest)
	return r
}

// ParseAndValidateRestoreResult combines strict parsing, validation, and
// normalization.
func ParseAndValidateRestoreResult(data []byte) (*RestoreResult, []diag.Diagnostic) {
	return parseAndValidate(data, ParseRestoreResultStrict, ValidateRestoreResult, NormalizeRestoreResult)
}

// --- upload params ---

// ParseUploadParamsStrict decodes JSON into UploadParams.
func ParseUploadParamsStrict(data []byte) (*UploadParams, []diag.Diagnostic) {
	return parseStrict[UploadParams](data, "upload params")
}

// ValidateUploadParams checks a well-formed key, a result carrying a status, and
// a present, well-formed manifest. Upload provenance is syntactically checked
// when supplied but remains an untrusted assertion that the provider must
// overwrite from authenticated context before persisting an entry.
func ValidateUploadParams(p *UploadParams) []diag.Diagnostic {
	if p == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "params are nil")}
	}
	var diags []diag.Diagnostic
	if !ValidKey(p.Key) {
		diags = append(diags, diag.Errorf(CodeInvalidKey, "key", "malformed cache key %q", p.Key))
	}
	diags = append(diags, validateActionResult("result", p.Result)...)
	if p.Manifest == nil {
		diags = append(diags, diag.Errorf(CodeRequiredField, "manifest", "a manifest is required"))
	} else {
		diags = append(diags, validateManifestFiles("manifest", p.Manifest)...)
	}
	diags = append(diags, validateEntryProvenance("", p.Producer, p.ProducerIdentity, p.Channel)...)
	return diags
}

// NormalizeUploadParams sorts the manifest files for deterministic output.
func NormalizeUploadParams(p *UploadParams) *UploadParams {
	if p == nil {
		return nil
	}
	NormalizeManifestFiles(p.Manifest)
	return p
}

// ParseAndValidateUploadParams combines strict parsing, validation, and
// normalization.
func ParseAndValidateUploadParams(data []byte) (*UploadParams, []diag.Diagnostic) {
	return parseAndValidate(data, ParseUploadParamsStrict, ValidateUploadParams, NormalizeUploadParams)
}

// --- marker lookup params ---

// ParseMarkerLookupParamsStrict decodes JSON into MarkerLookupParams.
func ParseMarkerLookupParamsStrict(data []byte) (*MarkerLookupParams, []diag.Diagnostic) {
	return parseStrict[MarkerLookupParams](data, "marker lookup params")
}

// ValidateMarkerLookupParams checks the run-marker key fields.
func ValidateMarkerLookupParams(p *MarkerLookupParams) []diag.Diagnostic {
	if p == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "params are nil")}
	}
	return validateProviderMarkerKey(p.Workspace, p.Branch, p.Commands, p.Selection)
}

// NormalizeMarkerLookupParams trims and normalizes the key fields in place.
func NormalizeMarkerLookupParams(p *MarkerLookupParams) *MarkerLookupParams {
	if p == nil {
		return nil
	}
	p.Workspace = strings.TrimSpace(p.Workspace)
	p.Branch = strings.TrimSpace(p.Branch)
	p.ParamsHash = strings.TrimSpace(p.ParamsHash)
	p.Commands = NormalizeRunMarkerCommands(p.Commands)
	if p.Selection == "" {
		p.Selection = RunMarkerSelectionAll
	}
	return p
}

// ParseAndValidateMarkerLookupParams combines strict parsing, validation, and
// normalization.
func ParseAndValidateMarkerLookupParams(data []byte) (*MarkerLookupParams, []diag.Diagnostic) {
	return parseAndValidate(data, ParseMarkerLookupParamsStrict, ValidateMarkerLookupParams, NormalizeMarkerLookupParams)
}

// --- marker lookup result ---

// ParseMarkerLookupResultStrict decodes JSON into MarkerLookupResult.
func ParseMarkerLookupResultStrict(data []byte) (*MarkerLookupResult, []diag.Diagnostic) {
	return parseStrict[MarkerLookupResult](data, "marker lookup result")
}

// ValidateMarkerLookupResult checks a well-formed marker when one is present.
func ValidateMarkerLookupResult(r *MarkerLookupResult) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "result is nil")}
	}
	return validateRunMarker(r.Marker)
}

// NormalizeMarkerLookupResult trims the marker fields in place.
func NormalizeMarkerLookupResult(r *MarkerLookupResult) *MarkerLookupResult {
	if r == nil {
		return nil
	}
	normalizeRunMarker(r.Marker)
	return r
}

// ParseAndValidateMarkerLookupResult combines strict parsing, validation, and
// normalization.
func ParseAndValidateMarkerLookupResult(data []byte) (*MarkerLookupResult, []diag.Diagnostic) {
	return parseAndValidate(data, ParseMarkerLookupResultStrict, ValidateMarkerLookupResult, NormalizeMarkerLookupResult)
}

// --- marker write params ---

// ParseMarkerWriteParamsStrict decodes JSON into MarkerWriteParams.
func ParseMarkerWriteParamsStrict(data []byte) (*MarkerWriteParams, []diag.Diagnostic) {
	return parseStrict[MarkerWriteParams](data, "marker write params")
}

// ValidateMarkerWriteParams checks the run-marker key fields and a present SHA.
func ValidateMarkerWriteParams(p *MarkerWriteParams) []diag.Diagnostic {
	if p == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "params are nil")}
	}
	diags := validateProviderMarkerKey(p.Workspace, p.Branch, p.Commands, p.Selection)
	if strings.TrimSpace(p.SHA) == "" {
		diags = append(diags, diag.Errorf(CodeRequiredField, "sha", "run marker sha is required"))
	}
	return diags
}

// NormalizeMarkerWriteParams trims and normalizes the key fields in place.
func NormalizeMarkerWriteParams(p *MarkerWriteParams) *MarkerWriteParams {
	if p == nil {
		return nil
	}
	p.Workspace = strings.TrimSpace(p.Workspace)
	p.Branch = strings.TrimSpace(p.Branch)
	p.ParamsHash = strings.TrimSpace(p.ParamsHash)
	p.SHA = strings.TrimSpace(p.SHA)
	p.ObservedSHA = strings.TrimSpace(p.ObservedSHA)
	p.Commands = NormalizeRunMarkerCommands(p.Commands)
	if p.Selection == "" {
		p.Selection = RunMarkerSelectionAll
	}
	return p
}

// ParseAndValidateMarkerWriteParams combines strict parsing, validation, and
// normalization.
func ParseAndValidateMarkerWriteParams(data []byte) (*MarkerWriteParams, []diag.Diagnostic) {
	return parseAndValidate(data, ParseMarkerWriteParamsStrict, ValidateMarkerWriteParams, NormalizeMarkerWriteParams)
}

// --- summary result ---

// ParseSummaryResultStrict decodes JSON into SummaryResult.
func ParseSummaryResultStrict(data []byte) (*SummaryResult, []diag.Diagnostic) {
	return parseStrict[SummaryResult](data, "summary result")
}

// ValidateSummaryResult checks well-formed known digests when present.
func ValidateSummaryResult(r *SummaryResult) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "result is nil")}
	}
	return validateDigestList("knownDigests", r.KnownDigests)
}

// NormalizeSummaryResult sorts the known digests for deterministic output.
func NormalizeSummaryResult(r *SummaryResult) *SummaryResult {
	if r == nil {
		return nil
	}
	sort.Strings(r.KnownDigests)
	return r
}

// ParseAndValidateSummaryResult combines strict parsing, validation, and
// normalization.
func ParseAndValidateSummaryResult(data []byte) (*SummaryResult, []diag.Diagnostic) {
	return parseAndValidate(data, ParseSummaryResultStrict, ValidateSummaryResult, NormalizeSummaryResult)
}

// --- prefetch result ---

// ParsePrefetchResultStrict decodes JSON into PrefetchResult.
func ParsePrefetchResultStrict(data []byte) (*PrefetchResult, []diag.Diagnostic) {
	return parseStrict[PrefetchResult](data, "prefetch result")
}

// ValidatePrefetchResult has no hard invariants; it exists for symmetry and to
// reject unknown fields via strict parsing.
func ValidatePrefetchResult(r *PrefetchResult) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "result is nil")}
	}
	return nil
}

// ParseAndValidatePrefetchResult combines strict parsing and validation.
func ParseAndValidatePrefetchResult(data []byte) (*PrefetchResult, []diag.Diagnostic) {
	return parseAndValidate(data, ParsePrefetchResultStrict, ValidatePrefetchResult, func(r *PrefetchResult) *PrefetchResult { return r })
}

// --- upload result ---

// ParseUploadResultStrict decodes JSON into UploadResult.
func ParseUploadResultStrict(data []byte) (*UploadResult, []diag.Diagnostic) {
	return parseStrict[UploadResult](data, "upload result")
}

// ValidateUploadResult has no hard invariants beyond strict parsing.
func ValidateUploadResult(r *UploadResult) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "result is nil")}
	}
	return nil
}

// ParseAndValidateUploadResult combines strict parsing and validation.
func ParseAndValidateUploadResult(data []byte) (*UploadResult, []diag.Diagnostic) {
	return parseAndValidate(data, ParseUploadResultStrict, ValidateUploadResult, func(r *UploadResult) *UploadResult { return r })
}

// --- marker write result ---

// ParseMarkerWriteResultStrict decodes JSON into MarkerWriteResult.
func ParseMarkerWriteResultStrict(data []byte) (*MarkerWriteResult, []diag.Diagnostic) {
	return parseStrict[MarkerWriteResult](data, "marker write result")
}

// ValidateMarkerWriteResult checks a well-formed marker when one is present.
func ValidateMarkerWriteResult(r *MarkerWriteResult) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "result is nil")}
	}
	return validateRunMarker(r.Marker)
}

// NormalizeMarkerWriteResult trims the marker fields in place.
func NormalizeMarkerWriteResult(r *MarkerWriteResult) *MarkerWriteResult {
	if r == nil {
		return nil
	}
	normalizeRunMarker(r.Marker)
	return r
}

// ParseAndValidateMarkerWriteResult combines strict parsing, validation, and
// normalization.
func ParseAndValidateMarkerWriteResult(data []byte) (*MarkerWriteResult, []diag.Diagnostic) {
	return parseAndValidate(data, ParseMarkerWriteResultStrict, ValidateMarkerWriteResult, NormalizeMarkerWriteResult)
}

// --- object get params ---

// ParseObjectGetParamsStrict decodes JSON into ObjectGetParams.
func ParseObjectGetParamsStrict(data []byte) (*ObjectGetParams, []diag.Diagnostic) {
	return parseStrict[ObjectGetParams](data, "object get params")
}

// ValidateObjectGetParams checks a well-formed namespace, a bounded set of
// well-formed ids (an empty set is valid — a no-op lookup), and a closed
// channel filter. An EMPTY AcceptChannels means "every channel" and is the
// permissive default, so it is deliberately not an error; a listed channel must
// be a known one, because an unknown string would otherwise silently filter
// everything out and read as a cold cache.
func ValidateObjectGetParams(p *ObjectGetParams) []diag.Diagnostic {
	if p == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "params are nil")}
	}
	diags := validateObjectNamespace("namespace", p.Namespace)
	diags = append(diags, validateObjectIDs("ids", p.IDs)...)
	for i, channel := range p.AcceptChannels {
		if !channel.Valid() {
			diags = append(diags, diag.Errorf(CodeInvalidChannel, fmt.Sprintf("acceptChannels[%d]", i),
				"unknown cache-entry channel %q", channel))
		}
	}
	return diags
}

// NormalizeObjectGetParams sorts the id set and the channel filter so the
// canonical form of one lookup is stable whatever order the caller batched it
// in. Neither order carries meaning: the result is a set.
func NormalizeObjectGetParams(p *ObjectGetParams) *ObjectGetParams {
	if p == nil {
		return nil
	}
	p.Namespace = strings.TrimSpace(p.Namespace)
	sort.Strings(p.IDs)
	sort.Slice(p.AcceptChannels, func(i, j int) bool { return p.AcceptChannels[i] < p.AcceptChannels[j] })
	return p
}

// ParseAndValidateObjectGetParams combines strict parsing, validation, and
// normalization.
func ParseAndValidateObjectGetParams(data []byte) (*ObjectGetParams, []diag.Diagnostic) {
	return parseAndValidate(data, ParseObjectGetParamsStrict, ValidateObjectGetParams, NormalizeObjectGetParams)
}

// --- object get result ---

// ParseObjectGetResultStrict decodes JSON into ObjectGetResult.
func ParseObjectGetResultStrict(data []byte) (*ObjectGetResult, []diag.Diagnostic) {
	return parseStrict[ObjectGetResult](data, "object get result")
}

// ValidateObjectGetResult checks every hit: a well-formed id, a well-formed
// digest (the caller resolves it to a path under the blob-exchange directory),
// a non-negative size, and closed producer/channel enums when present. An empty
// result is valid — every requested id missed.
func ValidateObjectGetResult(r *ObjectGetResult) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "result is nil")}
	}
	var diags []diag.Diagnostic
	if len(r.Objects) > MaxKeysPerRequest {
		diags = append(diags, diag.Errorf(CodeTooManyKeys, "objects",
			"%d objects exceeds the per-request limit of %d", len(r.Objects), MaxKeysPerRequest))
	}
	for i, obj := range r.Objects {
		field := fmt.Sprintf("objects[%d]", i)
		diags = append(diags, validateObjectID(field+".id", obj.ID)...)
		if !ValidDigest(obj.Digest) {
			diags = append(diags, diag.Errorf(CodeInvalidDigest, field+".digest", "malformed digest %q", obj.Digest))
		}
		diags = append(diags, validateObjectSize(field+".size", obj.Size)...)
		diags = append(diags, validateObjectProvenance(field, obj.Producer, obj.Channel)...)
	}
	return diags
}

// NormalizeObjectGetResult sorts the hits by id for deterministic output.
func NormalizeObjectGetResult(r *ObjectGetResult) *ObjectGetResult {
	if r == nil {
		return nil
	}
	sort.Slice(r.Objects, func(i, j int) bool { return r.Objects[i].ID < r.Objects[j].ID })
	return r
}

// ParseAndValidateObjectGetResult combines strict parsing, validation, and
// normalization.
func ParseAndValidateObjectGetResult(data []byte) (*ObjectGetResult, []diag.Diagnostic) {
	return parseAndValidate(data, ParseObjectGetResultStrict, ValidateObjectGetResult, NormalizeObjectGetResult)
}

// --- object put params ---

// ParseObjectPutParamsStrict decodes JSON into ObjectPutParams.
func ParseObjectPutParamsStrict(data []byte) (*ObjectPutParams, []diag.Diagnostic) {
	return parseStrict[ObjectPutParams](data, "object put params")
}

// ValidateObjectPutParams checks a well-formed namespace and, for every offered
// object, a well-formed id and digest and a non-negative size. It carries no
// provenance to validate: a caller cannot assert a trust channel on a put, the
// provider derives it (see ObjectPutParams).
func ValidateObjectPutParams(p *ObjectPutParams) []diag.Diagnostic {
	if p == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "params are nil")}
	}
	diags := validateObjectNamespace("namespace", p.Namespace)
	if len(p.Objects) > MaxKeysPerRequest {
		diags = append(diags, diag.Errorf(CodeTooManyKeys, "objects",
			"%d objects exceeds the per-request limit of %d", len(p.Objects), MaxKeysPerRequest))
	}
	for i, obj := range p.Objects {
		field := fmt.Sprintf("objects[%d]", i)
		diags = append(diags, validateObjectID(field+".id", obj.ID)...)
		if !ValidDigest(obj.Digest) {
			diags = append(diags, diag.Errorf(CodeInvalidDigest, field+".digest", "malformed digest %q", obj.Digest))
		}
		diags = append(diags, validateObjectSize(field+".size", obj.Size)...)
	}
	return diags
}

// NormalizeObjectPutParams sorts the offered objects by id for deterministic
// output.
func NormalizeObjectPutParams(p *ObjectPutParams) *ObjectPutParams {
	if p == nil {
		return nil
	}
	p.Namespace = strings.TrimSpace(p.Namespace)
	sort.Slice(p.Objects, func(i, j int) bool { return p.Objects[i].ID < p.Objects[j].ID })
	return p
}

// ParseAndValidateObjectPutParams combines strict parsing, validation, and
// normalization.
func ParseAndValidateObjectPutParams(data []byte) (*ObjectPutParams, []diag.Diagnostic) {
	return parseAndValidate(data, ParseObjectPutParamsStrict, ValidateObjectPutParams, NormalizeObjectPutParams)
}

// --- object put result ---

// ParseObjectPutResultStrict decodes JSON into ObjectPutResult.
func ParseObjectPutResultStrict(data []byte) (*ObjectPutResult, []diag.Diagnostic) {
	return parseStrict[ObjectPutResult](data, "object put result")
}

// ValidateObjectPutResult checks a non-negative accepted count.
func ValidateObjectPutResult(r *ObjectPutResult) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(CodeRequiredField, "", "result is nil")}
	}
	if r.Accepted < 0 {
		return []diag.Diagnostic{diag.Errorf(CodeInvalidSize, "accepted",
			"accepted count %d must not be negative", r.Accepted)}
	}
	return nil
}

// ParseAndValidateObjectPutResult combines strict parsing and validation.
func ParseAndValidateObjectPutResult(data []byte) (*ObjectPutResult, []diag.Diagnostic) {
	return parseAndValidate(data, ParseObjectPutResultStrict, ValidateObjectPutResult, func(r *ObjectPutResult) *ObjectPutResult { return r })
}

// --- shared helpers ---

// ValidObjectNamespace reports whether s is a well-formed object-cache
// namespace: 1..MaxObjectNamespaceLength characters of [a-z0-9-]. The closed
// alphabet is what lets a provider use the namespace as a storage path segment
// without escaping it.
func ValidObjectNamespace(s string) bool {
	if s == "" || len(s) > MaxObjectNamespaceLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return true
}

// ValidObjectID reports whether s is a well-formed object id: non-empty
// lowercase hex, at most MaxObjectIDLength characters. Ids are opaque to the
// provider, so the only rule is a closed alphabet that is safe to use as a
// storage path segment and cannot smuggle a separator.
func ValidObjectID(s string) bool {
	if s == "" || len(s) > MaxObjectIDLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

func validateObjectNamespace(field, namespace string) []diag.Diagnostic {
	if !ValidObjectNamespace(namespace) {
		return []diag.Diagnostic{diag.Errorf(CodeInvalidNamespace, field,
			"object namespace %q must match [a-z0-9-]{1,%d}", namespace, MaxObjectNamespaceLength)}
	}
	return nil
}

func validateObjectID(field, id string) []diag.Diagnostic {
	if !ValidObjectID(id) {
		return []diag.Diagnostic{diag.Errorf(CodeInvalidObjectID, field,
			"object id %q must be non-empty lowercase hex of at most %d characters", id, MaxObjectIDLength)}
	}
	return nil
}

// validateObjectIDs reports a diagnostic for every malformed id in the list and
// one for a batch over the per-request limit.
func validateObjectIDs(field string, ids []string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if len(ids) > MaxKeysPerRequest {
		diags = append(diags, diag.Errorf(CodeTooManyKeys, field,
			"%d ids exceeds the per-request limit of %d", len(ids), MaxKeysPerRequest))
	}
	for i, id := range ids {
		diags = append(diags, validateObjectID(fmt.Sprintf("%s[%d]", field, i), id)...)
	}
	return diags
}

func validateObjectSize(field string, size int64) []diag.Diagnostic {
	if size < 0 {
		return []diag.Diagnostic{diag.Errorf(CodeInvalidSize, field, "object size %d must not be negative", size)}
	}
	return nil
}

// validateObjectProvenance checks an object hit's optional provenance pair.
// Unlike a task entry (validateEntryProvenance) an object carries no producer
// identity, and either field may stand alone: a channel-less provider omits
// both, and the trust decision only ever reads the channel.
func validateObjectProvenance(field string, producer Producer, channel Channel) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if producer != "" && !producer.Valid() {
		diags = append(diags, diag.Errorf(CodeInvalidProducer, field+".producer",
			"unknown cache-entry producer %q", producer))
	}
	if channel != "" && !channel.Valid() {
		diags = append(diags, diag.Errorf(CodeInvalidChannel, field+".channel",
			"unknown cache-entry channel %q", channel))
	}
	return diags
}

// validateDigestList reports a diagnostic for every malformed digest in the
// list, under the given field prefix.
func validateDigestList(field string, digests []string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for i, d := range digests {
		if !ValidDigest(d) {
			diags = append(diags, diag.Errorf(CodeInvalidDigest, fmt.Sprintf("%s[%d]", field, i),
				"malformed digest %q", d))
		}
	}
	return diags
}

// validateEntryProvenance checks the provider RPC's optional provenance triple.
// All fields may be absent for a v1/channel-less provider, but a provider that
// supplies any provenance field must supply a known producer and channel plus
// a non-blank opaque producer identity.
func validateEntryProvenance(field string, producer Producer, identity string, channel Channel) []diag.Diagnostic {
	if producer == "" && identity == "" && channel == "" {
		return nil
	}
	prefix := ""
	if field != "" {
		prefix = field + "."
	}
	var diags []diag.Diagnostic
	if producer == "" {
		diags = append(diags, diag.Errorf(CodeRequiredField, prefix+"producer", "producer is required when provenance is present"))
	} else if !producer.Valid() {
		diags = append(diags, diag.Errorf(CodeInvalidProducer, prefix+"producer", "unknown cache-entry producer %q", producer))
	}
	if strings.TrimSpace(identity) == "" {
		diags = append(diags, diag.Errorf(CodeRequiredField, prefix+"producerIdentity", "producer identity is required when provenance is present"))
	}
	if channel == "" {
		diags = append(diags, diag.Errorf(CodeRequiredField, prefix+"channel", "channel is required when provenance is present"))
	} else if !channel.Valid() {
		diags = append(diags, diag.Errorf(CodeInvalidChannel, prefix+"channel", "unknown cache-entry channel %q", channel))
	}
	return diags
}

// validateProviderMarkerKey checks the run-marker key fields shared by the
// lookup and write ops. Unlike validateRunMarkerKey it carries no protocol
// version (the provider envelope carries it).
func validateProviderMarkerKey(workspace, branch string, commands []string, selection RunMarkerSelection) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if strings.TrimSpace(workspace) == "" {
		diags = append(diags, diag.Errorf(CodeRequiredField, "workspace", "workspace is required"))
	}
	if strings.TrimSpace(branch) == "" {
		diags = append(diags, diag.Errorf(CodeRequiredField, "branch", "branch is required"))
	}
	if len(NormalizeRunMarkerCommands(commands)) == 0 {
		diags = append(diags, diag.Errorf(CodeRequiredField, "commands", "command list is required"))
	}
	if selection == "" || !selection.Valid() {
		diags = append(diags, diag.Errorf(CodeInvalidSelection, "selection", "unknown run marker selection %q", selection))
	}
	return diags
}
