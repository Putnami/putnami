package httproutes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Canonicalize validates and normalizes a route inventory, sorts routes and
// methods deterministically, and computes the protocol digest. The input is
// never mutated. Existing Schema, Protocol, and Digest values are ignored so
// callers can build an artifact directly from framework-native route facts.
func Canonicalize(routes []Route) (*Manifest, []diag.Diagnostic) {
	canonical := &Manifest{
		Schema:   SchemaURL,
		Protocol: Protocol,
		Routes:   cloneRoutes(routes),
	}
	normalizeMethods(canonical.Routes)
	sortRoutes(canonical.Routes)

	diags := ValidateRoutes(canonical.Routes)
	if diag.HasErrors(diags) {
		return nil, diags
	}

	digest, err := computeDigest(canonical.Routes)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeSerialization, "", "serialize digest projection: %v", err)}
	}
	canonical.Digest = digest
	return canonical, nil
}

// CanonicalizeManifest canonicalizes the route facts in m. It requires the
// protocol identifier when one is supplied, but deliberately recomputes the
// schema reference and digest.
func CanonicalizeManifest(m *Manifest) (*Manifest, []diag.Diagnostic) {
	if m == nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "manifest is nil")}
	}
	if m.Protocol != "" && m.Protocol != Protocol {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocol, "protocol",
			"protocol %q is not supported (want %q)", m.Protocol, Protocol)}
	}
	return Canonicalize(m.Routes)
}

// CanonicalJSON returns the normative byte form of m after canonicalization:
// two-space JSON indentation, stable field/route/method order, and one trailing
// newline.
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

func cloneRoutes(routes []Route) []Route {
	if len(routes) == 0 {
		return []Route{}
	}
	out := make([]Route, len(routes))
	for i, route := range routes {
		out[i] = route
		out[i].Methods = append([]string(nil), route.Methods...)
	}
	return out
}

func normalizeMethods(routes []Route) {
	for i := range routes {
		for j := range routes[i].Methods {
			routes[i].Methods[j] = strings.ToUpper(routes[i].Methods[j])
		}
		sort.Strings(routes[i].Methods)
	}
}

func sortRoutes(routes []Route) {
	sort.SliceStable(routes, func(i, j int) bool {
		return routeSortKey(routes[i]) < routeSortKey(routes[j])
	})
}

func routeSortKey(route Route) string {
	matchRank := "2"
	switch route.Match {
	case MatchExact:
		matchRank = "0"
	case MatchTemplate:
		matchRank = "1"
	}
	public := "0"
	if route.PublicEdge {
		public = "1"
	}
	return semanticPath(route) + "\x00" + matchRank + "\x00" + route.Path + "\x00" +
		strings.Join(route.Methods, ",") + "\x00" + public + "\x00" +
		route.Provenance.Project + "\x00" + route.Provenance.Package + "\x00" +
		string(route.Provenance.SourceKind) + "\x00" + route.Provenance.EvidencePath
}

type digestProjection struct {
	Protocol string  `json:"protocol"`
	Routes   []Route `json:"routes"`
}

func computeDigest(routes []Route) (string, error) {
	projection := digestProjection{Protocol: Protocol, Routes: routes}
	data, err := json.MarshalIndent(projection, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
