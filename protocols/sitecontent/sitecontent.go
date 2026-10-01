// Package sitecontent defines site-content-bundle/v1: the cross-repo wire
// contract for shipping site content produced in one repository (e.g. Putnami
// Cloud's docs) into another repository's site (putnami.dev) without either
// side importing the other's build system.
//
// A "site content bundle" is a SINGLE publishable tar.gz archive: the reserved
// bundle.json manifest at the archive root (ManifestEntryName) plus every
// payload file. One digest addresses manifest and payload together, so the
// manifest cannot escape the consumer's content-address pin — a separately
// fetched or registry-side manifest is NOT part of this contract. The manifest
// declares:
//
//   - formatVersion — the bundle format, "major.minor". A consumer MUST ignore
//     a bundle whose MAJOR is newer than the major it implements and fall back
//     to its baked-in content; see CompatibleFormatVersion. This is part of the
//     contract, not the implementation.
//   - name — a stable logical identifier for the bundle.
//   - mounts — the site URL prefixes this bundle's content occupies. Mounts
//     partition URL space: two mounts (within one bundle, across bundles, or
//     against the site's own tree) may never overlap.
//   - source — the {repo, commit} the bundle was produced from, for provenance.
//   - files — every payload file as {path, digest}. Digests are sha256 bare-hex
//     (64 lowercase hex chars), consistent with the put registry and artifact
//     store.
//
// Payload rules (part of the contract, enforced by VerifyArchive):
//
//   - tar.gz, forward-slash relative paths only — no absolute paths, no ".."
//     traversal, no symlinks;
//   - every payload file is listed in files with a matching digest, and every
//     listed file is present (the manifest and payload agree exactly);
//   - every file path falls under a declared mount;
//   - the reserved bundle.json entry is the manifest carrier, not a payload
//     file: it must be a regular file at the archive root and is exempt from
//     the files/mount rules.
//
// # Lifecycle
//
// produce → publish → pin → merge → overlay. A producer assembles the archive
// (BuildArchive, which self-verifies) and publishes it to a registry addressed
// by digest. A consumer pins a specific digest (baked builds fetch by digest,
// never by a build-time channel), verifies the archive (VerifyArchive: extract
// bundle.json, gate on the format-version major, verify the payload against
// the manifest), and merges the bundle's mounts into its site tree — rejecting
// any mount that overlaps content it already owns. See the merge helpers
// (merge.go) and README.md for the produce/consume split.
//
// This package owns only the wire contract: the Go types, strict parsing, the
// JSON schema, the payload-verification and merge reference helpers, and the
// shared fixture corpus. The producer (Putnami Cloud, another repo) publishes
// bundles to the put registry; the consumer (sites/putnami.dev) pins a digest,
// verifies it, and overlays the payload. Both must reproduce exactly these
// rules — see README.md for the produce/consume split and the current
// conformance evidence.
package sitecontent

// ProtocolVersion is the sitecontent protocol package version. It is pinned by
// conformance_test.go so any bump is intentional. It is distinct from a
// bundle's FormatVersion: ProtocolVersion versions this package's Go/parser
// surface, FormatVersion versions the on-the-wire bundle format a consumer
// gates on.
const ProtocolVersion = 1

// FormatVersion is the bundle format version a producer stamps into every
// bundle.json ("major.minor"). FormatMajor is the major a consumer built
// against this package implements; a bundle with a newer major is ignored (the
// consumer falls back to baked content). Manifest objects are closed shapes:
// adding a field or changing a shape, enum, or payload rule requires a major
// bump. A minor bump may only describe a revision that existing consumers can
// validate under the unchanged schema and rules.
const (
	SchemaID      = "https://putnami.dev/schemas/protocol/sitecontent/site-content-bundle.json"
	FormatVersion = "1.0"
	FormatMajor   = 1
)

// Manifest is bundle.json: the declaration that travels alongside a bundle's
// tar.gz payload.
type Manifest struct {
	// Schema is an optional JSON Schema reference for editor validation.
	Schema string `json:"$schema,omitempty"`
	// FormatVersion is the bundle format version ("major.minor"). A consumer
	// gates on its major before attempting a merge (CompatibleFormatVersion).
	FormatVersion string `json:"formatVersion"`
	// Name is a stable logical identifier for the bundle (the join key a site
	// uses to track which bundle owns which mounts).
	Name string `json:"name"`
	// Mounts are the site URL prefixes this bundle's content occupies. At least
	// one is required and no two may overlap.
	Mounts []Mount `json:"mounts"`
	// Source records where the bundle was produced from, for provenance.
	Source Source `json:"source"`
	// Files lists every payload file with its sha256 bare-hex digest. The
	// manifest and the tar.gz payload must agree exactly.
	Files []File `json:"files"`
}

// Mount is one site URL prefix a bundle's content occupies, e.g.
// {"urlPrefix": "/docs/cloud"}. Every payload file path must fall under one of
// the bundle's mounts, and mounts may not overlap the site's own tree or
// another bundle's mounts.
type Mount struct {
	// URLPrefix is an absolute, normalized site URL path prefix: it starts with
	// "/", has at least one path segment, uses lowercase URL-safe segments, and
	// carries no "." or ".." segment and no trailing slash. The site root "/" is
	// not a valid mount (a bundle may not own the whole site).
	URLPrefix string `json:"urlPrefix"`
}

// Source records the provenance of a bundle: the repository and commit the
// content was produced from.
type Source struct {
	// Repo is the source repository identifier (free-form, e.g. "acme-platform"
	// or a clone URL).
	Repo string `json:"repo"`
	// Commit is the source commit the content was produced from (an opaque,
	// whitespace-free ref such as a git SHA).
	Commit string `json:"commit"`
}

// File is one payload entry: a payload-relative path and the sha256 bare-hex
// digest of its contents.
type File struct {
	// Path is the forward-slash payload-relative path. It must be relative (no
	// leading "/"), carry no "." or ".." segment, and use no backslash. The file
	// is served at the URL "/"+Path, which must fall under a declared mount.
	Path string `json:"path"`
	// Digest is the sha256 digest of the file contents as 64 lowercase hex chars
	// (bare-hex, no "sha256:" prefix).
	Digest string `json:"digest"`
}

// URLPath is the site URL a payload file at p is served at: "/"+p.
func URLPath(p string) string { return "/" + p }
