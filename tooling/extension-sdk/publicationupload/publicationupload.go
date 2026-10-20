// Package publicationupload uploads one member of a publication outbox to its
// registry: an npm version, a Go module version, an OCI image, or a Put
// registry version of a put or archive member.
//
// Upload holds the upload and nothing else. Its caller reads the outbox,
// checks the member against its plan and its project's declarations, and
// resolves the registry before it calls Upload. Upload reads only the files
// the member's entries name, with publicationoutbox.Outbox.ReadFile and
// Outbox.Layout, and never reads outbox.json, so a descriptor rewritten after
// the caller's check cannot redirect it.
//
// Upload asks its caller for the bearer of the registry it uploads to, at most
// once, and hands it to the SDK uploader of the member's ecosystem
// (npmpublish, gomodpublish, putpublish or oci.PushLayout), which sends it to
// that registry alone, through no ambient proxy, and puts it in no error.
//
// An Upload error matches ErrArtifactMismatch or ErrArtifactConflict under
// errors.Is when the failure is one of those two, and neither otherwise.
package publicationupload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/gomodpublish"
	"go.putnami.dev/sdk/extension/internal/artifacterr"
	"go.putnami.dev/sdk/extension/npmpublish"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/putpublish"
)

var (
	// ErrArtifactMismatch marks an Upload error for an artifact whose size or
	// digest is not the one the member's entry names.
	ErrArtifactMismatch = artifacterr.ErrMismatch
	// ErrArtifactConflict marks an Upload error for a registry that already
	// holds another artifact at the member's version.
	ErrArtifactConflict = artifacterr.ErrConflict
)

// BearerFunc returns the publish bearer for the registry at target: its
// scheme and host, and for npm, Go and Put its path. An error ends the upload
// before any request.
type BearerFunc func(ctx context.Context, target *url.URL) (string, error)

// Request is one member to upload.
type Request struct {
	// Outbox is the root directory of the publication outbox that holds the
	// member's files.
	Outbox string
	// Member is the outbox member the caller read and checked. Upload reads
	// the files its entries name and refuses a size or digest other than an
	// entry's.
	Member extproto.OutboxMember
	// Registry is the registry endpoint the caller resolved for an npm, go,
	// put or archive member. An oci member ignores it: its repository names
	// the registry host.
	Registry string
	// Kind is the release-set member kind of a put or archive member
	// (releaseset.KindFor). Other ecosystems ignore it.
	Kind distribution.MemberKind
}

// Result is what the registry stores.
type Result struct {
	// Digest is the member's artifact digest: the npm tarball's, the Go
	// module zip's, the OCI manifest's, or the SHA-256 of the Put manifest
	// payload the registry stores.
	Digest string
	// Platforms maps "<os>/<arch>" to a blob digest for an archive member. It
	// is nil for any other member.
	Platforms map[string]string
}

// Upload uploads the member of request and returns what the registry stores.
// It refuses, before it asks for the bearer, a member the outbox contract
// does not accept (extproto.ValidatePublicationOutbox) and a registry endpoint
// its ecosystem's rules refuse. It reads and verifies an npm, Go or Put
// member's files before it asks for the bearer; oci.PushLayout hashes an OCI
// layout's manifest before its first request. A version the registry holds at
// the same digest is reused; one it holds at another digest is an error.
func Upload(ctx context.Context, request Request, bearer BearerFunc) (Result, error) {
	if bearer == nil {
		return Result{}, errors.New("publication upload: no bearer source")
	}
	if err := checkMember(request.Member); err != nil {
		return Result{}, err
	}
	outbox, err := publicationoutbox.Open(request.Outbox)
	if err != nil {
		return Result{}, err
	}
	switch request.Member.Ecosystem {
	case extproto.OutboxEcosystemNPM:
		return uploadNPM(ctx, outbox, request, bearer)
	case extproto.OutboxEcosystemGo:
		return uploadGo(ctx, outbox, request, bearer)
	case extproto.OutboxEcosystemOCI:
		return uploadOCI(ctx, outbox, request.Member, bearer)
	case extproto.OutboxEcosystemPut, extproto.OutboxEcosystemArchive:
		return uploadPut(ctx, outbox, request, bearer)
	}
	return Result{}, fmt.Errorf("ecosystem %q has no upload", request.Member.Ecosystem)
}

// checkMember refuses a member the outbox contract does not accept on its
// own: an unknown ecosystem, a missing or extra block, or an entry whose path,
// size or digest the contract refuses.
func checkMember(member extproto.OutboxMember) error {
	diagnostics := extproto.ValidatePublicationOutbox(&extproto.PublicationOutbox{
		ProtocolVersion: extproto.PublicationOutboxVersion, Members: []extproto.OutboxMember{member},
	})
	if len(diagnostics) == 0 {
		return nil
	}
	messages := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		messages = append(messages, diagnostic.String())
	}
	return fmt.Errorf("invalid publication member: %s", strings.Join(messages, "; "))
}

// uploadNPM uploads the member to the npm registry of request, normalized by
// the managed rules (extproto.ManagedNPMRegistry).
func uploadNPM(ctx context.Context, outbox *publicationoutbox.Outbox, request Request, bearer BearerFunc) (Result, error) {
	member := request.Member
	endpoint, err := extproto.ManagedNPMRegistry(request.Registry)
	if err != nil {
		return Result{}, err
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return Result{}, err
	}
	tarball, err := outbox.ReadFile(member.NPM.Tarball)
	if err != nil {
		return Result{}, err
	}
	manifest, err := outbox.ReadFile(member.NPM.Manifest)
	if err != nil {
		return Result{}, err
	}
	token, err := bearer(ctx, target)
	if err != nil {
		return Result{}, err
	}
	client, err := npmpublish.NewHTTPClient()
	if err != nil {
		return Result{}, err
	}
	artifact := npmpublish.Artifact{Name: member.Coordinate, Version: member.Version, Manifest: manifest, Tarball: tarball}
	if _, err := npmpublish.Publish(ctx, client, endpoint, token, artifact); err != nil {
		return Result{}, err
	}
	return Result{Digest: npmpublish.Digest(tarball)}, nil
}

// uploadGo uploads the module to the Go module origin of request
// (gomodpublish.ValidateRegistryURL). The module's .info must state the
// member's version, and the registry must store the packed zip.
func uploadGo(ctx context.Context, outbox *publicationoutbox.Outbox, request Request, bearer BearerFunc) (Result, error) {
	member := request.Member
	endpoint, err := gomodpublish.ValidateRegistryURL(request.Registry)
	if err != nil {
		return Result{}, err
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return Result{}, err
	}
	zip, err := outbox.ReadFile(member.Go.Zip)
	if err != nil {
		return Result{}, err
	}
	mod, err := outbox.ReadFile(member.Go.Mod)
	if err != nil {
		return Result{}, err
	}
	info, err := outbox.ReadFile(member.Go.Info)
	if err != nil {
		return Result{}, err
	}
	var stated struct{ Version string }
	if err := json.Unmarshal(info, &stated); err != nil || stated.Version != member.Version {
		return Result{}, fmt.Errorf("the module .info does not state version %s", member.Version)
	}
	token, err := bearer(ctx, target)
	if err != nil {
		return Result{}, err
	}
	client, err := gomodpublish.NewHTTPClient()
	if err != nil {
		return Result{}, err
	}
	digest, _, err := gomodpublish.Publish(ctx, client, nil, endpoint, token, gomodpublish.Module{
		Path: member.Coordinate, Version: member.Version, Zip: zip, Mod: mod,
	})
	if err != nil {
		return Result{}, err
	}
	if digest != member.Go.Zip.Digest {
		return Result{}, fmt.Errorf("the registry stored zip digest %s, not the packed %s", digest, member.Go.Zip.Digest)
	}
	return Result{Digest: digest}, nil
}

// uploadPut uploads a Put registry member to the registry of request
// (putpublish.ValidateRegistryURL): the blobs its manifest references, one at
// a time, then the manifest, with no channel. Every check that needs no
// registry runs before the bearer is asked for: the member's shape for its
// kind (putpublish.Check), then the size and digest of every blob, streamed
// from the outbox. Each blob is read and verified again when it is uploaded.
// The digest is the SHA-256 of the manifest payload the registry stores,
// which must be the packed manifest's.
func uploadPut(ctx context.Context, outbox *publicationoutbox.Outbox, request Request, bearer BearerFunc) (Result, error) {
	packed := request.Member
	endpoint, err := putpublish.ValidateRegistryURL(request.Registry)
	if err != nil {
		return Result{}, err
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return Result{}, err
	}
	manifest, err := outbox.ReadFile(packed.Put.Manifest)
	if err != nil {
		return Result{}, err
	}
	member := putpublish.Member{
		Kind: request.Kind, Coordinate: packed.Coordinate, Version: packed.Version,
		MediaType: packed.Put.MediaType, Manifest: manifest,
	}
	for _, blob := range packed.Put.Blobs {
		file := blob.File()
		member.Blobs = append(member.Blobs, putpublish.Blob{
			MediaType: blob.MediaType, Digest: blob.Digest, Size: blob.Size,
			Read: func() ([]byte, error) { return outbox.ReadFile(file) },
		})
	}
	if _, err := putpublish.Check(member); err != nil {
		return Result{}, err
	}
	for _, blob := range packed.Put.Blobs {
		if err := outbox.VerifyFile(blob.File()); err != nil {
			return Result{}, err
		}
	}
	token, err := bearer(ctx, target)
	if err != nil {
		return Result{}, err
	}
	client, err := putpublish.NewHTTPClient()
	if err != nil {
		return Result{}, err
	}
	published, err := putpublish.Publish(ctx, client, endpoint, token, member)
	if err != nil {
		return Result{}, err
	}
	if published.Digest != packed.Put.Manifest.Digest {
		return Result{}, fmt.Errorf("the registry stored manifest %s, not the packed %s", published.Digest, packed.Put.Manifest.Digest)
	}
	return Result{Digest: published.Digest, Platforms: published.Platforms}, nil
}

// uploadOCI pushes the packed layout to the member's repository, whose
// registry host the bearer is asked for, and tags it with the member's tags.
// The registry must hold the packed manifest.
func uploadOCI(ctx context.Context, outbox *publicationoutbox.Outbox, member extproto.OutboxMember, bearer BearerFunc) (Result, error) {
	repository, err := name.NewRepository(member.OCI.Repository, name.StrictValidation)
	if err != nil {
		return Result{}, fmt.Errorf("OCI repository %q: %w", member.OCI.Repository, err)
	}
	layout, err := outbox.Layout(member.OCI.Layout)
	if err != nil {
		return Result{}, err
	}
	token, err := bearer(ctx, &url.URL{Scheme: repository.Scheme(), Host: repository.RegistryStr()})
	if err != nil {
		return Result{}, err
	}
	pushed, err := oci.PushLayout(ctx, layout, oci.LayoutTarget{
		Repository: member.OCI.Repository, Digest: member.OCI.Digest, Tags: member.OCI.Tags,
	}, token)
	if err != nil {
		return Result{}, err
	}
	if pushed.Digest != member.OCI.Digest {
		return Result{}, fmt.Errorf("the registry holds manifest %s, not the packed %s", pushed.Digest, member.OCI.Digest)
	}
	return Result{Digest: pushed.Digest}, nil
}
