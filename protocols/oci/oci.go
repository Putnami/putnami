// Package oci defines the Putnami OCI registry extensions that layer on top of
// the standard OCI distribution spec. A publisher pushes image content with any
// standard registry client; these contracts are the OPTIONAL fast paths a
// Putnami-managed registry advertises and the framework uses when present,
// always degrading to the standard distribution API elsewhere.
//
// # tag-digest/v1
//
// Assign every release ref to a digest in one authenticated, digest-addressed
// call instead of one GET+PUT manifest round trip per tag. Capability discovery
// mirrors the remote-cache protocol: probe an endpoint, take the fast path when
// the registry offers it, fall back to the standard distribution API anywhere
// else (gcr, ghcr, Docker Hub, ...).
//
// Server contract (implemented by the Putnami OCI registry, in its own repo):
//
//	GET  /v2/_putnami/capabilities
//	     → 200 {"apis": ["tag-digest/v1", ...]}; anything else → no fast path
//	POST /v2/_putnami/tag-digest
//	     {"repository": "<repo path>", "digest": "sha256:...", "tags": ["0.0.0-abc1234", "latest"]}
//	     → 2xx on success (the registry applies all tags to the digest atomically
//	       and validates that the digest exists)
//
// The "_putnami" path segment cannot collide with an image repository:
// distribution repository name components must match [a-z0-9]+ separated by
// ./_/- and may not start with "_" — the same reservation trick as /v2/_catalog.
// Authentication reuses the distribution token flow with push scope on the
// repository, so the fast path needs no extra credentials.
package oci

import "slices"

// ProtocolVersion is the current Putnami OCI extension protocol version. It is
// bumped whenever a wire-visible change would break an existing client or
// server, so each side can reject a mismatch instead of misreading a payload.
const ProtocolVersion = 1

// Capability discovery and endpoint paths served by a Putnami OCI registry.
const (
	// CapabilitiesPath is probed to discover the Putnami extensions a registry
	// offers. A non-200 response, or a body missing the api, means "use the
	// standard distribution API".
	CapabilitiesPath = "/v2/_putnami/capabilities"
	// TagDigestPath assigns tags to a digest atomically (tag-digest/v1).
	TagDigestPath = "/v2/_putnami/tag-digest"
	// APITagDigestV1 is the capability string advertised in CapabilitiesResponse
	// and required before a client may use TagDigestPath.
	APITagDigestV1 = "tag-digest/v1"
)

// CapabilitiesResponse is the body of GET /v2/_putnami/capabilities.
type CapabilitiesResponse struct {
	// APIs lists the Putnami extension capabilities the registry advertises.
	// Unknown entries are ignored, so a registry can advertise future
	// capabilities without breaking an older client.
	APIs []string `json:"apis"`
}

// SupportsTagDigest reports whether the registry advertises tag-digest/v1.
func (c CapabilitiesResponse) SupportsTagDigest() bool {
	return slices.Contains(c.APIs, APITagDigestV1)
}

// TagDigestRequest is the body of POST /v2/_putnami/tag-digest. The registry
// applies every tag to Digest atomically and validates that Digest exists.
type TagDigestRequest struct {
	// Repository is the image repository path without the registry host,
	// e.g. "team/app".
	Repository string `json:"repository"`
	// Digest is the manifest digest to tag: "sha256:" + 64 lowercase hex chars.
	Digest string `json:"digest"`
	// Tags are the refs to assign to Digest; at least one is required.
	Tags []string `json:"tags"`
}
