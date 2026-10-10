package deliverycli

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	putserverclient "go.putnami.dev/cloud/clients/put-server/go"
	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	registry "go.putnami.dev/protocol/registry"
)

// Refusal codes of the publication-v1 ops besides the protocol's own
// (registry.PublicationRefusalCodes).
const (
	// refusalPublicationRefused is Delivery or put-server refusing a request
	// for a reason no protocol code states. Nothing moved.
	refusalPublicationRefused = "publication_refused"
	// refusalPublicationUnavailable is Delivery or put-server not answering,
	// or answering outside its contract. A release answered with it has an
	// unknown outcome, and a later release of the same plan is tried again.
	refusalPublicationUnavailable = "publication_unavailable"
)

// Bounds of the publication ops. Each op answers inside the engine's own
// timeout for it (30 s; 2 min for release), so the engine never sends an op
// again while this provider still serves the first one.
const (
	publicationOpTimeout      = 25 * time.Second
	publicationReleaseTimeout = 110 * time.Second
	// publicationPutAttempt bounds one put-server call.
	publicationPutAttempt = 45 * time.Second
	// publicationMinAttempt is the least time a second put-server attempt
	// gets; with less left in the op, the first failure is the answer.
	publicationMinAttempt = 5 * time.Second
	// publicationRetryWait is the wait before the second attempt.
	publicationRetryWait = 500 * time.Millisecond
	// planningBearerMargin is how long the planning bearer must still be
	// valid to be reused for a resolve.
	planningBearerMargin = 30 * time.Second
)

// publisher serves the publication-v1 ops of one hosted session (registry ADR
// 0003): resolve, open, the publish credential, and release.
//
// Delivery decides what the run may publish. Its planning answer names the
// namespace, the commit, the channels and the forward-only channels, with a
// read-only bearer for resolve; its answer to the plan freezes the plan's
// digest on the run and carries the plan's write bearer. put-server resolves
// and releases release sets under those bearers. This provider holds the
// session's state between them: the heads it resolved, the open plan, and the
// one release of that plan. Before a release moves a forward-only channel, it
// asks Delivery whether the run is still the head of its default branch: a run
// that is not publishes its set and leaves those channels where they are. A
// plan whose immutable channel already has a head when it opens is a rerun of
// a commit that released it: it publishes its set and moves no channel.
type publisher struct {
	caller        *capabilityCaller
	runCredential string
	endpoints     publicationEndpoints
	put           *putserverclient.PutClient

	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
	diagnose  func(format string, args ...any)
	safeText  func(text string) string
	addSecret func(secret string)

	opTimeout      time.Duration
	releaseTimeout time.Duration
	putAttempt     time.Duration
	retryWait      time.Duration

	planningFlight singleFlight[planningResult]
	publishFlight  singleFlight[bearerResult]

	mu        sync.Mutex
	authority *publicationAuthority
	planning  *issuedBearer
	// resolved holds, per channel, the id of every head this session
	// resolved for it with the generation it was resolved at; "" (at 0) is
	// no head.
	resolved map[string]map[string]uint64
	// inherited holds, per member key, the artifact encoding of every member
	// a resolved head carries: what a release may carry without planning it.
	inherited map[string]map[string]bool
	opening   *pendingAnswer
	opened    *registry.OpenParams
	// held is the open plan's immutable channel when it already had a head
	// at open; nil otherwise.
	held      *heldChannel
	publish   *issuedBearer
	releasing *pendingRelease
	released  map[string]releaseRecord
}

// publicationAnswer is an op's payload or its refusal.
type publicationAnswer struct {
	payload any
	refusal *registry.CredentialRefusal
}

// pendingAnswer is an open in progress; a second open of the same plan waits
// for its answer.
type pendingAnswer struct {
	digest string
	done   chan struct{}
	answer publicationAnswer
}

// pendingRelease is a release in progress; a second release of the same set
// waits for its answer.
type pendingRelease struct {
	ref    distribution.ReleaseSetRef
	done   chan struct{}
	answer publicationAnswer
}

// releaseRecord is the definitive answer to a plan's release and the set ref
// the release derived.
type releaseRecord struct {
	ref    distribution.ReleaseSetRef
	answer publicationAnswer
}

// heldChannel is an immutable channel that already has a head: the run is a
// rerun, and its release leaves every channel where it is.
type heldChannel struct {
	name       string
	head       string
	generation uint64
}

// staying names the channels a release leaves where they are, each with the
// generation at which the engine answer reports it.
type staying map[string]uint64

func (s staying) has(channel string) bool {
	_, stays := s[channel]
	return stays
}

type planningResult struct {
	authority publicationAuthority
	bearer    issuedBearer
	refusal   *registry.CredentialRefusal
}

type bearerResult struct {
	bearer  issuedBearer
	refusal *registry.CredentialRefusal
}

func newPublisher(caller *capabilityCaller, runCredential string, endpoints publicationEndpoints) (*publisher, error) {
	put, err := newPutServerClient(endpoints.putBase, caller.client)
	if err != nil {
		return nil, err
	}
	return &publisher{
		caller:         caller,
		runCredential:  runCredential,
		endpoints:      endpoints,
		put:            put,
		now:            time.Now,
		sleep:          sleepContext,
		opTimeout:      publicationOpTimeout,
		releaseTimeout: publicationReleaseTimeout,
		putAttempt:     publicationPutAttempt,
		retryWait:      publicationRetryWait,
		resolved:       make(map[string]map[string]uint64),
		inherited:      make(map[string]map[string]bool),
		released:       make(map[string]releaseRecord),
	}, nil
}

func publicationRefusal(code, format string, args ...any) publicationAnswer {
	return publicationAnswer{refusal: &registry.CredentialRefusal{Code: code, Message: fmt.Sprintf(format, args...)}}
}

// refuse answers a refusal this provider decided and reports it on stderr.
func (p *publisher) refuse(op, code, format string, args ...any) publicationAnswer {
	answer := publicationRefusal(code, format, args...)
	p.diagnose("%s: refused (%s): %s", op, code, answer.refusal.Message)
	return answer
}

// resolve answers the heads of the request's channels from put-server, under
// the planning bearer, and records them: a release may expect only a head
// this session resolved, and carry unplanned only members those heads carry.
func (p *publisher) resolve(session context.Context, payload json.RawMessage) publicationAnswer {
	params, err := registry.ParseResolveParams(payload)
	if err != nil {
		return publicationRefusal(refusalInvalidRequest, "the resolve payload does not follow the publication-v1 protocol")
	}
	body, err := delegatedRequest(payload)
	if err != nil {
		return publicationRefusal(refusalInvalidRequest, "the resolve payload does not follow the publication-v1 protocol")
	}
	ctx, cancel := context.WithTimeout(session, p.opTimeout)
	defer cancel()
	planning := p.planningGrant(ctx)
	if planning.refusal != nil {
		return publicationAnswer{refusal: planning.refusal}
	}
	if params.Request.Namespace != planning.authority.Namespace {
		return p.refuse("resolve", registry.RefusalNamespaceForbidden, "this run publishes to namespace %s, not %s", planning.authority.Namespace, params.Request.Namespace)
	}
	response, err := p.putResolve(ctx, planning.bearer.bearer, body, &params.Request)
	if err != nil {
		refusal, _ := p.putRefusal("resolve", err)
		return publicationAnswer{refusal: refusal}
	}
	p.record(response)
	return publicationAnswer{payload: registry.ResolveResult{Response: *response}}
}

func (p *publisher) record(response *distribution.ResolveResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for channel, head := range response.Heads {
		refs := p.resolved[channel]
		if refs == nil {
			refs = make(map[string]uint64)
			p.resolved[channel] = refs
		}
		if head == nil {
			refs[""] = 0
			continue
		}
		refs[head.Ref.ID] = head.Generation
		if head.ReleaseSet == nil {
			continue
		}
		for _, member := range head.ReleaseSet.Members {
			key := memberKey(member.Ecosystem, member.Coordinate)
			if p.inherited[key] == nil {
				p.inherited[key] = make(map[string]bool)
			}
			p.inherited[key][artifactEncoding(member)] = true
		}
	}
}

// open opens the plan once Delivery accepts it. The same plan again gets the
// same answer; another plan, while one is open or opening, is refused.
func (p *publisher) open(session context.Context, payload json.RawMessage) publicationAnswer {
	params, err := registry.ParseOpenParams(payload)
	if err != nil {
		return publicationRefusal(refusalInvalidRequest, "the open payload does not follow the publication-v1 protocol")
	}
	digest := params.Plan.PlanDigest
	p.mu.Lock()
	if p.opened != nil {
		opened := p.opened.Plan.PlanDigest
		p.mu.Unlock()
		if opened != digest {
			return p.refuse("open", registry.RefusalPlanAlreadyOpen, "plan %s is already open in this session", opened)
		}
		return publicationAnswer{payload: registry.OpenResult{PlanDigest: digest}}
	}
	if pending := p.opening; pending != nil {
		p.mu.Unlock()
		if pending.digest != digest {
			return p.refuse("open", registry.RefusalPlanAlreadyOpen, "plan %s is being opened in this session", pending.digest)
		}
		select {
		case <-pending.done:
			return pending.answer
		case <-session.Done():
			return publicationAnswer{}
		}
	}
	pending := &pendingAnswer{digest: digest, done: make(chan struct{})}
	p.opening = pending
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(session, p.opTimeout)
	answer, held := p.openPlan(ctx, params)
	cancel()

	p.mu.Lock()
	p.opening = nil
	if answer.refusal == nil {
		p.opened, p.held = params, held
	}
	p.mu.Unlock()
	pending.answer = answer
	close(pending.done)
	return answer
}

// openPlan checks the plan against the run's publication authority, then has
// Delivery freeze it and mint its write bearer. The engine's ancestry
// statement is kept with the open plan and not judged: whether a forward-only
// channel moves is Delivery's answer at release, so a run whose channels will
// not move still opens its plan and uploads its artifacts. An immutable channel
// that already has a head does not refuse the plan either: it is answered as
// held, and the release leaves every channel where it is.
func (p *publisher) openPlan(ctx context.Context, params *registry.OpenParams) (publicationAnswer, *heldChannel) {
	planning := p.planningAuthority(ctx)
	if planning.refusal != nil {
		return publicationAnswer{refusal: planning.refusal}, nil
	}
	authority, plan := planning.authority, params.Plan
	switch {
	case plan.Namespace != authority.Namespace:
		return p.refuse("open", registry.RefusalNamespaceForbidden, "this run publishes to namespace %s, not %s", authority.Namespace, plan.Namespace), nil
	case plan.SourceRevision != authority.SourceRevision:
		return p.refuse("open", registry.RefusalPlanMismatch, "the plan publishes %s, but this run builds %s", plan.SourceRevision, authority.SourceRevision), nil
	case !slices.Equal(plan.Channels, authority.Channels) || plan.ImmutableChannel != authority.ImmutableChannel:
		return p.refuse("open", registry.RefusalPlanMismatch, "the plan's channels are not this run's channels [%s] with immutable channel %q",
			strings.Join(authority.Channels, " "), authority.ImmutableChannel), nil
	}
	held, refusal := p.immutableHead(ctx, plan)
	if refusal != nil {
		refusal.Message += "; the plan is not open"
		p.diagnose("open: immutable channel %s cannot be read (%s): %s", plan.ImmutableChannel, refusal.Code, refusal.Message)
		return publicationAnswer{refusal: refusal}, nil
	}
	if held != nil {
		p.diagnose("open: immutable channel %s already has head %s; this run is a rerun: it publishes its artifacts and moves no channel", held.name, held.head)
	}
	grant, ok := p.publishFlight.do(ctx, func() bearerResult { return p.mintPublish(ctx, plan) })
	switch {
	case !ok:
		return publicationRefusal(refusalPublicationUnavailable, "the open ended before Delivery answered the plan"), nil
	case grant.refusal != nil:
		return publicationAnswer{refusal: grant.refusal}, nil
	}
	return publicationAnswer{payload: registry.OpenResult{PlanDigest: plan.PlanDigest}}, held
}

// credential answers the publish credential: the open plan's write bearer
// for the publication origins. Before open it is refused (plan_not_open).
func (p *publisher) credential(session context.Context) credentialAnswer {
	p.mu.Lock()
	opened := p.opened
	p.mu.Unlock()
	if opened == nil {
		return refusal(registry.RefusalPlanNotOpen, "no plan is open in this session")
	}
	ctx, cancel := context.WithTimeout(session, p.opTimeout)
	defer cancel()
	grant := p.publishBearer(ctx, opened.Plan, false)
	if grant.refusal != nil {
		return credentialAnswer{refusal: grant.refusal}
	}
	credential := &registry.Credential{
		Bearer:    grant.bearer.bearer,
		ExpiresAt: grant.bearer.expiresAt.UTC().Format(time.RFC3339),
		Hosts:     slices.Clone(p.endpoints.hosts),
	}
	if err := registry.ValidateCredential(*credential); err != nil {
		p.diagnose("publish credential: the plan's bearer cannot be passed on: %v", err)
		return refusal(refusalPublicationUnavailable, "the plan's write bearer does not follow the credential protocol")
	}
	return credentialAnswer{credential: credential}
}

// release releases the open plan's set through put-server, at most once per
// plan digest. A release of the same digest and set gets the first
// definitive answer; one of another set is refused (plan_mismatch). A
// release whose outcome is unknown stores nothing: sent again, put-server
// answers it already-current when the first one applied.
func (p *publisher) release(session context.Context, payload json.RawMessage) publicationAnswer {
	params, err := registry.ParseReleaseParams(payload)
	if err != nil {
		return publicationRefusal(refusalInvalidRequest, "the release payload does not follow the publication-v1 protocol")
	}
	body, err := delegatedRequest(payload)
	if err != nil {
		return publicationRefusal(refusalInvalidRequest, "the release payload does not follow the publication-v1 protocol")
	}
	ref, diagnostics := distribution.DeriveReleaseSetRef(&params.Request.ReleaseSet)
	if diag.HasErrors(diagnostics) {
		return p.refuse("release", registry.RefusalPlanMismatch, "the released set has no release-set ref")
	}
	digest := params.PlanDigest

	p.mu.Lock()
	opened, authority, held := p.opened, p.authority, p.held
	switch {
	case opened == nil:
		p.mu.Unlock()
		return p.refuse("release", registry.RefusalPlanNotOpen, "no plan is open in this session")
	case digest != opened.Plan.PlanDigest:
		p.mu.Unlock()
		return p.refuse("release", registry.RefusalPlanMismatch, "the release names plan %s, but plan %s is open", digest, opened.Plan.PlanDigest)
	}
	if record, done := p.released[digest]; done {
		p.mu.Unlock()
		if record.ref != ref {
			return p.refuse("release", registry.RefusalPlanMismatch, "plan %s was released with set %s", digest, record.ref.ID)
		}
		return record.answer
	}
	if pending := p.releasing; pending != nil {
		p.mu.Unlock()
		if pending.ref != ref {
			return p.refuse("release", registry.RefusalPlanMismatch, "plan %s is being released with set %s", digest, pending.ref.ID)
		}
		select {
		case <-pending.done:
			return pending.answer
		case <-session.Done():
			return publicationAnswer{}
		}
	}
	answer := p.mismatchLocked(opened, params)
	if answer.refusal == nil {
		answer = p.unresolvedLocked(*authority, params)
	}
	if answer.refusal != nil {
		p.released[digest] = releaseRecord{ref: ref, answer: answer}
		p.mu.Unlock()
		p.diagnose("release: refused (%s): %s", answer.refusal.Code, answer.refusal.Message)
		return answer
	}
	pending := &pendingRelease{ref: ref, done: make(chan struct{})}
	p.releasing = pending
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(session, p.releaseTimeout)
	answer, definitive := p.releasePlan(ctx, opened.Plan, *authority, held, params, body)
	cancel()

	p.mu.Lock()
	p.releasing = nil
	if definitive {
		p.released[digest] = releaseRecord{ref: ref, answer: answer}
	}
	p.mu.Unlock()
	pending.answer = answer
	close(pending.done)
	return answer
}

// releasePlan releases the plan's set and reports whether the answer is
// definitive. Each put-server release goes under a write bearer minted for
// it, so Delivery checks again, at the instant the channels move, that the run
// may still publish the plan.
//
// Channels follow two rules. A run whose immutable channel already had a head
// at open is a rerun of a commit that released it (D19): no channel of the
// release moves, mutable ones included, and the run ends green. The same holds
// when put-server refuses the release because a concurrent run of the same
// commit created that channel in the meantime. Otherwise a forward-only
// channel moves only when Delivery says the run is still the head of its
// default branch (D18); when it is not, those channels stay where they are and
// the run ends green. When it is, they move from the heads put-server holds
// now; a head that moved again before the move is read once more, and a second
// miss is the conflict. Every channel that stays is answered with the set as
// its head. Without a forward-only channel, the release goes to put-server as
// the engine sent it.
//
// A channel that is neither forward-only nor held is never skipped on the
// engine's ancestry statement: a pull-request or topic-branch channel would
// then stay frozen after a force-push, whose new commit descends from no
// earlier head. The JS broker did not skip them either.
func (p *publisher) releasePlan(ctx context.Context, plan registry.PublicationPlan, authority publicationAuthority, held *heldChannel, params *registry.ReleaseParams, body []byte) (publicationAnswer, bool) {
	request := &params.Request
	if held != nil {
		return p.rerun(ctx, plan, request, body, held, "already has")
	}
	answer, definitive := p.releaseChannels(ctx, plan, authority, request, body)
	if answer.refusal == nil || answer.refusal.Code != registry.RefusalChannelImmutable || plan.ImmutableChannel == "" {
		return answer, definitive
	}
	// put-server moved no channel. When a concurrent run of the same commit
	// created the immutable channel since open, this run is a rerun after all.
	heads, refusal, _ := p.readHeads(ctx, plan.Namespace, []string{plan.ImmutableChannel})
	if refusal != nil {
		p.diagnose("release: immutable channel %s cannot be read again (%s); the refusal stands", plan.ImmutableChannel, refusal.Code)
		return answer, definitive
	}
	head := heads[plan.ImmutableChannel]
	if head == nil {
		return answer, definitive
	}
	return p.rerun(ctx, plan, request, body, &heldChannel{name: plan.ImmutableChannel, head: head.Ref.ID, generation: head.Generation}, "now has")
}

// rerun answers the release of a rerun, whose immutable channel held has a
// head (D19): no channel moves, put-server is not called, and the engine reads
// already-current with the set as the head of every channel, the held one at
// the generation it holds and the others at the generation this session
// resolved, as a skipped forward-only channel is answered. state words the
// held head on stderr.
func (p *publisher) rerun(ctx context.Context, plan registry.PublicationPlan, request *distribution.ReleaseRequest, body []byte, held *heldChannel, state string) (publicationAnswer, bool) {
	skipped := staying{}
	names := make([]string, 0, len(request.Channels))
	for _, channel := range request.Channels {
		names = append(names, channel.Name)
		skipped[channel.Name] = p.resolvedGeneration(channel)
	}
	skipped[held.name] = held.generation
	p.diagnose("release: immutable channel %s %s head %s; this run is a rerun and channels [%s] stay where they are",
		held.name, state, held.head, strings.Join(names, " "))
	answer, definitive, _ := p.releaseExcept(ctx, plan, request, body, request.Channels, false, skipped, nil)
	return answer, definitive
}

// releaseChannels releases a plan whose immutable channel had no head at open:
// forward-only channels follow Delivery's advance answer, and every other
// channel goes to put-server as the engine sent it.
func (p *publisher) releaseChannels(ctx context.Context, plan registry.PublicationPlan, authority publicationAuthority, request *distribution.ReleaseRequest, body []byte) (publicationAnswer, bool) {
	forwardOnly := make(map[string]bool)
	for _, channel := range request.Channels {
		if authority.forwardOnly(channel.Name) {
			forwardOnly[channel.Name] = true
		}
	}
	if len(forwardOnly) == 0 {
		answer, definitive, _ := p.releaseExcept(ctx, plan, request, body, request.Channels, false, nil, nil)
		return answer, definitive
	}
	answer, definitive, retry := p.releaseForwardOnly(ctx, plan, request, body, forwardOnly)
	if !retry {
		return answer, definitive
	}
	p.diagnose("release: a forward-only channel moved after it was read; reading it again")
	answer, definitive, retry = p.releaseForwardOnly(ctx, plan, request, body, forwardOnly)
	if retry {
		p.diagnose("release: a forward-only channel moved again after it was read; put-server moved no channel (conflict)")
	}
	return answer, definitive
}

// releaseForwardOnly is one attempt at a release that names forward-only
// channels. The third result reports a put-server conflict on forward-only
// channels alone: their heads moved between the read and the move.
func (p *publisher) releaseForwardOnly(ctx context.Context, plan registry.PublicationPlan, request *distribution.ReleaseRequest, body []byte, forwardOnly map[string]bool) (publicationAnswer, bool, bool) {
	advance, head, refusal := p.askAdvance(ctx)
	if refusal != nil {
		return publicationAnswer{refusal: refusal}, refusal.Code != refusalPublicationUnavailable, false
	}
	if !advance {
		// A superseded run or a rerun of an older commit: its artifacts are
		// uploaded, and its forward-only channels stay where they are.
		names := make([]string, 0, len(forwardOnly))
		left := staying{}
		for _, channel := range request.Channels {
			if forwardOnly[channel.Name] {
				names = append(names, channel.Name)
				left[channel.Name] = p.resolvedGeneration(channel)
			}
		}
		newer := "Delivery names no newer head"
		if head != "" {
			newer = "its head is " + head
		}
		p.diagnose("release: %s is not the head of its default branch (%s); forward-only channels [%s] stay where they are",
			plan.SourceRevision, newer, strings.Join(names, " "))
		answer, definitive, _ := p.releaseExcept(ctx, plan, request, body, request.Channels, false, left, nil)
		return answer, definitive, false
	}
	names := make([]string, 0, len(request.Channels))
	for _, channel := range request.Channels {
		names = append(names, channel.Name)
	}
	heads, refusal, decided := p.readHeads(ctx, request.Namespace, names)
	if refusal != nil {
		refusal.Message += "; no channel moved"
		return publicationAnswer{refusal: refusal}, decided, false
	}
	// The engine compared against the heads it resolved, minutes ago. A
	// forward-only channel moves from the head it carries now, provided the
	// set restates that head: otherwise the move would put back an older
	// artifact another run published since.
	channels := slices.Clone(request.Channels)
	refreshed := false
	for index, channel := range channels {
		current := heads[channel.Name]
		if !forwardOnly[channel.Name] || !headDiffers(channel.Expected, current) {
			continue
		}
		if !preservesHead(current, request.ReleaseSet, plan) {
			p.diagnose("release: forward-only channel %s moved to %s since it was resolved and carries members this run did not rebuild; no channel moved (conflict)",
				channel.Name, current.Ref.ID)
			return p.answerEngine(plan, request, headsConflict(request, heads), nil)
		}
		channels[index].Expected = headRef(current)
		refreshed = true
	}
	return p.releaseExcept(ctx, plan, request, body, channels, refreshed, nil, forwardOnly)
}

// releaseExcept releases the set on put-server for every channel not skipped,
// with channels as the channel list, and answers the engine for every channel
// of its request. rewritten reports that channels carry other expectations
// than the engine's; without it and without a skipped channel, put-server gets
// the engine's bytes verbatim. With no channel left, nothing is sent and the
// answer is already-current. The third result reports a put-server conflict
// on forwardOnly channels alone.
func (p *publisher) releaseExcept(ctx context.Context, plan registry.PublicationPlan, request *distribution.ReleaseRequest, body []byte, channels []distribution.ChannelRequest, rewritten bool, skipped staying, forwardOnly map[string]bool) (publicationAnswer, bool, bool) {
	rest := make([]distribution.ChannelRequest, 0, len(channels))
	for _, channel := range channels {
		if !skipped.has(channel.Name) {
			rest = append(rest, channel)
		}
	}
	response := &distribution.ReleaseResponse{ProtocolVersion: distribution.ProtocolVersion, Outcome: distribution.ReleaseOutcomeAlreadyCurrent, Current: map[string]*distribution.ChannelHead{}}
	retry := false
	if len(rest) > 0 {
		sent, sentBody := request, body
		if rewritten || len(rest) != len(request.Channels) {
			encoded, err := withChannels(body, rest)
			if err != nil {
				return publicationRefusal(refusalInvalidRequest, "the release request cannot be encoded for put-server"), true, false
			}
			narrowed := *request
			narrowed.Channels = rest
			sent, sentBody = &narrowed, encoded
		}
		grant := p.publishBearer(ctx, plan, true)
		if grant.refusal != nil {
			return publicationAnswer{refusal: grant.refusal}, grant.refusal.Code != refusalPublicationUnavailable, false
		}
		released, err := p.putRelease(ctx, grant.bearer.bearer, sentBody, sent)
		if err != nil {
			answer, definitive := p.releaseRefusal(plan, err)
			return answer, definitive, false
		}
		conflict := released.Outcome == distribution.ReleaseOutcomeConflict
		retry = conflict && onlyForwardOnlyMoved(sent, released, forwardOnly)
		if conflict && !retry {
			p.diagnose("release: a channel head moved since it was resolved; put-server moved no channel (conflict)")
		}
		response = released
	}
	answer, definitive, _ := p.answerEngine(plan, request, response, skipped)
	return answer, definitive, retry
}

// answerEngine completes response, which answers the channels put-server was
// asked about, with every skipped channel, and binds it to the engine's
// request. A skipped channel is reported at the released set, at its own
// generation, when the release succeeded, and at the engine's own expectation
// in a conflict: it did not move either way. The third result is false, for
// releaseForwardOnly.
func (p *publisher) answerEngine(plan registry.PublicationPlan, request *distribution.ReleaseRequest, response *distribution.ReleaseResponse, skipped staying) (publicationAnswer, bool, bool) {
	ref, _ := distribution.DeriveReleaseSetRef(&request.ReleaseSet)
	current := make(map[string]*distribution.ChannelHead, len(request.Channels))
	maps.Copy(current, response.Current)
	for _, channel := range request.Channels {
		generation, stays := skipped[channel.Name]
		if !stays {
			continue
		}
		switch {
		case response.Outcome != distribution.ReleaseOutcomeConflict:
			current[channel.Name] = &distribution.ChannelHead{Ref: ref, Generation: generation}
		case channel.Expected != nil:
			current[channel.Name] = &distribution.ChannelHead{Ref: *channel.Expected, Generation: generation}
		default:
			current[channel.Name] = nil
		}
	}
	answer, definitive := p.engineAnswer(plan, request, &distribution.ReleaseResponse{ProtocolVersion: distribution.ProtocolVersion, Outcome: response.Outcome, Current: current})
	return answer, definitive, false
}

// askAdvance asks Delivery whether the run may move its forward-only channels
// now. A refusal moved no channel; only one Delivery decided is definitive.
func (p *publisher) askAdvance(ctx context.Context) (advance bool, head string, refusal *registry.CredentialRefusal) {
	answer := p.caller.post(ctx, nativePublicationAdvancePath, p.runCredential, []byte("{}"), forwardOnlyHeaders)
	if answer.outcome != capabilityGranted {
		refusal := p.deliveryRefusal("channel advance", nativePublicationAdvancePath, answer)
		refusal.Message += "; no channel moved"
		return false, "", refusal
	}
	advance, head, err := parseAdvanceAnswer(answer.body)
	if err != nil {
		p.diagnose("channel advance: Delivery answered HTTP %d with an answer this provider cannot use: %v", answer.status, err)
		return false, "", &registry.CredentialRefusal{Code: refusalPublicationUnavailable, Message: "Delivery answered the channel advance outside its contract; no channel moved"}
	}
	return advance, head, nil
}

// readHeads resolves channels under the planning bearer and reports whether a
// refusal is definitive. It records nothing: what a release may expect and
// carry unplanned is what the engine's resolves returned, not what another
// run wrote since. Each refusal is a fresh value the caller may extend.
func (p *publisher) readHeads(ctx context.Context, namespace string, channels []string) (map[string]*distribution.ChannelHead, *registry.CredentialRefusal, bool) {
	planning := p.planningGrant(ctx)
	if planning.refusal != nil {
		refusal := *planning.refusal
		return nil, &refusal, refusal.Code != refusalPublicationUnavailable
	}
	resolve := distribution.ResolveRequest{ProtocolVersion: distribution.ProtocolVersion, Namespace: namespace, Channels: channels}
	body, err := json.Marshal(resolve)
	if err != nil {
		return nil, &registry.CredentialRefusal{Code: refusalInvalidRequest, Message: "the channel resolve cannot be encoded"}, true
	}
	response, err := p.putResolve(ctx, planning.bearer.bearer, body, &resolve)
	if err != nil {
		refusal, definitive := p.putRefusal("resolve", err)
		return nil, refusal, definitive
	}
	return response.Heads, nil, true
}

// immutableHead is the plan's immutable channel when it already has a head,
// nil when it has none or the plan names none: a channel that has a head was
// released by an earlier run of the commit. The session's resolves answer when
// they read the channel; the engine does not resolve a tag's channel, so
// otherwise this provider reads it, without recording what it read.
func (p *publisher) immutableHead(ctx context.Context, plan registry.PublicationPlan) (*heldChannel, *registry.CredentialRefusal) {
	name := plan.ImmutableChannel
	if name == "" {
		return nil, nil
	}
	p.mu.Lock()
	refs, read := p.resolved[name]
	ids := make([]string, 0, len(refs))
	for id := range refs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var held *heldChannel
	if len(ids) > 0 {
		held = &heldChannel{name: name, head: ids[0], generation: refs[ids[0]]}
	}
	p.mu.Unlock()
	if read {
		return held, nil
	}
	heads, refusal, _ := p.readHeads(ctx, plan.Namespace, []string{name})
	if refusal != nil {
		return nil, refusal
	}
	if head := heads[name]; head != nil {
		return &heldChannel{name: name, head: head.Ref.ID, generation: head.Generation}, nil
	}
	return nil, nil
}

// engineAnswer binds a release answer to the engine's request: released
// becomes already-current when the engine expected the set on every channel,
// and the answer must pass the engine's exchange check. An answer that does
// not has an unknown outcome.
func (p *publisher) engineAnswer(plan registry.PublicationPlan, request *distribution.ReleaseRequest, response *distribution.ReleaseResponse) (publicationAnswer, bool) {
	ref, _ := distribution.DeriveReleaseSetRef(&request.ReleaseSet)
	if response.Outcome == distribution.ReleaseOutcomeReleased && everyChannelExpects(request.Channels, ref) {
		response.Outcome = distribution.ReleaseOutcomeAlreadyCurrent
	}
	if diagnostics := distribution.ValidateReleaseExchange(request, response); diag.HasErrors(diagnostics) {
		p.diagnose("release: the answer does not answer the engine's request: %s", diag.ErrorText(diagnostics))
		return publicationRefusal(refusalPublicationUnavailable, "the release answer does not answer the engine's request; the outcome of plan %s is unknown: read the channel heads before publishing again", plan.PlanDigest), false
	}
	return publicationAnswer{payload: registry.ReleaseResult{Response: *response}}, true
}

// releaseRefusal answers a put-server release that failed.
func (p *publisher) releaseRefusal(plan registry.PublicationPlan, err error) (publicationAnswer, bool) {
	refusal, definitive := p.putRefusal("release", err)
	if !definitive {
		refusal.Message = fmt.Sprintf("%s; the outcome of plan %s is unknown: read the channel heads before publishing again", refusal.Message, plan.PlanDigest)
	}
	return publicationAnswer{refusal: refusal}, definitive
}

// resolvedGeneration is the generation at which this session resolved the
// head channel expects, or 1 when it expects no head or one this session
// did not read the generation of.
func (p *publisher) resolvedGeneration(channel distribution.ChannelRequest) uint64 {
	if channel.Expected == nil {
		return 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if generation := p.resolved[channel.Name][channel.Expected.ID]; generation > 0 {
		return generation
	}
	return 1
}

// preservesHead reports whether set restates head member for member, so a
// move from head drops nothing another run put there: every member of head is
// carried unchanged or rebuilt by the plan, and every member set carries
// without planning it is in head, unchanged. A head whose members this
// provider cannot read is not preserved.
func preservesHead(head *distribution.ChannelHead, set distribution.ReleaseSet, plan registry.PublicationPlan) bool {
	if head == nil {
		return true
	}
	if head.ReleaseSet == nil {
		return false
	}
	planned := make(map[string]bool, len(plan.Members))
	for _, member := range plan.Members {
		planned[memberKey(member.Ecosystem, member.Coordinate)] = true
	}
	mine := make(map[string]string, len(set.Members))
	for _, member := range set.Members {
		mine[memberKey(member.Ecosystem, member.Coordinate)] = artifactEncoding(member)
	}
	held := make(map[string]string, len(head.ReleaseSet.Members))
	for _, member := range head.ReleaseSet.Members {
		key := memberKey(member.Ecosystem, member.Coordinate)
		held[key] = artifactEncoding(member)
		carried, present := mine[key]
		if !present || (carried != held[key] && !planned[key]) {
			return false
		}
	}
	for key, encoding := range mine {
		if !planned[key] && held[key] != encoding {
			return false
		}
	}
	return true
}

// headsConflict is a conflict naming every requested channel as heads say it
// stands, pointers only. No channel moved.
func headsConflict(request *distribution.ReleaseRequest, heads map[string]*distribution.ChannelHead) *distribution.ReleaseResponse {
	current := make(map[string]*distribution.ChannelHead, len(request.Channels))
	for _, channel := range request.Channels {
		var pointer *distribution.ChannelHead
		if head := heads[channel.Name]; head != nil {
			pointer = &distribution.ChannelHead{Ref: head.Ref, Generation: head.Generation}
		}
		current[channel.Name] = pointer
	}
	return &distribution.ReleaseResponse{ProtocolVersion: distribution.ProtocolVersion, Outcome: distribution.ReleaseOutcomeConflict, Current: current}
}

// onlyForwardOnlyMoved reports whether the heads a conflict reports differ
// from the request's expectations on forward-only channels alone. Reading
// those heads again may succeed; a moved channel of another kind would
// conflict again.
func onlyForwardOnlyMoved(request *distribution.ReleaseRequest, response *distribution.ReleaseResponse, forwardOnly map[string]bool) bool {
	moved := false
	for _, channel := range request.Channels {
		if !headDiffers(channel.Expected, response.Current[channel.Name]) {
			continue
		}
		if !forwardOnly[channel.Name] {
			return false
		}
		moved = true
	}
	return moved
}

// headDiffers reports whether head is not the head expected names.
func headDiffers(expected *distribution.ReleaseSetRef, head *distribution.ChannelHead) bool {
	switch {
	case expected == nil && head == nil:
		return false
	case expected == nil || head == nil:
		return true
	}
	return *expected != head.Ref
}

// headRef is the expectation that head is the channel's head.
func headRef(head *distribution.ChannelHead) *distribution.ReleaseSetRef {
	if head == nil {
		return nil
	}
	ref := head.Ref
	return &ref
}

// everyChannelExpects reports whether every channel expects ref as its head.
func everyChannelExpects(channels []distribution.ChannelRequest, ref distribution.ReleaseSetRef) bool {
	for _, channel := range channels {
		if channel.Expected == nil || *channel.Expected != ref {
			return false
		}
	}
	return len(channels) > 0
}

// mismatchLocked refuses a release whose request is not the open plan's: the
// ancestry stated at open, the plan's namespace and channels in order, the
// immutable flag on exactly the plan's immutable channel, every planned
// member at its planned version and provenance, and every other member
// carried unchanged from a head this session resolved. p.mu is held.
func (p *publisher) mismatchLocked(opened *registry.OpenParams, params *registry.ReleaseParams) publicationAnswer {
	plan, request := opened.Plan, params.Request
	if !sameJSON(params.Ancestry, opened.Ancestry) {
		return publicationRefusal(registry.RefusalPlanMismatch, "the release states another ancestry than the open did")
	}
	if request.Namespace != plan.Namespace || request.ReleaseSet.Namespace != plan.Namespace {
		return publicationRefusal(registry.RefusalPlanMismatch, "the release is in namespace %s, the plan in %s", request.Namespace, plan.Namespace)
	}
	names := make([]string, 0, len(request.Channels))
	for _, channel := range request.Channels {
		names = append(names, channel.Name)
		if channel.Immutable != (channel.Name == plan.ImmutableChannel) {
			return publicationRefusal(registry.RefusalPlanMismatch, "channel %s is immutable exactly when it is the plan's immutable channel", channel.Name)
		}
	}
	if !slices.Equal(names, plan.Channels) {
		return publicationRefusal(registry.RefusalPlanMismatch, "the release advances [%s], the plan [%s]", strings.Join(names, " "), strings.Join(plan.Channels, " "))
	}
	set := distribution.NormalizeReleaseSet(&request.ReleaseSet)
	members := make(map[string]distribution.ReleaseSetMember, len(set.Members))
	for _, member := range set.Members {
		members[memberKey(member.Ecosystem, member.Coordinate)] = member
	}
	planned := make(map[string]bool, len(plan.Members))
	for _, want := range plan.Members {
		key := memberKey(want.Ecosystem, want.Coordinate)
		planned[key] = true
		got, present := members[key]
		if !present || got.Version != want.Version || got.SourceRevision != want.SourceRevision || got.SelectionFingerprint != want.SelectionFingerprint {
			return publicationRefusal(registry.RefusalPlanMismatch, "the released set does not carry %s/%s at version %s from the plan's build", want.Ecosystem, want.Coordinate, want.Version)
		}
	}
	for _, member := range set.Members {
		key := memberKey(member.Ecosystem, member.Coordinate)
		if !planned[key] && !p.inherited[key][artifactEncoding(member)] {
			return publicationRefusal(registry.RefusalPlanMismatch, "member %s/%s is neither planned nor carried by a head this session resolved", member.Ecosystem, member.Coordinate)
		}
	}
	return publicationAnswer{}
}

// unresolvedLocked refuses a release that expects, on a forward-only channel,
// a head this session did not resolve: the engine compares only against a
// head it read through this provider. p.mu is held.
func (p *publisher) unresolvedLocked(authority publicationAuthority, params *registry.ReleaseParams) publicationAnswer {
	for _, channel := range params.Request.Channels {
		if !authority.forwardOnly(channel.Name) || channel.Expected == nil {
			continue
		}
		if _, resolved := p.resolved[channel.Name][channel.Expected.ID]; !resolved {
			return publicationRefusal(registry.RefusalPlanMismatch, "the release expects head %s of forward-only channel %s, which this session did not resolve", channel.Expected.ID, channel.Name)
		}
	}
	return publicationAnswer{}
}

// planningGrant returns the run's publication authority with a planning
// bearer valid for planningBearerMargin more, minting one when needed.
func (p *publisher) planningGrant(ctx context.Context) planningResult {
	p.mu.Lock()
	if p.planning != nil && p.planning.expiresAt.Sub(p.now()) > planningBearerMargin {
		result := planningResult{authority: *p.authority, bearer: *p.planning}
		p.mu.Unlock()
		return result
	}
	p.mu.Unlock()
	result, ok := p.planningFlight.do(ctx, func() planningResult { return p.mintPlanning(ctx) })
	if !ok {
		return planningResult{refusal: &registry.CredentialRefusal{Code: refusalPublicationUnavailable, Message: "the op ended before Delivery answered the publication planning"}}
	}
	return result
}

// planningAuthority returns the run's publication authority, asking Delivery
// only when this session has not read it yet.
func (p *publisher) planningAuthority(ctx context.Context) planningResult {
	p.mu.Lock()
	if p.authority != nil {
		result := planningResult{authority: *p.authority}
		p.mu.Unlock()
		return result
	}
	p.mu.Unlock()
	return p.planningGrant(ctx)
}

func (p *publisher) mintPlanning(ctx context.Context) planningResult {
	issuedAt := p.now()
	answer := p.caller.post(ctx, nativePublicationPlanningPath, p.runCredential, []byte("{}"), forwardOnlyHeaders)
	if answer.outcome != capabilityGranted {
		return planningResult{refusal: p.deliveryRefusal("publication planning", nativePublicationPlanningPath, answer)}
	}
	authority, bearer, err := parsePlanningGrant(answer.body, issuedAt)
	if err != nil {
		p.diagnose("publication planning: Delivery answered HTTP %d with a grant this provider cannot use: %v", answer.status, err)
		return planningResult{refusal: &registry.CredentialRefusal{Code: refusalPublicationUnavailable, Message: "Delivery answered the publication planning with a grant that does not follow its contract"}}
	}
	p.addSecret(bearer.bearer)
	p.mu.Lock()
	if p.authority != nil && !p.authority.equal(authority) {
		p.mu.Unlock()
		p.diagnose("publication planning: Delivery answered another publication authority than earlier in this session")
		return planningResult{refusal: &registry.CredentialRefusal{Code: refusalPublicationUnavailable, Message: "Delivery changed this run's publication authority during the session"}}
	}
	if p.authority == nil {
		p.authority = &authority
	}
	p.planning = &bearer
	p.mu.Unlock()
	return planningResult{authority: authority, bearer: bearer}
}

// publishBearer returns the open plan's write bearer. It is reused until half
// of its lifetime has passed unless renew asks for a fresh one; Delivery
// mints another when the same plan is posted again.
func (p *publisher) publishBearer(ctx context.Context, plan registry.PublicationPlan, renew bool) bearerResult {
	if !renew {
		p.mu.Lock()
		if p.publish != nil && !p.publish.halfSpent(p.now()) {
			result := bearerResult{bearer: *p.publish}
			p.mu.Unlock()
			return result
		}
		p.mu.Unlock()
	}
	result, ok := p.publishFlight.do(ctx, func() bearerResult { return p.mintPublish(ctx, plan) })
	if !ok {
		return bearerResult{refusal: &registry.CredentialRefusal{Code: refusalPublicationUnavailable, Message: "the op ended before Delivery answered the plan"}}
	}
	return result
}

func (p *publisher) mintPublish(ctx context.Context, plan registry.PublicationPlan) bearerResult {
	body, err := json.Marshal(plan)
	if err != nil {
		return bearerResult{refusal: &registry.CredentialRefusal{Code: refusalInvalidRequest, Message: "the plan cannot be encoded"}}
	}
	issuedAt := p.now()
	answer := p.caller.post(ctx, nativePublicationPublishPath, p.runCredential, body, forwardOnlyHeaders)
	if answer.outcome != capabilityGranted {
		return bearerResult{refusal: p.deliveryRefusal("publication plan", nativePublicationPublishPath, answer)}
	}
	bearer, err := parsePublishGrant(answer.body, plan.PlanDigest, issuedAt)
	if err != nil {
		p.diagnose("publication plan: Delivery answered HTTP %d with a grant this provider cannot use: %v", answer.status, err)
		return bearerResult{refusal: &registry.CredentialRefusal{Code: refusalPublicationUnavailable, Message: "Delivery answered the plan with a grant that does not follow its contract"}}
	}
	p.addSecret(bearer.bearer)
	p.mu.Lock()
	p.publish = &bearer
	p.mu.Unlock()
	return bearerResult{bearer: bearer}
}

// deliveryRefusal maps a Delivery answer without a grant to the refusal the
// op answers. On the plan route, a 400 is a plan Delivery's authority does
// not match and a digest drift is another plan already frozen on the run.
// Every other refusal passes Delivery's message on: this provider never
// answers not_forward, since a superseded run publishes and only its
// forward-only channels stay put.
func (p *publisher) deliveryRefusal(stage, route string, answer capabilityAnswer) *registry.CredentialRefusal {
	switch answer.outcome {
	case capabilityAbsent:
		p.diagnose("%s: Delivery answered HTTP 204 without a capability", stage)
		return &registry.CredentialRefusal{Code: refusalPublicationUnavailable, Message: "Delivery answered no publication capability for this run"}
	case capabilityRefused:
		_, message := capabilityRefusal(answer.body)
		code := refusalPublicationRefused
		if route == nativePublicationPublishPath {
			switch {
			case answer.status == http.StatusBadRequest:
				code = registry.RefusalPlanMismatch
			case answer.status == http.StatusConflict && strings.Contains(message, "digest drift"):
				code = registry.RefusalPlanAlreadyOpen
			}
		}
		p.diagnose("%s: Delivery refused (HTTP %d, %s)", stage, answer.status, code)
		text := fmt.Sprintf("Delivery refused the %s (HTTP %d)", stage, answer.status)
		if message = p.safeText(message); message != "" {
			text += ": " + message
		}
		return &registry.CredentialRefusal{Code: code, Message: p.safeText(text)}
	default:
		text := fmt.Sprintf("Delivery did not answer the %s", stage)
		if answer.status != 0 {
			text = fmt.Sprintf("Delivery did not answer the %s (last answer HTTP %d)", stage, answer.status)
		}
		p.diagnose("%s", text)
		return &registry.CredentialRefusal{Code: refusalPublicationUnavailable, Message: text}
	}
}

// delegatedRequest returns the bytes of the payload's request member, the
// distribution document this provider forwards verbatim.
func delegatedRequest(payload json.RawMessage) ([]byte, error) {
	var wire struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil || len(wire.Request) == 0 {
		return nil, fmt.Errorf("the payload carries no request")
	}
	return wire.Request, nil
}

func memberKey(ecosystem distribution.Ecosystem, coordinate string) string {
	return string(ecosystem) + "\x00" + coordinate
}

// artifactEncoding is a member's identity as an artifact: its normalized
// encoding without project and kind. Those two attribute the member to a
// workload, and the engine stamps them on members it carries from a head that
// had none, so they do not make a carried member another artifact.
func artifactEncoding(member distribution.ReleaseSetMember) string {
	normalized := distribution.NormalizeReleaseSet(&distribution.ReleaseSet{Members: []distribution.ReleaseSetMember{member}}).Members[0]
	normalized.Project, normalized.Kind = "", ""
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func sameJSON(a, b any) bool {
	left, errLeft := json.Marshal(a)
	right, errRight := json.Marshal(b)
	return errLeft == nil && errRight == nil && string(left) == string(right)
}

// singleFlight runs one call at a time. A caller that arrives while a call
// runs waits for its result instead of starting another.
type singleFlight[T any] struct {
	mu      sync.Mutex
	current *flightCall[T]
}

type flightCall[T any] struct {
	done  chan struct{}
	value T
}

// do runs fn, or waits for the call in progress. ok is false when ctx ended
// while it waited.
func (g *singleFlight[T]) do(ctx context.Context, fn func() T) (value T, ok bool) {
	g.mu.Lock()
	call := g.current
	if call == nil {
		call = &flightCall[T]{done: make(chan struct{})}
		g.current = call
		g.mu.Unlock()
		call.value = fn()
		g.mu.Lock()
		g.current = nil
		g.mu.Unlock()
		close(call.done)
		return call.value, true
	}
	g.mu.Unlock()
	select {
	case <-call.done:
		return call.value, true
	case <-ctx.Done():
		return value, false
	}
}
