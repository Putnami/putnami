package jobs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"

	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	registryproto "go.putnami.dev/protocol/registry"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/releaseset"
)

// PublicationProvider is the publication-v1 surface of the run's credential
// provider (registry ADR 0003). A release-set publish that obtains one
// resolves, opens, uploads and releases through it, in that order, and asks
// no other authority: no nested provider process starts, and no publication
// job receives a credential.
type PublicationProvider interface {
	// Resolve answers the heads of the request's channels.
	Resolve(ctx context.Context, request *distribution.ResolveRequest) (*distribution.ResolveResponse, error)
	// Open opens the plan the run publishes, once, before any upload.
	Open(ctx context.Context, params *registryproto.OpenParams) error
	// Release releases the opened plan's set. A refusal is an error that
	// names its code; no channel moved.
	Release(ctx context.Context, params *registryproto.ReleaseParams) (*distribution.ReleaseResponse, error)
	// PublishBearer returns the publish bearer for target. It is an error when
	// the provider's credential does not name target's host and port.
	PublishBearer(ctx context.Context, target *url.URL) (string, error)
}

// PublicationSource answers the run's publication provider. A nil provider
// with a nil error means the run has none: no source is installed, or the
// provider did not negotiate publication-v1. An error is a provider failure,
// and the publish fails rather than falling back to another authority.
type PublicationSource func(ctx context.Context) (PublicationProvider, error)

// publicationSource is the installed source; nil without one.
var publicationSource atomic.Pointer[PublicationSource]

// InstallPublicationProvider makes source the run's publication provider, and
// returns the function that restores the previous source. A nil source
// removes it.
func InstallPublicationProvider(source PublicationSource) (restore func()) {
	var next *PublicationSource
	if source != nil {
		next = &source
	}
	previous := publicationSource.Swap(next)
	return func() { publicationSource.Store(previous) }
}

// currentPublicationProvider answers the installed source, or nil without
// one.
func currentPublicationProvider(ctx context.Context) (PublicationProvider, error) {
	source := publicationSource.Load()
	if source == nil {
		return nil, nil
	}
	return (*source)(ctx)
}

// AncestryReader is the ancestry snapshot the engine read before any
// repository code ran: the commits its bound commit reaches.
type AncestryReader interface {
	// SourceRevision is the bound commit, "" when it could not be read.
	SourceRevision() string
	// Commits is the number of commits the snapshot holds.
	Commits() int
	// Shallow reports a snapshot read from a shallow clone.
	Shallow() bool
	// Position is rev's index in the snapshot, the bound commit at 0, and
	// false when the snapshot does not hold rev.
	Position(rev string) (int, bool)
	// Err is why the snapshot holds no commit, nil when it was read whole.
	Err() error
}

// sessionResolver resolves through the publication provider. Release is not
// part of it: a publication-v1 release carries the opened plan's digest, the
// ancestry and the evidence (ReleaseSetRun.release).
type sessionResolver struct{ publication PublicationProvider }

func (r sessionResolver) Resolve(ctx context.Context, request *distribution.ResolveRequest) (*distribution.ResolveResponse, error) {
	return r.publication.Resolve(ctx, request)
}

func (sessionResolver) Release(context.Context, *distribution.ReleaseRequest) (*distribution.ReleaseResponse, error) {
	return nil, errors.New("a publication-v1 release names its opened plan; it goes through the session")
}

// Publication reports whether the run publishes through a publication-v1
// provider.
func (run *ReleaseSetRun) Publication() bool {
	return run != nil && run.publication != nil
}

// BindPublication builds what the open node sends: the plan tuple and the
// engine's ancestry statement for every channel the plan advances. ancestry
// is the snapshot read before any repository code ran; barrier lists the
// invocation commands a bound request requires every publication to wait for,
// nil for a local run. A run without a publication provider binds nothing.
//
// The statement is refused, and the run fails before any job, when the
// snapshot is missing, failed, shallow, or bound to another commit than the
// plan's source revision.
func (run *ReleaseSetRun) BindPublication(ancestry AncestryReader, barrier []string) error {
	if !run.Publication() {
		return nil
	}
	plan, err := run.publicationPlan()
	if err != nil {
		return err
	}
	statement, err := run.publicationAncestry(ancestry, plan)
	if err != nil {
		return err
	}
	run.open = &registryproto.OpenParams{Plan: plan, Ancestry: statement}
	run.barrierCommands = slices.Clone(barrier)
	return nil
}

// publicationPlan is the plan tuple open carries: the same tuple, and the same
// digest, as the release-plan contract.
func (run *ReleaseSetRun) publicationPlan() (registryproto.PublicationPlan, error) {
	var zero registryproto.PublicationPlan
	handoff, err := run.capabilityPlan()
	if err != nil {
		return zero, fmt.Errorf("release-set publish: %w", err)
	}
	plan := registryproto.PublicationPlan{
		ProtocolVersion:  registryproto.PublicationPlanProtocolVersion,
		Namespace:        handoff.Namespace,
		SourceRevision:   handoff.SourceRevision,
		Channels:         handoff.Channels,
		ImmutableChannel: handoff.ImmutableChannel,
		Members:          make([]registryproto.PublicationPlanMember, 0, len(handoff.Members)),
	}
	for _, member := range handoff.Members {
		plan.Members = append(plan.Members, registryproto.PublicationPlanMember{
			Ecosystem: member.Ecosystem, Coordinate: member.Coordinate, Version: member.Version,
			SourceRevision: member.SourceRevision, SelectionFingerprint: member.SelectionFingerprint,
		})
	}
	slices.SortFunc(plan.Members, func(a, b registryproto.PublicationPlanMember) int {
		return cmp.Or(cmp.Compare(a.Ecosystem, b.Ecosystem), cmp.Compare(a.Coordinate, b.Coordinate))
	})
	if plan.PlanDigest, err = registryproto.PlanDigest(plan); err != nil {
		return zero, fmt.Errorf("release-set publish: %w", err)
	}
	if err := registryproto.ValidatePublicationPlan(plan); err != nil {
		return zero, fmt.Errorf("release-set publish: %w", err)
	}
	return plan, nil
}

// publicationAncestry states, for every channel of plan in order, the head's
// source revision and whether the snapshot holds it. The immutable channel
// and a channel without a head state no revision.
func (run *ReleaseSetRun) publicationAncestry(reader AncestryReader, plan registryproto.PublicationPlan) (registryproto.PublicationAncestry, error) {
	var zero registryproto.PublicationAncestry
	switch {
	case reader == nil:
		return zero, errors.New("release-set publish: the run read no ancestry before repository code ran")
	case reader.Err() != nil:
		return zero, fmt.Errorf("release-set publish: %w", reader.Err())
	case reader.Shallow():
		return zero, errors.New("release-set publish: the ancestry was read from a shallow clone; fetch the full history")
	case reader.Commits() < 1:
		return zero, errors.New("release-set publish: the ancestry snapshot holds no commit")
	case reader.SourceRevision() != plan.SourceRevision:
		return zero, fmt.Errorf("release-set publish: the ancestry was read from %s, but the plan publishes %s",
			reader.SourceRevision(), plan.SourceRevision)
	}
	statement := registryproto.PublicationAncestry{
		SourceRevision:  plan.SourceRevision,
		SnapshotCommits: reader.Commits(),
		Channels:        make([]registryproto.PublicationChannelAncestry, 0, len(plan.Channels)),
	}
	for _, name := range plan.Channels {
		entry := registryproto.PublicationChannelAncestry{Name: name}
		if head := run.plan.Heads[name]; head != nil && name != run.immutableChannel {
			entry.HeadSourceRevision, entry.Ancestor = headAncestry(reader, head)
		}
		statement.Channels = append(statement.Channels, entry)
	}
	if err := registryproto.ValidatePublicationAncestry(statement); err != nil {
		return zero, fmt.Errorf("release-set publish: %w", err)
	}
	return statement, nil
}

// headAncestry is the source revision of head and whether reader holds it. A
// head records a source revision per member, not one for the set: an
// unchanged member keeps the revision it was built from. The head is an
// ancestor when the snapshot holds every member's revision, and its revision
// is then the one nearest the bound commit. Otherwise the first revision, in
// sorted order, that the snapshot does not hold is reported, not an ancestor.
// A head with no member states no revision.
func headAncestry(reader AncestryReader, head *distribution.ChannelHead) (string, bool) {
	if head.ReleaseSet == nil {
		return "", false
	}
	revisions := make([]string, 0, len(head.ReleaseSet.Members))
	for _, member := range head.ReleaseSet.Members {
		if member.SourceRevision != "" {
			revisions = append(revisions, member.SourceRevision)
		}
	}
	slices.Sort(revisions)
	revisions = slices.Compact(revisions)
	nearest, nearestPosition := "", -1
	for _, revision := range revisions {
		position, held := reader.Position(revision)
		if !held {
			return revision, false
		}
		if nearestPosition < 0 || position < nearestPosition {
			nearest, nearestPosition = revision, position
		}
	}
	return nearest, nearest != ""
}

// release sends the one release of the run. Without a publication provider it
// is the provider process's release. With one, it carries the opened plan's
// digest, the ancestry stated at open, and the evidence inline; the answer is
// bound to the request before it is trusted.
func (run *ReleaseSetRun) release(ctx context.Context, request *distribution.ReleaseRequest, finalSet *distribution.ReleaseSet) (*distribution.ReleaseResponse, error) {
	if !run.Publication() {
		return run.provider.Release(run.withMemberEvidence(withSelectedImageEvidence(ctx, run.plan, finalSet), finalSet), request)
	}
	if run.open == nil {
		return nil, errors.New("no plan was opened")
	}
	evidence, err := run.publicationEvidence(finalSet)
	if err != nil {
		return nil, err
	}
	response, err := run.publication.Release(ctx, &registryproto.ReleaseParams{
		PlanDigest: run.open.Plan.PlanDigest,
		Request:    *request,
		Ancestry:   run.open.Ancestry,
		Evidence:   evidence,
	})
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, errors.New("the provider returned no release answer")
	}
	if diagnostics := distribution.ValidateReleaseExchange(request, response); diag.HasErrors(diagnostics) {
		return nil, fmt.Errorf("invalid provider exchange: %s", firstReleaseSetDiagnostic(diagnostics))
	}
	return response, nil
}

// publicationEvidence is the evidence a publication-v1 release carries: the
// image of every selected OCI member by project, and every selected member's
// publishing route, each list in its protocol order.
func (run *ReleaseSetRun) publicationEvidence(set *distribution.ReleaseSet) (registryproto.PublicationEvidence, error) {
	evidence := registryproto.PublicationEvidence{
		Images:  []runtimeproto.ReleaseSetPublishedImage{},
		Members: []registryproto.PublicationMemberEvidence{},
	}
	final := make(map[string]distribution.ReleaseSetMember, len(set.Members))
	for _, member := range set.Members {
		final[releaseset.MemberKey(member.Ecosystem, member.Coordinate)] = member
	}
	var images []runtimeproto.ReleaseSetPublishedImage
	for _, selected := range run.plan.SelectedMembers() {
		key := releaseset.MemberKey(selected.Ecosystem, selected.Coordinate)
		member, ok := final[key]
		if !ok {
			return evidence, fmt.Errorf("the released set lacks selected member %s", printableReleaseKey(key))
		}
		project := strings.TrimPrefix(selected.ProjectID, "/")
		if selected.Ecosystem == distribution.Ecosystem(extproto.OutboxEcosystemOCI) {
			images = append(images, runtimeproto.ReleaseSetPublishedImage{Project: project, Digest: member.ArtifactDigest})
		}
		route, ok := run.routes[key]
		if !ok {
			return evidence, fmt.Errorf("selected member %s has no publishing route", printableReleaseKey(key))
		}
		evidence.Members = append(evidence.Members, registryproto.PublicationMemberEvidence{
			Project: project, Ecosystem: string(member.Ecosystem), Coordinate: member.Coordinate,
			Version: member.Version, Digest: member.ArtifactDigest,
			Publisher: route.publisher, Command: route.publishCommand, Step: route.publishStep,
		})
	}
	slices.SortFunc(evidence.Members, func(a, b registryproto.PublicationMemberEvidence) int {
		return cmp.Or(cmp.Compare(a.Ecosystem, b.Ecosystem), cmp.Compare(a.Coordinate, b.Coordinate))
	})
	if len(images) > 0 {
		encoded, err := runtimeproto.MarshalReleaseSetPublishedImages(images)
		if err != nil {
			return evidence, fmt.Errorf("release evidence: %w", err)
		}
		if evidence.Images, err = runtimeproto.ParseReleaseSetPublishedImages(encoded); err != nil {
			return evidence, fmt.Errorf("release evidence: %w", err)
		}
	}
	if err := registryproto.ValidatePublicationEvidence(evidence); err != nil {
		return evidence, fmt.Errorf("release evidence: %w", err)
	}
	return evidence, nil
}
