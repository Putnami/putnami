package datacli

import (
	"fmt"
	"path/filepath"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/migration/bundle/publication"
	"go.putnami.dev/sdk/extension/publicationoutbox"
)

// Paths inside the publication outbox. A job packs one member, so fixed names
// cannot collide.
const (
	migrationOutboxBlobPath     = "put/blob-0"
	migrationOutboxManifestPath = "put/manifest.json"
)

// PackedMigration is the member PackPreparedMigration packed. It carries no
// migration ref: Data accepts the member when a deploy first selects it.
type PackedMigration struct {
	Status         string `json:"status"`
	Coordinate     string `json:"coordinate"`
	Version        string `json:"version"`
	BundleDigest   string `json:"bundle_digest"`
	BlobDigest     string `json:"blob_digest"`
	ArtifactDigest string `json:"artifact_digest"`
}

// PackPreparedMigration packs one prepared migration into the publication
// outbox the engine gave this job (go.putnami.dev/protocol/extension): the
// bundle tar blob and the migration manifest, packed by publication.Pack, the
// function Data's publisher stores with. The engine uploads the member and
// Data accepts it at the first deploy that selects it, so this makes no Data
// call, needs no credential and reports no published member. projectID is the
// plan's project id of the member, with its leading slash.
//
// Every validation refusal, Data's canonical scope included, happens before
// the outbox is touched. A write that fails later can leave a partial outbox:
// files without a descriptor, or a member that was never committed. That is
// harmless. The outbox belongs to this one job, the job fails, and the engine
// refuses a member whose descriptor or files are missing or differ.
func PackPreparedMigration(outbox, projectID string, prepared *PreparedMigration, selection MigrationSelection) (*PackedMigration, error) {
	if prepared == nil {
		return nil, clicore.NewError("migration publication was not prepared", clicore.ExitUsage)
	}
	if selection.SourceRevision == "" || selection.SelectionFingerprint == "" {
		return nil, clicore.NewError("migration publication requires exact release provenance", clicore.ExitUsage)
	}
	if !filepath.IsAbs(outbox) {
		return nil, clicore.NewError(extensionproto.PublicationOutboxEnv+" must name an absolute directory", clicore.ExitUsage)
	}
	if !strings.HasPrefix(projectID, "/") {
		return nil, clicore.NewError("migration publication packs only a member of a planned project; the plan names no project id", clicore.ExitUsage)
	}
	namespace, packageName, found := strings.Cut(prepared.ExpectedCoordinate, "/")
	if !found {
		return nil, clicore.NewError("migration publication has no native Put coordinate", clicore.ExitUsage)
	}
	packed, err := publication.Pack(publication.Input{
		Application: prepared.Application, Namespace: namespace, Package: packageName, Version: prepared.Version,
		SourceRevision: selection.SourceRevision, SelectionFingerprint: selection.SelectionFingerprint, Files: prepared.Files,
	})
	if err != nil {
		return nil, clicore.NewError(err.Error(), clicore.ExitUsage)
	}
	// The bundle manifest's declared digest is what dry-run reported and what
	// the direct path checks Data's acceptance against.
	if packed.BundleDigest != prepared.BundleDigest {
		return nil, clicore.NewError(fmt.Sprintf("migration bundle digest %s differs from the digest its manifest declares, %s", packed.BundleDigest, prepared.BundleDigest), clicore.ExitUsage)
	}
	writer, err := publicationoutbox.NewWriter(outbox)
	if err != nil {
		return nil, clicore.NewError("open the publication outbox: "+err.Error(), clicore.ExitUsage)
	}
	blob, err := writer.WriteFile(migrationOutboxBlobPath, packed.Tarball)
	if err != nil {
		return nil, clicore.NewError("pack migration bundle blob: "+err.Error(), clicore.ExitAPI)
	}
	manifest, err := writer.WriteFile(migrationOutboxManifestPath, packed.Canonical)
	if err != nil {
		return nil, clicore.NewError("pack migration manifest: "+err.Error(), clicore.ExitAPI)
	}
	if blob.Digest != packed.BlobDigest || manifest.Digest != packed.ArtifactDigest {
		return nil, clicore.NewError("the publication outbox recorded other digests than the packed migration", clicore.ExitAPI)
	}
	member := extensionproto.OutboxMember{
		Ecosystem: extensionproto.OutboxEcosystemPut, Coordinate: prepared.ExpectedCoordinate, Version: prepared.Version, Project: projectID,
		Put: &extensionproto.OutboxPut{
			MediaType: publication.ManifestMediaType, Manifest: manifest,
			Blobs: []extensionproto.OutboxPutBlob{{Path: blob.Path, Digest: blob.Digest, Size: blob.Size, MediaType: publication.BlobMediaType}},
		},
	}
	if err := writer.Add(member); err != nil {
		return nil, clicore.NewError("pack "+prepared.ExpectedCoordinate+"@"+prepared.Version+": "+err.Error(), clicore.ExitUsage)
	}
	if err := writer.Commit(); err != nil {
		return nil, clicore.NewError("commit the publication outbox: "+err.Error(), clicore.ExitAPI)
	}
	return &PackedMigration{
		Status: "packed", Coordinate: prepared.ExpectedCoordinate, Version: prepared.Version,
		BundleDigest: packed.BundleDigest, BlobDigest: packed.BlobDigest, ArtifactDigest: packed.ArtifactDigest,
	}, nil
}

// ReportPackedMigration writes the packed member as the command's result.
func ReportPackedMigration(params map[string]any, ioctx clicore.IO, packed *PackedMigration) {
	clicore.WriteResult(packed, params, ioctx, fmt.Sprintf("Packed Data migration %s@%s into the publication outbox (artifact %s).",
		packed.Coordinate, packed.Version, clicore.ShortDigest(packed.ArtifactDigest)))
}
