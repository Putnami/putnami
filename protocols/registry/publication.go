package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// publication-v1 is the capability through which the engine's one credential
// provider session also resolves channel heads, opens a publication plan and
// releases its set (doc/adr/0003-publication-ops-behind-a-negotiated-capability.md).
// The engine offers CapabilityPublicationV1 at initialize; a provider that
// echoes it answers these ops on the same JSONL session:
//
//	→ {"protocolVersion":1,"id":2,"op":"resolve","payload":{"request":{…distribution.ResolveRequest…}}}
//	← {"protocolVersion":1,"id":2,"ok":true,"payload":{"response":{…distribution.ResolveResponse…}}}
//	→ {"protocolVersion":1,"id":3,"op":"open","payload":{"plan":{…PublicationPlan…},"ancestry":{…}}}
//	← {"protocolVersion":1,"id":3,"ok":true,"payload":{"planDigest":"sha256:…"}}
//	→ {"protocolVersion":1,"id":4,"op":"credential","payload":{"purpose":"publish"}}
//	→ {"protocolVersion":1,"id":5,"op":"release","payload":{"planDigest":"sha256:…","request":{…},"ancestry":{…},"evidence":{…}}}
//	← {"protocolVersion":1,"id":5,"ok":true,"payload":{"response":{…distribution.ReleaseResponse…}}}
//
// The request and response members of resolve and release are
// distribution/release-set/v2 documents, decoded and validated by
// go.putnami.dev/protocol/distribution; null is admitted only where that
// protocol admits it.
const (
	// PublicationPlanProtocolVersion is the version of the plan tuple open
	// carries.
	PublicationPlanProtocolVersion = 1
	// MaxPublicationPlanBytes bounds the JSON encoding of one plan tuple with
	// planDigest omitted, the bytes PlanDigest hashes.
	MaxPublicationPlanBytes = 1 << 20
	// MaxAncestrySnapshotCommits bounds the commits one ancestry snapshot
	// reads from the run's source revision.
	MaxAncestrySnapshotCommits = 1_000_000
	// MaxPublicationEvidenceBytes bounds the JSON encoding of each evidence
	// list of a release.
	MaxPublicationEvidenceBytes = 4 << 20
	// MaxEvidenceProjectBytes bounds the project of one evidence entry.
	MaxEvidenceProjectBytes = 512
	// MaxEvidenceTextBytes bounds the publisher, command and step of one
	// member evidence entry.
	MaxEvidenceTextBytes = 256
)

// Wire grammars of publication-v1, shared with schemas/credential-provider-v1.json.
const (
	// PlanDigestPattern is the grammar of a plan digest and of every other
	// sha256 digest publication-v1 carries.
	PlanDigestPattern = `^sha256:[0-9a-f]{64}$`
	// SourceRevisionPattern is the grammar of a source revision: a full
	// lowercase hex commit.
	SourceRevisionPattern = `^[0-9a-f]{40}$`
)

// Refusal codes a publication-v1 provider answers with. Each matches
// RefusalCodePattern. A compare-and-swap miss at release is not a refusal: it
// is a release answer whose outcome is conflict.
const (
	// RefusalPlanNotOpen: no plan is open in the session, and the op needs
	// one: credential with purpose publish, or release.
	RefusalPlanNotOpen = "plan_not_open"
	// RefusalPlanAlreadyOpen: open names a plan whose planDigest differs from
	// the plan already open in the session.
	RefusalPlanAlreadyOpen = "plan_already_open"
	// RefusalPlanMismatch: release names a planDigest other than the opened
	// plan's, carries a request that does not match the opened plan, or asks
	// again under a planDigest whose stored release derived another set ref.
	RefusalPlanMismatch = "plan_mismatch"
	// RefusalArtifactMissing: a selected member of the opened plan has no
	// artifact in its registry at release.
	RefusalArtifactMissing = "artifact_missing"
	// RefusalArtifactDigestMismatch: a member's artifactDigest differs from
	// the artifact its registry stores.
	RefusalArtifactDigestMismatch = "artifact_digest_mismatch"
	// RefusalNotForward: an advanced channel's head is not the run's source
	// revision or one of its ancestors, at open or at release.
	RefusalNotForward = "not_forward"
	// RefusalConflict: a concurrent publication holds one of the plan's
	// channels; nothing was recorded.
	RefusalConflict = "conflict"
	// RefusalChannelImmutable: the plan moves an immutable channel that
	// already has a head, or names an existing immutable channel as mutable.
	RefusalChannelImmutable = "channel_immutable"
	// RefusalNamespaceForbidden: the run is not allowed to publish to the
	// plan's namespace.
	RefusalNamespaceForbidden = "namespace_forbidden"
)

// PublicationRefusalCodes lists the publication-v1 refusal codes in their
// declared order.
var PublicationRefusalCodes = []string{
	RefusalPlanNotOpen, RefusalPlanAlreadyOpen, RefusalPlanMismatch,
	RefusalArtifactMissing, RefusalArtifactDigestMismatch, RefusalNotForward,
	RefusalConflict, RefusalChannelImmutable, RefusalNamespaceForbidden,
}

var (
	sha256Digest    = regexp.MustCompile(PlanDigestPattern)
	sourceRevision  = regexp.MustCompile(SourceRevisionPattern)
	ecosystemFormat = regexp.MustCompile(distribution.EcosystemPattern)
)

// ResolveParams is the payload of resolve.
type ResolveParams struct {
	// Request is the distribution/release-set/v2 resolve request, in its
	// channels form.
	Request distribution.ResolveRequest `json:"request"`
}

// ResolveResult answers resolve.
type ResolveResult struct {
	// Response answers Request with one head entry per requested channel.
	// The engine binds it to the request with distribution.ValidateResolveExchange.
	Response distribution.ResolveResponse `json:"response"`
}

// OpenParams is the payload of open: the plan the run publishes and the
// ancestry the engine read before any repository code ran.
type OpenParams struct {
	// Plan is the plan tuple; Plan.PlanDigest is PlanDigest(Plan).
	Plan PublicationPlan `json:"plan"`
	// Ancestry names, for each of Plan.Channels in order, whether its head is
	// the plan's source revision or one of its ancestors.
	Ancestry PublicationAncestry `json:"ancestry"`
}

// OpenResult answers open.
type OpenResult struct {
	// PlanDigest echoes OpenParams.Plan.PlanDigest.
	PlanDigest string `json:"planDigest"`
}

// ReleaseParams is the payload of release.
type ReleaseParams struct {
	// PlanDigest names the opened plan this release completes. It is the
	// idempotency key: a provider releases at most once per planDigest.
	PlanDigest string `json:"planDigest"`
	// Request is the distribution/release-set/v2 release request.
	Request distribution.ReleaseRequest `json:"request"`
	// Ancestry names, for each of Request.Channels in order, whether its
	// expected head is the source revision or one of its ancestors.
	Ancestry PublicationAncestry `json:"ancestry"`
	// Evidence binds published images and member routes to this release.
	Evidence PublicationEvidence `json:"evidence"`
}

// ReleaseResult answers release.
type ReleaseResult struct {
	// Response answers Request. The engine binds it to the request with
	// distribution.ValidateReleaseExchange.
	Response distribution.ReleaseResponse `json:"response"`
}

// PublicationPlan is the plan tuple: every selected member of one publication
// before its artifact bytes exist. Its JSON encoding with PlanDigest omitted
// is what PlanDigest hashes.
type PublicationPlan struct {
	// ProtocolVersion is PublicationPlanProtocolVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// Namespace owns every member and channel of the plan.
	Namespace string `json:"namespace"`
	// SourceRevision is the full commit every selected member is built from.
	SourceRevision string `json:"sourceRevision"`
	// Channels lists every channel the publication advances, in order.
	Channels []string `json:"channels"`
	// ImmutableChannel names the channel of Channels created once and never
	// moved again, when the publication creates one.
	ImmutableChannel string `json:"immutableChannel,omitempty"`
	// Members lists the selected members in (ecosystem, coordinate) order. A
	// plan that selects nothing has an empty list.
	Members []PublicationPlanMember `json:"members"`
	// PlanDigest is PlanDigest of this plan. It is set on the wire and
	// omitted from the hashed bytes.
	PlanDigest string `json:"planDigest,omitempty"`
}

// PublicationPlanMember is one selected member of a plan.
type PublicationPlanMember struct {
	// Ecosystem identifies the member registry.
	Ecosystem distribution.Ecosystem `json:"ecosystem"`
	// Coordinate is the member coordinate in that registry.
	Coordinate string `json:"coordinate"`
	// Version is the immutable version this publication uploads.
	Version string `json:"version"`
	// SourceRevision is the plan's SourceRevision.
	SourceRevision string `json:"sourceRevision"`
	// SelectionFingerprint is the member's selection fingerprint.
	SelectionFingerprint string `json:"selectionFingerprint"`
}

// PublicationAncestry is the engine's statement about the channels a
// publication advances, read from the repository before any repository code
// ran. The provider records it; it does not verify it.
type PublicationAncestry struct {
	// SourceRevision is the commit the snapshot was read from: the plan's
	// SourceRevision.
	SourceRevision string `json:"sourceRevision"`
	// SnapshotCommits counts the commits reachable from SourceRevision,
	// itself included: 1 to MaxAncestrySnapshotCommits.
	SnapshotCommits int `json:"snapshotCommits"`
	// Channels has one entry per advanced channel, in the plan's order.
	Channels []PublicationChannelAncestry `json:"channels"`
}

// PublicationChannelAncestry is the ancestry of one advanced channel.
type PublicationChannelAncestry struct {
	// Name is the channel.
	Name string `json:"name"`
	// HeadSourceRevision is the source revision of the channel's head; absent
	// when the channel has no head.
	HeadSourceRevision string `json:"headSourceRevision,omitempty"`
	// Ancestor reports whether HeadSourceRevision is SourceRevision or one of
	// its ancestors. It is false when HeadSourceRevision is absent and true
	// when it equals SourceRevision.
	Ancestor bool `json:"ancestor"`
}

// PublicationEvidence binds what the run published to its release.
type PublicationEvidence struct {
	// Images binds each published workload image to its project, in project
	// order.
	Images []runtimeproto.ReleaseSetPublishedImage `json:"images"`
	// Members binds each selected member to the route that published it, in
	// (ecosystem, coordinate) order.
	Members []PublicationMemberEvidence `json:"members"`
}

// PublicationMemberEvidence names one published member and the extension
// command and step that published it. The provider treats it as an
// assertion and verifies the artifact itself.
type PublicationMemberEvidence struct {
	Project    string `json:"project"`
	Ecosystem  string `json:"ecosystem"`
	Coordinate string `json:"coordinate"`
	Version    string `json:"version"`
	Digest     string `json:"digest"`
	Publisher  string `json:"publisher"`
	Command    string `json:"command"`
	Step       string `json:"step"`
}

// PlanDigest returns sha256:<lowercase hex> of the JSON encoding of plan with
// PlanDigest omitted: the identity open carries and release names. It fails
// when that encoding exceeds MaxPublicationPlanBytes.
func PlanDigest(plan PublicationPlan) (string, error) {
	plan.PlanDigest = ""
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("registry: encode the publication plan: %w", err)
	}
	if len(encoded) > MaxPublicationPlanBytes {
		return "", fmt.Errorf("registry: the publication plan exceeds %d bytes", MaxPublicationPlanBytes)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ValidatePublicationPlan checks a plan as open carries it: the version, the
// namespace and channel list under the distribution grammar, the source
// revision, an immutable channel among the channels, members unique in
// (ecosystem, coordinate) order that share the plan's source revision, and a
// PlanDigest equal to PlanDigest(plan).
func ValidatePublicationPlan(plan PublicationPlan) error {
	if plan.ProtocolVersion != PublicationPlanProtocolVersion {
		return fmt.Errorf("registry: plan protocolVersion must be %d", PublicationPlanProtocolVersion)
	}
	if len(plan.Channels) == 0 {
		return errors.New("registry: plan names no channel")
	}
	selector := distribution.ResolveRequest{ProtocolVersion: distribution.ProtocolVersion, Namespace: plan.Namespace, Channels: plan.Channels}
	if diagnostics := distribution.ValidateResolveRequest(&selector); diag.HasErrors(diagnostics) {
		return diagnosticError("plan", diagnostics)
	}
	if !sourceRevision.MatchString(plan.SourceRevision) {
		return fmt.Errorf("registry: plan sourceRevision must match %s", SourceRevisionPattern)
	}
	if plan.ImmutableChannel != "" && !slices.Contains(plan.Channels, plan.ImmutableChannel) {
		return errors.New("registry: plan immutableChannel is not one of its channels")
	}
	if plan.Members == nil || len(plan.Members) > distribution.MaxMembers {
		return fmt.Errorf("registry: plan members must be an array of at most %d members", distribution.MaxMembers)
	}
	for index, member := range plan.Members {
		field := fmt.Sprintf("plan members[%d]", index)
		if err := validMemberIdentity(field, string(member.Ecosystem), member.Coordinate, member.Version); err != nil {
			return err
		}
		if member.SourceRevision != plan.SourceRevision {
			return fmt.Errorf("registry: %s sourceRevision is not the plan's sourceRevision", field)
		}
		if !sha256Digest.MatchString(member.SelectionFingerprint) {
			return fmt.Errorf("registry: %s selectionFingerprint must match %s", field, PlanDigestPattern)
		}
		if index > 0 && !memberKeyBefore(string(plan.Members[index-1].Ecosystem), plan.Members[index-1].Coordinate, string(member.Ecosystem), member.Coordinate) {
			return errors.New("registry: plan members are not unique and in (ecosystem, coordinate) order")
		}
	}
	digest, err := PlanDigest(plan)
	if err != nil {
		return err
	}
	if plan.PlanDigest != digest {
		return errors.New("registry: plan planDigest is not the digest of the plan")
	}
	return nil
}

// ValidatePublicationAncestry checks the source revision, the snapshot size,
// and each channel entry: a unique portable name, a head revision that is
// absent or a full commit, ancestor false without a head revision, and
// ancestor true when the head revision is the source revision.
func ValidatePublicationAncestry(ancestry PublicationAncestry) error {
	if !sourceRevision.MatchString(ancestry.SourceRevision) {
		return fmt.Errorf("registry: ancestry sourceRevision must match %s", SourceRevisionPattern)
	}
	if ancestry.SnapshotCommits < 1 || ancestry.SnapshotCommits > MaxAncestrySnapshotCommits {
		return fmt.Errorf("registry: ancestry snapshotCommits must be 1 to %d", MaxAncestrySnapshotCommits)
	}
	if len(ancestry.Channels) == 0 || len(ancestry.Channels) > distribution.MaxChannelsPerRelease {
		return fmt.Errorf("registry: ancestry channels must list 1 to %d channels", distribution.MaxChannelsPerRelease)
	}
	seen := make(map[string]bool, len(ancestry.Channels))
	for index, channel := range ancestry.Channels {
		field := fmt.Sprintf("ancestry channels[%d]", index)
		if !distribution.IsPortableChannel(channel.Name) || seen[channel.Name] {
			return fmt.Errorf("registry: %s name is not a unique channel matching %s", field, distribution.ChannelPattern)
		}
		seen[channel.Name] = true
		switch {
		case channel.HeadSourceRevision == "":
			if channel.Ancestor {
				return fmt.Errorf("registry: %s names no head revision, so it reports no ancestor", field)
			}
		case !sourceRevision.MatchString(channel.HeadSourceRevision):
			return fmt.Errorf("registry: %s headSourceRevision must match %s", field, SourceRevisionPattern)
		case channel.HeadSourceRevision == ancestry.SourceRevision && !channel.Ancestor:
			return fmt.Errorf("registry: %s head is the source revision itself, which is its own ancestor", field)
		}
	}
	return nil
}

// ValidatePublicationEvidence checks both lists: images under the rules of
// go.putnami.dev/protocol/runtime's published-images evidence, in project
// order; members with a bounded project, a distribution member identity, a
// sha256 digest and bounded route names, in (ecosystem, coordinate) order, at
// most distribution.MaxMembers of them. Each list is an array, possibly empty,
// of at most MaxPublicationEvidenceBytes when encoded.
func ValidatePublicationEvidence(evidence PublicationEvidence) error {
	if evidence.Images == nil || evidence.Members == nil {
		return errors.New("registry: evidence images and members must be arrays")
	}
	if len(evidence.Images) > 0 {
		encoded, err := runtimeproto.MarshalReleaseSetPublishedImages(evidence.Images)
		if err != nil {
			return fmt.Errorf("registry: evidence images: %w", err)
		}
		canonical, err := runtimeproto.ParseReleaseSetPublishedImages(encoded)
		if err != nil {
			return fmt.Errorf("registry: evidence images: %w", err)
		}
		if !slices.Equal(canonical, evidence.Images) {
			return errors.New("registry: evidence images are not unique and in project order")
		}
	}
	if len(evidence.Members) > distribution.MaxMembers {
		return fmt.Errorf("registry: evidence names more than %d members", distribution.MaxMembers)
	}
	encoded, err := json.Marshal(evidence.Members)
	if err != nil || len(encoded) > MaxPublicationEvidenceBytes {
		return fmt.Errorf("registry: evidence members exceed %d bytes", MaxPublicationEvidenceBytes)
	}
	for index, member := range evidence.Members {
		field := fmt.Sprintf("evidence members[%d]", index)
		if !boundedToken(member.Project, MaxEvidenceProjectBytes) || member.Project != strings.TrimSpace(member.Project) {
			return fmt.Errorf("registry: %s project must be 1 to %d bytes of trimmed text", field, MaxEvidenceProjectBytes)
		}
		if err := validMemberIdentity(field, member.Ecosystem, member.Coordinate, member.Version); err != nil {
			return err
		}
		if !sha256Digest.MatchString(member.Digest) {
			return fmt.Errorf("registry: %s digest must match %s", field, PlanDigestPattern)
		}
		for _, text := range [...]struct{ name, value string }{{"publisher", member.Publisher}, {"command", member.Command}, {"step", member.Step}} {
			if !boundedToken(text.value, MaxEvidenceTextBytes) {
				return fmt.Errorf("registry: %s %s must be 1 to %d bytes of text", field, text.name, MaxEvidenceTextBytes)
			}
		}
		if index > 0 && !memberKeyBefore(evidence.Members[index-1].Ecosystem, evidence.Members[index-1].Coordinate, member.Ecosystem, member.Coordinate) {
			return errors.New("registry: evidence members are not unique and in (ecosystem, coordinate) order")
		}
	}
	return nil
}

// ParseResolveParams strictly decodes a resolve payload. Its request is a
// valid distribution resolve request that names channels, never a releaseId.
func ParseResolveParams(payload json.RawMessage) (*ResolveParams, error) {
	var wire struct {
		Request json.RawMessage `json:"request"`
	}
	if err := strictPublicationPayload(payload, &wire, "request"); err != nil {
		return nil, err
	}
	request, diagnostics := distribution.ParseAndValidateResolveRequest(wire.Request)
	if request == nil || diag.HasErrors(diagnostics) {
		return nil, diagnosticError("resolve request", diagnostics)
	}
	if request.ReleaseID != "" || len(request.Channels) == 0 {
		return nil, errors.New("registry: a resolve request names channels, never a releaseId")
	}
	return &ResolveParams{Request: *request}, nil
}

// ParseResolveResult strictly decodes a resolve answer. Its response is a
// valid distribution resolve response that carries heads.
func ParseResolveResult(payload json.RawMessage) (*ResolveResult, error) {
	var wire struct {
		Response json.RawMessage `json:"response"`
	}
	if err := strictPublicationPayload(payload, &wire, "response"); err != nil {
		return nil, err
	}
	response, diagnostics := distribution.ParseAndValidateResolveResponse(wire.Response)
	if response == nil || diag.HasErrors(diagnostics) {
		return nil, diagnosticError("resolve response", diagnostics)
	}
	if response.Heads == nil {
		return nil, errors.New("registry: a resolve answer carries heads, never a release")
	}
	return &ResolveResult{Response: *response}, nil
}

// ParseOpenParams strictly decodes an open payload: a valid plan, a valid
// ancestry read from the plan's source revision, and one ancestry entry per
// plan channel, in the plan's order.
func ParseOpenParams(payload json.RawMessage) (*OpenParams, error) {
	var params OpenParams
	if err := strictPublicationPayload(payload, &params, ""); err != nil {
		return nil, err
	}
	if err := ValidatePublicationPlan(params.Plan); err != nil {
		return nil, err
	}
	if err := ValidatePublicationAncestry(params.Ancestry); err != nil {
		return nil, err
	}
	if params.Ancestry.SourceRevision != params.Plan.SourceRevision {
		return nil, errors.New("registry: ancestry sourceRevision is not the plan's sourceRevision")
	}
	if !slices.Equal(ancestryChannelNames(params.Ancestry), params.Plan.Channels) {
		return nil, errors.New("registry: ancestry channels are not the plan's channels in order")
	}
	return &params, nil
}

// ParseOpenResult strictly decodes an open answer.
func ParseOpenResult(payload json.RawMessage) (*OpenResult, error) {
	var result OpenResult
	if err := strictPublicationPayload(payload, &result, ""); err != nil {
		return nil, err
	}
	if !sha256Digest.MatchString(result.PlanDigest) {
		return nil, fmt.Errorf("registry: open planDigest must match %s", PlanDigestPattern)
	}
	return &result, nil
}

// ParseReleaseParams strictly decodes a release payload: a plan digest, a
// valid distribution release request, a valid ancestry with one entry per
// request channel in order and no head revision for a channel expected to
// have no head, and valid evidence.
func ParseReleaseParams(payload json.RawMessage) (*ReleaseParams, error) {
	var wire struct {
		PlanDigest string              `json:"planDigest"`
		Request    json.RawMessage     `json:"request"`
		Ancestry   PublicationAncestry `json:"ancestry"`
		Evidence   PublicationEvidence `json:"evidence"`
	}
	if err := strictPublicationPayload(payload, &wire, "request"); err != nil {
		return nil, err
	}
	if !sha256Digest.MatchString(wire.PlanDigest) {
		return nil, fmt.Errorf("registry: release planDigest must match %s", PlanDigestPattern)
	}
	request, diagnostics := distribution.ParseAndValidateReleaseRequest(wire.Request)
	if request == nil || diag.HasErrors(diagnostics) {
		return nil, diagnosticError("release request", diagnostics)
	}
	if err := ValidatePublicationAncestry(wire.Ancestry); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(request.Channels))
	for _, channel := range request.Channels {
		names = append(names, channel.Name)
	}
	if !slices.Equal(ancestryChannelNames(wire.Ancestry), names) {
		return nil, errors.New("registry: ancestry channels are not the release channels in order")
	}
	for index, channel := range request.Channels {
		if channel.Expected == nil && wire.Ancestry.Channels[index].HeadSourceRevision != "" {
			return nil, fmt.Errorf("registry: ancestry channels[%d] names a head revision for a channel expected to have no head", index)
		}
	}
	if err := ValidatePublicationEvidence(wire.Evidence); err != nil {
		return nil, err
	}
	return &ReleaseParams{PlanDigest: wire.PlanDigest, Request: *request, Ancestry: wire.Ancestry, Evidence: wire.Evidence}, nil
}

// ParseReleaseResult strictly decodes a release answer. Its response is a
// valid distribution release response.
func ParseReleaseResult(payload json.RawMessage) (*ReleaseResult, error) {
	var wire struct {
		Response json.RawMessage `json:"response"`
	}
	if err := strictPublicationPayload(payload, &wire, "response"); err != nil {
		return nil, err
	}
	response, diagnostics := distribution.ParseAndValidateReleaseResponse(wire.Response)
	if response == nil || diag.HasErrors(diagnostics) {
		return nil, diagnosticError("release response", diagnostics)
	}
	return &ReleaseResult{Response: *response}, nil
}

// strictPublicationPayload decodes a publication-v1 payload: a payload within
// the line bounds of its op, one level below the envelope, in which every
// member without omitempty is present and every omitempty member is absent
// rather than empty. delegated names the top-level member that carries a
// distribution document, "" when none does.
func strictPublicationPayload(payload json.RawMessage, target any, delegated string) error {
	if len(payload) == 0 {
		return errors.New("registry: missing payload")
	}
	var paths [][]string
	if delegated != "" {
		paths = [][]string{{delegated}}
	}
	bounds := lineBounds{maxBytes: MaxPublicationLineBytes, maxDepth: maxPublicationDepth - 1}
	if _, err := strictScan(payload, bounds, paths); err != nil {
		return err
	}
	return strictDecodeComplete(payload, target)
}

// validMemberIdentity applies the distribution member rules: an ecosystem
// identifier, and a coordinate and a version of bounded length without control
// characters.
func validMemberIdentity(field, ecosystem, coordinate, version string) error {
	if !ecosystemFormat.MatchString(ecosystem) {
		return fmt.Errorf("registry: %s ecosystem must match %s", field, distribution.EcosystemPattern)
	}
	if !boundedToken(coordinate, distribution.MaxCoordinateBytes) {
		return fmt.Errorf("registry: %s coordinate must be 1 to %d bytes without control characters", field, distribution.MaxCoordinateBytes)
	}
	if !boundedToken(version, distribution.MaxVersionBytes) {
		return fmt.Errorf("registry: %s version must be 1 to %d bytes without control characters", field, distribution.MaxVersionBytes)
	}
	return nil
}

func boundedToken(value string, limit int) bool {
	return value != "" && len(value) <= limit && boundedText(value)
}

// memberKeyBefore reports whether (ecosystem, coordinate) a sorts strictly
// before b.
func memberKeyBefore(ecosystemA, coordinateA, ecosystemB, coordinateB string) bool {
	if ecosystemA != ecosystemB {
		return ecosystemA < ecosystemB
	}
	return coordinateA < coordinateB
}

func ancestryChannelNames(ancestry PublicationAncestry) []string {
	names := make([]string, 0, len(ancestry.Channels))
	for _, channel := range ancestry.Channels {
		names = append(names, channel.Name)
	}
	return names
}

// diagnosticError reports the first error diagnostic of a distribution
// validator.
func diagnosticError(subject string, diagnostics []diag.Diagnostic) error {
	errs := diag.Errors(diagnostics)
	if len(errs) == 0 {
		return fmt.Errorf("registry: invalid %s", subject)
	}
	return fmt.Errorf("registry: invalid %s: %s", subject, errs[0].String())
}
