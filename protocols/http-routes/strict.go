package httproutes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Stable diagnostic codes emitted by the v1 parser and validator.
const (
	ErrorCodeParseError          = "http_routes.parse_error"
	ErrorCodeUnknownField        = "http_routes.unknown_field"
	ErrorCodeInvalidProtocol     = "http_routes.invalid_protocol"
	ErrorCodeInvalidSchema       = "http_routes.invalid_schema"
	ErrorCodeInvalidDigest       = "http_routes.invalid_digest"
	ErrorCodeDigestMismatch      = "http_routes.digest_mismatch"
	ErrorCodeInvalidMatch        = "http_routes.invalid_match"
	ErrorCodeInvalidPath         = "http_routes.invalid_path"
	ErrorCodeUnsupportedPattern  = "http_routes.unsupported_pattern"
	ErrorCodeInvalidMethod       = "http_routes.invalid_method"
	ErrorCodeDuplicateMethod     = "http_routes.duplicate_method"
	ErrorCodeMissingProvenance   = "http_routes.missing_provenance"
	ErrorCodeInvalidSourceKind   = "http_routes.invalid_source_kind"
	ErrorCodeInvalidEvidencePath = "http_routes.invalid_evidence_path"
	ErrorCodeDuplicateRoute      = "http_routes.duplicate_route"
	ErrorCodeVisibilityOverlap   = "http_routes.visibility_overlap"
	ErrorCodeSerialization       = "http_routes.serialization_error"
	ErrorCodeInvalidProof        = "http_routes.invalid_proof"
)

// ValidErrorCodes is the closed diagnostic taxonomy shared by consumers and
// conformance fixtures.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError: true, ErrorCodeUnknownField: true,
	ErrorCodeInvalidProtocol: true, ErrorCodeInvalidSchema: true,
	ErrorCodeInvalidDigest: true, ErrorCodeDigestMismatch: true,
	ErrorCodeInvalidMatch: true, ErrorCodeInvalidPath: true,
	ErrorCodeUnsupportedPattern: true, ErrorCodeInvalidMethod: true,
	ErrorCodeDuplicateMethod: true, ErrorCodeMissingProvenance: true,
	ErrorCodeInvalidSourceKind: true, ErrorCodeInvalidEvidencePath: true,
	ErrorCodeDuplicateRoute: true, ErrorCodeVisibilityOverlap: true,
	ErrorCodeSerialization: true, ErrorCodeInvalidProof: true,
}

var (
	parameterNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	digestPattern        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// featureIDPattern and requirementIDPattern are the feature and segment ID
	// grammars of go.putnami.dev/protocol/features, which a test binding uses
	// for the same two keys. They are copied, not imported: this module depends
	// on the features module only in its tests.
	featureIDPattern     = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*(?:/[a-z0-9]+(?:-[a-z0-9]+)*)+$`)
	requirementIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

var validMethods = map[string]bool{
	"DELETE":  true,
	"GET":     true,
	"HEAD":    true,
	"OPTIONS": true,
	"PATCH":   true,
	"POST":    true,
	"PUT":     true,
}

var validSourceKinds = map[SourceKind]bool{
	SourceTypedAPI: true, SourceFileRoute: true, SourceStaticMount: true,
	SourcePublicFile: true, SourceManual: true,
}

// ParseManifest strictly decodes a manifest. Unknown fields and trailing JSON
// values are rejected.
func ParseManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	type routeWire struct {
		Match      MatchKind       `json:"match"`
		Path       string          `json:"path"`
		Methods    []string        `json:"methods"`
		PublicEdge *bool           `json:"publicEdge"`
		Provenance Provenance      `json:"provenance"`
		Proves     json.RawMessage `json:"proves"`
	}
	type manifestWire struct {
		Schema   string          `json:"$schema"`
		Protocol string          `json:"protocol"`
		Routes   []routeWire     `json:"routes"`
		Digest   json.RawMessage `json:"digest"`
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

	var digest string
	if wire.Digest != nil {
		if err := json.Unmarshal(wire.Digest, &digest); err != nil || bytes.Equal(bytes.TrimSpace(wire.Digest), []byte("null")) {
			return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "digest", "digest must be a string, or absent")}
		}
		if digest == "" {
			return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidDigest, "digest",
				"digest must be sha256 followed by 64 lowercase hexadecimal characters, or absent")}
		}
	}
	manifest := Manifest{
		Schema:   wire.Schema,
		Protocol: wire.Protocol,
		Routes:   make([]Route, len(wire.Routes)),
	}
	manifest.Digest = digest
	for i, route := range wire.Routes {
		if route.PublicEdge == nil {
			field := fmt.Sprintf("routes[%d].publicEdge", i)
			return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, field,
				"publicEdge is required and must be a boolean")}
		}
		proves, proveDiag := parseProves(fmt.Sprintf("routes[%d].proves", i), route.Proves)
		if proveDiag != nil {
			return nil, []diag.Diagnostic{*proveDiag}
		}
		manifest.Routes[i] = Route{
			Match:      route.Match,
			Path:       route.Path,
			Methods:    route.Methods,
			PublicEdge: *route.PublicEdge,
			Provenance: route.Provenance,
			Proves:     proves,
		}
	}
	return &manifest, nil
}

// parseProves strictly decodes one route's proves member. An absent member is
// nil; an explicit null, a non-array list, a non-object entry and a missing or
// non-string key are parse errors, as in the TypeScript decoder. Like every
// other member, encoding/json matches the keys without regard to case.
func parseProves(field string, raw json.RawMessage) ([]Proof, *diag.Diagnostic) {
	if len(raw) == 0 {
		return nil, nil
	}
	type proofWire struct {
		Feature     *string `json:"feature"`
		Requirement *string `json:"requirement"`
		At          *string `json:"at"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var entries *[]*proofWire
	if err := dec.Decode(&entries); err != nil {
		d := decodeDiagnostic(err)
		if d.Code == ErrorCodeUnknownField {
			d.Field = field + "." + d.Field
		} else {
			d.Field = field
		}
		return nil, &d
	}
	if entries == nil {
		d := diag.Errorf(ErrorCodeParseError, field, "proves must be an array of objects when present")
		return nil, &d
	}
	proves := make([]Proof, len(*entries))
	for i, entry := range *entries {
		entryField := fmt.Sprintf("%s[%d]", field, i)
		if entry == nil {
			d := diag.Errorf(ErrorCodeParseError, entryField, "a proves entry must be an object")
			return nil, &d
		}
		if entry.Feature == nil || entry.Requirement == nil || entry.At == nil {
			d := diag.Errorf(ErrorCodeParseError, entryField, "a proves entry requires string feature, requirement and at")
			return nil, &d
		}
		proves[i] = Proof{Feature: *entry.Feature, Requirement: *entry.Requirement, At: *entry.At}
	}
	return proves, nil
}

// ParseAndValidateManifest strictly parses and validates a manifest in either
// form. The artifact form carries a digest, which must match the canonical
// route inventory; the committed form carries none, and the returned manifest
// holds the digest computed from its routes. Either way the returned Digest is
// the inventory's digest.
func ParseAndValidateManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	manifest, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	digest, diags := validateManifest(manifest)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	manifest.Digest = digest
	return manifest, nil
}

// ValidateManifest validates the envelope and every route, then verifies a
// present digest against the canonicalized inventory. An empty Digest is the
// committed form, which carries no digest.
func ValidateManifest(manifest *Manifest) []diag.Diagnostic {
	_, diags := validateManifest(manifest)
	return diags
}

// validateManifest is ValidateManifest that also answers the canonical digest
// of a valid manifest.
func validateManifest(manifest *Manifest) (string, []diag.Diagnostic) {
	if manifest == nil {
		return "", []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "manifest is nil")}
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
	diags = append(diags, ValidateRoutes(manifest.Routes)...)
	if manifest.Digest != "" && !digestPattern.MatchString(manifest.Digest) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDigest, "digest",
			"digest must be sha256 followed by 64 lowercase hexadecimal characters"))
	}
	if diag.HasErrors(diags) {
		return "", diags
	}
	canonical, canonicalDiags := Canonicalize(manifest.Routes)
	if diag.HasErrors(canonicalDiags) {
		return "", append(diags, canonicalDiags...)
	}
	if manifest.Digest != "" && manifest.Digest != canonical.Digest {
		return "", append(diags, diag.Errorf(ErrorCodeDigestMismatch, "digest",
			"digest %q does not match canonical inventory digest %q", manifest.Digest, canonical.Digest))
	}
	return canonical.Digest, diags
}

// ValidateRoutes validates individual route shapes followed by cross-route
// duplicate and visibility-overlap checks. Findings are returned in stable
// input order.
func ValidateRoutes(routes []Route) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for i, route := range routes {
		diags = append(diags, validateRoute(fmt.Sprintf("routes[%d]", i), route)...)
	}
	if diag.HasErrors(diags) {
		return diags
	}
	for i := 0; i < len(routes); i++ {
		for j := i + 1; j < len(routes); j++ {
			if !methodsOverlap(routes[i].Methods, routes[j].Methods) {
				continue
			}
			if sameMatchLanguage(routes[i], routes[j]) {
				diags = append(diags, diag.Errorf(ErrorCodeDuplicateRoute, fmt.Sprintf("routes[%d]", j),
					"route duplicates routes[%d] for at least one HTTP method", i))
				continue
			}
			if routes[i].PublicEdge != routes[j].PublicEdge && pathsOverlap(routes[i], routes[j]) {
				diags = append(diags, diag.Errorf(ErrorCodeVisibilityOverlap, fmt.Sprintf("routes[%d]", j),
					"route overlaps routes[%d] for at least one HTTP method but publicEdge differs", i))
			}
		}
	}
	return diags
}

func validateRoute(field string, route Route) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if route.Match != MatchExact && route.Match != MatchPrefix && route.Match != MatchTemplate {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidMatch, field+".match",
			"match %q is not in the v1 set: exact, prefix, template", route.Match))
	} else {
		diags = append(diags, validatePath(field+".path", route.Match, route.Path)...)
	}
	if len(route.Methods) == 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidMethod, field+".methods",
			"at least one HTTP method is required"))
	}
	seenMethods := map[string]bool{}
	for i, method := range route.Methods {
		methodField := fmt.Sprintf("%s.methods[%d]", field, i)
		if method != strings.ToUpper(method) || !validMethods[method] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidMethod, methodField,
				"method %q is not a canonical v1 HTTP method", method))
		}
		if seenMethods[method] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicateMethod, methodField,
				"method %q appears more than once", method))
		}
		seenMethods[method] = true
	}
	if strings.TrimSpace(route.Provenance.Project) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingProvenance, field+".provenance.project",
			"provenance.project is required"))
	}
	if !validSourceKinds[route.Provenance.SourceKind] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSourceKind, field+".provenance.sourceKind",
			"sourceKind %q is not in the v1 set", route.Provenance.SourceKind))
	}
	if evidence := route.Provenance.EvidencePath; evidence != "" {
		if strings.Contains(evidence, "\\") || strings.HasPrefix(evidence, "/") ||
			path.Clean(evidence) != evidence || evidence == ".." || strings.HasPrefix(evidence, "../") {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidEvidencePath, field+".provenance.evidencePath",
				"evidencePath must be a normalized workspace-relative path that does not escape the project"))
		}
	}
	if route.Proves != nil {
		diags = append(diags, validateProves(field+".proves", route)...)
	}
	return diags
}

// IsPageRoute reports whether route is a page: a file-route whose methods are
// exactly GET and HEAD. Only a page route may carry a proves list.
func IsPageRoute(route Route) bool {
	if route.Provenance.SourceKind != SourceFileRoute || len(route.Methods) != 2 {
		return false
	}
	hasGet, hasHead := false, false
	for _, method := range route.Methods {
		hasGet = hasGet || method == "GET"
		hasHead = hasHead || method == "HEAD"
	}
	return hasGet && hasHead
}

// validateProves checks a route's proves list: only a page route carries one,
// it is never empty, each entry names a feature and a requirement by their ID
// grammars and a concrete path the route admits, and no entry repeats.
func validateProves(field string, route Route) []diag.Diagnostic {
	if !IsPageRoute(route) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProof, field,
			"proves is allowed only on a page route: a file-route whose methods are exactly GET and HEAD")}
	}
	if len(route.Proves) == 0 {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProof, field,
			"proves must list at least one requirement when present")}
	}
	routePathValid := validatePath("", route.Match, route.Path) == nil
	var diags []diag.Diagnostic
	seen := map[Proof]bool{}
	for i, proof := range route.Proves {
		entryField := fmt.Sprintf("%s[%d]", field, i)
		if !featureIDPattern.MatchString(proof.Feature) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProof, entryField+".feature",
				"feature %q is not a feature ID such as \"acme/change-review\"", proof.Feature))
		}
		if !requirementIDPattern.MatchString(proof.Requirement) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProof, entryField+".requirement",
				"requirement %q is not a requirement ID such as \"tabs-in-mission-order\"", proof.Requirement))
		}
		if atDiags := validatePath(entryField+".at", MatchExact, proof.At); atDiags != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProof, entryField+".at",
				"at must be a concrete request path: %s", atDiags[0].Message))
		} else if routePathValid && !pathsOverlap(Route{Match: MatchExact, Path: proof.At}, route) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProof, entryField+".at",
				"at %q is not a path the route %q admits", proof.At, route.Path))
		}
		if seen[proof] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProof, entryField,
				"proves entry repeats an earlier entry"))
		}
		seen[proof] = true
	}
	return diags
}

func validatePath(field string, kind MatchKind, routePath string) []diag.Diagnostic {
	if routePath == "" || routePath[0] != '/' {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPath, field, "path must be absolute and begin with '/'")}
	}
	if strings.Contains(routePath, "//") {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPath, field, "path must not contain repeated '/' separators")}
	}
	if strings.ContainsAny(routePath, "?#\\*[]()") || strings.Contains(routePath, "{...") {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeUnsupportedPattern, field,
			"wildcards, catch-alls, regular expressions, query strings, fragments, and backslashes are unsupported")}
	}
	if strings.Contains(routePath, ":") {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeUnsupportedPattern, field,
			"provider-native ':param' syntax is unsupported; convert named segments to '{param}'")}
	}
	if err := validateEscaping(routePath); err != nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPath, field, "%v", err)}
	}
	for _, segment := range strings.Split(routePath, "/")[1:] {
		if segment == "." || segment == ".." {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPath, field, "dot segments are not canonical route paths")}
		}
	}

	parameterCount := 0
	catchAllCount := 0
	for _, segment := range strings.Split(routePath, "/")[1:] {
		if !strings.ContainsAny(segment, "{}") {
			continue
		}
		if len(segment) < 3 || segment[0] != '{' || segment[len(segment)-1] != '}' ||
			strings.Count(segment, "{") != 1 || strings.Count(segment, "}") != 1 {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeUnsupportedPattern, field,
				"template parameters must occupy one complete segment and use '{name}' syntax")}
		}
		inner := segment[1 : len(segment)-1]
		// A '{name...}' segment is a catch-all: it matches one or more complete
		// segments (slashes included). Its name reuses the single-segment
		// parameter grammar; the trailing "..." marks the catch-all. The
		// standalone "{..." check above still rejects an anonymous "{...}".
		if name, ok := strings.CutSuffix(inner, "..."); ok {
			if !parameterNamePattern.MatchString(name) {
				return []diag.Diagnostic{diag.Errorf(ErrorCodeUnsupportedPattern, field,
					"template parameters must occupy one complete segment and use '{name}' syntax")}
			}
			catchAllCount++
		} else if !parameterNamePattern.MatchString(inner) {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeUnsupportedPattern, field,
				"template parameters must occupy one complete segment and use '{name}' syntax")}
		}
		parameterCount++
	}
	// At most one catch-all per path. The runtime trie router supports a single
	// unbounded catch-all (optionally before a fixed suffix), never two.
	if catchAllCount > 1 {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeUnsupportedPattern, field,
			"a path may contain at most one '{name...}' catch-all segment")}
	}

	switch kind {
	case MatchExact:
		if parameterCount != 0 {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeUnsupportedPattern, field,
				"exact paths cannot contain template parameters")}
		}
	case MatchPrefix:
		if parameterCount != 0 {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeUnsupportedPattern, field,
				"prefix paths cannot contain template parameters")}
		}
		if routePath == "/" {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeUnsupportedPattern, field,
				"the root prefix is forbidden because a static mount must never imply '/*'")}
		}
		if !strings.HasSuffix(routePath, "/") {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPath, field,
				"prefix paths must end in '/' so segment ownership is explicit")}
		}
	case MatchTemplate:
		if parameterCount == 0 {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPath, field,
				"template paths must contain at least one '{name}' segment")}
		}
	}
	return nil
}

func validateEscaping(routePath string) error {
	for i := 0; i < len(routePath); i++ {
		c := routePath[i]
		if c >= 0x80 || c < 0x21 || c == 0x7f {
			return fmt.Errorf("path must use visible ASCII; UTF-8 bytes must be percent-encoded")
		}
		if c != '%' {
			continue
		}
		if i+2 >= len(routePath) || !isUpperHex(routePath[i+1]) || !isUpperHex(routePath[i+2]) {
			return fmt.Errorf("percent escapes must use two uppercase hexadecimal digits")
		}
		decoded := fromHex(routePath[i+1])<<4 | fromHex(routePath[i+2])
		if decoded == '/' || decoded == '\\' || decoded == '%' || decoded == '?' || decoded == '#' || decoded < 0x20 || decoded == 0x7f {
			return fmt.Errorf("percent-encoded separators, percent signs, query/fragment markers, and controls are forbidden")
		}
		if isUnreserved(decoded) {
			return fmt.Errorf("percent-encoded unreserved characters must be written literally")
		}
		i += 2
	}
	unescaped, err := url.PathUnescape(routePath)
	if err != nil || !utf8.ValidString(unescaped) {
		return fmt.Errorf("percent escapes must encode valid UTF-8")
	}
	return nil
}

func isUpperHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'A' && c <= 'F' }
func fromHex(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'A' + 10
}
func isUnreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", rune(c))
}

func decodeDiagnostic(err error) diag.Diagnostic {
	message := err.Error()
	if strings.HasPrefix(message, "json: unknown field ") {
		field := strings.Trim(strings.TrimPrefix(message, "json: unknown field "), `"`)
		return diag.Errorf(ErrorCodeUnknownField, field, "%s", message)
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

func methodsOverlap(a, b []string) bool {
	seen := make(map[string]bool, len(a))
	for _, method := range a {
		seen[method] = true
	}
	for _, method := range b {
		if seen[method] {
			return true
		}
	}
	return false
}

func semanticPath(route Route) string {
	if route.Match != MatchTemplate {
		return route.Path
	}
	segments := strings.Split(route.Path, "/")
	for i, segment := range segments {
		// A catch-all normalizes to a token distinct from a single-segment
		// parameter so "/x/{id}" and "/x/{id...}" are never duplicates.
		if isCatchAllSegment(segment) {
			segments[i] = "{...}"
		} else if len(segment) >= 2 && segment[0] == '{' && segment[len(segment)-1] == '}' {
			segments[i] = "{}"
		}
	}
	return strings.Join(segments, "/")
}

func sameMatchLanguage(a, b Route) bool {
	return a.Match == b.Match && semanticPath(a) == semanticPath(b)
}

func pathsOverlap(a, b Route) bool {
	// A catch-all can absorb a variable number of segments, so it cannot be
	// compared with the fixed-length routines below. Route any pair involving one
	// to the segment-boundary analysis, which over-approximates overlap so a
	// public catch-all can never silently coexist with a private route it matches.
	if isCatchAll(a) || isCatchAll(b) {
		return patternsOverlap(toPattern(a), toPattern(b))
	}
	if a.Match == MatchPrefix && b.Match == MatchPrefix {
		return strings.HasPrefix(a.Path, b.Path) || strings.HasPrefix(b.Path, a.Path)
	}
	if a.Match == MatchPrefix {
		return prefixOverlaps(a.Path, b)
	}
	if b.Match == MatchPrefix {
		return prefixOverlaps(b.Path, a)
	}
	return fixedPatternsOverlap(a, b)
}

// pathPattern is a segment-boundary view of a route used for catch-all overlap.
// A fixed route stores all its segments in prefix; a catch-all (or a '/'-mount)
// stores the segments before the catch-all in prefix and those after it in
// suffix, with hasCatchAll marking the variable, one-or-more-segment gap.
type pathPattern struct {
	prefix      []string
	suffix      []string
	hasCatchAll bool
}

func toPattern(route Route) pathPattern {
	if route.Match == MatchPrefix {
		// A '/'-terminated prefix owns its segments plus one or more further
		// segments — the same reachable set as a trailing "{rest...}" catch-all.
		return pathPattern{prefix: strings.Split(strings.TrimSuffix(route.Path, "/"), "/")[1:], hasCatchAll: true}
	}
	segments := strings.Split(route.Path, "/")[1:]
	for i, segment := range segments {
		if isCatchAllSegment(segment) {
			return pathPattern{prefix: segments[:i], suffix: segments[i+1:], hasCatchAll: true}
		}
	}
	return pathPattern{prefix: segments, hasCatchAll: false}
}

func patternsOverlap(a, b pathPattern) bool {
	switch {
	case a.hasCatchAll && b.hasCatchAll:
		// Both have a variable gap: for a long-enough path the gaps never collide,
		// so overlap turns on whether the overlapping fixed heads and fixed tails
		// are mutually satisfiable. Positions only one pattern constrains fall in
		// the other's gap and impose nothing.
		return sharedSegmentsCompatible(a.prefix, b.prefix, false) &&
			sharedSegmentsCompatible(a.suffix, b.suffix, true)
	case a.hasCatchAll:
		return fixedMatchesCatchAll(b.prefix, a)
	case b.hasCatchAll:
		return fixedMatchesCatchAll(a.prefix, b)
	default:
		if len(a.prefix) != len(b.prefix) {
			return false
		}
		return sharedSegmentsCompatible(a.prefix, b.prefix, false)
	}
}

// fixedMatchesCatchAll reports whether a fixed-length segment list can satisfy a
// catch-all pattern: it must be long enough for the catch-all to absorb at least
// one segment, and its bounding segments must be compatible with the catch-all's
// fixed prefix and suffix.
func fixedMatchesCatchAll(fixed []string, catch pathPattern) bool {
	if len(fixed) < len(catch.prefix)+len(catch.suffix)+1 {
		return false
	}
	for i, segment := range catch.prefix {
		if !segmentsCompatible(fixed[i], segment) {
			return false
		}
	}
	for i, segment := range catch.suffix {
		if !segmentsCompatible(fixed[len(fixed)-len(catch.suffix)+i], segment) {
			return false
		}
	}
	return true
}

// sharedSegmentsCompatible compares the overlapping ends of two segment lists.
// With fromEnd it aligns them by their tails (a shared suffix); otherwise by
// their heads (a shared prefix). Positions only one list reaches are absorbed by
// the counterpart's catch-all and never force disjointness.
func sharedSegmentsCompatible(a, b []string, fromEnd bool) bool {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		ai, bi := i, i
		if fromEnd {
			ai, bi = len(a)-1-i, len(b)-1-i
		}
		if !segmentsCompatible(a[ai], b[bi]) {
			return false
		}
	}
	return true
}

// segmentsCompatible reports whether two fixed segments can match a common
// concrete segment. A single-segment parameter matches any non-empty segment;
// two literals must be equal.
func segmentsCompatible(a, b string) bool {
	if isParameterSegment(a) || isParameterSegment(b) {
		return true
	}
	return a == b
}

func isCatchAllSegment(segment string) bool {
	if len(segment) < 3 || segment[0] != '{' || segment[len(segment)-1] != '}' {
		return false
	}
	name, ok := strings.CutSuffix(segment[1:len(segment)-1], "...")
	return ok && parameterNamePattern.MatchString(name)
}

func isCatchAll(route Route) bool {
	if route.Match != MatchTemplate {
		return false
	}
	for _, segment := range strings.Split(route.Path, "/")[1:] {
		if isCatchAllSegment(segment) {
			return true
		}
	}
	return false
}

func prefixOverlaps(prefix string, other Route) bool {
	if other.Match == MatchExact {
		return strings.HasPrefix(other.Path, prefix)
	}
	prefixSegments := strings.Split(strings.TrimSuffix(prefix, "/"), "/")[1:]
	otherSegments := strings.Split(other.Path, "/")[1:]
	if len(otherSegments) <= len(prefixSegments) {
		return false
	}
	for i, want := range prefixSegments {
		got := otherSegments[i]
		if isParameterSegment(got) {
			continue
		}
		if want != got {
			return false
		}
	}
	return true
}

func fixedPatternsOverlap(a, b Route) bool {
	aSegments := strings.Split(a.Path, "/")
	bSegments := strings.Split(b.Path, "/")
	if len(aSegments) != len(bSegments) {
		return false
	}
	for i := range aSegments {
		if aSegments[i] == bSegments[i] ||
			isParameterSegment(aSegments[i]) && bSegments[i] != "" ||
			isParameterSegment(bSegments[i]) && aSegments[i] != "" {
			continue
		}
		return false
	}
	return true
}

func isParameterSegment(segment string) bool {
	return len(segment) >= 3 && segment[0] == '{' && segment[len(segment)-1] == '}'
}
