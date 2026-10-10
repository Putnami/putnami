package distributioncli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	put "go.putnami.dev/protocol/put"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/putpublish"
)

// The publication outbox (go.putnami.dev/protocol/extension): under the
// engine's publication-v1 capability a publish job receives a private
// directory in PUTNAMI_PUBLICATION_OUTBOX and no registry credential. The job
// packs its one planned member there and uploads nothing; the engine uploads
// the packed bytes and the credential provider's release moves the channels.
// The packed manifest is the exact payload the direct path publishes, so the
// member's artifact digest is the same on both paths.

// Paths inside the outbox. A job packs one member, so fixed names cannot
// collide.
const (
	outboxManifestPath   = "put/manifest.json"
	outboxBlobPathFormat = "put/blob-%d"
)

// PublicationOutbox is the outbox directory the engine gave this publish job,
// or "" when the job publishes on its own route.
func PublicationOutbox(env map[string]string) string {
	return clicore.EnvGet(env, extensionproto.PublicationOutboxEnv)
}

// OutboxWithoutPlanError refuses a job that holds an outbox but no
// release-set plan: only a planned member can be packed, and an outbox job
// holds no credential to publish anything else. subject names the publication,
// such as "Config publication".
func OutboxWithoutPlanError(subject string) error {
	return clicore.NewError(subject+" packs into the publication outbox only a member the release-set plan selects; the job carries no plan", clicore.ExitUsage)
}

// putOutboxPacker stages one put or archive member in an outbox: its blobs,
// then its manifest and the descriptor that names them.
type putOutboxPacker struct {
	dir    string
	writer *publicationoutbox.Writer
	files  int
	blobs  []extensionproto.OutboxPutBlob
}

func newPutOutboxPacker(outbox string) (*putOutboxPacker, error) {
	if !filepath.IsAbs(outbox) {
		return nil, clicore.NewError(extensionproto.PublicationOutboxEnv+" must name an absolute directory", clicore.ExitUsage)
	}
	writer, err := publicationoutbox.NewWriter(outbox)
	if err != nil {
		return nil, clicore.NewError("open the publication outbox: "+err.Error(), clicore.ExitUsage)
	}
	return &putOutboxPacker{dir: outbox, writer: writer}, nil
}

// addBlobFile copies the file at src into the outbox and returns the digest
// and size Put answers for those bytes, "sha256:<hex>" and the byte length.
func (p *putOutboxPacker) addBlobFile(src, mediaType string) (string, int64, error) {
	file, err := p.writer.CopyFile(p.nextBlobPath(), src)
	if err != nil {
		return "", 0, clicore.NewError("pack blob "+filepath.Base(src)+": "+err.Error(), clicore.ExitAPI)
	}
	p.recordBlob(file, mediaType)
	return file.Digest, file.Size, nil
}

// addGzipBlobBytes writes the gzip archive data into the outbox and returns
// its Put digest. The site-content bundle is the only blob packed from memory.
func (p *putOutboxPacker) addGzipBlobBytes(data []byte) (string, error) {
	file, err := p.writer.WriteFile(p.nextBlobPath(), data)
	if err != nil {
		return "", clicore.NewError("pack blob: "+err.Error(), clicore.ExitAPI)
	}
	p.recordBlob(file, put.GzipBlobMediaType)
	return file.Digest, nil
}

func (p *putOutboxPacker) nextBlobPath() string {
	path := fmt.Sprintf(outboxBlobPathFormat, p.files)
	p.files++
	return path
}

// recordBlob lists a blob once per digest. Put stores one blob per digest, so
// a second file with the same bytes is the same blob, and a put block names
// each digest once.
func (p *putOutboxPacker) recordBlob(file extensionproto.OutboxFile, mediaType string) {
	for _, blob := range p.blobs {
		if blob.Digest == file.Digest {
			return
		}
	}
	p.blobs = append(p.blobs, extensionproto.OutboxPutBlob{Path: file.Path, Digest: file.Digest, Size: file.Size, MediaType: mediaType})
}

// commit writes the manifest payload, adds the one member that names it and
// the blobs it references, and writes the descriptor. kind is the member's
// release-set kind and project the plan's project id, with its leading slash.
// A packed blob the manifest does not reference is left out: Put links exactly
// the referenced blobs, and the engine uploads a put block only when its blobs
// are exactly those.
//
// Before it writes the manifest, commit runs the put-write/v1 check the
// engine's upload node runs (putpublish.Check), so a member the engine would
// refuse, such as an archive with an invalid platform key, fails here.
func (p *putOutboxPacker) commit(kind distributionproto.MemberKind, ecosystem, coordinate, version, project, mediaType string, manifest []byte) (extensionproto.OutboxMember, error) {
	references, err := put.BlobReferences(manifest)
	if err != nil {
		return extensionproto.OutboxMember{}, clicore.NewError("pack manifest: "+err.Error(), clicore.ExitAPI)
	}
	var blobs []extensionproto.OutboxPutBlob
	for _, blob := range p.blobs {
		if slices.Contains(references, blob.Digest) {
			blobs = append(blobs, blob)
		}
	}
	if len(blobs) != len(references) {
		return extensionproto.OutboxMember{}, clicore.NewError(fmt.Sprintf("pack manifest: it references %d blob(s); the job packed %d of them", len(references), len(blobs)), clicore.ExitAPI)
	}
	if err := p.check(kind, coordinate, version, mediaType, manifest, blobs); err != nil {
		return extensionproto.OutboxMember{}, err
	}
	file, err := p.writer.WriteFile(outboxManifestPath, manifest)
	if err != nil {
		return extensionproto.OutboxMember{}, clicore.NewError("pack manifest: "+err.Error(), clicore.ExitAPI)
	}
	member := extensionproto.OutboxMember{
		Ecosystem: ecosystem, Coordinate: coordinate, Version: version, Project: project,
		Put: &extensionproto.OutboxPut{MediaType: mediaType, Manifest: file, Blobs: blobs},
	}
	if err := p.writer.Add(member); err != nil {
		return extensionproto.OutboxMember{}, clicore.NewError("pack "+coordinate+"@"+version+": "+err.Error(), clicore.ExitUsage)
	}
	if err := p.writer.Commit(); err != nil {
		return extensionproto.OutboxMember{}, clicore.NewError("commit the publication outbox: "+err.Error(), clicore.ExitAPI)
	}
	return member, nil
}

// check applies putpublish.Check to the member commit is about to write. A
// blob reads back from the outbox file the packer wrote.
func (p *putOutboxPacker) check(kind distributionproto.MemberKind, coordinate, version, mediaType string, manifest []byte, blobs []extensionproto.OutboxPutBlob) error {
	member := putpublish.Member{Kind: kind, Coordinate: coordinate, Version: version, MediaType: mediaType, Manifest: manifest}
	for _, blob := range blobs {
		path := filepath.Join(p.dir, filepath.FromSlash(blob.Path))
		member.Blobs = append(member.Blobs, putpublish.Blob{
			MediaType: blob.MediaType, Digest: blob.Digest, Size: blob.Size,
			Read: func() ([]byte, error) { return os.ReadFile(path) },
		})
	}
	if _, err := putpublish.Check(member); err != nil {
		return clicore.NewError("pack "+coordinate+"@"+version+": "+err.Error(), clicore.ExitUsage)
	}
	return nil
}

// PackConfigMember packs one canonical Config-authored member into the
// publication outbox: the manifest payload PublishConfigMember publishes, with
// the same media type and no blob. It uploads nothing and moves no channel.
// projectID is the plan's project id of the member.
func PackConfigMember(outbox, projectID string, publication ConfigMemberPublication) (*ConfigMemberPublicationResult, error) {
	if err := checkConfigMemberPublication(publication); err != nil {
		return nil, err
	}
	packer, err := newPutOutboxPacker(outbox)
	if err != nil {
		return nil, err
	}
	coordinate := publication.Namespace + "/" + publication.Package
	member, err := packer.commit(distributionproto.KindConfig, extensionproto.OutboxEcosystemPut, coordinate, publication.Version, projectID,
		configAuthoredMemberMediaType, publication.Payload)
	if err != nil {
		return nil, err
	}
	return &ConfigMemberPublicationResult{
		Coordinate: coordinate, Version: publication.Version, ArtifactDigest: member.Put.Manifest.Digest,
	}, nil
}
