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
// plan's write bearer, which this process mints or reuses (publishBearer),
// asked again before each registry request so it is renewed during a long
// upload. The kit gets it only for a publication origin, and no answer
// carries it.
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
	// The kit asks the bearer before each registry request, so a long upload
	// sends the bearer this process holds at that request: publishBearer
	// renews it once half its lifetime has passed.
	bearer := func(request context.Context, target *url.URL) (string, error) {
		if !origins.Serves(target) {
			return "", errors.New("the upload asked a bearer for a host outside this run's publication origins")
		}
		ctx, cancel := context.WithTimeout(request, p.opTimeout)
		defer cancel()
		renewed := p.publishBearer(ctx, opened.Plan, false)
		if renewed.refusal != nil {
			return "", fmt.Errorf("the plan's write bearer could not be renewed: %s", renewed.refusal.Message)
		}
		return renewed.bearer.bearer, nil
	}
	// A clean build's version names its commit: a version the registry holds
	// with another manifest was then published by an earlier attempt of this
	// commit, and is adopted so a retry passes.
	adopt := (member.Ecosystem == extproto.OutboxEcosystemPut || member.Ecosystem == extproto.OutboxEcosystemArchive) &&
		registry.VersionNamesRevision(member.Version, opened.Plan.SourceRevision)
	stored, err := publicationupload.Upload(session, publicationupload.Request{
		Outbox: params.Outbox, Member: member, Registry: params.Registry, Kind: params.Kind, AdoptPublished: adopt,
	}, bearer)
	if err != nil {
		return p.uploadRefusal(name, err)
	}
	if stored.Adopted {
		p.diagnose("upload: adopted the %s an earlier attempt of commit %s published", name, opened.Plan.SourceRevision)
	}
	result := registry.UploadResult{Digest: stored.Digest, Platforms: stored.Platforms, Adopted: stored.Adopted}
	if err := registry.ValidateUploadResult(result); err != nil {
		return p.uploadRefusal(name, err)
	}
	return publicationAnswer{payload: result}
}

// uploadRefusal maps a failed upload to its refusal: an artifact whose size or
// digest is not the member's is artifact_digest_mismatch, a registry that
// already holds another artifact at the version is artifact_conflict, whose
// message says a retry of this commit cannot pass, and any other failure is
// upload_failed; a registry that answered 401 or 403 to the bearer is named
// so. The message is redacted of every secret
// this session knows.
func (p *publisher) uploadRefusal(name string, err error) publicationAnswer {
	code := registry.RefusalUploadFailed
	reason := ""
	switch {
	case errors.Is(err, publicationupload.ErrArtifactMismatch):
		code = registry.RefusalArtifactDigestMismatch
	case errors.Is(err, publicationupload.ErrArtifactConflict):
		code = registry.RefusalArtifactConflict
		reason = "the version holds other bytes, and a retry of this commit cannot pass: "
	case errors.Is(err, publicationupload.ErrBearerRefused):
		reason = "the registry refused the publish bearer: "
	}
	message := p.safeText(fmt.Sprintf("the upload of the %s failed: %s%v", name, reason, err))
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
