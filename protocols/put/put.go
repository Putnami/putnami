// Package put defines the Putnami Put registry immutable write protocol:
// put-write/v1. A publisher uploads the blobs a version references, then
// publishes the version's manifest without a channel. The version is private
// and immutable, and no channel moves: a release-set provider moves channels
// at release.
//
// Server contract (write side), relative to the registry base URL whose
// downloads are {base}/{namespace}/{package}/download:
//
//	POST {base}/{namespace}/{package}/blobs           (Content-Type: the blob media type)
//	     <blob bytes>  → 201 {"id","digest","size","media_type","created_at"}
//	POST {base}/{namespace}/{package}/publish         (Content-Type: application/json)
//	     {"version","media_type","payload"}
//	     → 201 {"package","version":{...},"manifest":{...},"channel":null}
//	     → 409 when the version is already published
//	GET  {base}/{namespace}/{package}/versions/{version}/manifest
//	     → 200 {"id","package_id","media_type","payload","created_at"}
//
// The member digest of a version is "sha256:" and the hex SHA-256 of the
// manifest payload the registry stores. A payload in canonical form
// (ValidatePayload) is stored byte for byte, so a publisher knows the digest
// before it uploads anything.
package put

import (
	"encoding/json"
	"net/url"

	distribution "go.putnami.dev/protocol/distribution"
)

// ProtocolVersion is the current put-write protocol version. It is bumped
// whenever a wire-visible change would break an existing client or server.
const ProtocolVersion = 1

// MaxManifestBytes bounds one manifest payload.
const MaxManifestBytes = 4 << 20

// MaxVersionBytes bounds one version string.
const MaxVersionBytes = 256

// MaxArchivePlatforms bounds the platforms of one archive manifest. It equals
// the release set's per-member platform bound.
const MaxArchivePlatforms = distribution.MaxPlatformsPerMember

// BlobUploadPath is the endpoint that accepts one blob of a package.
func BlobUploadPath(namespace, pkg string) string { return "/" + namespace + "/" + pkg + "/blobs" }

// PublishPath is the endpoint that publishes one version of a package.
func PublishPath(namespace, pkg string) string { return "/" + namespace + "/" + pkg + "/publish" }

// ManifestPath is the endpoint that answers the stored manifest of one
// version. The version is path-escaped.
func ManifestPath(namespace, pkg, version string) string {
	return "/" + namespace + "/" + pkg + "/versions/" + url.PathEscape(version) + "/manifest"
}

// PublishContentType is the Content-Type of a publish request.
const PublishContentType = "application/json"

// The manifest media types, one per member kind.
const (
	// ArchiveManifestMediaType is a release archive: the archive payload
	// (ArchivePayload) maps each platform to its blob.
	ArchiveManifestMediaType = "application/vnd.putnami.archive+json"
	// ConfigManifestMediaType is an authored config member. It references no
	// blob.
	ConfigManifestMediaType = "application/vnd.putnami.config.authored-member+json"
	// MigrationManifestMediaType is a migration manifest. It references its
	// migration bundle blob.
	MigrationManifestMediaType = "application/vnd.putnami.data.migration.v2+json"
	// DocManifestMediaType is a site-content bundle. It references the bundle
	// archive blob.
	DocManifestMediaType = "application/vnd.putnami.sitecontent.bundle+json"
	// DeploymentManifestMediaType is a workload's deployment declaration: the
	// canonical bytes of infra protocol version 2's aggregated manifest. It
	// references no blob. A new infra protocol version is a new media type.
	DeploymentManifestMediaType = "application/vnd.putnami.infra.deployment.v2+json"
)

// The blob media types.
const (
	// GzipBlobMediaType is a gzip-compressed archive.
	GzipBlobMediaType = "application/gzip"
	// BinaryBlobMediaType is a raw executable. Only the CLI's own release
	// publishes one (tooling/cli decision D-W2); every other archive is gzip.
	BinaryBlobMediaType = "application/octet-stream"
	// MigrationBundleBlobMediaType is a migration bundle.
	MigrationBundleBlobMediaType = "application/vnd.putnami.migration-bundle.v1.tar"
)

// Profile is what a member of one kind publishes: the media type of its
// manifest and the media types its blobs may carry.
type Profile struct {
	// Kind is the release-set member kind.
	Kind distribution.MemberKind
	// ManifestMediaType is the media type the manifest is published with.
	ManifestMediaType string
	// BlobMediaTypes are the media types a blob of the member may carry. Empty
	// means the member references no blob.
	BlobMediaTypes []string
}

// Profiles is the closed vocabulary of member kinds put-write/v1 publishes, in
// the order of distribution.MemberKinds.
var Profiles = []Profile{
	{Kind: distribution.KindConfig, ManifestMediaType: ConfigManifestMediaType},
	{Kind: distribution.KindMigration, ManifestMediaType: MigrationManifestMediaType, BlobMediaTypes: []string{MigrationBundleBlobMediaType}},
	{Kind: distribution.KindDoc, ManifestMediaType: DocManifestMediaType, BlobMediaTypes: []string{GzipBlobMediaType}},
	{Kind: distribution.KindArchive, ManifestMediaType: ArchiveManifestMediaType, BlobMediaTypes: []string{GzipBlobMediaType, BinaryBlobMediaType}},
	{Kind: distribution.KindDeployment, ManifestMediaType: DeploymentManifestMediaType},
}

// ProfileFor returns the profile of kind, and false for a kind put-write/v1
// does not publish.
func ProfileFor(kind distribution.MemberKind) (Profile, bool) {
	for _, profile := range Profiles {
		if profile.Kind == kind {
			return profile, true
		}
	}
	return Profile{}, false
}

// BlobReceipt is the body of a 201 answer to a blob upload: the blob the
// registry stored.
type BlobReceipt struct {
	// ID is the registry's identifier of the blob.
	ID string `json:"id"`
	// Digest is "sha256:" and the 64 lowercase hex characters of the SHA-256
	// of the stored bytes.
	Digest string `json:"digest"`
	// Size is the length of the stored bytes.
	Size int64 `json:"size"`
	// MediaType is the media type the blob was uploaded with.
	MediaType string `json:"media_type"`
	// CreatedAt is when the registry first stored the blob, in RFC 3339.
	CreatedAt string `json:"created_at"`
}

// PublishRequest is the body of a publish request. It carries no channel, no
// visibility and no source reference: the version is published private, and
// no channel moves.
type PublishRequest struct {
	// Version is the version to publish.
	Version string `json:"version"`
	// MediaType is the manifest media type.
	MediaType string `json:"media_type"`
	// Payload is the manifest payload, in canonical form (ValidatePayload).
	Payload json.RawMessage `json:"payload"`
}

// PublishResponse is the body of a 201 answer to a publish request.
type PublishResponse struct {
	// Package is "<namespace>/<package>".
	Package string `json:"package"`
	// Version is the published version.
	Version Version `json:"version"`
	// Manifest is the stored manifest.
	Manifest Manifest `json:"manifest"`
	// Channel is null: no channel moved.
	Channel json.RawMessage `json:"channel"`
}

// Version is one published version of a package.
type Version struct {
	// ID is the registry's identifier of the version.
	ID string `json:"id"`
	// PackageID is the registry's identifier of the package.
	PackageID string `json:"package_id"`
	// Version is the version string.
	Version string `json:"version"`
	// ManifestID is the identifier of the version's manifest.
	ManifestID string `json:"manifest_id"`
	// State is VersionStatePublished.
	State string `json:"state"`
	// Visibility is VisibilityPrivate.
	Visibility string `json:"visibility"`
	// CreatedAt is when the version was created, in RFC 3339.
	CreatedAt string `json:"created_at"`
}

// Manifest is a stored manifest: the publish response's manifest, and the body
// of a 200 answer to a manifest read.
type Manifest struct {
	// ID is the registry's identifier of the manifest.
	ID string `json:"id"`
	// PackageID is the registry's identifier of the package.
	PackageID string `json:"package_id"`
	// MediaType is the manifest media type.
	MediaType string `json:"media_type"`
	// Payload is the stored payload.
	Payload json.RawMessage `json:"payload"`
	// CreatedAt is when the manifest was stored, in RFC 3339.
	CreatedAt string `json:"created_at"`
}

// VersionStatePublished is the state of a published version.
const VersionStatePublished = "published"

// VisibilityPrivate is the visibility of a version published without a
// visibility.
const VisibilityPrivate = "private"

// ArchivePayload is the manifest payload of an archive member.
type ArchivePayload struct {
	// Artifacts maps "<os>-<arch>" to the blob of that platform. Two platforms
	// may name the same blob.
	Artifacts map[string]ArchiveArtifact `json:"artifacts"`
}

// ArchiveArtifact is the blob of one platform of an archive.
type ArchiveArtifact struct {
	// Digest is the blob digest.
	Digest string `json:"digest"`
	// Size is the blob size.
	Size int64 `json:"size"`
}
