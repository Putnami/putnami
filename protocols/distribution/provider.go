package distribution

import (
	"encoding/json"
	"fmt"
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
)

// UnmarshalJSON retains selector presence after strict structural inspection.
// This distinguishes an omitted selector from an explicitly empty selector
// without making the public fields pointers or breaking existing callers.
func (request *ResolveRequest) UnmarshalJSON(data []byte) error {
	type resolveRequestWire struct {
		ProtocolVersion int      `json:"protocolVersion"`
		Namespace       string   `json:"namespace"`
		Channels        []string `json:"channels"`
		ReleaseID       string   `json:"releaseId"`
	}
	var wire resolveRequestWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*request = ResolveRequest{
		ProtocolVersion:  wire.ProtocolVersion,
		Namespace:        wire.Namespace,
		Channels:         wire.Channels,
		ReleaseID:        wire.ReleaseID,
		channelsPresent:  fields["channels"] != nil,
		releaseIDPresent: fields["releaseId"] != nil,
	}
	return nil
}

// UnmarshalJSON retains source presence so an explicitly empty channel cannot
// masquerade as an omitted one when releaseId is also present.
func (source *ChannelSource) UnmarshalJSON(data []byte) error {
	type channelSourceWire struct {
		Channel   string `json:"channel"`
		ReleaseID string `json:"releaseId"`
	}
	var wire channelSourceWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*source = ChannelSource{
		Channel:          wire.Channel,
		ReleaseID:        wire.ReleaseID,
		channelPresent:   fields["channel"] != nil,
		releaseIDPresent: fields["releaseId"] != nil,
	}
	return nil
}

// ParseAndValidateResolveRequest strictly parses a resolve request.
func ParseAndValidateResolveRequest(data []byte) (*ResolveRequest, []diag.Diagnostic) {
	return parseAndValidate(data, resolveRequestShape, ValidateResolveRequest)
}

// ValidateResolveRequest checks version, namespace, and the exclusive selector:
// either a bounded list of unique portable channel names, or exactly one
// immutable release-set id.
func ValidateResolveRequest(request *ResolveRequest) []diag.Diagnostic {
	if request == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "resolve request is nil")}
	}
	diagnostics := validateWireVersion(request.ProtocolVersion)
	diagnostics = append(diagnostics, validateNamespace(request.Namespace)...)
	if request.channelsSelected() == request.releaseIDSelected() {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSelector, "", "resolve requires exactly one of channels or releaseId"))
		return boundDiagnostics(diagnostics)
	}
	if request.releaseIDSelected() {
		diagnostics = append(diagnostics, ValidateReleaseSetID("releaseId", request.ReleaseID)...)
		return boundDiagnostics(diagnostics)
	}
	diagnostics = append(diagnostics, validateChannelNames("channels", request.Channels)...)
	return boundDiagnostics(diagnostics)
}

func (request *ResolveRequest) channelsSelected() bool {
	return request.channelsPresent || len(request.Channels) > 0
}

func (request *ResolveRequest) releaseIDSelected() bool {
	return request.releaseIDPresent || request.ReleaseID != ""
}

// validateChannelNames checks a bounded list of unique portable channel names.
// An immutable release-set id is never a channel name: callers state immutable
// intent with releaseId.
func validateChannelNames(field string, channels []string) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if len(channels) == 0 || len(channels) > MaxChannelsPerRelease {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeBoundsExceeded, field, "%d channels are outside the range 1..%d", len(channels), MaxChannelsPerRelease))
	}
	seen := make(map[string]bool, len(channels))
	limit := min(len(channels), MaxChannelsPerRelease)
	for index := 0; index < limit; index++ {
		name := channels[index]
		entry := indexedField(field, index)
		if IsReleaseSetID(name) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSelector, entry, "an immutable release-set id must use releaseId, not a channel name"))
			continue
		}
		diagnostics = append(diagnostics, validateChannel(entry, name)...)
		if seen[name] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateField, entry, "channel %q appears more than once", name))
		}
		seen[name] = true
	}
	return diagnostics
}

// ParseAndValidateResolveResponse strictly parses a resolve response and
// normalizes every release set it carries.
func ParseAndValidateResolveResponse(data []byte) (*ResolveResponse, []diag.Diagnostic) {
	response, diagnostics := parseAndValidate(data, resolveResponseShape, ValidateResolveResponse)
	if response != nil {
		for _, head := range response.Heads {
			normalizeHeadSet(head)
		}
		normalizeHeadSet(response.Release)
	}
	return response, diagnostics
}

func normalizeHeadSet(head *ChannelHead) {
	if head != nil && head.ReleaseSet != nil {
		head.ReleaseSet = NormalizeReleaseSet(head.ReleaseSet)
	}
}

// ValidateResolveResponse checks the resolve envelope. A channel answer carries
// heads, one entry per requested channel; a null entry is a complete answer
// meaning the channel has no head. A release-id answer carries release, whose
// generation is 0 because an immutable set is named by no channel. Every
// present head carries its full snapshot and a ref that recomputes from it.
func ValidateResolveResponse(response *ResolveResponse) []diag.Diagnostic {
	if response == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "resolve response is nil")}
	}
	diagnostics := validateWireVersion(response.ProtocolVersion)
	if (response.Heads != nil) == (response.Release != nil) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSelector, "", "a resolve answer carries exactly one of heads or release"))
		return boundDiagnostics(diagnostics)
	}
	if response.Release != nil {
		diagnostics = append(diagnostics, validateChannelHead("release", response.Release, true)...)
		if response.Release.Generation != 0 {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidGeneration, "release.generation", "an immutable release is named by no channel and reports generation 0, got %d", response.Release.Generation))
		}
		return boundDiagnostics(diagnostics)
	}
	if len(response.Heads) == 0 || len(response.Heads) > MaxChannelsPerRelease {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeBoundsExceeded, "heads", "%d heads are outside the range 1..%d", len(response.Heads), MaxChannelsPerRelease))
	}
	for _, name := range sortedHeadNames(response.Heads) {
		field := joinField("heads", name)
		diagnostics = append(diagnostics, validateChannel(field, name)...)
		head := response.Heads[name]
		if head == nil {
			continue
		}
		diagnostics = append(diagnostics, validateChannelHead(field, head, true)...)
		if head.Generation == 0 {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidGeneration, joinField(field, "generation"), "a channel head reports a generation of at least 1"))
		}
	}
	return boundDiagnostics(diagnostics)
}

// ValidateResolveExchange binds the answer to its request: a channel selector
// answers exactly the requested names, an immutable releaseId selector always
// resolves to exactly that id, and every returned set belongs to the requested
// namespace.
func ValidateResolveExchange(request *ResolveRequest, response *ResolveResponse) []diag.Diagnostic {
	diagnostics := ValidateResolveRequest(request)
	diagnostics = append(diagnostics, ValidateResolveResponse(response)...)
	if request == nil || response == nil {
		return boundDiagnostics(diagnostics)
	}
	if request.releaseIDSelected() {
		if response.Release == nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeRefMismatch, "release", "an immutable releaseId selector cannot resolve to an absent head"))
		} else if request.ReleaseID != response.Release.Ref.ID {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeRefMismatch, "release.ref.id", "resolved release-set id %q does not match requested releaseId %q", response.Release.Ref.ID, request.ReleaseID))
		}
		diagnostics = append(diagnostics, validateHeadNamespace("release", response.Release, request.Namespace)...)
		return boundDiagnostics(diagnostics)
	}
	requested := make(map[string]bool, len(request.Channels))
	for _, name := range request.Channels {
		requested[name] = true
		if _, answered := response.Heads[name]; !answered {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeMissingField, joinField("heads", name), "resolve must answer every requested channel"))
		}
	}
	for _, name := range sortedHeadNames(response.Heads) {
		if !requested[name] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnknownField, joinField("heads", name), "resolve answered channel %q, which was not requested", name))
			continue
		}
		diagnostics = append(diagnostics, validateHeadNamespace(joinField("heads", name), response.Heads[name], request.Namespace)...)
	}
	return boundDiagnostics(diagnostics)
}

func validateHeadNamespace(field string, head *ChannelHead, namespace string) []diag.Diagnostic {
	if head == nil || head.ReleaseSet == nil || head.ReleaseSet.Namespace == namespace {
		return nil
	}
	return []diag.Diagnostic{diag.Errorf(ErrorCodeRefMismatch, joinField(field, "releaseSet.namespace"), "resolved namespace %q does not match requested namespace %q", head.ReleaseSet.Namespace, namespace)}
}

// ParseAndValidateChannelHead strictly parses one channel head, the shape a
// publish job receives when the CLI hands it the release it must publish
// against. Its release set is normalized when present.
func ParseAndValidateChannelHead(data []byte) (*ChannelHead, []diag.Diagnostic) {
	head, diagnostics := parseAndValidate(data, channelHeadShape, ValidateChannelHead)
	normalizeHeadSet(head)
	return head, diagnostics
}

// ValidateChannelHead checks a standalone head: a well-formed ref, and, when the
// full snapshot is carried, a set that recomputes exactly that ref.
func ValidateChannelHead(head *ChannelHead) []diag.Diagnostic {
	if head == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "channel head is nil")}
	}
	return boundDiagnostics(validateChannelHead("", head, false))
}

// validateChannelHead validates one head's ref and, when carried, the agreement
// between its snapshot and that ref. requireSet demands the full snapshot,
// which a resolve answer always carries and a release answer never does.
func validateChannelHead(field string, head *ChannelHead, requireSet bool) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	diagnostics = append(diagnostics, ValidateReleaseSetRef(joinField(field, "ref"), head.Ref)...)
	if head.ReleaseSet == nil {
		if requireSet {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeMissingField, joinField(field, "releaseSet"), "a resolved head carries its full immutable snapshot"))
		}
		return diagnostics
	}
	diagnostics = append(diagnostics, ValidateReleaseSet(head.ReleaseSet)...)
	derived, derivedDiagnostics := DeriveReleaseSetRef(head.ReleaseSet)
	diagnostics = append(diagnostics, derivedDiagnostics...)
	if !diag.HasErrors(derivedDiagnostics) && !refsEqual(head.Ref, derived) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeRefMismatch, joinField(field, "ref"), "head ref does not match canonical release-set bytes"))
	}
	return diagnostics
}

// ParseAndValidateReleaseRequest strictly parses a release request and
// normalizes its release set. JSON null is accepted only for a channel's
// expected head and for the set-level visibility override.
func ParseAndValidateReleaseRequest(data []byte) (*ReleaseRequest, []diag.Diagnostic) {
	request, diagnostics := parseAndValidate(data, releaseRequestShape, ValidateReleaseRequest)
	if request != nil {
		request.ReleaseSet = *NormalizeReleaseSet(&request.ReleaseSet)
	}
	return request, diagnostics
}

// ValidateReleaseRequest checks the envelope, the full release set, every
// channel this release advances, and the visibility chain the repository
// declares. A member-level visibility entry must name a member of the set: a
// rule with nothing to apply to is a caller error, not a no-op.
func ValidateReleaseRequest(request *ReleaseRequest) []diag.Diagnostic {
	if request == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "release request is nil")}
	}
	diagnostics := validateWireVersion(request.ProtocolVersion)
	diagnostics = append(diagnostics, validateNamespace(request.Namespace)...)
	diagnostics = append(diagnostics, ValidateReleaseSet(&request.ReleaseSet)...)
	if request.ReleaseSet.Namespace != "" && request.Namespace != request.ReleaseSet.Namespace {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeRefMismatch, "releaseSet.namespace", "release-set namespace %q does not match the released namespace %q", request.ReleaseSet.Namespace, request.Namespace))
	}
	if len(request.Channels) == 0 || len(request.Channels) > MaxChannelsPerRelease {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeBoundsExceeded, "channels", "%d channels are outside the range 1..%d", len(request.Channels), MaxChannelsPerRelease))
	}
	seen := make(map[string]bool, len(request.Channels))
	limit := min(len(request.Channels), MaxChannelsPerRelease)
	for index := 0; index < limit; index++ {
		channel := request.Channels[index]
		field := indexedField("channels", index)
		diagnostics = append(diagnostics, validateChannel(joinField(field, "name"), channel.Name)...)
		if seen[channel.Name] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateField, joinField(field, "name"), "channel %q appears more than once", channel.Name))
		}
		seen[channel.Name] = true
		if channel.Expected != nil {
			diagnostics = append(diagnostics, ValidateReleaseSetRef(joinField(field, "expected"), *channel.Expected)...)
		}
		diagnostics = append(diagnostics, validateVisibility(joinField(field, "visibility"), channel.Visibility)...)
	}
	diagnostics = append(diagnostics, validateVisibilityChain("visibility", request.Visibility, &request.ReleaseSet)...)
	diagnostics = append(diagnostics, ValidateMirrorTargets(request.Mirrors)...)
	return boundDiagnostics(diagnostics)
}

// validateVisibilityChain checks every declared level of the inheritance chain.
// A silent level inherits and is not an error; a stated level must be one of the
// three ordered levels, and a member entry must name a member of the set.
func validateVisibilityChain(field string, chain VisibilityChain, releaseSet *ReleaseSet) []diag.Diagnostic {
	diagnostics := validateVisibility(joinField(field, "repo"), chain.Repo)
	if len(chain.Registries) > MaxRegistryKinds {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeBoundsExceeded, joinField(field, "registries"), "%d registry kinds exceed the limit of %d", len(chain.Registries), MaxRegistryKinds))
	}
	kinds := make([]string, 0, len(chain.Registries))
	for kind := range chain.Registries {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		entry := joinField(joinField(field, "registries"), kind)
		diagnostics = append(diagnostics, validateEcosystem(entry, Ecosystem(kind))...)
		diagnostics = append(diagnostics, validateVisibility(entry, chain.Registries[kind])...)
	}
	if chain.Versions.Stable != "" {
		diagnostics = append(diagnostics, validateVisibility(joinField(field, "versions.stable"), chain.Versions.Stable)...)
	}
	if chain.Versions.Prerelease != "" {
		diagnostics = append(diagnostics, validateVisibility(joinField(field, "versions.prerelease"), chain.Versions.Prerelease)...)
	}
	if chain.Set != nil {
		diagnostics = append(diagnostics, validateVisibility(joinField(field, "set"), *chain.Set)...)
	}
	present := make(map[coordinateKey]bool, len(releaseSet.Members))
	for _, member := range releaseSet.Members {
		present[coordinateKey{member.Ecosystem, member.Coordinate}] = true
	}
	for index, member := range chain.Members {
		entry := indexedField(joinField(field, "members"), index)
		diagnostics = append(diagnostics, validateEcosystem(joinField(entry, "ecosystem"), member.Ecosystem)...)
		diagnostics = append(diagnostics, validateCoordinate(joinField(entry, "coordinate"), member.Coordinate)...)
		diagnostics = append(diagnostics, validateVisibility(joinField(entry, "visibility"), member.Visibility)...)
		if !present[coordinateKey{member.Ecosystem, member.Coordinate}] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnclosedDependency, entry, "visibility names (%q, %q), which is not a member of this release set", member.Ecosystem, member.Coordinate))
		}
	}
	return diagnostics
}

// ParseAndValidateReleaseResponse strictly parses a release response. A head is
// null only for a conflict against a channel that still has no head.
func ParseAndValidateReleaseResponse(data []byte) (*ReleaseResponse, []diag.Diagnostic) {
	return parseAndValidate(data, releaseResponseShape, ValidateReleaseResponse)
}

// ValidateReleaseResponse checks the exclusive release outcome vocabulary and
// the per-channel heads. Both success forms report every channel's head; a head
// that exists carries a generation of at least 1.
func ValidateReleaseResponse(response *ReleaseResponse) []diag.Diagnostic {
	if response == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "release response is nil")}
	}
	diagnostics := validateWireVersion(response.ProtocolVersion)
	if len(response.Outcome) > MaxTokenBytes {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeBoundsExceeded, "outcome", "release outcome contains %d bytes, exceeding the limit of %d", len(response.Outcome), MaxTokenBytes))
		return boundDiagnostics(diagnostics)
	}
	switch response.Outcome {
	case ReleaseOutcomeReleased, ReleaseOutcomeAlreadyCurrent, ReleaseOutcomeConflict:
	default:
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidOutcome, "outcome", "release outcome %q is not released, already-current, or conflict", response.Outcome))
	}
	if len(response.Current) == 0 || len(response.Current) > MaxChannelsPerRelease {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeBoundsExceeded, "current", "%d heads are outside the range 1..%d", len(response.Current), MaxChannelsPerRelease))
	}
	success := response.Outcome == ReleaseOutcomeReleased || response.Outcome == ReleaseOutcomeAlreadyCurrent
	for _, name := range sortedHeadNames(response.Current) {
		field := joinField("current", name)
		diagnostics = append(diagnostics, validateChannel(field, name)...)
		head := response.Current[name]
		if head == nil {
			if success {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeMissingField, field, "%s reports the released set as the head of every requested channel", response.Outcome))
			}
			continue
		}
		diagnostics = append(diagnostics, validateChannelHead(field, head, false)...)
		if head.ReleaseSet != nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeFieldNotAllowed, joinField(field, "releaseSet"), "a release answer reports pointers only; the caller already holds the set"))
		}
		if head.Generation == 0 {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidGeneration, joinField(field, "generation"), "a channel head reports a generation of at least 1"))
		}
	}
	return boundDiagnostics(diagnostics)
}

// ValidateReleaseExchange enforces the one-step multi-channel release: the
// answer names exactly the requested channels; a success reports the submitted
// set as the head of every one of them; a conflict never masquerades as success
// and must name at least one head that differs from what the caller expected.
func ValidateReleaseExchange(request *ReleaseRequest, response *ReleaseResponse) []diag.Diagnostic {
	diagnostics := ValidateReleaseRequest(request)
	diagnostics = append(diagnostics, ValidateReleaseResponse(response)...)
	if request == nil || response == nil {
		return boundDiagnostics(diagnostics)
	}
	derived, derivedDiagnostics := DeriveReleaseSetRef(&request.ReleaseSet)
	diagnostics = append(diagnostics, derivedDiagnostics...)
	expected := make(map[string]*ReleaseSetRef, len(request.Channels))
	for _, channel := range request.Channels {
		expected[channel.Name] = channel.Expected
		if _, answered := response.Current[channel.Name]; !answered {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeMissingField, joinField("current", channel.Name), "release must report every requested channel"))
		}
	}
	for _, name := range sortedHeadNames(response.Current) {
		if _, requested := expected[name]; !requested {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnknownField, joinField("current", name), "release reported channel %q, which was not requested", name))
		}
	}
	if diag.HasErrors(derivedDiagnostics) {
		return boundDiagnostics(diagnostics)
	}
	switch response.Outcome {
	case ReleaseOutcomeReleased, ReleaseOutcomeAlreadyCurrent:
		for _, name := range sortedHeadNames(response.Current) {
			if head := response.Current[name]; head != nil && !refsEqual(head.Ref, derived) {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeRefMismatch, joinField("current", name), "%s must report the released set as the head of %q", response.Outcome, name))
			}
		}
		if response.Outcome == ReleaseOutcomeReleased && everyChannelAlreadyAt(request.Channels, derived) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidOutcome, "outcome", "a set already expected as the head of every channel must use already-current, not released"))
		}
	case ReleaseOutcomeConflict:
		if !anyHeadDiffersFromExpected(expected, response.Current) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidOutcome, "outcome", "every reported head equals its expected head; conflict is not a valid result"))
		}
	}
	return boundDiagnostics(diagnostics)
}

// everyChannelAlreadyAt reports whether the caller expected the released set as
// the head of every listed channel, which is already-current, never released.
func everyChannelAlreadyAt(channels []ChannelRequest, derived ReleaseSetRef) bool {
	if len(channels) == 0 {
		return false
	}
	for _, channel := range channels {
		if channel.Expected == nil || !refsEqual(*channel.Expected, derived) {
			return false
		}
	}
	return true
}

// anyHeadDiffersFromExpected reports whether at least one observed head is not
// the head the caller compared against, which is what a conflict means.
func anyHeadDiffersFromExpected(expected map[string]*ReleaseSetRef, current map[string]*ChannelHead) bool {
	for name, want := range expected {
		head, reported := current[name]
		if !reported {
			continue
		}
		switch {
		case want == nil && head == nil:
		case want == nil || head == nil:
			return true
		case !refsEqual(*want, head.Ref):
			return true
		}
	}
	return false
}

// ParseAndValidateChannelSetRequest strictly parses a channel-set request. JSON
// null is accepted only for expected, where it asserts that no head exists.
func ParseAndValidateChannelSetRequest(data []byte) (*ChannelSetRequest, []diag.Diagnostic) {
	return parseAndValidate(data, channelSetRequestShape, ValidateChannelSetRequest)
}

// ValidateChannelSetRequest checks the metadata-only move: a portable target
// channel, its compare-and-swap expectation, and a source that is exactly one
// of an existing channel or an immutable release-set id.
func ValidateChannelSetRequest(request *ChannelSetRequest) []diag.Diagnostic {
	if request == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "channel-set request is nil")}
	}
	diagnostics := validateWireVersion(request.ProtocolVersion)
	diagnostics = append(diagnostics, validateNamespace(request.Namespace)...)
	diagnostics = append(diagnostics, validateChannel("channel", request.Channel)...)
	if request.Expected != nil {
		diagnostics = append(diagnostics, ValidateReleaseSetRef("expected", *request.Expected)...)
	}
	channelSelected := request.From.channelPresent || request.From.Channel != ""
	releaseIDSelected := request.From.releaseIDPresent || request.From.ReleaseID != ""
	switch {
	case channelSelected == releaseIDSelected:
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSelector, "from", "channel-set requires exactly one of from.channel or from.releaseId"))
	case releaseIDSelected:
		diagnostics = append(diagnostics, ValidateReleaseSetID("from.releaseId", request.From.ReleaseID)...)
	case IsReleaseSetID(request.From.Channel):
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSelector, "from.channel", "an immutable release-set id must use from.releaseId, not from.channel"))
	default:
		diagnostics = append(diagnostics, validateChannel("from.channel", request.From.Channel)...)
	}
	return boundDiagnostics(diagnostics)
}

// ValidateChannelSetExchange checks the compare-and-swap result of one
// metadata-only move. The answer names exactly the requested target channel;
// conflict means its observed head differs from the expectation. A released
// answer cannot report the unchanged expected head. When the source is an
// immutable release id, either successful outcome must point at that exact
// set. A source channel's head is provider-owned state and cannot be
// reconstructed by the client, so its identity is intentionally not guessed
// here. In particular, already-current remains valid after an idempotent retry
// even when the original expectation no longer names the current head.
func ValidateChannelSetExchange(request *ChannelSetRequest, response *ReleaseResponse) []diag.Diagnostic {
	diagnostics := ValidateChannelSetRequest(request)
	diagnostics = append(diagnostics, ValidateReleaseResponse(response)...)
	if request == nil || response == nil {
		return boundDiagnostics(diagnostics)
	}
	head, answered := response.Current[request.Channel]
	if !answered {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeMissingField, joinField("current", request.Channel), "channel-set must report the requested target channel"))
	}
	for _, name := range sortedHeadNames(response.Current) {
		if name != request.Channel {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnknownField, joinField("current", name), "channel-set reported channel %q, want only %q", name, request.Channel))
		}
	}
	if !answered {
		return boundDiagnostics(diagnostics)
	}

	matchesExpected := request.Expected != nil && head != nil && refsEqual(*request.Expected, head.Ref)
	switch response.Outcome {
	case ReleaseOutcomeReleased:
		if matchesExpected {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidOutcome, "outcome", "channel %q still names its expected head; channel-set must report already-current, not released", request.Channel))
		}
	case ReleaseOutcomeConflict:
		if (request.Expected == nil && head == nil) || matchesExpected {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidOutcome, "outcome", "conflict requires channel %q to differ from its expected head", request.Channel))
		}
	}

	if (response.Outcome == ReleaseOutcomeReleased || response.Outcome == ReleaseOutcomeAlreadyCurrent) &&
		request.From.ReleaseID != "" && head != nil && head.Ref.ID != request.From.ReleaseID {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeRefMismatch, joinField("current", request.Channel),
			"channel-set from release id %q reported head %q", request.From.ReleaseID, head.Ref.ID))
	}
	return boundDiagnostics(diagnostics)
}

// ParseAndValidateChannelStatusRequest strictly parses a channel-status request.
func ParseAndValidateChannelStatusRequest(data []byte) (*ChannelStatusRequest, []diag.Diagnostic) {
	return parseAndValidate(data, channelStatusRequestShape, ValidateChannelStatusRequest)
}

// ValidateChannelStatusRequest checks the envelope, namespace, and channel.
func ValidateChannelStatusRequest(request *ChannelStatusRequest) []diag.Diagnostic {
	if request == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "channel-status request is nil")}
	}
	diagnostics := validateWireVersion(request.ProtocolVersion)
	diagnostics = append(diagnostics, validateNamespace(request.Namespace)...)
	diagnostics = append(diagnostics, validateChannel("channel", request.Channel)...)
	return boundDiagnostics(diagnostics)
}

// ParseAndValidateChannelStatusResponse strictly parses a channel-status
// response. A null desired head is a complete answer: the channel is empty.
func ParseAndValidateChannelStatusResponse(data []byte) (*ChannelStatusResponse, []diag.Diagnostic) {
	response, diagnostics := parseAndValidate(data, channelStatusResponseShape, ValidateChannelStatusResponse)
	if response != nil {
		normalizeHeadSet(response.Desired)
	}
	return response, diagnostics
}

// ValidateChannelStatusResponse checks the desired head and the observed
// generations. Every observed key is an ecosystem identifier: the registry kind
// that applied the projection.
func ValidateChannelStatusResponse(response *ChannelStatusResponse) []diag.Diagnostic {
	if response == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "channel-status response is nil")}
	}
	diagnostics := validateWireVersion(response.ProtocolVersion)
	if response.Desired != nil {
		diagnostics = append(diagnostics, validateChannelHead("desired", response.Desired, false)...)
		if response.Desired.Generation == 0 {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidGeneration, "desired.generation", "a channel head reports a generation of at least 1"))
		}
	}
	if len(response.Observed) > MaxRegistryKinds {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeBoundsExceeded, "observed", "%d registry kinds exceed the limit of %d", len(response.Observed), MaxRegistryKinds))
	}
	kinds := make([]string, 0, len(response.Observed))
	for kind := range response.Observed {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		diagnostics = append(diagnostics, validateEcosystem(joinField("observed", kind), Ecosystem(kind))...)
	}
	return boundDiagnostics(diagnostics)
}

// ParseAndValidateReleaseSetPublishOutcome strictly parses the successful
// result payload used under data.releaseSet.
func ParseAndValidateReleaseSetPublishOutcome(data []byte) (*ReleaseSetPublishOutcome, []diag.Diagnostic) {
	return parseAndValidate(data, publishOutcomeShape, ValidateReleaseSetPublishOutcome)
}

// ValidateReleaseSetPublishOutcome checks the envelope, the namespace, the
// published ref, and the head of every channel the publication advanced. Every
// channel names the same immutable set: that is what one release means.
func ValidateReleaseSetPublishOutcome(outcome *ReleaseSetPublishOutcome) []diag.Diagnostic {
	if outcome == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "release-set publish outcome is nil")}
	}
	diagnostics := validateWireVersion(outcome.ProtocolVersion)
	diagnostics = append(diagnostics, validateNamespace(outcome.Namespace)...)
	diagnostics = append(diagnostics, ValidateReleaseSetRef("ref", outcome.Ref)...)
	if len(outcome.Channels) == 0 || len(outcome.Channels) > MaxChannelsPerRelease {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeBoundsExceeded, "channels", "%d channels are outside the range 1..%d", len(outcome.Channels), MaxChannelsPerRelease))
	}
	for _, name := range sortedHeadNames(outcome.Channels) {
		field := joinField("channels", name)
		diagnostics = append(diagnostics, validateChannel(field, name)...)
		head := outcome.Channels[name]
		if head == nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeMissingField, field, "a successful publication reports the head of every channel it advanced"))
			continue
		}
		diagnostics = append(diagnostics, validateChannelHead(field, head, false)...)
		if head.Generation == 0 {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidGeneration, joinField(field, "generation"), "a channel head reports a generation of at least 1"))
		}
		if IsReleaseSetID(outcome.Ref.ID) && head.Ref.ID != outcome.Ref.ID {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeRefMismatch, joinField(field, "ref.id"), "channel %q reports head %q instead of the published set %q", name, head.Ref.ID, outcome.Ref.ID))
		}
	}
	return boundDiagnostics(diagnostics)
}

func sortedHeadNames(heads map[string]*ChannelHead) []string {
	names := make([]string, 0, len(heads))
	for name := range heads {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func indexedField(prefix string, index int) string {
	return fmt.Sprintf("%s[%d]", prefix, index)
}

// validateWireVersion pins one envelope to the exact protocol version. There is
// exactly one: version 1 is deleted, and a document that claims it is refused
// here rather than read on a best-effort basis.
func validateWireVersion(version int) []diag.Diagnostic {
	if version != ProtocolVersion {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "protocolVersion %d is not supported (want exact integer %d)", version, ProtocolVersion)}
	}
	return nil
}

func parseAndValidate[T any](data []byte, shape *jsonShape, validate func(*T) []diag.Diagnostic) (*T, []diag.Diagnostic) {
	value, diagnostics := strictDecode[T](data, shape)
	if value == nil {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, validate(value)...)
	diagnostics = boundDiagnostics(diagnostics)
	if diag.HasErrors(diagnostics) {
		return nil, diagnostics
	}
	return value, diagnostics
}
