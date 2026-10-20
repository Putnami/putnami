package deliverycli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/oci"
	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/publicationupload"
)

// upload uploads one member of the open plan to its registry with the SDK
// upload kit (uploader-v1) and answers the digest the registry stores.
//
// Before any registry request, it refuses an upload sent before open
// (plan_not_open), one that names another plan or a member the open plan does
// not publish (plan_mismatch), and one whose registry host is not one of this
// run's publication origins (registry_not_served). The bearer is the open
// plan's write bearer, which this process mints or reuses (publishBearer). The
// kit gets it only for a publication origin, and no answer carries it.
func (p *publisher) upload(session context.Context, payload json.RawMessage) publicationAnswer {
	params, err := registry.ParseUploadParams(payload)
	if err != nil {
		return publicationRefusal(refusalInvalidRequest, "the upload payload does not follow the uploader-v1 protocol")
	}
	member := params.Member
	name := fmt.Sprintf("%s member %s@%s", member.Ecosystem, member.Coordinate, member.Version)

	p.mu.Lock()
	opened := p.opened
	p.mu.Unlock()
	switch {
	case opened == nil:
		return p.refuse("upload", registry.RefusalPlanNotOpen, "no plan is open in this session")
	case params.PlanDigest != opened.Plan.PlanDigest:
		return p.refuse("upload", registry.RefusalPlanMismatch, "the upload names plan %s, but plan %s is open", params.PlanDigest, opened.Plan.PlanDigest)
	case !publishes(opened.Plan, member):
		return p.refuse("upload", registry.RefusalPlanMismatch, "plan %s does not publish the %s", opened.Plan.PlanDigest, name)
	}
	origins := registry.Credential{Hosts: p.endpoints.hosts}
	if target, err := uploadTarget(params); err != nil || !origins.Serves(target) {
		return p.refuse("upload", registry.RefusalRegistryNotServed, "the %s goes to a registry outside this run's publication origins (%s)",
			name, strings.Join(p.endpoints.hosts, ", "))
	}

	ctx, cancel := context.WithTimeout(session, p.opTimeout)
	grant := p.publishBearer(ctx, opened.Plan, false)
	cancel()
	if grant.refusal != nil {
		return publicationAnswer{refusal: grant.refusal}
	}
	bearer := func(_ context.Context, target *url.URL) (string, error) {
		if !origins.Serves(target) {
			return "", errors.New("the upload asked a bearer for a host outside this run's publication origins")
		}
		return grant.bearer.bearer, nil
	}
	stored, err := publicationupload.Upload(session, publicationupload.Request{
		Outbox: params.Outbox, Member: member, Registry: params.Registry, Kind: params.Kind,
	}, bearer)
	if err != nil {
		return p.uploadRefusal(name, err)
	}
	result := registry.UploadResult{Digest: stored.Digest, Platforms: stored.Platforms}
	if err := registry.ValidateUploadResult(result); err != nil {
		return p.uploadRefusal(name, err)
	}
	return publicationAnswer{payload: result}
}

// uploadRefusal maps a failed upload to its refusal: an artifact whose size or
// digest is not the member's is artifact_digest_mismatch, a registry that
// already holds another artifact at the version is artifact_conflict, and any
// other failure is upload_failed. The message is redacted of every secret
// this session knows.
func (p *publisher) uploadRefusal(name string, err error) publicationAnswer {
	code := registry.RefusalUploadFailed
	switch {
	case errors.Is(err, publicationupload.ErrArtifactMismatch):
		code = registry.RefusalArtifactDigestMismatch
	case errors.Is(err, publicationupload.ErrArtifactConflict):
		code = registry.RefusalArtifactConflict
	}
	message := p.safeText(fmt.Sprintf("the upload of the %s failed: %v", name, err))
	p.diagnose("upload: refused (%s): %s", code, message)
	return publicationAnswer{refusal: &registry.CredentialRefusal{Code: code, Message: message}}
}

// publishes reports whether plan publishes member: a plan member of the same
// ecosystem, coordinate and version.
func publishes(plan registry.PublicationPlan, member extproto.OutboxMember) bool {
	return slices.ContainsFunc(plan.Members, func(planned registry.PublicationPlanMember) bool {
		return string(planned.Ecosystem) == member.Ecosystem && planned.Coordinate == member.Coordinate && planned.Version == member.Version
	})
}

// uploadTarget is the registry an upload reaches: the request's registry for
// an npm, go, put or archive member; for an oci member, the registry host of
// its repository, with the scheme an OCI client reaches that host with.
func uploadTarget(params *registry.UploadParams) (*url.URL, error) {
	if params.Member.Ecosystem != extproto.OutboxEcosystemOCI {
		return url.Parse(params.Registry)
	}
	host, err := oci.RegistryHost(params.Member.OCI.Repository)
	if err != nil {
		return nil, err
	}
	return &url.URL{Scheme: oci.RegistryScheme(host), Host: host}, nil
}
