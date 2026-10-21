package grpcservices

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Canonicalize validates and normalizes a service inventory, sorts services
// and methods deterministically, and computes the protocol digest. The input
// is never mutated. Two services with the same name, transport and basePath
// are an error, whatever their other fields: a producer that observes one
// service twice collapses the identical copies before it calls Canonicalize.
func Canonicalize(services []Service) (*Manifest, []diag.Diagnostic) {
	canonical := &Manifest{
		Schema:   SchemaURL,
		Protocol: Protocol,
		Services: cloneServices(services),
	}
	sortServices(canonical.Services)

	diags := ValidateServices(canonical.Services)
	if diag.HasErrors(diags) {
		return nil, diags
	}

	digest, err := computeDigest(canonical.Services)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeSerialization, "", "serialize digest projection: %v", err)}
	}
	canonical.Digest = digest
	return canonical, nil
}

// CanonicalizeManifest canonicalizes the services in m. It requires the
// protocol identifier when one is supplied, and recomputes the schema
// reference and digest.
func CanonicalizeManifest(m *Manifest) (*Manifest, []diag.Diagnostic) {
	if m == nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "manifest is nil")}
	}
	if m.Protocol != "" && m.Protocol != Protocol {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocol, "protocol",
			"protocol %q is not supported (want %q)", m.Protocol, Protocol)}
	}
	return Canonicalize(m.Services)
}

// CanonicalJSON returns the normative byte form of m after canonicalization:
// two-space JSON indentation, stable field, service and method order, and one
// trailing newline.
func CanonicalJSON(m *Manifest) ([]byte, []diag.Diagnostic) {
	canonical, diags := CanonicalizeManifest(m)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	data, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeSerialization, "", "serialize canonical manifest: %v", err)}
	}
	return append(data, '\n'), nil
}

// cloneServices deep-copies services, gives every service a non-nil method
// list so it serializes as [], and sorts each method list by name.
func cloneServices(services []Service) []Service {
	out := make([]Service, len(services))
	for i, service := range services {
		out[i] = service
		out[i].Methods = append([]Method{}, service.Methods...)
		sort.SliceStable(out[i].Methods, func(a, b int) bool {
			return out[i].Methods[a].Name < out[i].Methods[b].Name
		})
	}
	return out
}

func sortServices(services []Service) {
	sort.SliceStable(services, func(i, j int) bool {
		return serviceLess(services[i], services[j])
	})
}

// serviceLess orders services by name, then transport, then basePath, each
// compared bytewise. The triple identifies a service, so the order is total
// over every inventory that validates.
func serviceLess(a, b Service) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.Transport != b.Transport {
		return a.Transport < b.Transport
	}
	return a.BasePath < b.BasePath
}

// digestProjection is the document the digest hashes: the manifest without its
// schema reference and digest.
type digestProjection struct {
	// Protocol is the v1 wire contract identifier (Protocol).
	Protocol string `json:"protocol"`
	// Services is the canonical, sorted service inventory.
	Services []Service `json:"services"`
}

func computeDigest(services []Service) (string, error) {
	projection := digestProjection{Protocol: Protocol, Services: services}
	data, err := json.MarshalIndent(projection, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
