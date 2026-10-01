// Package httproutes defines putnami.http-routes.v1, the language-neutral
// inventory that frameworks emit and deployment systems consume when building
// a public HTTP edge allowlist.
//
// The inventory is deliberately independent of OpenAPI. Typed API routes may
// be checked against OpenAPI, but file-system web routes, static mounts, and
// exact public files are first-class route sources in this protocol.
package httproutes

const (
	// Protocol identifies the v1 wire contract. A consumer must fail closed on
	// any other identifier.
	Protocol = "putnami.http-routes.v1"

	// SchemaURL is the canonical JSON Schema reference stamped on emitted
	// inventories.
	SchemaURL = "https://putnami.dev/schemas/putnami-http-routes-v1.json"
)

// MatchKind describes how a path is matched at the public edge.
type MatchKind string

const (
	// MatchExact matches only Path, including its trailing-slash form.
	MatchExact MatchKind = "exact"
	// MatchPrefix matches Path and every request path beginning with Path. A
	// prefix must end in '/' and the root prefix is forbidden.
	MatchPrefix MatchKind = "prefix"
	// MatchTemplate matches path segments against a template. A parameter is one
	// complete segment written as {name} and never consumes '/'. A path may also
	// carry at most one catch-all segment written {name...}, which matches one or
	// more complete segments (slashes included) at a single position, optionally
	// bounded by fixed or {name} segments before and after it.
	MatchTemplate MatchKind = "template"
)

// SourceKind classifies the source that discovered a route. It describes
// provenance only; it does not change matching behavior.
type SourceKind string

// SourceKind values identify the route fact's discovery source.
const (
	SourceTypedAPI    SourceKind = "typed-api"
	SourceFileRoute   SourceKind = "file-route"
	SourceStaticMount SourceKind = "static-mount"
	SourcePublicFile  SourceKind = "public-file"
	SourceManual      SourceKind = "manual"
)

// Provenance points back to the framework fact that contributed a route.
// Project and SourceKind are required. EvidencePath is workspace-relative when
// present and must not escape the project.
type Provenance struct {
	// Project is the project ID that contributed the route.
	Project string `json:"project"`
	// Package is the package within Project that contributed the route, when
	// applicable.
	Package string `json:"package,omitempty"`
	// SourceKind classifies the discovery source that produced this route fact.
	SourceKind   SourceKind `json:"sourceKind"`
	EvidencePath string     `json:"evidencePath,omitempty"`
}

// Route is one application reachability declaration.
//
// PublicEdge is intentionally explicit for both public and non-public routes.
// Keeping the complete inventory lets validators reject a public prefix or
// template that would also expose an overlapping non-public route.
type Route struct {
	// Match is how Path is matched at the public edge.
	Match MatchKind `json:"match"`
	// Path is the exact path, '/'-terminated prefix, or {name} template being
	// declared, depending on Match.
	Path string `json:"path"`
	// Methods are the HTTP methods the route accepts.
	Methods []string `json:"methods"`
	// PublicEdge marks exposure at the public edge. It is kept explicit for
	// non-public routes too so validators can reject overlapping exposure.
	PublicEdge bool `json:"publicEdge"`
	// Provenance points back to the framework fact that contributed this route.
	Provenance Provenance `json:"provenance"`
}

// Manifest is the canonical putnami.http-routes.v1 artifact. Field order is
// normative: Go and TypeScript serializers emit this order with two-space
// indentation and a trailing newline.
type Manifest struct {
	// Schema is the canonical JSON Schema reference (SchemaURL).
	Schema string `json:"$schema"`
	// Protocol is the v1 wire contract identifier (Protocol).
	Protocol string `json:"protocol"`
	// Routes is the complete inventory, public and non-public.
	Routes []Route `json:"routes"`
	Digest string  `json:"digest"`
}
