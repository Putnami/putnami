// Package gomod defines the Putnami Go module registry write protocol:
// gomod-write/v1. Public Go modules are normally "published" just by pushing a
// VCS tag; a Putnami Go module registry instead accepts an authenticated upload
// so private modules and channel (dist-tag) routing work. The registry is a
// proprietary workload — only a Putnami gomod registry implements the write
// side; the framework ships the client and the cloud repo implements the server.
//
// Server contract (write side):
//
//	POST /{module}/-/blobs/upload          (Content-Type: application/zip)
//	     <zip bytes>     → 201 {"digest": "sha256:..."}
//	PUT  /{module}/@v/{version}            (Content-Type: application/json)
//	     {"go_mod": "...", "zip_digest": "sha256:...", "dist_tag": "latest"}
//	     → 201
//	POST /{module}/@v/{version}/release
//	     → 200 {"module":"...","version":"...","visibility":"public"}
//
// Reads use the standard Go module proxy protocol (GET /{module}/@v/{version}.info,
// .mod, .zip), so `go mod download` works unmodified; only the write side is
// bespoke.
package gomod

// ProtocolVersion is the current gomod-write protocol version. It is bumped
// whenever a wire-visible change would break an existing client or server.
const ProtocolVersion = 1

// BlobUploadPath is the endpoint that accepts a module zip. module is a full
// module path (may contain slashes and dots, e.g. "go.putnami.dev/http").
func BlobUploadPath(module string) string { return "/" + module + "/-/blobs/upload" }

// VersionPath is the endpoint that publishes a module version.
func VersionPath(module, version string) string { return "/" + module + "/@v/" + version }

// ReleasePath is the endpoint that idempotently makes an already-published
// module version public. The registry's package visibility ceiling still
// applies; this operation never raises it.
func ReleasePath(module, version string) string { return VersionPath(module, version) + "/release" }

// Content types for the body-bearing write messages.
const (
	BlobContentType    = "application/zip"
	VersionContentType = "application/json"
	ReleaseContentType = "application/json"
)

// BlobUploadResponse is the body returned by POST {BlobUploadPath}.
type BlobUploadResponse struct {
	// Digest is the content address the registry assigned the uploaded zip:
	// "sha256:" + 64 lowercase hex chars. It is echoed back in the subsequent
	// PublishVersionRequest.
	Digest string `json:"digest"`
}

// PublishVersionRequest is the body of PUT {VersionPath}. The JSON field names
// are owned by the registry server and must not drift — they are pinned by the
// conformance suite.
type PublishVersionRequest struct {
	// GoMod is the verbatim go.mod file content for the version.
	GoMod string `json:"go_mod"`
	// ZipDigest is the digest returned by the prior blob upload.
	ZipDigest string `json:"zip_digest"`
	// DistTag is the optional release channel (e.g. "latest", "canary"). It is
	// omitted when publishing without channel routing.
	DistTag string `json:"dist_tag,omitempty"`
}

// ReleaseVisibility is the visibility value acknowledged by the release
// operation. v1 has exactly one successful state: public.
type ReleaseVisibility string

// ReleaseVisibilityPublic is the only successful release visibility in v1.
const ReleaseVisibilityPublic ReleaseVisibility = "public"

// ReleaseVersionResponse is the closed response body returned by POST
// {ReleasePath}. Module and Version identify the exact immutable version the
// registry released; clients must compare both with their request.
type ReleaseVersionResponse struct {
	// Module is the exact immutable module coordinate made public.
	Module string `json:"module"`
	// Version is the exact immutable version made public.
	Version string `json:"version"`
	// Visibility is the registry-confirmed release visibility. A successful v1
	// response must contain ReleaseVisibilityPublic.
	Visibility ReleaseVisibility `json:"visibility"`
}
