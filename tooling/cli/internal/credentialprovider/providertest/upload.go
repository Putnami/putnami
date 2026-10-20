package providertest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	extension "go.putnami.dev/protocol/extension"
	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/gomodpublish"
	"go.putnami.dev/sdk/extension/npmpublish"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/putpublish"
)

// Upload is one member the provider uploaded, and the answer it gave.
type Upload struct {
	// Params is the upload request.
	Params registry.UploadParams
	// Result is the provider's answer.
	Result registry.UploadResult
}

// Upload uploads one packed member of the open plan, as an uploader-v1
// provider does: with the credential it holds (Config.Bearer), to the
// request's registry only when the credential serves that registry's host.
// It reads every file of the member from the outbox and refuses one whose
// size or digest is not the member's. An npm, Go, put or archive member goes
// to its registry through the SDK publisher of its ecosystem; an OCI member's
// layout is checked to hold the packed manifest and recorded, with no push.
// Config.UploadAnswer may rewrite the answer.
func (s *Session) Upload(ctx context.Context, params registry.UploadParams) (*registry.UploadResult, *registry.CredentialRefusal) {
	p := s.provider
	if code := p.config.Refuse[registry.CredentialOpUpload]; code != "" {
		return nil, refuse(code, "the provider refuses upload")
	}
	s.mu.Lock()
	open := s.open
	s.mu.Unlock()
	switch {
	case open == nil:
		return nil, refuse(registry.RefusalPlanNotOpen, "no plan is open in this session")
	case params.PlanDigest != open.Plan.PlanDigest:
		return nil, refuse(registry.RefusalPlanMismatch, "upload names another plan than the open one")
	}
	target, err := url.Parse(params.Registry)
	credential := registry.Credential{Bearer: p.config.Bearer, Hosts: p.config.Hosts}
	if err != nil || p.config.Bearer == "" || !credential.Serves(target) {
		return nil, refuse(registry.RefusalRegistryNotServed, "the provider holds no credential for the upload registry")
	}
	outbox, err := publicationoutbox.Open(params.Outbox)
	if err != nil {
		return nil, refuse(registry.RefusalUploadFailed, boundedMessage(err))
	}
	var result *registry.UploadResult
	switch params.Member.Ecosystem {
	case extension.OutboxEcosystemNPM:
		result, err = uploadNPM(ctx, outbox, params, p.config.Bearer)
	case extension.OutboxEcosystemGo:
		result, err = uploadGo(ctx, outbox, params, p.config.Bearer)
	case extension.OutboxEcosystemOCI:
		result, err = checkLayout(outbox, params.Member)
	case extension.OutboxEcosystemPut, extension.OutboxEcosystemArchive:
		result, err = uploadPut(ctx, outbox, params, p.config.Bearer)
	default:
		err = fmt.Errorf("ecosystem %q has no upload", params.Member.Ecosystem)
	}
	var mismatch *artifactMismatchError
	switch {
	case errors.As(err, &mismatch):
		return nil, refuse(registry.RefusalArtifactDigestMismatch, boundedMessage(err))
	case err != nil:
		return nil, refuse(registry.RefusalUploadFailed, boundedMessage(err))
	}
	if p.config.UploadAnswer != nil {
		answered := p.config.UploadAnswer(params, *result)
		result = &answered
	}
	p.mu.Lock()
	p.uploads = append(p.uploads, Upload{Params: params, Result: *result})
	p.mu.Unlock()
	return result, nil
}

// Uploads returns every upload the provider applied, in order.
func (p *Provider) Uploads() []Upload {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.uploads)
}

// artifactMismatchError is an outbox file whose size or digest is not the
// member's.
type artifactMismatchError struct{ err error }

func (e *artifactMismatchError) Error() string { return e.err.Error() }

// readFile reads file from outbox, verified against its size and digest.
func readFile(outbox *publicationoutbox.Outbox, file extension.OutboxFile) ([]byte, error) {
	data, err := outbox.ReadFile(file)
	if err != nil {
		return nil, &artifactMismatchError{err: err}
	}
	return data, nil
}

func uploadNPM(ctx context.Context, outbox *publicationoutbox.Outbox, params registry.UploadParams, bearer string) (*registry.UploadResult, error) {
	member := params.Member
	endpoint, err := extension.ManagedNPMRegistry(params.Registry)
	if err != nil {
		return nil, err
	}
	tarball, err := readFile(outbox, member.NPM.Tarball)
	if err != nil {
		return nil, err
	}
	manifest, err := readFile(outbox, member.NPM.Manifest)
	if err != nil {
		return nil, err
	}
	client, err := npmpublish.NewHTTPClient()
	if err != nil {
		return nil, err
	}
	artifact := npmpublish.Artifact{Name: member.Coordinate, Version: member.Version, Manifest: manifest, Tarball: tarball}
	if _, err := npmpublish.Publish(ctx, client, endpoint, bearer, artifact); err != nil {
		return nil, err
	}
	return &registry.UploadResult{Digest: npmpublish.Digest(tarball)}, nil
}

func uploadGo(ctx context.Context, outbox *publicationoutbox.Outbox, params registry.UploadParams, bearer string) (*registry.UploadResult, error) {
	member := params.Member
	endpoint, err := gomodpublish.ValidateRegistryURL(params.Registry)
	if err != nil {
		return nil, err
	}
	zip, err := readFile(outbox, member.Go.Zip)
	if err != nil {
		return nil, err
	}
	mod, err := readFile(outbox, member.Go.Mod)
	if err != nil {
		return nil, err
	}
	info, err := readFile(outbox, member.Go.Info)
	if err != nil {
		return nil, err
	}
	var stated struct{ Version string }
	if err := json.Unmarshal(info, &stated); err != nil || stated.Version != member.Version {
		return nil, fmt.Errorf("the module .info does not state version %s", member.Version)
	}
	client, err := gomodpublish.NewHTTPClient()
	if err != nil {
		return nil, err
	}
	digest, _, err := gomodpublish.Publish(ctx, client, nil, endpoint, bearer, gomodpublish.Module{
		Path: member.Coordinate, Version: member.Version, Zip: zip, Mod: mod,
	})
	if err != nil {
		return nil, err
	}
	return &registry.UploadResult{Digest: digest}, nil
}

// uploadPut checks the member's shape for its kind (putpublish.Check) and the
// size and digest of every blob before it sends anything, then uploads the
// blobs and the manifest with no channel.
func uploadPut(ctx context.Context, outbox *publicationoutbox.Outbox, params registry.UploadParams, bearer string) (*registry.UploadResult, error) {
	packed := params.Member
	endpoint, err := putpublish.ValidateRegistryURL(params.Registry)
	if err != nil {
		return nil, err
	}
	manifest, err := readFile(outbox, packed.Put.Manifest)
	if err != nil {
		return nil, err
	}
	member := putpublish.Member{
		Kind: params.Kind, Coordinate: packed.Coordinate, Version: packed.Version,
		MediaType: packed.Put.MediaType, Manifest: manifest,
	}
	for _, blob := range packed.Put.Blobs {
		file := blob.File()
		member.Blobs = append(member.Blobs, putpublish.Blob{
			MediaType: blob.MediaType, Digest: blob.Digest, Size: blob.Size,
			Read: func() ([]byte, error) { return readFile(outbox, file) },
		})
	}
	if _, err := putpublish.Check(member); err != nil {
		return nil, err
	}
	for _, blob := range packed.Put.Blobs {
		if err := outbox.VerifyFile(blob.File()); err != nil {
			return nil, &artifactMismatchError{err: err}
		}
	}
	client, err := putpublish.NewHTTPClient()
	if err != nil {
		return nil, err
	}
	published, err := putpublish.Publish(ctx, client, endpoint, bearer, member)
	if err != nil {
		return nil, err
	}
	return &registry.UploadResult{Digest: published.Digest, Platforms: published.Platforms}, nil
}

// checkLayout answers the digest of the manifest an OCI member's layout holds:
// its index.json must list the packed digest, and the blob at that digest
// must hash to it. Nothing is pushed.
func checkLayout(outbox *publicationoutbox.Outbox, member extension.OutboxMember) (*registry.UploadResult, error) {
	layout, err := outbox.Layout(member.OCI.Layout)
	if err != nil {
		return nil, &artifactMismatchError{err: err}
	}
	data, err := os.ReadFile(filepath.Join(layout, "index.json"))
	if err != nil {
		return nil, &artifactMismatchError{err: err}
	}
	var index layoutIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, &artifactMismatchError{err: fmt.Errorf("layout index: %w", err)}
	}
	if !slices.ContainsFunc(index.Manifests, func(entry layoutDescriptor) bool { return entry.Digest == member.OCI.Digest }) {
		return nil, &artifactMismatchError{err: errors.New("the layout index does not list the packed manifest")}
	}
	hex, found := strings.CutPrefix(member.OCI.Digest, "sha256:")
	if !found {
		return nil, &artifactMismatchError{err: errors.New("the packed manifest digest is not sha256")}
	}
	manifest, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", hex))
	if err != nil {
		return nil, &artifactMismatchError{err: err}
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
	if digest != member.OCI.Digest {
		return nil, &artifactMismatchError{err: fmt.Errorf("the layout holds manifest %s, not the packed %s", digest, member.OCI.Digest)}
	}
	return &registry.UploadResult{Digest: digest}, nil
}

// layoutIndex is the part of an OCI layout's index.json checkLayout reads.
type layoutIndex struct {
	Manifests []layoutDescriptor `json:"manifests"`
}

// layoutDescriptor is one manifest an OCI layout's index lists.
type layoutDescriptor struct {
	Digest string `json:"digest"`
}

// boundedMessage is err's text cut to registry.MaxRefusalMessageBytes.
func boundedMessage(err error) string {
	message := err.Error()
	if len(message) <= registry.MaxRefusalMessageBytes {
		return message
	}
	return strings.ToValidUTF8(message[:registry.MaxRefusalMessageBytes], "")
}
