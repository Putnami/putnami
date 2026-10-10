package deliverycli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	registry "go.putnami.dev/protocol/registry"
)

const (
	pubNamespace = "acme"
	pubCanary    = "canary"
	pubImmutable = "v1.2.3"
	pubNPMOrigin = "https://npm.putnami.dev"
	pubIngestAPI = "/v1/delivery/records"
)

var (
	pubSource       = strings.Repeat("a", 40)
	pubParentSource = strings.Repeat("b", 40)
	pubOtherSource  = strings.Repeat("c", 40)
)

func pubDigest(fill string) string { return "sha256:" + strings.Repeat(fill, 64) }

func pubMember(ecosystem, coordinate, version, source, digest, fingerprint string) distribution.ReleaseSetMember {
	return distribution.ReleaseSetMember{
		Ecosystem: distribution.Ecosystem(ecosystem), Coordinate: coordinate, Version: version,
		ArtifactDigest: pubDigest(digest), Dependencies: []distribution.ReleaseSetDependency{},
		SourceRevision: source, SelectionFingerprint: pubDigest(fingerprint),
	}
}

// pubInherited is the member the base head carries and the plan does not
// publish: a release carries it unchanged.
func pubInherited() distribution.ReleaseSetMember {
	return pubMember("go", "go.acme.dev/lib", "v1.0.0", pubParentSource, "1", "2")
}

func pubBaseSet() distribution.ReleaseSet {
	return distribution.ReleaseSet{ProtocolVersion: distribution.ProtocolVersion, Namespace: pubNamespace, Members: []distribution.ReleaseSetMember{
		pubInherited(),
		pubMember("npm", "@acme/web", "1.2.2", pubParentSource, "3", "4"),
		pubMember("put", "acme/app", "1.2.2", pubParentSource, "5", "6"),
	}}
}

func pubReleasedSet() distribution.ReleaseSet {
	return distribution.ReleaseSet{ProtocolVersion: distribution.ProtocolVersion, Namespace: pubNamespace, Members: []distribution.ReleaseSetMember{
		pubInherited(),
		pubMember("npm", "@acme/web", "1.2.3", pubSource, "7", "8"),
		pubMember("put", "acme/app", "1.2.3", pubSource, "9", "a"),
	}}
}

func pubRef(t *testing.T, set distribution.ReleaseSet) distribution.ReleaseSetRef {
	t.Helper()
	ref, diagnostics := distribution.DeriveReleaseSetRef(&set)
	if diag.HasErrors(diagnostics) {
		t.Fatalf("derive ref: %s", diag.ErrorText(diagnostics))
	}
	return ref
}

func pubSeal(t *testing.T, plan registry.PublicationPlan) registry.PublicationPlan {
	t.Helper()
	plan.PlanDigest = ""
	digest, err := registry.PlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.PlanDigest = digest
	return plan
}

func pubPlan(t *testing.T) registry.PublicationPlan {
	t.Helper()
	return pubSeal(t, registry.PublicationPlan{
		ProtocolVersion: registry.PublicationPlanProtocolVersion, Namespace: pubNamespace, SourceRevision: pubSource,
		Channels: []string{pubCanary, pubImmutable}, ImmutableChannel: pubImmutable,
		Members: []registry.PublicationPlanMember{
			{Ecosystem: "npm", Coordinate: "@acme/web", Version: "1.2.3", SourceRevision: pubSource, SelectionFingerprint: pubDigest("8")},
			{Ecosystem: "put", Coordinate: "acme/app", Version: "1.2.3", SourceRevision: pubSource, SelectionFingerprint: pubDigest("a")},
		},
	})
}

func pubAncestry() registry.PublicationAncestry {
	return registry.PublicationAncestry{SourceRevision: pubSource, SnapshotCommits: 12, Channels: []registry.PublicationChannelAncestry{
		{Name: pubCanary, HeadSourceRevision: pubParentSource, Ancestor: true},
		{Name: pubImmutable},
	}}
}

func pubOpenParams(t *testing.T) registry.OpenParams {
	t.Helper()
	return registry.OpenParams{Plan: pubPlan(t), Ancestry: pubAncestry()}
}

func pubReleaseRequest(expected *distribution.ReleaseSetRef) distribution.ReleaseRequest {
	return distribution.ReleaseRequest{
		ProtocolVersion: distribution.ProtocolVersion, Namespace: pubNamespace, ReleaseSet: pubReleasedSet(),
		Channels: []distribution.ChannelRequest{
			{Name: pubCanary, Expected: expected, Visibility: distribution.VisibilityPrivate},
			{Name: pubImmutable, Visibility: distribution.VisibilityPrivate, Immutable: true},
		},
		Visibility: distribution.VisibilityChain{Repo: distribution.VisibilityPrivate},
	}
}

// pubRelease is one release payload: the request bytes the engine sends, and
// the request they parse to.
type pubRelease struct {
	planDigest string
	request    distribution.ReleaseRequest
	ancestry   registry.PublicationAncestry
}

func (r *pubRig) defaultRelease() pubRelease {
	return pubRelease{planDigest: pubPlan(r.t).PlanDigest, request: pubReleaseRequest(&r.baseRef), ancestry: pubAncestry()}
}

// payload encodes the release with its request in an order put-server's
// generated client would not produce, so a forwarded body that equals
// requestBytes was forwarded verbatim.
func (p pubRelease) payload(t *testing.T) (payload, requestBytes []byte) {
	t.Helper()
	requestBytes, err := json.Marshal(p.request)
	if err != nil {
		t.Fatal(err)
	}
	payload, err = json.Marshal(struct {
		PlanDigest string                       `json:"planDigest"`
		Request    json.RawMessage              `json:"request"`
		Ancestry   registry.PublicationAncestry `json:"ancestry"`
		Evidence   json.RawMessage              `json:"evidence"`
	}{p.planDigest, requestBytes, p.ancestry, json.RawMessage(`{"images":[],"members":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ParseReleaseParams(payload); err != nil {
		t.Fatalf("the test release does not follow the protocol: %v", err)
	}
	return payload, requestBytes
}

// pubClock is the publisher's clock; Delivery's fake mints by it too.
type pubClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *pubClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *pubClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// deliveryPlanMirror is Delivery's plan body (the delivery API's
// NativePublicationPlan), decoded the way Delivery decodes it.
type deliveryPlanMirror struct {
	ProtocolVersion  int                    `json:"protocolVersion"`
	Namespace        string                 `json:"namespace"`
	SourceRevision   string                 `json:"sourceRevision"`
	Channels         []string               `json:"channels"`
	ImmutableChannel string                 `json:"immutableChannel,omitempty"`
	Members          []deliveryMemberMirror `json:"members"`
	PlanDigest       string                 `json:"planDigest"`
}

type deliveryMemberMirror struct {
	Ecosystem            string `json:"ecosystem"`
	Coordinate           string `json:"coordinate"`
	Version              string `json:"version"`
	SourceRevision       string `json:"sourceRevision"`
	SelectionFingerprint string `json:"selectionFingerprint"`
}

// digest is Delivery's NativePublicationPlanDigest.
func (p deliveryPlanMirror) digest() string {
	body, _ := json.Marshal(struct {
		ProtocolVersion  int                    `json:"protocolVersion"`
		Namespace        string                 `json:"namespace"`
		SourceRevision   string                 `json:"sourceRevision"`
		Channels         []string               `json:"channels"`
		ImmutableChannel string                 `json:"immutableChannel,omitempty"`
		Members          []deliveryMemberMirror `json:"members"`
	}{p.ProtocolVersion, p.Namespace, p.SourceRevision, p.Channels, p.ImmutableChannel, p.Members})
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fakeDelivery serves Delivery's native publication routes. Every request is
// checked against the contract; the hooks answer in place of the model when
// they return true.
type fakeDelivery struct {
	t     *testing.T
	clock *pubClock
	url   string

	mu             sync.Mutex
	namespace      string
	channels       []string
	immutable      string
	forwardOnly    []string
	expiresIn      int64
	frozen         string
	planningCalls  int
	publishCalls   int
	onPlanning     func(call int, w http.ResponseWriter) bool
	onPublish      func(call int, w http.ResponseWriter, plan deliveryPlanMirror) bool
	publishStarted chan struct{}
	// advance and advanceHead are the advance answer: whether the run is
	// still the head of its default branch, and the newer head when not.
	advance      bool
	advanceHead  string
	advanceCalls int
	onAdvance    func(call int, w http.ResponseWriter) bool
}

func newFakeDelivery(t *testing.T, clock *pubClock) *fakeDelivery {
	d := &fakeDelivery{
		t: t, clock: clock, namespace: pubNamespace, channels: []string{pubCanary, pubImmutable},
		immutable: pubImmutable, forwardOnly: []string{pubCanary}, expiresIn: 300, advance: true,
	}
	srv := httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(srv.Close)
	d.url = srv.URL + pubIngestAPI
	return d
}

func (d *fakeDelivery) set(fn func(d *fakeDelivery)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d)
}

func (d *fakeDelivery) calls() (planning, publish int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.planningCalls, d.publishCalls
}

func (d *fakeDelivery) advances() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.advanceCalls
}

func (d *fakeDelivery) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		d.t.Errorf("Delivery request method = %s, want POST", r.Method)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+testRunCredential {
		d.t.Errorf("Delivery authorization = %q, want the run credential", got)
	}
	if got := r.Header.Get(channelAdvanceHeader); got != channelAdvanceForwardOnly {
		d.t.Errorf("Delivery %s = %q, want %s", channelAdvanceHeader, got, channelAdvanceForwardOnly)
	}
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Path {
	case pubIngestAPI + nativePublicationPlanningPath:
		if string(body) != "{}" {
			d.t.Errorf("planning body = %q, want {}", body)
		}
		d.planning(w)
	case pubIngestAPI + nativePublicationPublishPath:
		d.publish(w, body)
	case pubIngestAPI + nativePublicationAdvancePath:
		if string(body) != "{}" {
			d.t.Errorf("advance body = %q, want {}", body)
		}
		d.answerAdvance(w)
	default:
		d.t.Errorf("Delivery request path = %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (d *fakeDelivery) planning(w http.ResponseWriter) {
	d.mu.Lock()
	d.planningCalls++
	call, hook := d.planningCalls, d.onPlanning
	answer := map[string]any{
		"protocolVersion": 1, "workspaceId": "ws_acme", "runId": "run_42", "sourceRevision": pubSource,
		"namespace": d.namespace, "channels": d.channels, "immutableChannel": d.immutable,
		"forwardOnlyChannels": d.forwardOnly, "expiresUnix": d.clock.Now().Add(time.Hour).Unix(),
		"access_token": fmt.Sprintf("planning-bearer-%d", call), "token_type": "Bearer", "expires_in": d.expiresIn,
		"scope": "put:acme/release-set:read", "jti": fmt.Sprintf("jti-planning-%d", call),
	}
	d.mu.Unlock()
	if hook != nil && hook(call, w) {
		return
	}
	writeJSON(w, http.StatusCreated, answer)
}

func (d *fakeDelivery) answerAdvance(w http.ResponseWriter) {
	d.mu.Lock()
	d.advanceCalls++
	call, hook := d.advanceCalls, d.onAdvance
	answer := map[string]any{"protocolVersion": 1, "advance": d.advance}
	if d.advanceHead != "" {
		answer["head"] = d.advanceHead
	}
	d.mu.Unlock()
	if hook != nil && hook(call, w) {
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

func (d *fakeDelivery) publish(w http.ResponseWriter, body []byte) {
	var plan deliveryPlanMirror
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		d.t.Errorf("Delivery cannot decode the plan %s: %v", body, err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid native publication request body"})
		return
	}
	d.mu.Lock()
	d.publishCalls++
	call, hook, started := d.publishCalls, d.onPublish, d.publishStarted
	d.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if hook != nil && hook(call, w, plan) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case plan.PlanDigest != plan.digest():
		d.t.Errorf("the plan digest %s is not Delivery's digest %s", plan.PlanDigest, plan.digest())
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "native publication plan digest is invalid"})
		return
	case plan.Namespace != d.namespace || plan.SourceRevision != pubSource ||
		!slices.Equal(plan.Channels, d.channels) || plan.ImmutableChannel != d.immutable:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "native publication plan does not match server authority"})
		return
	case d.frozen != "" && d.frozen != plan.PlanDigest:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "native publication plan digest drift"})
		return
	}
	d.frozen = plan.PlanDigest
	writeJSON(w, http.StatusCreated, map[string]any{
		"protocolVersion": 1, "planDigest": plan.PlanDigest,
		"access_token": fmt.Sprintf("publish-bearer-%d", call), "token_type": "Bearer", "expires_in": d.expiresIn,
		"scope": "put:acme/app:write", "jti": fmt.Sprintf("jti-publish-%d", call),
	})
}

// fakePutServer serves put-server's release-set routes under /put with a
// compare-and-swap channel model. The hooks answer in place of the model
// when they return true.
type fakePutServer struct {
	t   *testing.T
	url string

	mu    sync.Mutex
	heads map[string]*distribution.ChannelHead
	// immutable names the channels a release created immutable: every later
	// move of one is refused, whatever the request says it is.
	immutable      map[string]bool
	resolveCalls   int
	releaseCalls   int
	resolveBodies  [][]byte
	releaseBodies  [][]byte
	resolveBearers []string
	releaseBearers []string
	onResolve      func(call int, w http.ResponseWriter, r *http.Request) bool
	onRelease      func(call int, w http.ResponseWriter, r *http.Request) bool
	releaseStarted chan struct{}
}

func newFakePutServer(t *testing.T, base distribution.ReleaseSet) *fakePutServer {
	ref := pubRef(t, base)
	p := &fakePutServer{t: t, heads: map[string]*distribution.ChannelHead{
		pubCanary: {Ref: ref, Generation: 3, ReleaseSet: &base},
	}, immutable: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(srv.Close)
	p.url = srv.URL
	return p
}

func (p *fakePutServer) set(fn func(p *fakePutServer)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(p)
}

func (p *fakePutServer) calls() (resolve, release int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resolveCalls, p.releaseCalls
}

// move makes set the head of channel at the next generation, as another
// run's release would.
func (p *fakePutServer) move(channel string, set distribution.ReleaseSet) distribution.ReleaseSetRef {
	p.t.Helper()
	ref := pubRef(p.t, set)
	p.mu.Lock()
	defer p.mu.Unlock()
	generation := uint64(1)
	if current := p.heads[channel]; current != nil {
		generation = current.Generation + 1
	}
	p.heads[channel] = &distribution.ChannelHead{Ref: ref, Generation: generation, ReleaseSet: &set}
	return ref
}

// head is the current head of channel, pointer only.
func (p *fakePutServer) head(channel string) *distribution.ChannelHead {
	p.mu.Lock()
	defer p.mu.Unlock()
	return pointerOnly(p.heads[channel])
}

func putError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": http.StatusText(status), "code": code, "message": message})
}

func (p *fakePutServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		p.t.Errorf("put-server request method = %s, want POST", r.Method)
	}
	body, _ := io.ReadAll(r.Body)
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch r.URL.Path {
	case "/put/_/release-sets/resolve":
		p.mu.Lock()
		p.resolveCalls++
		p.resolveBodies = append(p.resolveBodies, body)
		p.resolveBearers = append(p.resolveBearers, bearer)
		call, hook := p.resolveCalls, p.onResolve
		p.mu.Unlock()
		if hook != nil && hook(call, w, r) {
			return
		}
		p.resolve(w, body)
	case "/put/_/release-sets/release":
		p.mu.Lock()
		p.releaseCalls++
		p.releaseBodies = append(p.releaseBodies, body)
		p.releaseBearers = append(p.releaseBearers, bearer)
		call, hook, started := p.releaseCalls, p.onRelease, p.releaseStarted
		p.mu.Unlock()
		if started != nil {
			started <- struct{}{}
		}
		if hook != nil && hook(call, w, r) {
			return
		}
		p.release(w, body)
	default:
		p.t.Errorf("put-server request path = %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (p *fakePutServer) resolve(w http.ResponseWriter, body []byte) {
	request, diagnostics := distribution.ParseAndValidateResolveRequest(body)
	if diag.HasErrors(diagnostics) {
		putError(w, http.StatusBadRequest, "http.bad_request", diag.ErrorText(diagnostics))
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	heads := make(map[string]*distribution.ChannelHead, len(request.Channels))
	for _, channel := range request.Channels {
		heads[channel] = p.heads[channel]
	}
	writeJSON(w, http.StatusOK, distribution.ResolveResponse{ProtocolVersion: distribution.ProtocolVersion, Heads: heads})
}

func pointerOnly(head *distribution.ChannelHead) *distribution.ChannelHead {
	if head == nil {
		return nil
	}
	return &distribution.ChannelHead{Ref: head.Ref, Generation: head.Generation}
}

func (p *fakePutServer) release(w http.ResponseWriter, body []byte) {
	request, diagnostics := distribution.ParseAndValidateReleaseRequest(body)
	if diag.HasErrors(diagnostics) {
		putError(w, http.StatusBadRequest, "http.bad_request", diag.ErrorText(diagnostics))
		return
	}
	derived, _ := distribution.DeriveReleaseSetRef(&request.ReleaseSet)
	p.mu.Lock()
	defer p.mu.Unlock()
	conflict, allAt := false, true
	for _, channel := range request.Channels {
		current := p.heads[channel.Name]
		if p.immutable[channel.Name] && !channel.Immutable {
			putError(w, http.StatusConflict, "registry.release_set.channel_immutable", "the channel is immutable")
			return
		}
		if current != nil && current.Ref == derived {
			continue
		}
		allAt = false
		if channel.Immutable && current != nil {
			putError(w, http.StatusConflict, "registry.release_set.channel_immutable", "immutable channel already has a head")
			return
		}
		switch {
		case channel.Expected == nil && current == nil:
		case channel.Expected != nil && current != nil && *channel.Expected == current.Ref:
		default:
			conflict = true
		}
	}
	answer := distribution.ReleaseResponse{ProtocolVersion: distribution.ProtocolVersion, Current: map[string]*distribution.ChannelHead{}}
	switch {
	case conflict:
		answer.Outcome = distribution.ReleaseOutcomeConflict
	case allAt:
		answer.Outcome = distribution.ReleaseOutcomeAlreadyCurrent
	default:
		answer.Outcome = distribution.ReleaseOutcomeReleased
		set := request.ReleaseSet
		for _, channel := range request.Channels {
			current := p.heads[channel.Name]
			if current != nil && current.Ref == derived {
				continue
			}
			generation := uint64(1)
			if current != nil {
				generation = current.Generation + 1
			}
			p.heads[channel.Name] = &distribution.ChannelHead{Ref: derived, Generation: generation, ReleaseSet: &set}
			if channel.Immutable {
				p.immutable[channel.Name] = true
			}
		}
	}
	for _, channel := range request.Channels {
		answer.Current[channel.Name] = pointerOnly(p.heads[channel.Name])
	}
	writeJSON(w, http.StatusOK, answer)
}

// pubRig is one hosted session with publication-v1 negotiated, between the
// engine (the test) and fakes of Delivery and put-server.
type pubRig struct {
	t        *testing.T
	clock    *pubClock
	delivery *fakeDelivery
	put      *fakePutServer
	h        *credentialHarness
	baseRef  distribution.ReleaseSetRef
	id       int
}

// newPubRig starts the fakes and a provider that serves publication-v1;
// configure adjusts the server before it starts.
func newPubRig(t *testing.T, configure func(server *credentialServer)) *pubRig {
	t.Helper()
	clock := &pubClock{now: time.Now().UTC().Truncate(time.Second)}
	r := &pubRig{t: t, clock: clock, delivery: newFakeDelivery(t, clock), put: newFakePutServer(t, pubBaseSet())}
	r.baseRef = pubRef(t, pubBaseSet())
	server := newTestCredentialServer(r.delivery.url, nil, nil)
	server.publication = publicationEndpointsFrom(map[string]string{publishPutURLEnv: r.put.url, publishNPMURLEnv: pubNPMOrigin})
	server.configurePublisher = func(p *publisher) {
		p.now = clock.Now
		p.retryWait = 0
		p.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	}
	if configure != nil {
		configure(server)
	}
	r.h = runCredentialServer(t, server)
	return r
}

func publicationInitializeLine(id int, runCredential string) string {
	payload := map[string]any{"protocolVersion": 1, "capabilities": []string{registry.CapabilityCredentialV1, registry.CapabilityPublicationV1}}
	if runCredential != "" {
		payload["runCredential"] = runCredential
	}
	encoded, _ := json.Marshal(map[string]any{"protocolVersion": 1, "id": id, "op": "initialize", "payload": payload})
	return string(encoded)
}

// initialize offers both capabilities and returns the echo.
func (r *pubRig) initialize(runCredential string) []string {
	r.t.Helper()
	r.id++
	r.h.send(publicationInitializeLine(r.id, runCredential))
	response := r.h.next(registry.CredentialOpInitialize)
	result, err := registry.ParseCredentialInitializeResult(response.Payload)
	if err != nil || !response.OK {
		r.t.Fatalf("initialize = %+v (%v)", response, err)
	}
	offered := []string{registry.CapabilityCredentialV1, registry.CapabilityPublicationV1}
	r.h.negotiated = registry.NegotiatedCapabilities(offered, result.Capabilities)
	return result.Capabilities
}

// start initializes a hosted session and requires the publication-v1 echo.
func (r *pubRig) start() *pubRig {
	r.t.Helper()
	if echo := r.initialize(testRunCredential); !slices.Contains(echo, registry.CapabilityPublicationV1) {
		r.t.Fatalf("echo = %v, want publication-v1; stderr %s", echo, r.h.stderr.String())
	}
	return r
}

func (r *pubRig) op(op registry.CredentialOp, payload any) *registry.CredentialResponse {
	r.t.Helper()
	r.send(op, payload)
	return r.h.next(op)
}

func (r *pubRig) send(op registry.CredentialOp, payload any) int {
	r.t.Helper()
	r.id++
	encoded, err := json.Marshal(map[string]any{"protocolVersion": 1, "id": r.id, "op": op, "payload": payload})
	if err != nil {
		r.t.Fatal(err)
	}
	r.h.send(string(encoded))
	return r.id
}

func pubResolveRequest(namespace string, channels ...string) distribution.ResolveRequest {
	return distribution.ResolveRequest{ProtocolVersion: distribution.ProtocolVersion, Namespace: namespace, Channels: channels}
}

func (r *pubRig) resolve(request distribution.ResolveRequest) *registry.CredentialResponse {
	r.t.Helper()
	return r.op(registry.CredentialOpResolve, registry.ResolveParams{Request: request})
}

func (r *pubRig) open(params registry.OpenParams) *registry.CredentialResponse {
	r.t.Helper()
	return r.op(registry.CredentialOpOpen, params)
}

func (r *pubRig) release(payload []byte) *registry.CredentialResponse {
	r.t.Helper()
	return r.op(registry.CredentialOpRelease, json.RawMessage(payload))
}

func (r *pubRig) publishCredential() *registry.CredentialResponse {
	r.t.Helper()
	r.id++
	r.h.send(credentialLine(r.id, registry.PurposePublish))
	return r.h.next(registry.CredentialOpCredential)
}

// collect reads n answers to op, by request id.
func (r *pubRig) collect(n int, op registry.CredentialOp) map[int64]*registry.CredentialResponse {
	r.t.Helper()
	answers := make(map[int64]*registry.CredentialResponse, n)
	for range n {
		response := r.h.next(op)
		answers[response.ID] = response
	}
	return answers
}

// opened resolves both plan channels and opens the default plan.
func (r *pubRig) opened() *pubRig {
	r.t.Helper()
	mustResolved(r.t, r.resolve(pubResolveRequest(pubNamespace, pubCanary, pubImmutable)))
	mustOpened(r.t, r.open(pubOpenParams(r.t)), pubPlan(r.t).PlanDigest)
	return r
}

// noSecret fails when stderr or any text carries the run credential or a
// bearer Delivery minted.
func (r *pubRig) noSecret(texts ...string) {
	r.t.Helper()
	for _, text := range append(texts, r.h.stderr.String()) {
		for _, secret := range []string{testRunCredential, "planning-bearer-", "publish-bearer-"} {
			if strings.Contains(text, secret) {
				r.t.Fatalf("%q carries a secret", text)
			}
		}
	}
}

func mustResolved(t *testing.T, response *registry.CredentialResponse) *distribution.ResolveResponse {
	t.Helper()
	if !response.OK {
		t.Fatalf("resolve refused: %+v", response.Error)
	}
	result, err := registry.ParseResolveResult(response.Payload)
	if err != nil {
		t.Fatalf("resolve result: %v", err)
	}
	return &result.Response
}

func mustOpened(t *testing.T, response *registry.CredentialResponse, digest string) {
	t.Helper()
	if !response.OK {
		t.Fatalf("open refused: %+v", response.Error)
	}
	result, err := registry.ParseOpenResult(response.Payload)
	if err != nil {
		t.Fatalf("open result: %v", err)
	}
	if result.PlanDigest != digest {
		t.Fatalf("open planDigest = %s, want the plan's %s", result.PlanDigest, digest)
	}
}

func mustReleased(t *testing.T, response *registry.CredentialResponse, requestBytes []byte) *distribution.ReleaseResponse {
	t.Helper()
	if !response.OK {
		t.Fatalf("release refused: %+v", response.Error)
	}
	result, err := registry.ParseReleaseResult(response.Payload)
	if err != nil {
		t.Fatalf("release result: %v", err)
	}
	request, diagnostics := distribution.ParseAndValidateReleaseRequest(requestBytes)
	if diag.HasErrors(diagnostics) {
		t.Fatalf("release request: %s", diag.ErrorText(diagnostics))
	}
	if diagnostics := distribution.ValidateReleaseExchange(request, &result.Response); diag.HasErrors(diagnostics) {
		t.Fatalf("the release answer fails the engine's exchange check: %s", diag.ErrorText(diagnostics))
	}
	return &result.Response
}

func TestPublicationEchoNeedsAnOfferAHostedRunDeliveryAndOrigins(t *testing.T) {
	cases := []struct {
		name          string
		runCredential string
		offer         bool
		configure     func(server *credentialServer)
		stderr        string
		// quiet requires stderr to say nothing about publication-v1.
		quiet bool
	}{
		{name: "hosted run with origins", runCredential: testRunCredential, offer: true},
		{name: "no run credential", offer: true},
		{name: "publication-v1 not offered", runCredential: testRunCredential},
		{name: "no ingest base", runCredential: testRunCredential, offer: true, stderr: SessionReporterIngestURLEnv,
			configure: func(server *credentialServer) { server.caller = newCapabilityCaller("") }},
		// A run the launcher gave no publication origin publishes nothing:
		// saying why publication-v1 is not served would be noise.
		{name: "no publication origin", runCredential: testRunCredential, offer: true, quiet: true,
			configure: func(server *credentialServer) { server.publication = publicationEndpointsFrom(map[string]string{}) }},
		{name: "no publication origin and no ingest base", runCredential: testRunCredential, offer: true, quiet: true,
			configure: func(server *credentialServer) {
				server.caller = newCapabilityCaller("")
				server.publication = publicationEndpointsFrom(map[string]string{})
			}},
		{name: "no Put origin", runCredential: testRunCredential, offer: true, stderr: publishPutURLEnv,
			configure: func(server *credentialServer) {
				server.publication = publicationEndpointsFrom(map[string]string{publishNPMURLEnv: pubNPMOrigin})
			}},
		{name: "invalid npm origin", runCredential: testRunCredential, offer: true, stderr: publishNPMURLEnv,
			configure: func(server *credentialServer) {
				server.publication = publicationEndpointsFrom(map[string]string{publishPutURLEnv: "https://put.putnami.dev", publishNPMURLEnv: "http://npm.putnami.dev"})
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPubRig(t, tc.configure)
			var echo []string
			if tc.offer {
				echo = r.initialize(tc.runCredential)
			} else {
				r.id++
				r.h.send(initializeLine(r.id, tc.runCredential))
				result, err := registry.ParseCredentialInitializeResult(r.h.next(registry.CredentialOpInitialize).Payload)
				if err != nil {
					t.Fatal(err)
				}
				echo = result.Capabilities
				r.h.negotiated = registry.NegotiatedCapabilities([]string{registry.CapabilityCredentialV1}, echo)
			}
			serves := tc.name == "hosted run with origins"
			if got := slices.Contains(echo, registry.CapabilityPublicationV1); got != serves {
				t.Fatalf("echo = %v, publication-v1 served = %v, want %v", echo, got, serves)
			}
			if tc.stderr != "" && !strings.Contains(r.h.stderr.String(), tc.stderr) {
				t.Fatalf("stderr = %q, want why publication-v1 is not served (%s)", r.h.stderr.String(), tc.stderr)
			}
			if tc.quiet && strings.Contains(r.h.stderr.String(), "publication-v1") {
				t.Fatalf("stderr = %q, want nothing about publication-v1", r.h.stderr.String())
			}
			if serves {
				return
			}
			// Without the echo: a publication op is outside the session, and
			// the publish purpose keeps its own credential path.
			// The engine reads no answer to an op it did not negotiate; this
			// one is parsed as a credential-v1 refusal.
			r.send(registry.CredentialOpResolve, registry.ResolveParams{Request: pubResolveRequest(pubNamespace, pubCanary)})
			refusal := mustRefusal(t, r.h.next(registry.CredentialOpCredential), refusalInvalidRequest)
			if !strings.Contains(refusal.Message, "did not negotiate") {
				t.Fatalf("refusal = %q, want the negotiation reason", refusal.Message)
			}
			if credential := mustCredential(t, r.publishCredential()); credential != nil {
				t.Fatalf("publish credential = %+v, want absence", credential)
			}
			if planning, publish := r.delivery.calls(); planning+publish != 0 {
				t.Fatalf("Delivery calls = %d, %d, want none", planning, publish)
			}
		})
	}
}

func TestPublicationReleasesThePlanThroughDeliveryAndPutServer(t *testing.T) {
	r := newPubRig(t, nil).start()

	resolveRequest := pubResolveRequest(pubNamespace, pubCanary, pubImmutable)
	resolved := mustResolved(t, r.resolve(resolveRequest))
	if diagnostics := distribution.ValidateResolveExchange(&resolveRequest, resolved); diag.HasErrors(diagnostics) {
		t.Fatalf("the resolve answer fails the engine's exchange check: %s", diag.ErrorText(diagnostics))
	}
	if head := resolved.Heads[pubCanary]; head == nil || head.Ref != r.baseRef {
		t.Fatalf("canary head = %+v, want the base set", head)
	}
	if head, answered := resolved.Heads[pubImmutable]; !answered || head != nil {
		t.Fatalf("immutable head = %+v (answered %v), want no head", head, answered)
	}

	mustRefusal(t, r.publishCredential(), registry.RefusalPlanNotOpen)

	plan := pubPlan(t)
	mustOpened(t, r.open(pubOpenParams(t)), plan.PlanDigest)
	credential := mustCredential(t, r.publishCredential())
	putHost := strings.TrimPrefix(r.put.url, "http://")
	if credential == nil || credential.Bearer != "publish-bearer-1" ||
		!reflect.DeepEqual(credential.Hosts, []string{putHost, "npm.putnami.dev"}) {
		t.Fatalf("publish credential = %+v, want the plan's write bearer for the publication origins", credential)
	}
	if want := r.clock.Now().Add(300 * time.Second).Format(time.RFC3339); credential.ExpiresAt != want {
		t.Fatalf("publish credential expiresAt = %s, want %s", credential.ExpiresAt, want)
	}

	payload, requestBytes := r.defaultRelease().payload(t)
	released := mustReleased(t, r.release(payload), requestBytes)
	if released.Outcome != distribution.ReleaseOutcomeReleased {
		t.Fatalf("release outcome = %s, want released", released.Outcome)
	}

	// Delivery said the run is the head of its default branch, and the
	// channels were read again before the move. The canary head had not
	// moved, so the release goes to put-server verbatim, under a write bearer
	// minted for it; both resolves went under the planning bearer.
	r.put.set(func(p *fakePutServer) {
		if len(p.releaseBodies) != 1 || !bytes.Equal(p.releaseBodies[0], requestBytes) {
			t.Fatalf("put-server release body = %s, want the engine's bytes %s", p.releaseBodies, requestBytes)
		}
		if len(p.resolveBodies) != 2 || !bytes.Equal(p.resolveBodies[0], mustJSON(t, resolveRequest)) || !bytes.Equal(p.resolveBodies[1], mustJSON(t, resolveRequest)) {
			t.Fatalf("put-server resolve bodies = %s, want the engine's and the release's read of the same channels", p.resolveBodies)
		}
		if !reflect.DeepEqual(p.resolveBearers, []string{"planning-bearer-1", "planning-bearer-1"}) || !reflect.DeepEqual(p.releaseBearers, []string{"publish-bearer-2"}) {
			t.Fatalf("put-server bearers = %v, %v", p.resolveBearers, p.releaseBearers)
		}
	})
	if planning, publish := r.delivery.calls(); planning != 1 || publish != 2 || r.delivery.advances() != 1 {
		t.Fatalf("Delivery calls = planning %d, publish %d, advance %d, want 1, 2 and 1", planning, publish, r.delivery.advances())
	}

	// The same release again gets the first answer; nothing is sent again.
	again := mustReleased(t, r.release(payload), requestBytes)
	if !reflect.DeepEqual(again, released) {
		t.Fatalf("repeated release = %+v, want %+v", again, released)
	}
	if _, releases := r.put.calls(); releases != 1 {
		t.Fatalf("put-server releases = %d, want 1", releases)
	}
	if _, publish := r.delivery.calls(); publish != 2 || r.delivery.advances() != 1 {
		t.Fatalf("Delivery publish calls = %d, advance calls = %d, want 2 and 1", publish, r.delivery.advances())
	}
	r.noSecret()
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestPublicationOpenIsIdempotentAndRefusesAnotherPlan(t *testing.T) {
	r := newPubRig(t, nil).start().opened()
	mustOpened(t, r.open(pubOpenParams(t)), pubPlan(t).PlanDigest)
	if _, publish := r.delivery.calls(); publish != 1 {
		t.Fatalf("Delivery publish calls = %d, want 1", publish)
	}
	other := pubOpenParams(t)
	other.Plan.Members[0].Version = "1.2.4"
	other.Plan = pubSeal(t, other.Plan)
	mustRefusal(t, r.open(other), registry.RefusalPlanAlreadyOpen)
	if _, publish := r.delivery.calls(); publish != 1 {
		t.Fatalf("Delivery publish calls = %d, want 1", publish)
	}
}

func TestPublicationOpensOnePlanAtATime(t *testing.T) {
	r := newPubRig(t, nil).start()
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	r.delivery.set(func(d *fakeDelivery) {
		d.publishStarted = started
		d.onPublish = func(int, http.ResponseWriter, deliveryPlanMirror) bool { <-release; return false }
	})
	first := r.send(registry.CredentialOpOpen, pubOpenParams(t))
	<-started
	second := r.send(registry.CredentialOpOpen, pubOpenParams(t))
	other := pubOpenParams(t)
	other.Plan.Members[0].Version = "1.2.4"
	other.Plan = pubSeal(t, other.Plan)
	third := r.send(registry.CredentialOpOpen, other)
	mustRefusal(t, r.h.next(registry.CredentialOpOpen), registry.RefusalPlanAlreadyOpen)
	time.Sleep(50 * time.Millisecond) // let the second open join the first
	close(release)
	answers := r.collect(2, registry.CredentialOpOpen)
	for _, id := range []int{first, second} {
		mustOpened(t, answers[int64(id)], pubPlan(t).PlanDigest)
	}
	if _, ok := answers[int64(third)]; ok {
		t.Fatal("the other plan was answered twice")
	}
	if _, publish := r.delivery.calls(); publish != 1 {
		t.Fatalf("Delivery publish calls = %d, want 1", publish)
	}
}

func TestPublicationOpenRefusesAPlanOutsideTheRunAuthority(t *testing.T) {
	withPlan := func(edit func(*registry.OpenParams)) func(*testing.T) registry.OpenParams {
		return func(t *testing.T) registry.OpenParams {
			params := pubOpenParams(t)
			edit(&params)
			params.Plan = pubSeal(t, params.Plan)
			return params
		}
	}
	cases := []struct {
		name    string
		params  func(*testing.T) registry.OpenParams
		resolve bool
		setup   func(r *pubRig)
		code    string
	}{
		{name: "namespace", code: registry.RefusalNamespaceForbidden, params: withPlan(func(p *registry.OpenParams) {
			p.Plan.Namespace = "other"
		})},
		{name: "commit", code: registry.RefusalPlanMismatch, params: withPlan(func(p *registry.OpenParams) {
			p.Plan.SourceRevision, p.Ancestry.SourceRevision = pubOtherSource, pubOtherSource
			for i := range p.Plan.Members {
				p.Plan.Members[i].SourceRevision = pubOtherSource
			}
		})},
		{name: "channels", code: registry.RefusalPlanMismatch, params: withPlan(func(p *registry.OpenParams) {
			p.Plan.Channels, p.Plan.ImmutableChannel = []string{pubCanary}, ""
			p.Ancestry.Channels = p.Ancestry.Channels[:1]
		})},
		{name: "immutable channel", code: registry.RefusalPlanMismatch, params: withPlan(func(p *registry.OpenParams) {
			p.Plan.ImmutableChannel = ""
		})},
		// The engine resolves no tag channel; this provider reads it at
		// open, and a plan it cannot read it for does not open.
		{name: "put-server cannot read the immutable channel", code: refusalPublicationUnavailable, params: pubOpenParams, setup: func(r *pubRig) {
			r.put.set(func(p *fakePutServer) {
				p.onResolve = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
					w.WriteHeader(http.StatusBadGateway)
					return true
				}
			})
		}},
		{name: "Delivery refuses the plan", code: registry.RefusalPlanMismatch, params: pubOpenParams, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onPublish = func(_ int, w http.ResponseWriter, _ deliveryPlanMirror) bool {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "native publication member is invalid"})
					return true
				}
			})
		}},
		{name: "Delivery froze another plan", code: registry.RefusalPlanAlreadyOpen, params: pubOpenParams, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) { d.frozen = pubDigest("f") })
		}},
		// Delivery refuses a superseded run only to a caller that does not
		// declare forward-only advance. Were it to, the open is refused, but
		// never as not_forward: this provider does not answer that code.
		{name: "Delivery refuses the run as superseded", code: refusalPublicationRefused, params: pubOpenParams, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onPublish = func(_ int, w http.ResponseWriter, _ deliveryPlanMirror) bool {
					writeJSON(w, http.StatusConflict, map[string]string{"error": "superseded", "message": "superseded", "code": "native_publication_superseded"})
					return true
				}
			})
		}},
		{name: "Delivery refuses the run", code: refusalPublicationRefused, params: pubOpenParams, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onPublish = func(_ int, w http.ResponseWriter, _ deliveryPlanMirror) bool {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "denied for " + testRunCredential, "code": "subscription_inactive"})
					return true
				}
			})
		}},
		{name: "Delivery unavailable", code: refusalPublicationUnavailable, params: pubOpenParams, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onPublish = func(_ int, w http.ResponseWriter, _ deliveryPlanMirror) bool {
					w.WriteHeader(http.StatusBadGateway)
					return true
				}
			})
		}},
		{name: "Delivery answers no capability", code: refusalPublicationUnavailable, params: pubOpenParams, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onPublish = func(_ int, w http.ResponseWriter, _ deliveryPlanMirror) bool {
					w.WriteHeader(http.StatusNoContent)
					return true
				}
			})
		}},
		{name: "Delivery grant for another plan", code: refusalPublicationUnavailable, params: pubOpenParams, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onPublish = func(_ int, w http.ResponseWriter, _ deliveryPlanMirror) bool {
					writeJSON(w, http.StatusCreated, map[string]any{"protocolVersion": 1, "planDigest": pubDigest("f"),
						"access_token": "publish-bearer-x", "token_type": "Bearer", "expires_in": 300, "scope": "s", "jti": "j"})
					return true
				}
			})
		}},
		{name: "Delivery refuses the planning", code: refusalPublicationRefused, params: pubOpenParams, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onPlanning = func(_ int, w http.ResponseWriter) bool {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "no publication for this run"})
					return true
				}
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPubRig(t, nil).start()
			if tc.setup != nil {
				tc.setup(r)
			}
			if tc.resolve {
				mustResolved(t, r.resolve(pubResolveRequest(pubNamespace, pubCanary, pubImmutable)))
			}
			refusal := mustRefusal(t, r.open(tc.params(t)), tc.code)
			r.noSecret(refusal.Message)
			// A refused open leaves no plan open.
			mustRefusal(t, r.publishCredential(), registry.RefusalPlanNotOpen)
		})
	}
}

// TestPublicationOpenDoesNotJudgeTheAncestry pins D18 at open: a forward-only
// channel whose head the commit does not descend from no longer refuses the
// plan. The run opens it, so its artifacts are uploaded, and Delivery decides
// at release whether the channel moves.
func TestPublicationOpenDoesNotJudgeTheAncestry(t *testing.T) {
	r := newPubRig(t, nil).start()
	mustResolved(t, r.resolve(pubResolveRequest(pubNamespace, pubCanary, pubImmutable)))
	params := pubOpenParams(t)
	params.Ancestry.Channels[0] = registry.PublicationChannelAncestry{Name: pubCanary, HeadSourceRevision: pubOtherSource}
	mustOpened(t, r.open(params), params.Plan.PlanDigest)
	if credential := mustCredential(t, r.publishCredential()); credential == nil {
		t.Fatal("publish credential absent, want the plan's write bearer for the uploads")
	}

	release := r.defaultRelease()
	release.ancestry = params.Ancestry
	payload, requestBytes := release.payload(t)
	if answer := mustReleased(t, r.release(payload), requestBytes); answer.Outcome != distribution.ReleaseOutcomeReleased {
		t.Fatalf("release = %+v, want released: the run is the head of its default branch", answer)
	}
}

func TestPublicationResolveRefusals(t *testing.T) {
	failing := func(status int, code string) func(r *pubRig) {
		return func(r *pubRig) {
			r.put.set(func(p *fakePutServer) {
				p.onResolve = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
					putError(w, status, code, "refused")
					return true
				}
			})
		}
	}
	answering := func(answer func(r *pubRig) any) func(r *pubRig) {
		return func(r *pubRig) {
			r.put.set(func(p *fakePutServer) {
				p.onResolve = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
					writeJSON(w, http.StatusOK, answer(r))
					return true
				}
			})
		}
	}
	cases := []struct {
		name      string
		namespace string
		setup     func(r *pubRig)
		code      string
		resolves  int
	}{
		{name: "another namespace", namespace: "other", code: registry.RefusalNamespaceForbidden},
		{name: "put-server refuses the bearer", setup: failing(http.StatusUnauthorized, "unauthorized"), code: registry.RefusalNamespaceForbidden, resolves: 1},
		{name: "put-server forbids the namespace", setup: failing(http.StatusForbidden, "forbidden"), code: registry.RefusalNamespaceForbidden, resolves: 1},
		{name: "put-server refuses the request", setup: failing(http.StatusBadRequest, "http.bad_request"), code: refusalPublicationRefused, resolves: 1},
		{name: "put-server unavailable", setup: failing(http.StatusInternalServerError, "http.internal_server"), code: refusalPublicationUnavailable, resolves: 2},
		{name: "put-server rate limits", setup: failing(http.StatusTooManyRequests, "rate_limited"), code: refusalPublicationUnavailable, resolves: 2},
		{name: "an answer missing a channel", code: refusalPublicationUnavailable, resolves: 1, setup: answering(func(*pubRig) any {
			return distribution.ResolveResponse{ProtocolVersion: distribution.ProtocolVersion, Heads: map[string]*distribution.ChannelHead{pubImmutable: nil}}
		})},
		{name: "an answer from another namespace", code: refusalPublicationUnavailable, resolves: 1, setup: answering(func(r *pubRig) any {
			set := pubBaseSet()
			set.Namespace = "other"
			return distribution.ResolveResponse{ProtocolVersion: distribution.ProtocolVersion, Heads: map[string]*distribution.ChannelHead{
				pubCanary: {Ref: pubRef(r.t, set), Generation: 1, ReleaseSet: &set}, pubImmutable: nil,
			}}
		})},
		{name: "an answer with a release", code: refusalPublicationUnavailable, resolves: 1, setup: answering(func(r *pubRig) any {
			set := pubBaseSet()
			return distribution.ResolveResponse{ProtocolVersion: distribution.ProtocolVersion, Release: &distribution.ChannelHead{Ref: r.baseRef, ReleaseSet: &set}}
		})},
		{name: "Delivery unavailable", code: refusalPublicationUnavailable, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onPlanning = func(_ int, w http.ResponseWriter) bool { w.WriteHeader(http.StatusServiceUnavailable); return true }
			})
		}},
		{name: "Delivery answers no capability", code: refusalPublicationUnavailable, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onPlanning = func(_ int, w http.ResponseWriter) bool { w.WriteHeader(http.StatusNoContent); return true }
			})
		}},
		{name: "Delivery planning outside its contract", code: refusalPublicationUnavailable, setup: func(r *pubRig) {
			r.delivery.set(func(d *fakeDelivery) {
				d.onPlanning = func(_ int, w http.ResponseWriter) bool {
					writeJSON(w, http.StatusCreated, map[string]any{"protocolVersion": 2})
					return true
				}
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPubRig(t, nil).start()
			if tc.setup != nil {
				tc.setup(r)
			}
			namespace := pubNamespace
			if tc.namespace != "" {
				namespace = tc.namespace
			}
			refusal := mustRefusal(t, r.resolve(pubResolveRequest(namespace, pubCanary, pubImmutable)), tc.code)
			if resolves, _ := r.put.calls(); resolves != tc.resolves {
				t.Fatalf("put-server resolves = %d, want %d", resolves, tc.resolves)
			}
			r.noSecret(refusal.Message)
		})
	}
}

func TestPublicationResolveRetriesAnUnansweredPutServerOnce(t *testing.T) {
	r := newPubRig(t, nil).start()
	r.put.set(func(p *fakePutServer) {
		p.onResolve = func(call int, w http.ResponseWriter, _ *http.Request) bool {
			if call == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return true
			}
			return false
		}
	})
	mustResolved(t, r.resolve(pubResolveRequest(pubNamespace, pubCanary)))
	if resolves, _ := r.put.calls(); resolves != 2 {
		t.Fatalf("put-server resolves = %d, want 2", resolves)
	}
}

func TestPublicationResolveAnswersInsideTheOpTimeout(t *testing.T) {
	r := newPubRig(t, func(server *credentialServer) {
		configure := server.configurePublisher
		server.configurePublisher = func(p *publisher) {
			configure(p)
			p.opTimeout = 200 * time.Millisecond
		}
	}).start()
	r.put.set(func(p *fakePutServer) {
		p.onResolve = func(_ int, _ http.ResponseWriter, req *http.Request) bool {
			<-req.Context().Done()
			return true
		}
	})
	started := time.Now()
	mustRefusal(t, r.resolve(pubResolveRequest(pubNamespace, pubCanary)), refusalPublicationUnavailable)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("resolve answered after %s", elapsed)
	}
}

func TestPublicationPlanningBearerIsReusedThenRenewed(t *testing.T) {
	r := newPubRig(t, nil).start()
	mustResolved(t, r.resolve(pubResolveRequest(pubNamespace, pubCanary)))
	mustResolved(t, r.resolve(pubResolveRequest(pubNamespace, pubCanary)))
	if planning, _ := r.delivery.calls(); planning != 1 {
		t.Fatalf("planning calls = %d, want 1 while the bearer is fresh", planning)
	}
	r.clock.Advance(275 * time.Second)
	mustResolved(t, r.resolve(pubResolveRequest(pubNamespace, pubCanary)))
	if planning, _ := r.delivery.calls(); planning != 2 {
		t.Fatalf("planning calls = %d, want 2 once the bearer is about to expire", planning)
	}
	r.put.set(func(p *fakePutServer) {
		if !reflect.DeepEqual(p.resolveBearers, []string{"planning-bearer-1", "planning-bearer-1", "planning-bearer-2"}) {
			t.Fatalf("resolve bearers = %v", p.resolveBearers)
		}
	})

	// Delivery answering another authority later in the session is refused.
	r.delivery.set(func(d *fakeDelivery) { d.channels, d.immutable, d.forwardOnly = []string{pubCanary}, "", nil })
	r.clock.Advance(275 * time.Second)
	mustRefusal(t, r.resolve(pubResolveRequest(pubNamespace, pubCanary)), refusalPublicationUnavailable)
	if !strings.Contains(r.h.stderr.String(), "another publication authority") {
		t.Fatalf("stderr = %q, want the authority drift", r.h.stderr.String())
	}
	r.noSecret()
}

func TestPublicationPublishCredentialIsReusedUntilHalfItsLifetime(t *testing.T) {
	r := newPubRig(t, nil).start().opened()
	if credential := mustCredential(t, r.publishCredential()); credential.Bearer != "publish-bearer-1" {
		t.Fatalf("bearer = %s, want the open's", credential.Bearer)
	}
	r.clock.Advance(149 * time.Second)
	if credential := mustCredential(t, r.publishCredential()); credential.Bearer != "publish-bearer-1" {
		t.Fatalf("bearer = %s, want the open's before half its lifetime", credential.Bearer)
	}
	r.clock.Advance(2 * time.Second)
	credential := mustCredential(t, r.publishCredential())
	if credential.Bearer != "publish-bearer-2" {
		t.Fatalf("bearer = %s, want a renewed one past half its lifetime", credential.Bearer)
	}
	if want := r.clock.Now().Add(300 * time.Second).Format(time.RFC3339); credential.ExpiresAt != want {
		t.Fatalf("expiresAt = %s, want %s", credential.ExpiresAt, want)
	}

	// Delivery refusing the renewal refuses the credential.
	r.delivery.set(func(d *fakeDelivery) {
		d.onPublish = func(_ int, w http.ResponseWriter, _ deliveryPlanMirror) bool {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "native publication run became terminal"})
			return true
		}
	})
	r.clock.Advance(151 * time.Second)
	mustRefusal(t, r.publishCredential(), refusalPublicationRefused)
	r.noSecret()
}

func TestPublicationReleaseRefusesARequestThatIsNotThePlan(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T, release *pubRelease)
	}{
		{name: "another plan digest", edit: func(_ *testing.T, release *pubRelease) { release.planDigest = pubDigest("f") }},
		{name: "another ancestry", edit: func(_ *testing.T, release *pubRelease) { release.ancestry.SnapshotCommits = 13 }},
		{name: "another namespace", edit: func(_ *testing.T, release *pubRelease) {
			release.request.Namespace, release.request.ReleaseSet.Namespace = "other", "other"
		}},
		{name: "a mutable immutable channel", edit: func(_ *testing.T, release *pubRelease) { release.request.Channels[1].Immutable = false }},
		{name: "an immutable mutable channel", edit: func(_ *testing.T, release *pubRelease) { release.request.Channels[0].Immutable = true }},
		{name: "a planned member at another version", edit: func(_ *testing.T, release *pubRelease) {
			release.request.ReleaseSet.Members[1].Version = "1.2.4"
		}},
		{name: "a planned member from another build", edit: func(_ *testing.T, release *pubRelease) {
			release.request.ReleaseSet.Members[2].SelectionFingerprint = pubDigest("e")
		}},
		{name: "a planned member missing", edit: func(_ *testing.T, release *pubRelease) {
			release.request.ReleaseSet.Members = release.request.ReleaseSet.Members[:2]
		}},
		{name: "an unplanned member no head carries", edit: func(_ *testing.T, release *pubRelease) {
			release.request.ReleaseSet.Members[0].Version = "v1.0.1"
		}},
		{name: "an unplanned member with another artifact", edit: func(_ *testing.T, release *pubRelease) {
			release.request.ReleaseSet.Members[0].ArtifactDigest = pubDigest("e")
		}},
		{name: "an expected head this session did not resolve", edit: func(t *testing.T, release *pubRelease) {
			other := pubRef(t, pubReleasedSet())
			release.request.Channels[0].Expected = &other
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPubRig(t, nil).start().opened()
			release := r.defaultRelease()
			tc.edit(t, &release)
			payload, _ := release.payload(t)
			refusal := mustRefusal(t, r.release(payload), registry.RefusalPlanMismatch)
			if _, releases := r.put.calls(); releases != 0 {
				t.Fatalf("put-server releases = %d, want none", releases)
			}
			if _, publish := r.delivery.calls(); publish != 1 || r.delivery.advances() != 0 {
				t.Fatalf("Delivery publish calls = %d, advance calls = %d, want only the open's", publish, r.delivery.advances())
			}
			r.noSecret(refusal.Message)
		})
	}
}

func TestPublicationReleaseIsFinalPerPlan(t *testing.T) {
	r := newPubRig(t, nil).start().opened()
	wrong := r.defaultRelease()
	wrong.request.ReleaseSet.Members[1].Version = "1.2.4"
	wrongPayload, _ := wrong.payload(t)
	mustRefusal(t, r.release(wrongPayload), registry.RefusalPlanMismatch)
	// The refusal is the plan's answer: the same request gets it again, and
	// another set is refused as another release of the plan.
	mustRefusal(t, r.release(wrongPayload), registry.RefusalPlanMismatch)
	payload, _ := r.defaultRelease().payload(t)
	refusal := mustRefusal(t, r.release(payload), registry.RefusalPlanMismatch)
	if !strings.Contains(refusal.Message, "was released with set") {
		t.Fatalf("refusal = %q, want the earlier release named", refusal.Message)
	}
	if _, releases := r.put.calls(); releases != 0 {
		t.Fatalf("put-server releases = %d, want none", releases)
	}
}

func TestPublicationReleaseBeforeOpenIsRefused(t *testing.T) {
	r := newPubRig(t, nil).start()
	payload, _ := r.defaultRelease().payload(t)
	mustRefusal(t, r.release(payload), registry.RefusalPlanNotOpen)
}

func TestPublicationReleaseCarriesInheritedMembersWhateverTheirAttribution(t *testing.T) {
	r := newPubRig(t, nil).start().opened()
	release := r.defaultRelease()
	release.request.ReleaseSet.Members[0].Project = "libs/lib"
	release.request.ReleaseSet.Members[0].Kind = distribution.KindLibrary
	payload, requestBytes := release.payload(t)
	if released := mustReleased(t, r.release(payload), requestBytes); released.Outcome != distribution.ReleaseOutcomeReleased {
		t.Fatalf("outcome = %s, want released", released.Outcome)
	}
}

// TestPublicationReleaseAnswersAMovedHeadItDoesNotRestateAsAConflict pins the
// member guard of a head run: the canary moved, since the engine resolved it,
// to a head carrying a member this run neither rebuilt nor carries at that
// version. Moving the canary from it would put the older member back, so the
// answer is a conflict and no channel moves.
func TestPublicationReleaseAnswersAMovedHeadItDoesNotRestateAsAConflict(t *testing.T) {
	r := newPubRig(t, nil).start().opened()
	moved := pubBaseSet()
	moved.Members[0].Version = "v1.0.9"
	movedRef := r.put.move(pubCanary, moved)
	payload, requestBytes := r.defaultRelease().payload(t)
	answer := mustReleased(t, r.release(payload), requestBytes)
	if answer.Outcome != distribution.ReleaseOutcomeConflict || answer.Current[pubCanary].Ref != movedRef || answer.Current[pubCanary].Generation != 4 {
		t.Fatalf("release = %+v, want a conflict naming the moved head", answer)
	}
	again := mustReleased(t, r.release(payload), requestBytes)
	if !reflect.DeepEqual(again, answer) {
		t.Fatalf("repeated release = %+v, want the stored conflict", again)
	}
	if _, releases := r.put.calls(); releases != 0 {
		t.Fatalf("put-server releases = %d, want none", releases)
	}
	if head := r.put.head(pubCanary); head.Ref != movedRef {
		t.Fatalf("canary head = %+v, want the moved head left in place", head)
	}
	if stderr := r.h.stderr.String(); !strings.Contains(stderr, "(conflict)") || !strings.Contains(stderr, movedRef.ID) {
		t.Fatalf("stderr = %q, want the conflict and the moved head reported", stderr)
	}
}

func TestPublicationReleaseMapsPutServerRefusals(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		code       string
		refusal    string
		definitive bool
	}{
		{name: "missing artifact", status: http.StatusConflict, code: "member_missing", refusal: registry.RefusalArtifactMissing, definitive: true},
		{name: "immutable channel", status: http.StatusConflict, code: "registry.release_set.channel_immutable", refusal: registry.RefusalChannelImmutable, definitive: true},
		{name: "conflict", status: http.StatusConflict, code: "conflict", refusal: registry.RefusalConflict, definitive: true},
		{name: "bearer refused", status: http.StatusUnauthorized, code: "unauthorized", refusal: registry.RefusalNamespaceForbidden, definitive: true},
		{name: "namespace forbidden", status: http.StatusForbidden, code: "forbidden", refusal: registry.RefusalNamespaceForbidden, definitive: true},
		{name: "bad request", status: http.StatusBadRequest, code: "http.bad_request", refusal: refusalPublicationRefused, definitive: true},
		{name: "undeclared refusal", status: http.StatusUnprocessableEntity, code: "unprocessable", refusal: refusalPublicationRefused, definitive: true},
		{name: "unavailable", status: http.StatusInternalServerError, code: "http.internal_server", refusal: refusalPublicationUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPubRig(t, nil).start().opened()
			r.put.set(func(p *fakePutServer) {
				p.onRelease = func(call int, w http.ResponseWriter, _ *http.Request) bool {
					if tc.definitive || call <= 2 {
						// A message carrying a bearer of this session never
						// reaches a refusal or stderr.
						putError(w, tc.status, tc.code, "refused near planning-bearer-1")
						return true
					}
					return false
				}
			})
			payload, requestBytes := r.defaultRelease().payload(t)
			refusal := mustRefusal(t, r.release(payload), tc.refusal)
			r.noSecret(refusal.Message)
			_, releases := r.put.calls()
			if tc.definitive {
				if releases != 1 {
					t.Fatalf("put-server releases = %d, want 1: a refusal put-server decided is not retried", releases)
				}
				mustRefusal(t, r.release(payload), tc.refusal)
				if _, again := r.put.calls(); again != 1 {
					t.Fatalf("put-server releases = %d, want the stored refusal", again)
				}
				return
			}
			if releases != 2 || !strings.Contains(refusal.Message, "outcome of plan") {
				t.Fatalf("releases = %d, refusal = %q, want one retry and an unknown outcome", releases, refusal.Message)
			}
			// An unknown outcome stores nothing: the release is sent again.
			mustReleased(t, r.release(payload), requestBytes)
			if _, again := r.put.calls(); again != 3 {
				t.Fatalf("put-server releases = %d, want 3", again)
			}
		})
	}
}

func TestPublicationReleaseRefusesAnAnswerOutsideTheProtocol(t *testing.T) {
	r := newPubRig(t, nil).start().opened()
	r.put.set(func(p *fakePutServer) {
		p.onRelease = func(call int, w http.ResponseWriter, _ *http.Request) bool {
			if call > 1 {
				return false
			}
			// released, but naming the base set as the head
			writeJSON(w, http.StatusOK, distribution.ReleaseResponse{
				ProtocolVersion: distribution.ProtocolVersion, Outcome: distribution.ReleaseOutcomeReleased,
				Current: map[string]*distribution.ChannelHead{
					pubCanary: {Ref: r.baseRef, Generation: 4}, pubImmutable: {Ref: r.baseRef, Generation: 1},
				},
			})
			return true
		}
	})
	payload, requestBytes := r.defaultRelease().payload(t)
	mustRefusal(t, r.release(payload), refusalPublicationUnavailable)
	mustReleased(t, r.release(payload), requestBytes)
}

func TestPublicationReleaseNeedsAFreshWriteBearer(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		code       string
		refusal    string
		definitive bool
	}{
		{name: "refused", status: http.StatusConflict, code: "native_publication_superseded", refusal: refusalPublicationRefused, definitive: true},
		{name: "unavailable", status: http.StatusServiceUnavailable, refusal: refusalPublicationUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPubRig(t, nil).start().opened()
			r.delivery.set(func(d *fakeDelivery) {
				d.onPublish = func(call int, w http.ResponseWriter, _ deliveryPlanMirror) bool {
					if call <= 3 {
						writeJSON(w, tc.status, map[string]string{"error": "refused", "code": tc.code})
						return true
					}
					return false
				}
			})
			payload, requestBytes := r.defaultRelease().payload(t)
			mustRefusal(t, r.release(payload), tc.refusal)
			if _, releases := r.put.calls(); releases != 0 {
				t.Fatalf("put-server releases = %d, want none without a write bearer", releases)
			}
			if tc.definitive {
				mustRefusal(t, r.release(payload), tc.refusal)
				return
			}
			mustReleased(t, r.release(payload), requestBytes)
		})
	}
}

func TestPublicationReleasesOncePerPlanUnderConcurrency(t *testing.T) {
	r := newPubRig(t, nil).start().opened()
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	r.put.set(func(p *fakePutServer) {
		p.releaseStarted = started
		p.onRelease = func(int, http.ResponseWriter, *http.Request) bool { <-release; return false }
	})
	payload, requestBytes := r.defaultRelease().payload(t)
	first := r.send(registry.CredentialOpRelease, json.RawMessage(payload))
	<-started
	second := r.send(registry.CredentialOpRelease, json.RawMessage(payload))
	other := r.defaultRelease()
	other.request.ReleaseSet.Members[1].Version = "1.2.4"
	otherPayload, _ := other.payload(t)
	r.send(registry.CredentialOpRelease, json.RawMessage(otherPayload))
	refusal := mustRefusal(t, r.h.next(registry.CredentialOpRelease), registry.RefusalPlanMismatch)
	if !strings.Contains(refusal.Message, "being released") {
		t.Fatalf("refusal = %q, want the release in progress named", refusal.Message)
	}
	time.Sleep(50 * time.Millisecond) // let the second release join the first
	close(release)
	answers := r.collect(2, registry.CredentialOpRelease)
	a, b := mustReleased(t, answers[int64(first)], requestBytes), mustReleased(t, answers[int64(second)], requestBytes)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("answers differ: %+v, %+v", a, b)
	}
	if _, releases := r.put.calls(); releases != 1 {
		t.Fatalf("put-server releases = %d, want 1", releases)
	}
}

func TestPublicationShutdownAbandonsAReleaseInProgress(t *testing.T) {
	r := newPubRig(t, nil).start().opened()
	started := make(chan struct{}, 1)
	r.put.set(func(p *fakePutServer) {
		p.releaseStarted = started
		p.onRelease = func(_ int, _ http.ResponseWriter, req *http.Request) bool { <-req.Context().Done(); return true }
	})
	payload, _ := r.defaultRelease().payload(t)
	r.send(registry.CredentialOpRelease, json.RawMessage(payload))
	<-started
	r.h.send(fmt.Sprintf(`{"protocolVersion":1,"id":%d,"op":"shutdown"}`, r.id+1))
	if response := r.h.next(registry.CredentialOpShutdown); !response.OK {
		t.Fatalf("shutdown = %+v", response)
	}
	r.h.noAnswer(100 * time.Millisecond)
	if err := r.h.exit(); err != nil {
		t.Fatalf("serve = %v", err)
	}
}

func TestPublicationLineBoundFollowsTheNegotiation(t *testing.T) {
	clock := &pubClock{now: time.Now()}
	delivery := newFakeDelivery(t, clock)
	server := newTestCredentialServer(delivery.url, nil, nil)
	server.publication = publicationEndpointsFrom(map[string]string{publishPutURLEnv: "https://put.putnami.dev"})
	padded := fmt.Sprintf(`{"protocolVersion":1,"id":2,"op":"resolve","payload":{"request":{"protocolVersion":2,"namespace":"acme","channels":["canary"]},"pad":%q}}`,
		strings.Repeat("x", registry.MaxCredentialLineBytes*2))
	input := publicationInitializeLine(1, testRunCredential) + "\n" + padded + "\n" + strings.Repeat("y", registry.MaxPublicationLineBytes+3) + "\n"
	var out bytes.Buffer
	err := server.serve(strings.NewReader(input), &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(registry.MaxPublicationLineBytes)) {
		t.Fatalf("serve = %v, want the publication line bound", err)
	}
	scanner := bufio.NewScanner(&out)
	answers := make([]*registry.CredentialResponse, 0, 2)
	negotiated := []string{registry.CapabilityCredentialV1, registry.CapabilityPublicationV1}
	for _, op := range []registry.CredentialOp{registry.CredentialOpInitialize, registry.CredentialOpResolve} {
		if !scanner.Scan() {
			t.Fatalf("answers = %d, want 2", len(answers))
		}
		response, err := registry.ParseNegotiatedCredentialResponse(scanner.Bytes(), op, negotiated)
		if err != nil {
			t.Fatal(err)
		}
		answers = append(answers, response)
	}
	// The padded line is read whole and refused for its unknown member: the
	// session did not end at the credential line bound.
	mustRefusal(t, answers[1], refusalInvalidRequest)
}

func TestReadRequestLine(t *testing.T) {
	cases := []struct {
		name  string
		input string
		limit int
		want  string
		err   error
	}{
		{name: "line", input: "abc\nnext", limit: 8, want: "abc"},
		{name: "crlf", input: "abc\r\n", limit: 8, want: "abc"},
		{name: "last line without newline", input: "abc", limit: 8, want: "abc"},
		{name: "end of stream", input: "", limit: 8, err: io.EOF},
		{name: "at the bound", input: "abcd\r\n", limit: 4, want: "abcd"},
		{name: "over the bound", input: "abcde\n", limit: 4, err: errRequestLineTooLong},
		{name: "over the bound without newline", input: "abcde", limit: 4, err: errRequestLineTooLong},
		{name: "longer than the buffer", input: strings.Repeat("z", 40) + "\n", limit: 64, want: strings.Repeat("z", 40)},
		{name: "far over the bound", input: strings.Repeat("z", 100) + "\n", limit: 64, err: errRequestLineTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, err := readRequestLine(bufio.NewReaderSize(strings.NewReader(tc.input), 16), tc.limit)
			if !errors.Is(err, tc.err) || (tc.err == nil && string(line) != tc.want) {
				t.Fatalf("readRequestLine = %q, %v, want %q, %v", line, err, tc.want, tc.err)
			}
		})
	}
}

func TestPublicationOrigin(t *testing.T) {
	cases := []struct {
		raw, origin, host string
	}{
		{raw: "https://put.putnami.dev", origin: "https://put.putnami.dev", host: "put.putnami.dev"},
		{raw: " https://PUT.Putnami.dev:443/ ", origin: "https://put.putnami.dev", host: "put.putnami.dev"},
		{raw: "https://put.putnami.dev:8443", origin: "https://put.putnami.dev:8443", host: "put.putnami.dev:8443"},
		{raw: "http://127.0.0.1:8080", origin: "http://127.0.0.1:8080", host: "127.0.0.1:8080"},
		{raw: "http://localhost:80", origin: "http://localhost", host: "localhost"},
		{raw: ""},
		{raw: "http://put.putnami.dev"},
		{raw: "ftp://put.putnami.dev"},
		{raw: "https://user:pass@put.putnami.dev"},
		{raw: "https://put.putnami.dev/put"},
		{raw: "https://put.putnami.dev?a=1"},
		{raw: "https://put.putnami.dev?"},
		{raw: "https://put.putnami.dev#f"},
		{raw: "mailto:ops@putnami.dev"},
		{raw: "https://put_putnami.dev"},
	}
	for _, tc := range cases {
		origin, host, err := publicationOrigin(tc.raw)
		if tc.origin == "" {
			if err == nil {
				t.Errorf("publicationOrigin(%q) = %q, %q, want an error", tc.raw, origin, host)
			}
			continue
		}
		if err != nil || origin != tc.origin || host != tc.host {
			t.Errorf("publicationOrigin(%q) = %q, %q, %v, want %q, %q", tc.raw, origin, host, err, tc.origin, tc.host)
		}
	}
}

func TestPublicationEndpointsFrom(t *testing.T) {
	endpoints := publicationEndpointsFrom(map[string]string{
		publishPutURLEnv: "https://put.putnami.dev/", publishNPMURLEnv: "https://npm.putnami.dev",
		publishGoURLEnv: "https://npm.putnami.dev", publishOCIURLEnv: " ",
	})
	if endpoints.err != nil || endpoints.absent || endpoints.putBase != "https://put.putnami.dev" ||
		!reflect.DeepEqual(endpoints.hosts, []string{"npm.putnami.dev", "put.putnami.dev"}) {
		t.Fatalf("endpoints = %+v", endpoints)
	}
	for name, env := range map[string]map[string]string{
		publishPutURLEnv: {publishNPMURLEnv: "https://npm.putnami.dev"},
		publishOCIURLEnv: {publishPutURLEnv: "https://put.putnami.dev", publishOCIURLEnv: "https://oci.putnami.dev/v2"},
	} {
		if got := publicationEndpointsFrom(env); got.err == nil || got.absent || !strings.Contains(got.err.Error(), name) {
			t.Errorf("endpoints from %v = %+v, want an error naming %s", env, got, name)
		}
	}
	// Blank values are no origin: the run publishes nothing.
	if got := publicationEndpointsFrom(map[string]string{publishPutURLEnv: " ", publishGoURLEnv: ""}); got.err == nil || !got.absent {
		t.Errorf("endpoints without an origin = %+v, want absent with an error", got)
	}
}

func TestParsePlanningGrant(t *testing.T) {
	issuedAt := time.Unix(1_800_000_000, 500_000_000)
	valid := func() map[string]any {
		return map[string]any{
			"protocolVersion": 1, "workspaceId": "ws", "runId": "run", "sourceRevision": pubSource, "namespace": pubNamespace,
			"channels": []string{pubCanary, pubImmutable}, "immutableChannel": pubImmutable, "forwardOnlyChannels": []string{pubCanary},
			"expiresUnix": issuedAt.Unix() + 3600, "access_token": "planning-bearer", "token_type": "bearer", "expires_in": 120,
			"scope": "read", "jti": "jti",
		}
	}
	body, _ := json.Marshal(valid())
	authority, bearer, err := parsePlanningGrant(body, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	want := publicationAuthority{WorkspaceID: "ws", RunID: "run", SourceRevision: pubSource, Namespace: pubNamespace,
		Channels: []string{pubCanary, pubImmutable}, ImmutableChannel: pubImmutable, ForwardOnly: []string{pubCanary}}
	if !reflect.DeepEqual(authority, want) || !authority.equal(want) {
		t.Fatalf("authority = %+v", authority)
	}
	if bearer.bearer != "planning-bearer" || !bearer.issuedAt.Equal(time.Unix(1_800_000_000, 0)) || !bearer.expiresAt.Equal(time.Unix(1_800_000_120, 0)) {
		t.Fatalf("bearer window = %+v, want it to start at the second before the request", bearer)
	}
	for name, edit := range map[string]func(map[string]any){
		"protocol version":       func(m map[string]any) { m["protocolVersion"] = 2 },
		"no run":                 func(m map[string]any) { m["runId"] = " " },
		"short commit":           func(m map[string]any) { m["sourceRevision"] = "abc" },
		"run deadline passed":    func(m map[string]any) { m["expiresUnix"] = issuedAt.Unix() },
		"invalid namespace":      func(m map[string]any) { m["namespace"] = "Acme!" },
		"no channel":             func(m map[string]any) { m["channels"] = []string{} },
		"foreign immutable":      func(m map[string]any) { m["immutableChannel"] = "v9" },
		"forward-only immutable": func(m map[string]any) { m["forwardOnlyChannels"] = []string{pubImmutable} },
		"forward-only foreign":   func(m map[string]any) { m["forwardOnlyChannels"] = []string{"main"} },
		"forward-only repeated":  func(m map[string]any) { m["forwardOnlyChannels"] = []string{pubCanary, pubCanary} },
		"token type":             func(m map[string]any) { m["token_type"] = "MAC" },
		"bearer with space":      func(m map[string]any) { m["access_token"] = "a b" },
		"no lifetime":            func(m map[string]any) { m["expires_in"] = 0 },
		"long lifetime":          func(m map[string]any) { m["expires_in"] = 301 },
		"no jti":                 func(m map[string]any) { m["jti"] = "" },
	} {
		grant := valid()
		edit(grant)
		body, _ := json.Marshal(grant)
		if _, _, err := parsePlanningGrant(body, issuedAt); err == nil {
			t.Errorf("%s: parsePlanningGrant accepted %s", name, body)
		}
	}
	if _, _, err := parsePlanningGrant([]byte("[]"), issuedAt); err == nil {
		t.Error("parsePlanningGrant accepted a body that is not an object")
	}
}

func TestParsePublishGrant(t *testing.T) {
	issuedAt := time.Unix(1_800_000_000, 0)
	digest := pubDigest("d")
	grant := func(version int, planDigest string) []byte {
		body, _ := json.Marshal(map[string]any{"protocolVersion": version, "planDigest": planDigest,
			"access_token": "publish-bearer", "token_type": "Bearer", "expires_in": 300, "scope": "write", "jti": "jti"})
		return body
	}
	bearer, err := parsePublishGrant(grant(1, digest), digest, issuedAt)
	if err != nil || bearer.bearer != "publish-bearer" || !bearer.expiresAt.Equal(issuedAt.Add(300*time.Second)) {
		t.Fatalf("parsePublishGrant = %+v, %v", bearer, err)
	}
	if bearer.halfSpent(issuedAt.Add(149*time.Second)) || !bearer.halfSpent(issuedAt.Add(150*time.Second)) {
		t.Fatal("halfSpent does not turn at half the lifetime")
	}
	for name, body := range map[string][]byte{
		"protocol version": grant(2, digest),
		"another plan":     grant(1, pubDigest("e")),
		"not an object":    []byte(`"x"`),
	} {
		if _, err := parsePublishGrant(body, digest, issuedAt); err == nil {
			t.Errorf("%s: parsePublishGrant accepted %s", name, body)
		}
	}
}

func TestPublicationPlanDigestIsDeliveryDigest(t *testing.T) {
	plan := pubPlan(t)
	mirror := deliveryPlanMirror{
		ProtocolVersion: plan.ProtocolVersion, Namespace: plan.Namespace, SourceRevision: plan.SourceRevision,
		Channels: plan.Channels, ImmutableChannel: plan.ImmutableChannel, PlanDigest: plan.PlanDigest,
	}
	for _, member := range plan.Members {
		mirror.Members = append(mirror.Members, deliveryMemberMirror{string(member.Ecosystem), member.Coordinate, member.Version, member.SourceRevision, member.SelectionFingerprint})
	}
	if mirror.digest() != plan.PlanDigest {
		t.Fatalf("Delivery digest %s, protocol digest %s", mirror.digest(), plan.PlanDigest)
	}
	// What the provider posts is the plan Delivery decodes, member for member.
	body, _ := json.Marshal(plan)
	var decoded deliveryPlanMirror
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil || !reflect.DeepEqual(decoded, mirror) {
		t.Fatalf("Delivery decodes %s as %+v (%v)", body, decoded, err)
	}
}

func TestSingleFlightReleasesAWaiterWhoseContextEnds(t *testing.T) {
	var group singleFlight[int]
	running := make(chan struct{})
	finish := make(chan struct{})
	go group.do(context.Background(), func() int { close(running); <-finish; return 1 })
	<-running
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := group.do(ctx, func() int { return 2 }); ok {
		t.Fatal("a waiter whose context ended got a value")
	}
	close(finish)
}
