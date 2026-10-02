// Package providertest is an in-process credential provider for tests. It
// speaks credential-v1 and, when its initialize answer echoes it,
// publication-v1 (registry ADR 0003) over the pipes credentialprovider.Connect
// takes, and it keeps the release-set ledger a publication provider owns:
// channel heads, stored sets, and one release per plan digest.
//
// A Provider is the server side. Each Pipes call serves one session; a plan
// opened in one session is not open in another. The ledger and the releases
// recorded by plan digest belong to the Provider and outlive every session.
//
// The package imports no engine code, so the engine's own packages can drive
// a Session directly (Provider.NewSession) and get the same answers a wire
// session gets.
package providertest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	registry "go.putnami.dev/protocol/registry"
)

// Config shapes one Provider. The zero value echoes every capability, holds
// no credential, refuses nothing and stores every artifact a release names.
type Config struct {
	// Echo is the capability list the initialize answer carries. Nil echoes
	// credential-v1 and publication-v1.
	Echo []string
	// Bearer is the credential the provider issues for both purposes, with
	// Hosts. Empty answers every credential request with absence.
	Bearer string
	// Hosts are the hosts the credential serves, sorted and unique.
	Hosts []string
	// Lifetime is how long an issued credential is valid; 0 is one hour.
	Lifetime time.Duration
	// Clock is the provider's clock; nil is time.Now.
	Clock func() time.Time
	// Refuse maps an op to the refusal code the provider answers every
	// request of that op with.
	Refuse map[registry.CredentialOp]string
	// Unanswered maps an op to how many of its requests the provider applies
	// and then leaves unanswered, as a provider whose answer is lost does.
	Unanswered map[registry.CredentialOp]int
	// Stored answers the artifact digest the member's registry stores, and
	// false when it stores none. Nil stores every artifact at the digest the
	// release names.
	Stored func(ecosystem, coordinate, version string) (digest string, stored bool)
}

// Call records one request the provider read.
type Call struct {
	// Op is the request's op.
	Op registry.CredentialOp
	// Purpose is the purpose of a credential request.
	Purpose string
	// PlanDigest is the plan an open or a release names.
	PlanDigest string
	// Code is the refusal code of a refused request, "" otherwise.
	Code string
	// Answered is false for a request the provider applied and left
	// unanswered (Config.Unanswered).
	Answered bool
}

// Provider is the server side of every session it serves.
type Provider struct {
	config Config

	mu          sync.Mutex
	offered     [][]string
	calls       []Call
	opens       []registry.OpenParams
	releases    int
	unanswered  map[registry.CredentialOp]int
	released    map[string]releaseRecord
	sets        map[string]distribution.ReleaseSet
	heads       map[string]distribution.ReleaseSetRef
	generations map[string]uint64
}

// releaseRecord is the first answer to one plan digest's release.
type releaseRecord struct {
	ref    distribution.ReleaseSetRef
	result registry.ReleaseResult
}

// New returns a Provider with an empty ledger.
func New(config Config) *Provider {
	if config.Echo == nil {
		config.Echo = []string{registry.CapabilityCredentialV1, registry.CapabilityPublicationV1}
	}
	if config.Lifetime == 0 {
		config.Lifetime = time.Hour
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &Provider{
		config:      config,
		unanswered:  maps.Clone(config.Unanswered),
		released:    make(map[string]releaseRecord),
		sets:        make(map[string]distribution.ReleaseSet),
		heads:       make(map[string]distribution.ReleaseSetRef),
		generations: make(map[string]uint64),
	}
}

// Pipes serves one session and returns the two ends credentialprovider.Connect
// takes: the engine writes requests into the first and reads answers from the
// second. The session ends at shutdown, at a line the protocol refuses, or
// when the engine closes its end.
func (p *Provider) Pipes() (requests io.WriteCloser, responses io.ReadCloser) {
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	go func() {
		p.Serve(requestReader, responseWriter)
		_ = responseWriter.Close()
		_, _ = io.Copy(io.Discard, requestReader)
	}()
	return requestWriter, responseReader
}

// Serve answers the request lines read from in on out, one session, until
// shutdown, a line the protocol refuses, or the end of in.
func (p *Provider) Serve(in io.Reader, out io.Writer) {
	session := p.NewSession()
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 4<<10), registry.MaxPublicationLineBytes+1)
	for scanner.Scan() {
		request, err := registry.ParseNegotiatedCredentialRequest(scanner.Bytes(), session.capabilities())
		if err != nil {
			return
		}
		payload, refusal, answer := session.handle(request)
		if !answer {
			continue
		}
		response := registry.CredentialResponse{ProtocolVersion: registry.CredentialProtocolVersion, ID: request.ID, OK: refusal == nil, Error: refusal}
		if refusal == nil && payload != nil {
			encoded, err := json.Marshal(payload)
			if err != nil {
				return
			}
			response.Payload = encoded
		}
		line, err := json.Marshal(response)
		if err != nil {
			return
		}
		if _, err := out.Write(append(line, '\n')); err != nil {
			return
		}
		if request.Op == registry.CredentialOpShutdown {
			return
		}
	}
}

// Session is one session's state: the capabilities it negotiated and the plan
// open in it. A Session is safe for concurrent use.
type Session struct {
	provider *Provider

	mu         sync.Mutex
	negotiated []string
	open       *registry.OpenParams
}

// NewSession returns a session with nothing negotiated and no plan open.
func (p *Provider) NewSession() *Session {
	return &Session{provider: p}
}

// handle applies one request and returns its answer: a payload or a refusal,
// and whether the provider writes that answer at all.
func (s *Session) handle(request *registry.CredentialRequest) (any, *registry.CredentialRefusal, bool) {
	call := Call{Op: request.Op, Answered: true}
	var payload any
	var refusal *registry.CredentialRefusal
	switch request.Op {
	case registry.CredentialOpInitialize:
		params, err := registry.ParseCredentialInitializeParams(request.Payload)
		if err != nil {
			refusal = refuse("invalid_request", "initialize is invalid")
			break
		}
		payload = s.Initialize(params.Capabilities)
	case registry.CredentialOpCredential:
		params, err := registry.ParseCredentialParams(request.Payload)
		if err != nil {
			refusal = refuse("invalid_request", "credential is invalid")
			break
		}
		call.Purpose = params.Purpose
		payload, refusal = s.Credential(params.Purpose)
	case registry.CredentialOpShutdown:
		payload = struct{}{}
	case registry.CredentialOpResolve:
		params, err := registry.ParseResolveParams(request.Payload)
		if err != nil {
			refusal = refuse("invalid_request", "resolve is invalid")
			break
		}
		payload, refusal = s.Resolve(params.Request)
	case registry.CredentialOpOpen:
		params, err := registry.ParseOpenParams(request.Payload)
		if err != nil {
			refusal = refuse("invalid_request", "open is invalid")
			break
		}
		call.PlanDigest = params.Plan.PlanDigest
		payload, refusal = s.Open(*params)
	case registry.CredentialOpRelease:
		params, err := registry.ParseReleaseParams(request.Payload)
		if err != nil {
			refusal = refuse("invalid_request", "release is invalid")
			break
		}
		call.PlanDigest = params.PlanDigest
		payload, refusal = s.Release(*params)
	}
	if refusal != nil {
		call.Code = refusal.Code
	}
	p := s.provider
	p.mu.Lock()
	if p.unanswered[request.Op] > 0 {
		p.unanswered[request.Op]--
		call.Answered = false
	}
	p.calls = append(p.calls, call)
	p.mu.Unlock()
	return payload, refusal, call.Answered
}

// Initialize negotiates the session: the offered capabilities the provider
// echoes. It answers the provider's echo.
func (s *Session) Initialize(offered []string) registry.CredentialInitializeResult {
	p := s.provider
	p.mu.Lock()
	p.offered = append(p.offered, slices.Clone(offered))
	p.mu.Unlock()
	s.mu.Lock()
	s.negotiated = registry.NegotiatedCapabilities(offered, p.config.Echo)
	s.mu.Unlock()
	return registry.CredentialInitializeResult{
		ProtocolVersion: registry.CredentialProtocolVersion,
		ProviderName:    "providertest",
		Capabilities:    slices.Clone(p.config.Echo),
	}
}

// Publication reports whether the session negotiated publication-v1.
func (s *Session) Publication() bool {
	return slices.Contains(s.capabilities(), registry.CapabilityPublicationV1)
}

func (s *Session) capabilities() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.negotiated
}

// Credential answers the purpose's credential. Under publication-v1 the
// publish credential is issued only while a plan is open.
func (s *Session) Credential(purpose string) (*registry.CredentialResult, *registry.CredentialRefusal) {
	p := s.provider
	if code := p.config.Refuse[registry.CredentialOpCredential]; code != "" {
		return nil, refuse(code, "the provider refuses the credential")
	}
	s.mu.Lock()
	open := s.open
	s.mu.Unlock()
	if purpose == registry.PurposePublish && s.Publication() && open == nil {
		return nil, refuse(registry.RefusalPlanNotOpen, "no plan is open in this session")
	}
	if p.config.Bearer == "" {
		return &registry.CredentialResult{}, nil
	}
	expiry := p.config.Clock().Add(p.config.Lifetime).UTC().Truncate(time.Second)
	return &registry.CredentialResult{Credential: &registry.Credential{
		Bearer:    p.config.Bearer,
		ExpiresAt: expiry.Format(time.RFC3339),
		Hosts:     slices.Clone(p.config.Hosts),
	}}, nil
}

// Resolve answers the head of every requested channel, null for a channel
// without one.
func (s *Session) Resolve(request distribution.ResolveRequest) (*registry.ResolveResult, *registry.CredentialRefusal) {
	p := s.provider
	if code := p.config.Refuse[registry.CredentialOpResolve]; code != "" {
		return nil, refuse(code, "the provider refuses resolve")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	heads := make(map[string]*distribution.ChannelHead, len(request.Channels))
	for _, channel := range request.Channels {
		heads[channel] = p.headLocked(request.Namespace, channel, true)
	}
	return &registry.ResolveResult{Response: distribution.ResolveResponse{ProtocolVersion: distribution.ProtocolVersion, Heads: heads}}, nil
}

// Open opens params.Plan. The same plan again answers as the first open did;
// another plan while one is open is refused. A channel whose head the engine
// states is not an ancestor of the plan's source revision is refused, and so
// is an immutable channel that already has a head.
func (s *Session) Open(params registry.OpenParams) (*registry.OpenResult, *registry.CredentialRefusal) {
	p := s.provider
	if code := p.config.Refuse[registry.CredentialOpOpen]; code != "" {
		return nil, refuse(code, "the provider refuses open")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open != nil {
		if s.open.Plan.PlanDigest != params.Plan.PlanDigest {
			return nil, refuse(registry.RefusalPlanAlreadyOpen, "another plan is open in this session")
		}
		return &registry.OpenResult{PlanDigest: params.Plan.PlanDigest}, nil
	}
	if refusal := notForward(params.Ancestry); refusal != nil {
		return nil, refusal
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if channel := params.Plan.ImmutableChannel; channel != "" && p.headLocked(params.Plan.Namespace, channel, false) != nil {
		return nil, refuse(registry.RefusalChannelImmutable, "channel "+channel+" already has a head")
	}
	opened := params
	s.open = &opened
	p.opens = append(p.opens, params)
	return &registry.OpenResult{PlanDigest: params.Plan.PlanDigest}, nil
}

// Release releases the open plan's set: it checks the request against the
// plan, every selected member's artifact against its registry and the
// ancestry, then advances every channel by compare-and-swap in one
// transaction. A plan digest is released at most once: a later release of it
// answers the first answer.
func (s *Session) Release(params registry.ReleaseParams) (*registry.ReleaseResult, *registry.CredentialRefusal) {
	p := s.provider
	if code := p.config.Refuse[registry.CredentialOpRelease]; code != "" {
		return nil, refuse(code, "the provider refuses release")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil {
		return nil, refuse(registry.RefusalPlanNotOpen, "no plan is open in this session")
	}
	plan := s.open.Plan
	if params.PlanDigest != plan.PlanDigest {
		return nil, refuse(registry.RefusalPlanMismatch, "release names another plan than the open one")
	}
	set := distribution.NormalizeReleaseSet(&params.Request.ReleaseSet)
	ref, diagnostics := distribution.DeriveReleaseSetRef(set)
	if diag.HasErrors(diagnostics) {
		return nil, refuse(registry.RefusalPlanMismatch, "the released set has no ref")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if record, done := p.released[params.PlanDigest]; done {
		if record.ref != ref {
			return nil, refuse(registry.RefusalPlanMismatch, "this plan was released with another set")
		}
		result := record.result
		return &result, nil
	}
	if refusal := p.matchesPlanLocked(plan, params.Request, set); refusal != nil {
		return nil, refusal
	}
	if refusal := notForward(params.Ancestry); refusal != nil {
		return nil, refusal
	}
	result := registry.ReleaseResult{Response: p.advanceLocked(params.Request, *set, ref)}
	p.released[params.PlanDigest] = releaseRecord{ref: ref, result: result}
	p.releases++
	return &result, nil
}

// matchesPlanLocked refuses a release whose request is not the open plan's:
// another namespace or channel list, a selected member missing or at another
// version, or a selected member whose artifact its registry does not store
// at the released digest.
func (p *Provider) matchesPlanLocked(plan registry.PublicationPlan, request distribution.ReleaseRequest, set *distribution.ReleaseSet) *registry.CredentialRefusal {
	channels := make([]string, 0, len(request.Channels))
	for _, channel := range request.Channels {
		channels = append(channels, channel.Name)
	}
	if request.Namespace != plan.Namespace || !slices.Equal(channels, plan.Channels) {
		return refuse(registry.RefusalPlanMismatch, "the release request is not the open plan's")
	}
	for _, planned := range plan.Members {
		index := slices.IndexFunc(set.Members, func(member distribution.ReleaseSetMember) bool {
			return member.Ecosystem == planned.Ecosystem && member.Coordinate == planned.Coordinate
		})
		if index < 0 || set.Members[index].Version != planned.Version {
			return refuse(registry.RefusalPlanMismatch, "the released set does not carry "+memberName(planned))
		}
		if p.config.Stored == nil {
			continue
		}
		digest, stored := p.config.Stored(string(planned.Ecosystem), planned.Coordinate, planned.Version)
		switch {
		case !stored:
			return refuse(registry.RefusalArtifactMissing, memberName(planned)+" has no artifact in its registry")
		case digest != set.Members[index].ArtifactDigest:
			return refuse(registry.RefusalArtifactDigestMismatch, memberName(planned)+" is stored at another digest")
		}
	}
	return nil
}

// advanceLocked applies the release transaction: every channel moves, or, on
// one compare-and-swap miss, none does.
func (p *Provider) advanceLocked(request distribution.ReleaseRequest, set distribution.ReleaseSet, ref distribution.ReleaseSetRef) distribution.ReleaseResponse {
	outcome := distribution.ReleaseOutcomeAlreadyCurrent
	for _, channel := range request.Channels {
		current, exists := p.heads[channelKey(request.Namespace, channel.Name)]
		switch {
		case exists && current == ref:
		case channel.Expected == nil && !exists, channel.Expected != nil && exists && current == *channel.Expected:
			if outcome != distribution.ReleaseOutcomeConflict {
				outcome = distribution.ReleaseOutcomeReleased
			}
		default:
			outcome = distribution.ReleaseOutcomeConflict
		}
	}
	if outcome != distribution.ReleaseOutcomeConflict {
		p.sets[ref.ID] = set
		for _, channel := range request.Channels {
			key := channelKey(request.Namespace, channel.Name)
			if p.heads[key] != ref {
				p.heads[key] = ref
				p.generations[key]++
			}
		}
	}
	response := distribution.ReleaseResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Outcome:         outcome,
		Current:         make(map[string]*distribution.ChannelHead, len(request.Channels)),
	}
	for _, channel := range request.Channels {
		response.Current[channel.Name] = p.headLocked(request.Namespace, channel.Name, false)
	}
	return response
}

// SetHead stores set and moves channel to it, as another writer would, and
// returns its ref.
func (p *Provider) SetHead(namespace, channel string, set distribution.ReleaseSet) (distribution.ReleaseSetRef, error) {
	normalized := distribution.NormalizeReleaseSet(&set)
	ref, diagnostics := distribution.DeriveReleaseSetRef(normalized)
	if diag.HasErrors(diagnostics) {
		return ref, fmt.Errorf("providertest: the set has no ref: %v", diag.Errors(diagnostics))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := channelKey(namespace, channel)
	p.sets[ref.ID] = *normalized
	p.heads[key] = ref
	p.generations[key]++
	return ref, nil
}

// Head returns the head of channel, and false when it has none.
func (p *Provider) Head(namespace, channel string) (distribution.ReleaseSetRef, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ref, ok := p.heads[channelKey(namespace, channel)]
	return ref, ok
}

// ReleaseSet returns the stored set ref names.
func (p *Provider) ReleaseSet(ref distribution.ReleaseSetRef) (distribution.ReleaseSet, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	set, ok := p.sets[ref.ID]
	return set, ok
}

// Calls returns every request read so far, in order.
func (p *Provider) Calls() []Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// CallsOf returns the requests of op read so far, in order.
func (p *Provider) CallsOf(op registry.CredentialOp) []Call {
	var calls []Call
	for _, call := range p.Calls() {
		if call.Op == op {
			calls = append(calls, call)
		}
	}
	return calls
}

// Offered returns the capabilities each initialize offered, in order.
func (p *Provider) Offered() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.offered)
}

// Opens returns every plan a session opened, in order. An open of the plan
// already open is not listed again.
func (p *Provider) Opens() []registry.OpenParams {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.opens)
}

// Releases counts the release transactions applied: one per plan digest.
func (p *Provider) Releases() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.releases
}

// headLocked returns channel's head, with its set when withSet, or nil.
func (p *Provider) headLocked(namespace, channel string, withSet bool) *distribution.ChannelHead {
	key := channelKey(namespace, channel)
	ref, exists := p.heads[key]
	if !exists {
		return nil
	}
	head := &distribution.ChannelHead{Ref: ref, Generation: p.generations[key]}
	if withSet {
		set := p.sets[ref.ID]
		head.ReleaseSet = distribution.NormalizeReleaseSet(&set)
	}
	return head
}

// notForward refuses an ancestry that states a channel head which is not the
// source revision or one of its ancestors.
func notForward(ancestry registry.PublicationAncestry) *registry.CredentialRefusal {
	for _, channel := range ancestry.Channels {
		if channel.HeadSourceRevision != "" && !channel.Ancestor {
			return refuse(registry.RefusalNotForward, "the head of channel "+channel.Name+" is not an ancestor of the source revision")
		}
	}
	return nil
}

func refuse(code, message string) *registry.CredentialRefusal {
	return &registry.CredentialRefusal{Code: code, Message: message}
}

func memberName(member registry.PublicationPlanMember) string {
	return strings.Join([]string{string(member.Ecosystem), member.Coordinate}, "/") + "@" + member.Version
}

func channelKey(namespace, channel string) string { return namespace + "\x00" + channel }
