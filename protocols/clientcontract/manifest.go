package clientcontract

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	// GeneratedManifestFile is the target-relative generated client inventory.
	GeneratedManifestFile = "client.putnami.json"
	// GeneratedManifestSchemaURL identifies the generated client manifest schema.
	GeneratedManifestSchemaURL = "https://putnami.dev/schemas/generated-client-manifest-v1.json"
	// GeneratedBy identifies artifacts emitted by the first-party generator.
	GeneratedBy = "@putnami/clientgen"
)

// GeneratedLanguage identifies a supported generated client language.
type GeneratedLanguage string

// Supported generated client languages.
const (
	GeneratedLanguageGo         GeneratedLanguage = "go"
	GeneratedLanguageTypeScript GeneratedLanguage = "ts"
)

// GeneratedClientManifestV1 inventories one generated target without secrets.
type GeneratedClientManifestV1 struct {
	// ProtocolVersion selects the client-contract compatibility version.
	ProtocolVersion int `json:"protocolVersion"`
	// GeneratedBy carries the required generator marker.
	GeneratedBy string `json:"generatedBy"`
	// Language identifies the generated target language.
	Language GeneratedLanguage `json:"language"`
	// Service repeats the immutable provider identity and audience.
	Service Service `json:"service"`
	// Binding records the generated public import and registration symbols.
	Binding GeneratedBinding `json:"binding"`
	// ContractSHA256 binds the target to the exact provider contract bytes.
	ContractSHA256 string `json:"contractSha256"`
	// Operations inventories generated operations in operation-ID order.
	Operations []GeneratedOperation `json:"operations"`
	// OmittedOperations names, in ascending order, the contract operations the
	// target's configuration leaves out — a Go target's go.omitOperations — so
	// they have no generated method. Operations and OmittedOperations together
	// account for every contract operation, and no operation ID is in both. It
	// is absent when the target generates every operation.
	OmittedOperations []string `json:"omittedOperations,omitempty"`
	// RuntimeCapabilities lists, in ascending order, the client runtime
	// capabilities the generated target cannot run without. It is derived from
	// the operations (see RequiredRuntimeCapabilities) and is absent when the
	// target needs nothing beyond the base runtime. The generated code pins the
	// same list against the runtime it is compiled or loaded with.
	RuntimeCapabilities []RuntimeCapability `json:"runtimeCapabilities,omitempty"`
	// Files inventories generated files in target-relative path order.
	Files []GeneratedFile `json:"files"`
}

// RuntimeCapability names one behavior a generated client needs from the
// client runtime beyond the base protocol. The vocabulary is closed. A
// capability this package names but the runtime of a target's language does not
// implement (RuntimeCapabilitiesImplementedBy) is one that runtime does not
// honor yet, so a target that requires it is refused rather than run without
// the declared behavior.
type RuntimeCapability string

// Runtime capabilities a generated target can require.
const (
	// RuntimeCapabilityResponseCache is the per-operation response cache:
	// fresh answers from memory, singleflight on a miss, serve-stale on a
	// provider failure, and invalidation by key prefix.
	RuntimeCapabilityResponseCache RuntimeCapability = "response-cache"
	// RuntimeCapabilitySSEContinuation is the negotiated SSE wire and its
	// continuation (ADR 0013): the wire marker and explicit terminal, the
	// position of the last delivered message, and one session across the
	// reopened connections.
	RuntimeCapabilitySSEContinuation RuntimeCapability = "sse-continuation"
)

// ImplementedRuntimeCapabilities is the closed set of capabilities that both
// the Go and the TypeScript client runtimes released with this protocol version
// implement. The generated manifest schema enumerates exactly this set.
//
// One runtime may implement a capability before the other: the manifest of a
// target is validated against the runtime of its own language
// (RuntimeCapabilitiesImplementedBy), so a Go target may require a capability
// the TypeScript runtime does not have yet, and a TypeScript target that needs
// it is still refused. The set grows when the last runtime catches up.
var ImplementedRuntimeCapabilities = commonRuntimeCapabilities()

// runtimeCapabilitiesByLanguage is what each client runtime implements, in
// ascending order. Each runtime pins its own list to its row in its tests.
//
// Both runtimes implement SSE continuation (ADR 0013); the rows stay separate
// so the next capability can land in one runtime before the other.
var runtimeCapabilitiesByLanguage = map[GeneratedLanguage][]RuntimeCapability{
	GeneratedLanguageGo:         {RuntimeCapabilityResponseCache, RuntimeCapabilitySSEContinuation},
	GeneratedLanguageTypeScript: {RuntimeCapabilityResponseCache, RuntimeCapabilitySSEContinuation},
}

// RuntimeCapabilitiesImplementedBy returns, in ascending order, the capabilities
// the client runtime of language implements. An unknown language implements
// none.
func RuntimeCapabilitiesImplementedBy(language GeneratedLanguage) []RuntimeCapability {
	return append([]RuntimeCapability(nil), runtimeCapabilitiesByLanguage[language]...)
}

// commonRuntimeCapabilities is the ascending intersection of every runtime's
// capabilities.
func commonRuntimeCapabilities() []RuntimeCapability {
	var common []RuntimeCapability
	for _, capability := range runtimeCapabilitiesByLanguage[GeneratedLanguageGo] {
		shared := true
		for _, capabilities := range runtimeCapabilitiesByLanguage {
			shared = shared && slices.Contains(capabilities, capability)
		}
		if shared {
			common = append(common, capability)
		}
	}
	return common
}

// RequiredRuntimeCapabilities derives the capabilities a set of generated
// operations requires, in ascending order: the response cache when an
// operation declares a cache policy, and SSE continuation when one of its
// transports declares a continuation.
func RequiredRuntimeCapabilities(operations []GeneratedOperation) []RuntimeCapability {
	cache, continuation := false, false
	for _, operation := range operations {
		cache = cache || operation.Cache != nil
		for _, transport := range operation.Transports {
			continuation = continuation || transport.Continuation() != nil
		}
	}
	var required []RuntimeCapability
	if cache {
		required = append(required, RuntimeCapabilityResponseCache)
	}
	if continuation {
		required = append(required, RuntimeCapabilitySSEContinuation)
	}
	return required
}

func implementedRuntimeCapability(language GeneratedLanguage, capability RuntimeCapability) bool {
	return slices.Contains(runtimeCapabilitiesByLanguage[language], capability)
}

// GeneratedBinding identifies the public import and typed registration symbols.
type GeneratedBinding struct {
	// ImportPath is the public package or module import path.
	ImportPath string `json:"importPath"`
	// Clients lists generated service registrations in canonical order.
	Clients []GeneratedBindingClient `json:"clients"`
}

// GeneratedBindingClient identifies one actual generated service registration.
type GeneratedBindingClient struct {
	// Service is the provider service name represented by this client.
	Service string `json:"service"`
	// ClientSymbol is the generated public client type or value name.
	ClientSymbol string `json:"clientSymbol"`
	// BindingSymbol is the generated typed registration symbol.
	BindingSymbol string `json:"bindingSymbol"`
}

// GeneratedOperation inventories one operation and its supported transports.
type GeneratedOperation struct {
	// OperationID is the provider-authored stable operation identity.
	OperationID string `json:"operationId"`
	// Service is the provider service that owns the operation.
	Service string `json:"service"`
	// MethodSymbol is the generated public method name.
	MethodSymbol string `json:"methodSymbol"`
	// Stream records the operation's message cardinality.
	Stream StreamMode `json:"stream"`
	// Transports preserves the provider's advertised transport order.
	Transports []Transport `json:"transports"`
	// Cache repeats the operation's declared response cache policy, so the
	// inventory states which operations a consumer may answer from memory.
	Cache *CachePolicy `json:"cache,omitempty"`
}

// GeneratedFile binds a generated target-relative path to its exact bytes.
type GeneratedFile struct {
	// Path is the canonical target-relative generated path.
	Path string `json:"path"`
	// SHA256 is the lowercase digest of the exact generated bytes.
	SHA256 string `json:"sha256"`
}

// GeneratedClientReference is the identity half of a committed generated client
// manifest: which provider service the target was generated from, at which
// exact contract bytes, in which language. It is what a workspace graph needs
// to place a generated client next to its provider, and nothing else.
//
// It is decoded TOLERANTLY, unlike ParseAndValidateGeneratedManifest: a member
// a later protocol version adds must not turn a readable reference into an
// unreadable one. A graph that drops an edge under-selects and serves a stale
// verdict; a graph that keeps one only over-orders.
type GeneratedClientReference struct {
	// Language is the target language exactly as the manifest spells it. It is
	// not restricted here: an unknown language still names both ends of the
	// edge, and the reader that generates for a language judges it.
	Language GeneratedLanguage
	// ServiceID is the provider service identity the target was generated from.
	ServiceID string
	// ContractSHA256 is the digest of the exact contract bytes the target was
	// generated at. It is the ONE provider-side input a generated target's own
	// actions read.
	ContractSHA256 string
}

// DecodeGeneratedClientReference reads the identity half of a committed
// generated client manifest.
//
// ok is false when the bytes do not parse, when the generator marker is not
// this generator's, or when the service identity or the contract digest is
// missing or malformed: a reference that cannot name both ends of the edge, or
// that no first-party generator wrote, is not one.
func DecodeGeneratedClientReference(data []byte) (GeneratedClientReference, bool) {
	var manifest struct {
		ProtocolVersion int               `json:"protocolVersion"`
		GeneratedBy     string            `json:"generatedBy"`
		Language        GeneratedLanguage `json:"language"`
		Service         Service           `json:"service"`
		ContractSHA256  string            `json:"contractSha256"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return GeneratedClientReference{}, false
	}
	if manifest.GeneratedBy != GeneratedBy || manifest.ProtocolVersion < 1 {
		return GeneratedClientReference{}, false
	}
	if blank(manifest.Service.ID) || !isSHA256(manifest.ContractSHA256) {
		return GeneratedClientReference{}, false
	}
	return GeneratedClientReference{
		Language:       manifest.Language,
		ServiceID:      strings.TrimSpace(manifest.Service.ID),
		ContractSHA256: manifest.ContractSHA256,
	}, true
}

// ParseAndValidateGeneratedManifest strictly decodes and validates an inventory.
func ParseAndValidateGeneratedManifest(data []byte) (*GeneratedClientManifestV1, []diag.Diagnostic) {
	var manifest GeneratedClientManifestV1
	if err := decodeStrict(data, &manifest); err != nil {
		return nil, []diag.Diagnostic{decodeDiagnostic(err)}
	}
	return &manifest, ValidateGeneratedManifest(&manifest)
}

// ValidateGeneratedManifest validates a generated client inventory.
func ValidateGeneratedManifest(manifest *GeneratedClientManifestV1) []diag.Diagnostic {
	if manifest == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "generated client manifest is nil")}
	}
	var diags []diag.Diagnostic
	if manifest.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidVersion, "protocolVersion",
			"protocolVersion %d is unsupported; expected %d", manifest.ProtocolVersion, ProtocolVersion))
	}
	if manifest.GeneratedBy != GeneratedBy {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidGeneratedManifest, "generatedBy",
			"generatedBy must be %q", GeneratedBy))
	}
	if manifest.Language != GeneratedLanguageGo && manifest.Language != GeneratedLanguageTypeScript {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidGeneratedManifest, "language",
			"language %q is unsupported", manifest.Language))
	}
	if blank(manifest.Service.ID) {
		diags = append(diags, required("service.id"))
	}
	if blank(manifest.Service.Audience) {
		diags = append(diags, required("service.audience"))
	}
	if blank(manifest.Binding.ImportPath) {
		diags = append(diags, required("binding.importPath"))
	}
	if len(manifest.Binding.Clients) == 0 {
		diags = append(diags, required("binding.clients"))
	}
	lastBinding := ""
	bindingServices := map[string]bool{}
	for i, binding := range manifest.Binding.Clients {
		field := fmt.Sprintf("binding.clients[%d]", i)
		if blank(binding.Service) {
			diags = append(diags, required(field+".service"))
		}
		if blank(binding.ClientSymbol) {
			diags = append(diags, required(field+".clientSymbol"))
		}
		if blank(binding.BindingSymbol) {
			diags = append(diags, required(field+".bindingSymbol"))
		}
		key := binding.Service + "\x00" + binding.ClientSymbol
		if i > 0 && key <= lastBinding {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidGeneratedManifest, field,
				"binding clients must be in ascending service and clientSymbol order"))
		}
		lastBinding = key
		bindingServices[binding.Service] = true
	}
	if !isSHA256(manifest.ContractSHA256) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidGeneratedManifest, "contractSha256",
			"contractSha256 must be 64 lowercase hexadecimal characters"))
	}
	if manifest.Operations == nil {
		diags = append(diags, required("operations"))
	}
	if manifest.Files == nil {
		diags = append(diags, required("files"))
	}

	operationIDs := map[string]bool{}
	lastOperationID := ""
	for i, operation := range manifest.Operations {
		field := fmt.Sprintf("operations[%d]", i)
		if blank(operation.OperationID) {
			diags = append(diags, required(field+".operationId"))
		}
		if blank(operation.Service) {
			diags = append(diags, required(field+".service"))
		} else if !bindingServices[operation.Service] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidGeneratedManifest, field+".service",
				"operation service %q has no generated binding client", operation.Service))
		}
		if blank(operation.MethodSymbol) {
			diags = append(diags, required(field+".methodSymbol"))
		}
		if operationIDs[operation.OperationID] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicate, field+".operationId",
				"operationId %q appears more than once", operation.OperationID))
		}
		if i > 0 && operation.OperationID <= lastOperationID {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidGeneratedManifest, field+".operationId",
				"operations must be in ascending operationId order"))
		}
		operationIDs[operation.OperationID] = true
		lastOperationID = operation.OperationID
		if !validStream(operation.Stream) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidEnum, field+".stream",
				"stream mode %q is unsupported", operation.Stream))
		}
		diags = append(diags, validateTransports(operation.Stream, operation.Transports)...)
		if operation.Cache != nil {
			diags = append(diags, validateCachePolicy(field+".cache", operation.Cache)...)
			if operation.Stream != StreamUnary {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, field+".cache",
					"a response cache requires a unary operation; stream mode %q has no single answer to store", operation.Stream))
			}
		}
	}
	for i, operationID := range manifest.OmittedOperations {
		field := fmt.Sprintf("omittedOperations[%d]", i)
		if blank(operationID) {
			diags = append(diags, required(field))
		}
		if operationIDs[operationID] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidGeneratedManifest, field,
				"operationId %q is both generated and omitted", operationID))
		}
		if i > 0 && operationID <= manifest.OmittedOperations[i-1] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidGeneratedManifest, field,
				"omitted operations must be unique and in ascending operationId order"))
		}
	}
	diags = append(diags, validateRuntimeCapabilities(manifest)...)

	filePaths := map[string]bool{}
	lastFilePath := ""
	for i, file := range manifest.Files {
		field := fmt.Sprintf("files[%d]", i)
		if err := validateGeneratedPath(file.Path); err != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidGeneratedManifest, field+".path", "%v", err))
		}
		if filePaths[file.Path] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicate, field+".path",
				"generated path %q appears more than once", file.Path))
		}
		if i > 0 && file.Path <= lastFilePath {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidGeneratedManifest, field+".path",
				"files must be in ascending path order"))
		}
		filePaths[file.Path] = true
		lastFilePath = file.Path
		if !isSHA256(file.SHA256) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidGeneratedManifest, field+".sha256",
				"sha256 must be 64 lowercase hexadecimal characters"))
		}
	}
	return diags
}

// validateRuntimeCapabilities refuses a manifest whose operations declare a
// behavior its runtime requirement does not name, or that names a capability
// the runtime of its language does not implement in this protocol version.
// Either way a consumer would run the target without the declared behavior,
// silently.
func validateRuntimeCapabilities(manifest *GeneratedClientManifestV1) []diag.Diagnostic {
	var diags []diag.Diagnostic
	declared := map[RuntimeCapability]bool{}
	for i, capability := range manifest.RuntimeCapabilities {
		field := fmt.Sprintf("runtimeCapabilities[%d]", i)
		if !implementedRuntimeCapability(manifest.Language, capability) {
			diags = append(diags, diag.Errorf(ErrorCodeUnsupportedRuntimeCapability, field,
				"runtime capability %q is not implemented by the %s client runtime of this protocol version",
				capability, manifest.Language))
		}
		if i > 0 && capability <= manifest.RuntimeCapabilities[i-1] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidGeneratedManifest, field,
				"runtime capabilities must be unique and in ascending order"))
		}
		declared[capability] = true
	}
	required := RequiredRuntimeCapabilities(manifest.Operations)
	for _, capability := range required {
		if !declared[capability] {
			diags = append(diags, diag.Errorf(ErrorCodeUnsupportedRuntimeCapability, "runtimeCapabilities",
				"operations declare %s but the target does not require the %q runtime capability", requiredBy(capability), capability))
		}
	}
	for _, capability := range manifest.RuntimeCapabilities {
		needed := false
		for _, want := range required {
			needed = needed || want == capability
		}
		if !needed && implementedRuntimeCapability(manifest.Language, capability) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidGeneratedManifest, "runtimeCapabilities",
				"runtime capability %q is required by no generated operation", capability))
		}
	}
	return diags
}

// requiredBy names the declaration that makes a capability required.
func requiredBy(capability RuntimeCapability) string {
	if capability == RuntimeCapabilitySSEContinuation {
		return "an sse continuation"
	}
	return "a response cache policy"
}

func isSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateGeneratedPath(value string) error {
	if blank(value) || strings.ContainsAny(value, "\\:") || path.IsAbs(value) {
		return fmt.Errorf("generated file path must be a non-empty relative slash path")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("generated file path must not contain control characters")
		}
	}
	clean := path.Clean(value)
	if clean != value || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("generated file path %q escapes or is not canonical", value)
	}
	if value == GeneratedManifestFile {
		return fmt.Errorf("generated manifest must not hash itself")
	}
	return nil
}

// SortGeneratedManifest canonicalizes the order-independent inventories.
func SortGeneratedManifest(manifest *GeneratedClientManifestV1) {
	if manifest == nil {
		return
	}
	sort.Slice(manifest.Operations, func(i, j int) bool {
		return manifest.Operations[i].OperationID < manifest.Operations[j].OperationID
	})
	sort.Strings(manifest.OmittedOperations)
	sort.Slice(manifest.Binding.Clients, func(i, j int) bool {
		a, b := manifest.Binding.Clients[i], manifest.Binding.Clients[j]
		return a.Service+"\x00"+a.ClientSymbol < b.Service+"\x00"+b.ClientSymbol
	})
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
}
