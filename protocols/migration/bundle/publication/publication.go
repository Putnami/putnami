// Package publication packs one migration publication. It validates the
// migration bundle and produces the exact bytes a registry stores for it: the
// bundle tar blob and the migration manifest that references that blob, with
// their digests and refs.
//
// Every path that publishes a migration calls Pack: a service that stores the
// bytes itself, and a CLI publisher that hands them to another uploader. One
// function on every path keeps the artifact digest identical, so a receiver can
// accept a stored publication later by repacking it and comparing bytes.
//
// Bundle validation and the bundle digest belong to the parent protocol,
// go.putnami.dev/protocol/migration; the deterministic tar belongs to
// go.putnami.dev/protocol/migration/bundle.
package publication

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing/fstest"

	protocolmigration "go.putnami.dev/protocol/migration"
	"go.putnami.dev/protocol/migration/bundle"
)

const (
	// Protocol names the migration manifest document.
	Protocol = "putnami.data.migration.v2"
	// ManifestMediaType is the registry media type of the migration manifest.
	ManifestMediaType = "application/vnd.putnami.data.migration.v2+json"
	// BlobMediaType is the registry media type of the bundle tar blob.
	BlobMediaType = bundle.BlobMediaType
)

var (
	applicationPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,254}$`)
	nativeAddressPartPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,254}$`)
)

// ErrInvalid marks a publication Pack refuses: an identity outside its bounds,
// an invalid or empty file, a bundle the migration-bundle protocol refuses, a
// bundle outside the publication scope (CheckScope), or a file the bundle does
// not reference.
var ErrInvalid = errors.New("invalid migration publication")

// Manifest is the immutable registry document of a migration publication. Its
// JSON encoding is the stored manifest payload, so the field order and names
// are part of the artifact digest.
type Manifest struct {
	Protocol             string `json:"protocol"`
	Application          string `json:"application"`
	Namespace            string `json:"namespace"`
	Package              string `json:"package"`
	Version              string `json:"version"`
	BundleDigest         string `json:"bundle_digest"`
	BlobDigest           string `json:"blob_digest"`
	BlobRef              string `json:"blob_ref"`
	SourceRevision       string `json:"source_revision"`
	SelectionFingerprint string `json:"selection_fingerprint"`
}

// Input is one migration publication: the application it belongs to, the
// registry namespace and package it is stored at, its version and release
// provenance, and the bundle files by slash-separated path.
type Input struct {
	Application          string
	Namespace            string
	Package              string
	Version              string
	SourceRevision       string
	SelectionFingerprint string
	Files                map[string][]byte
}

// Packed is the deterministic result of Pack. Tarball is the stored blob,
// Canonical is the manifest payload, and the digests are "sha256:<hex>" over
// those bytes, except BundleDigest, the bundle's bare-hex semantic digest.
type Packed struct {
	Bundle         protocolmigration.Bundle
	BundleDigest   string
	Tarball        []byte
	BlobDigest     string
	BlobRef        string
	Manifest       Manifest
	Canonical      []byte
	ArtifactDigest string
	ArtifactRef    string
}

// Pack validates the publication and packs its blob and manifest. The same
// input always packs to the same bytes.
//
// It refuses a bundle outside the publication scope (CheckScope), so every
// path that packs a publication refuses the same bundles.
//
// It refuses a bundle without an appName, as the protocol does, but does not
// compare the appName with the application: a build writes the project's
// manifest name there, which differs from its workspace path for a nested
// project. The bundle digest still covers the appName.
func Pack(in Input) (*Packed, error) {
	if !ValidApplication(in.Application) || !boundedToken(in.Version) || !boundedToken(in.SourceRevision) ||
		!boundedToken(in.SelectionFingerprint) || !ValidAddressPart(in.Namespace) || !ValidAddressPart(in.Package) || len(in.Files) == 0 {
		return nil, fmt.Errorf("%w: bounded publication identity and canonical bundle files are required", ErrInvalid)
	}
	mapfs := make(fstest.MapFS, len(in.Files))
	for name, contents := range in.Files {
		if !fs.ValidPath(name) || name == "." || len(contents) == 0 {
			return nil, fmt.Errorf("%w: bundle contains an invalid or empty file", ErrInvalid)
		}
		mapfs[name] = &fstest.MapFile{Data: append([]byte(nil), contents...), Mode: 0o600}
	}
	loaded, payloads, diagnostics := protocolmigration.LoadBundle(mapfs)
	if loaded == nil {
		return nil, fmt.Errorf("%w: migration bundle validation failed: %v", ErrInvalid, diagnostics)
	}
	if len(in.Files) != len(payloads)+1 {
		return nil, fmt.Errorf("%w: bundle contains files outside the strict migration manifest", ErrInvalid)
	}
	if err := CheckScope(*loaded); err != nil {
		return nil, err
	}
	tarball, err := bundle.Pack(mapfs)
	if err != nil {
		return nil, fmt.Errorf("%w: pack deterministic migration bundle: %w", ErrInvalid, err)
	}
	blobDigest := Digest(tarball)
	manifest := Manifest{
		Protocol: Protocol, Application: in.Application,
		Namespace: in.Namespace, Package: in.Package, Version: in.Version,
		BundleDigest: protocolmigration.ComputeBundleDigest(*loaded),
		BlobDigest:   blobDigest, BlobRef: BlobRef(in.Namespace, in.Package, blobDigest),
		SourceRevision: in.SourceRevision, SelectionFingerprint: in.SelectionFingerprint,
	}
	// A struct of strings always encodes.
	canonical, _ := json.Marshal(manifest)
	artifactDigest := Digest(canonical)
	return &Packed{
		Bundle: *loaded, BundleDigest: manifest.BundleDigest,
		Tarball: tarball, BlobDigest: blobDigest, BlobRef: manifest.BlobRef,
		Manifest: manifest, Canonical: canonical,
		ArtifactDigest: artifactDigest, ArtifactRef: ArtifactRef(in.Namespace, in.Package, artifactDigest),
	}, nil
}

// CheckScope refuses a bundle a publication cannot carry beyond what the
// migration-bundle protocol refuses: an operation of any kind but sql, an
// operation without a valid safety marker or an up payload hash, and two
// operations with one canonical identity. The identity is the operation's
// target with its framework name (FrameworkName), the key applied state is
// recorded under. The protocol's own duplicate check keys on kind, target,
// namespace and name, so it misses "auth" + "001" beside "" + "auth/001".
func CheckScope(b protocolmigration.Bundle) error {
	normalized := protocolmigration.NormalizeBundle(b)
	if len(normalized.Operations) == 0 {
		return fmt.Errorf("%w: migration bundle declares no operation", ErrInvalid)
	}
	seen := make(map[string]struct{}, len(normalized.Operations))
	for _, operation := range normalized.Operations {
		id := protocolmigration.CanonicalID(operation.Target, FrameworkName(operation.Namespace, operation.Name))
		switch {
		case operation.Kind != protocolmigration.KindSQL:
			return fmt.Errorf("%w: operation %s has kind %q; a migration publication accepts only sql operations", ErrInvalid, id, operation.Kind)
		case !protocolmigration.ValidSafetyClasses[operation.Safety] || operation.Up.Hash == "":
			return fmt.Errorf("%w: operation %s has no valid safety marker or up payload hash", ErrInvalid, id)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("%w: operation identity %s appears more than once in the bundle", ErrInvalid, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// FrameworkName is the name the framework's SQL loader gives an operation: a
// bare bundle name is stamped with its namespace, the default datasource when
// it has none, while an already-qualified name is kept. Applied state and
// execution evidence are recorded under this name.
func FrameworkName(namespace, name string) string {
	if name == "" || strings.Contains(name, "/") {
		return name
	}
	if namespace == "" {
		namespace = protocolmigration.DefaultDatasource
	}
	return namespace + "/" + name
}

// Digest is the registry content digest of data, "sha256:<hex>".
func Digest(data []byte) string {
	return "sha256:" + protocolmigration.ComputePayloadHash(data)
}

// BlobRef is the immutable ref of a migration blob in its registry package.
func BlobRef(namespace, pkg, digest string) string {
	return "put/" + namespace + "/" + pkg + "/blobs/" + digest
}

// ArtifactRef is the immutable content identity of a migration manifest: its
// package and artifact digest, never the version's mutable manifest pointer.
func ArtifactRef(namespace, pkg, digest string) string {
	return "put-manifest:" + namespace + "/" + pkg + "@" + digest
}

// ValidApplication reports whether application is a canonical publication
// identity: a lower-case workspace path or manifest name.
func ValidApplication(application string) bool {
	return applicationPattern.MatchString(application) && !strings.Contains(application, "..") && !strings.Contains(application, "//")
}

// ValidAddressPart reports whether part is a canonical registry namespace or
// package name.
func ValidAddressPart(part string) bool {
	return nativeAddressPartPattern.MatchString(part)
}

func boundedToken(value string) bool {
	return value != "" && len(value) <= 255 && strings.TrimSpace(value) == value && !strings.ContainsRune(value, '\x00')
}
