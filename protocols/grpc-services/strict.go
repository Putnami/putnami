package grpcservices

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Stable diagnostic codes emitted by the v1 parser and validator.
const (
	ErrorCodeParseError              = "grpc_services.parse_error"
	ErrorCodeUnknownField            = "grpc_services.unknown_field"
	ErrorCodeInvalidProtocol         = "grpc_services.invalid_protocol"
	ErrorCodeInvalidSchema           = "grpc_services.invalid_schema"
	ErrorCodeInvalidDigest           = "grpc_services.invalid_digest"
	ErrorCodeDigestMismatch          = "grpc_services.digest_mismatch"
	ErrorCodeInvalidServiceName      = "grpc_services.invalid_service_name"
	ErrorCodeInvalidTransport        = "grpc_services.invalid_transport"
	ErrorCodeInvalidBasePath         = "grpc_services.invalid_base_path"
	ErrorCodeInvalidMethods          = "grpc_services.invalid_methods"
	ErrorCodeInvalidMethodName       = "grpc_services.invalid_method_name"
	ErrorCodeDuplicateMethod         = "grpc_services.duplicate_method"
	ErrorCodeMissingProvenance       = "grpc_services.missing_provenance"
	ErrorCodeInvalidSourceKind       = "grpc_services.invalid_source_kind"
	ErrorCodeSourceTransportMismatch = "grpc_services.source_transport_mismatch"
	ErrorCodeDuplicateService        = "grpc_services.duplicate_service"
	ErrorCodeSerialization           = "grpc_services.serialization_error"
)

// ValidErrorCodes is the closed diagnostic taxonomy shared by consumers and
// conformance fixtures.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError: true, ErrorCodeUnknownField: true,
	ErrorCodeInvalidProtocol: true, ErrorCodeInvalidSchema: true,
	ErrorCodeInvalidDigest: true, ErrorCodeDigestMismatch: true,
	ErrorCodeInvalidServiceName: true, ErrorCodeInvalidTransport: true,
	ErrorCodeInvalidBasePath: true, ErrorCodeInvalidMethods: true,
	ErrorCodeInvalidMethodName: true, ErrorCodeDuplicateMethod: true,
	ErrorCodeMissingProvenance: true, ErrorCodeInvalidSourceKind: true,
	ErrorCodeSourceTransportMismatch: true, ErrorCodeDuplicateService: true,
	ErrorCodeSerialization: true,
}

var (
	// identifierPattern is one protobuf identifier segment. A leading
	// underscore is accepted because protoc accepts it and a gRPC server
	// registers whatever name the descriptor carries.
	identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// serviceNamePattern is one or more identifier segments joined by '.'.
	serviceNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	// prefixSegmentPattern is one segment of the mount prefix in front of a
	// Connect service name: unreserved and non-pattern sub-delimiter
	// characters only, never a percent escape.
	prefixSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9._~!$&'+,;=@-]+$`)
	digestPattern        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

var validSourceKinds = map[SourceKind]bool{
	SourceGRPCServer: true, SourceConnectGateway: true, SourceAPIBridge: true,
}

// sourceTransport is the one transport each source kind serves.
var sourceTransport = map[SourceKind]Transport{
	SourceGRPCServer:     TransportGRPC,
	SourceConnectGateway: TransportConnect,
	SourceAPIBridge:      TransportConnect,
}

// ParseManifest strictly decodes a manifest. Unknown fields, trailing JSON
// values, and a missing or null services list, methods list, methodsKnown or
// streaming flag are rejected.
func ParseManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	type methodWire struct {
		Name            string `json:"name"`            // RPC name.
		ClientStreaming *bool  `json:"clientStreaming"` // Required boolean.
		ServerStreaming *bool  `json:"serverStreaming"` // Required boolean.
	}
	type serviceWire struct {
		Name         string        `json:"name"`         // Fully qualified service name.
		Transport    Transport     `json:"transport"`    // grpc or connect.
		BasePath     string        `json:"basePath"`     // Connect URL prefix.
		MethodsKnown *bool         `json:"methodsKnown"` // Required boolean.
		Methods      *[]methodWire `json:"methods"`      // Required array.
		Provenance   Provenance    `json:"provenance"`   // Registering surface.
	}
	type manifestWire struct {
		Schema   string         `json:"$schema"`  // Canonical schema reference.
		Protocol string         `json:"protocol"` // Wire contract identifier.
		Services *[]serviceWire `json:"services"` // Required array.
		Digest   string         `json:"digest"`   // Canonical digest.
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire manifestWire
	if err := dec.Decode(&wire); err != nil {
		return nil, []diag.Diagnostic{decodeDiagnostic(err)}
	}
	if err := ensureEOF(dec); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "%v", err)}
	}
	if wire.Services == nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "services",
			"services is required and must be an array")}
	}

	manifest := Manifest{
		Schema:   wire.Schema,
		Protocol: wire.Protocol,
		Services: make([]Service, len(*wire.Services)),
		Digest:   wire.Digest,
	}
	for i, service := range *wire.Services {
		field := fmt.Sprintf("services[%d]", i)
		if service.MethodsKnown == nil {
			return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, field+".methodsKnown",
				"methodsKnown is required and must be a boolean")}
		}
		if service.Methods == nil {
			return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, field+".methods",
				"methods is required and must be an array")}
		}
		methods := make([]Method, len(*service.Methods))
		for j, method := range *service.Methods {
			methodField := fmt.Sprintf("%s.methods[%d]", field, j)
			if method.ClientStreaming == nil {
				return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, methodField+".clientStreaming",
					"clientStreaming is required and must be a boolean")}
			}
			if method.ServerStreaming == nil {
				return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, methodField+".serverStreaming",
					"serverStreaming is required and must be a boolean")}
			}
			methods[j] = Method{
				Name:            method.Name,
				ClientStreaming: *method.ClientStreaming,
				ServerStreaming: *method.ServerStreaming,
			}
		}
		manifest.Services[i] = Service{
			Name:         service.Name,
			Transport:    service.Transport,
			BasePath:     service.BasePath,
			MethodsKnown: *service.MethodsKnown,
			Methods:      methods,
			Provenance:   service.Provenance,
		}
	}
	return &manifest, nil
}

// ParseAndValidateManifest strictly parses and validates a manifest, including
// recomputing its digest from the canonical service inventory.
func ParseAndValidateManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	manifest, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	diags = ValidateManifest(manifest)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return manifest, nil
}

// ValidateManifest validates the envelope and every service, then verifies
// the digest against the canonicalized inventory.
func ValidateManifest(manifest *Manifest) []diag.Diagnostic {
	if manifest == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "manifest is nil")}
	}
	var diags []diag.Diagnostic
	if manifest.Schema != SchemaURL {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchema, "$schema",
			"$schema %q is not the canonical v1 schema %q", manifest.Schema, SchemaURL))
	}
	if manifest.Protocol != Protocol {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidProtocol, "protocol",
			"protocol %q is not supported (want %q)", manifest.Protocol, Protocol))
	}
	diags = append(diags, ValidateServices(manifest.Services)...)
	if !digestPattern.MatchString(manifest.Digest) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDigest, "digest",
			"digest must be sha256 followed by 64 lowercase hexadecimal characters"))
	} else if !diag.HasErrors(diags) {
		canonical, canonicalDiags := Canonicalize(manifest.Services)
		if diag.HasErrors(canonicalDiags) {
			diags = append(diags, canonicalDiags...)
		} else if manifest.Digest != canonical.Digest {
			diags = append(diags, diag.Errorf(ErrorCodeDigestMismatch, "digest",
				"digest %q does not match canonical inventory digest %q", manifest.Digest, canonical.Digest))
		}
	}
	return diags
}

// ValidateServices validates each service, then rejects two services that
// share a name, transport and basePath. Findings are returned in stable input
// order.
func ValidateServices(services []Service) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for i, service := range services {
		diags = append(diags, validateService(fmt.Sprintf("services[%d]", i), service)...)
	}
	if diag.HasErrors(diags) {
		return diags
	}
	first := make(map[string]int, len(services))
	for i, service := range services {
		key := service.Name + "\x00" + string(service.Transport) + "\x00" + service.BasePath
		if earlier, dup := first[key]; dup {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicateService, fmt.Sprintf("services[%d]", i),
				"service duplicates services[%d]: name, transport and basePath identify one served service", earlier))
			continue
		}
		first[key] = i
	}
	return diags
}

func validateService(field string, service Service) []diag.Diagnostic {
	var diags []diag.Diagnostic
	nameValid := serviceNamePattern.MatchString(service.Name)
	if !nameValid {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidServiceName, field+".name",
			"service name %q must be one or more identifiers [A-Za-z_][A-Za-z0-9_]* joined by '.'", service.Name))
	}
	transportValid := service.Transport == TransportGRPC || service.Transport == TransportConnect
	if !transportValid {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".transport",
			"transport %q is not in the v1 set: grpc, connect", service.Transport))
	} else {
		diags = append(diags, validateBasePath(field+".basePath", service, nameValid)...)
	}

	if !service.MethodsKnown && len(service.Methods) > 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidMethods, field+".methods",
			"methods must be empty when methodsKnown is false"))
	}
	seen := make(map[string]bool, len(service.Methods))
	for i, method := range service.Methods {
		methodField := fmt.Sprintf("%s.methods[%d].name", field, i)
		if !identifierPattern.MatchString(method.Name) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidMethodName, methodField,
				"method name %q must match [A-Za-z_][A-Za-z0-9_]*", method.Name))
		}
		if seen[method.Name] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicateMethod, methodField,
				"method %q appears more than once", method.Name))
		}
		seen[method.Name] = true
	}

	provenance := service.Provenance
	if strings.TrimSpace(provenance.Project) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingProvenance, field+".provenance.project",
			"provenance.project is required"))
	}
	if strings.TrimSpace(provenance.Package) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingProvenance, field+".provenance.package",
			"provenance.package is required"))
	}
	if !validSourceKinds[provenance.SourceKind] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSourceKind, field+".provenance.sourceKind",
			"sourceKind %q is not in the v1 set: grpc-server, connect-gateway, api-bridge", provenance.SourceKind))
	} else if transportValid && sourceTransport[provenance.SourceKind] != service.Transport {
		diags = append(diags, diag.Errorf(ErrorCodeSourceTransportMismatch, field+".provenance.sourceKind",
			"sourceKind %q serves transport %q, not %q",
			provenance.SourceKind, sourceTransport[provenance.SourceKind], service.Transport))
	}
	return diags
}

// validateBasePath enforces the transport's basePath rule. A connect basePath
// is an absolute '/'-terminated path ending in "/<name>/", optionally behind a
// mount prefix of plain segments. The rule names only this document: whether a
// route of the HTTP inventory matches basePath is a join a consumer makes.
func validateBasePath(field string, service Service, nameValid bool) []diag.Diagnostic {
	basePath := service.BasePath
	if service.Transport == TransportGRPC {
		if basePath != "" {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidBasePath, field,
				"basePath is forbidden for transport grpc: a native gRPC service has no HTTP path in this inventory")}
		}
		return nil
	}
	if basePath == "" {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidBasePath, field,
			"basePath is required for transport connect")}
	}
	if !nameValid {
		return nil
	}
	suffix := "/" + service.Name + "/"
	prefix, ok := strings.CutSuffix(basePath, suffix)
	if !ok {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidBasePath, field,
			"basePath %q must end in %q, the Connect URL prefix of the service", basePath, suffix)}
	}
	if prefix == "" {
		return nil
	}
	if prefix[0] != '/' {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidBasePath, field,
			"basePath %q must be absolute and begin with '/'", basePath)}
	}
	for segment := range strings.SplitSeq(prefix[1:], "/") {
		if segment == "." || segment == ".." || !prefixSegmentPattern.MatchString(segment) {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidBasePath, field,
				"basePath %q has a mount prefix segment %q that is empty, a dot segment, or not plain unreserved characters", basePath, segment)}
		}
	}
	return nil
}

func decodeDiagnostic(err error) diag.Diagnostic {
	message := err.Error()
	if rest, ok := strings.CutPrefix(message, "json: unknown field "); ok {
		return diag.Errorf(ErrorCodeUnknownField, strings.Trim(rest, `"`), "%s", message)
	}
	return diag.Errorf(ErrorCodeParseError, "", "%s", message)
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("manifest contains more than one JSON value")
}
