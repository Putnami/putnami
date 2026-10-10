package deliverycli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/procguard"
)

// Refusal codes the credential provider answers. Each one fails every request
// of its purpose for the rest of the engine process (registry ADR 0002).
const (
	// refusalInstallRefused is Delivery's refusal without a usable code.
	refusalInstallRefused = "install_refused"
	// refusalInstallUnavailable is Delivery not answering, or answering with
	// a credential this provider cannot pass on.
	refusalInstallUnavailable = "install_capability_unavailable"
	// refusalInstallUnconfigured is a run credential without an ingest base
	// that may carry it.
	refusalInstallUnconfigured = "install_capability_unconfigured"
)

// Codes of a request the provider cannot serve. The engine never sends one;
// they keep a broken peer from waiting for an answer that never comes.
const (
	refusalInvalidRequest       = "invalid_request"
	refusalInvalidRunCredential = "invalid_run_credential" //nolint:gosec // G101: a refusal code, not a credential
	refusalNotInitialized       = "not_initialized"
	refusalAlreadyInitialized   = "already_initialized"
	refusalInspectionGuard      = "inspection_guard_unavailable"
)

// localReadTimeout bounds the developer-machine source, which reads the
// sign-in and may mint a token. It stays under the engine's 30 s op timeout,
// so the engine gets an answer (absence) even when the auth server hangs.
const localReadTimeout = 25 * time.Second

// credentialDiagnosticBytes bounds one stderr diagnostic line.
const credentialDiagnosticBytes = 512

// ReadCredentialSource answers the read purpose on a machine without a run
// credential: a developer machine whose user enabled `--providers install`.
// A nil credential with a nil error is absence. An error is reported on stderr
// and answered as absence, so the engine keeps its native credentials. ctx
// ends when the provider stops waiting for the answer. The source must then
// stop and store no credential, except for a step that must not be cut
// halfway, such as a sign-in refresh. The next read waits for it to return.
type ReadCredentialSource func(ctx context.Context) (*registry.Credential, error)

// CredentialProvider runs the extension binary as the engine's credential
// provider: the provider half of the credential-provider RPC defined in
// protocol/registry (registry.CredentialProviderCommand). The engine starts it
// once per process when `--providers install` enables the read purpose and
// asks it for at most one credential per purpose at a time.
//
// With a hosted run's credential (the initialize runCredential), the read
// credential comes from Delivery's install capability for that run. Without
// one, local answers it; a nil local answers absence.
//
// A hosted run whose launcher exported its publication origins
// (CI_LIBRARY_PUBLISH_*) also serves publication-v1 when the engine offers
// it (registry ADR 0003, see publisher): resolve, open, the publish
// credential and release. Everywhere else the publish purpose is absence and
// publishing keeps its own credential path.
//
// The run credential stays in this process's memory. The process denies
// inspection before it reads its first line, never writes the credential to
// a file, an argument, an environment or a diagnostic, and starts no process
// that could receive it. The protocol owns stdout; diagnostics go to stderr.
func CredentialProvider(env map[string]string, local ReadCredentialSource) error {
	server := newCredentialServer(newCapabilityCaller(env[SessionReporterIngestURLEnv]), local, procguard.DenyInspection)
	server.publication = publicationEndpointsFrom(env)
	return server.serve(os.Stdin, os.Stdout, os.Stderr)
}

type credentialServer struct {
	caller         *capabilityCaller
	local          ReadCredentialSource
	denyInspection func() error
	localTimeout   time.Duration

	logw    io.Writer
	out     *bufio.Writer
	writeMu sync.Mutex

	// publication holds the origins publication-v1 serves; its err says why
	// a session does not serve it.
	publication publicationEndpoints
	// configurePublisher, when set, adjusts each session's publisher before
	// it serves; tests shorten its timeouts with it.
	configurePublisher func(*publisher)

	// Session state, read and written by the request loop only.
	guarded     bool
	initialized bool
	// negotiated is the session's capabilities, set by initialize.
	negotiated []string
	// publisher serves publication-v1 when the session negotiated it. It is
	// set by initialize, before any publication op starts.
	publisher *publisher

	// runCredential is the hosted run's bearer. It is set once by initialize,
	// before any credential op starts, and only read afterwards.
	runCredential string

	ctx    context.Context
	cancel context.CancelFunc
	work   sync.WaitGroup

	flightMu sync.Mutex
	flights  map[string]*credentialFlight

	// localSlot admits one local source call at a time. A call that timed
	// out keeps the slot until it returns, so a later read never runs beside
	// it: two sign-in reads could otherwise both write registries.json.
	localSlot chan struct{}

	// secrets are the bearers this session received from Delivery. Every
	// diagnostic and every message passed on from another component is
	// redacted of them, as of the run credential.
	secretsMu sync.Mutex
	secrets   []string
}

// credentialFlight is the one exchange in progress for a purpose. A request
// for a purpose that already has one waits for its answer instead of starting
// another.
type credentialFlight struct {
	done   chan struct{}
	answer credentialAnswer
}

// credentialAnswer is a credential, a refusal, or neither (absence).
type credentialAnswer struct {
	credential *registry.Credential
	refusal    *registry.CredentialRefusal
}

func newCredentialServer(caller *capabilityCaller, local ReadCredentialSource, denyInspection func() error) *credentialServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &credentialServer{
		caller:         caller,
		local:          local,
		denyInspection: denyInspection,
		localTimeout:   localReadTimeout,
		ctx:            ctx,
		cancel:         cancel,
		flights:        make(map[string]*credentialFlight),
		localSlot:      make(chan struct{}, 1),
		publication:    publicationEndpoints{err: errors.New("is not configured")},
	}
}

// serve answers requests until shutdown or stdin EOF. It returns an error
// only for a stream it cannot keep reading: a line over the protocol bound or
// a broken pipe. The bound is MaxCredentialLineBytes, and
// MaxPublicationLineBytes once the session negotiated publication-v1.
func (s *credentialServer) serve(in io.Reader, out, logw io.Writer) error {
	s.logw = logw
	s.out = bufio.NewWriter(out)
	defer s.stop()

	// Deny inspection before the first line, which may carry the run
	// credential. A process that cannot deny it still serves, but refuses a
	// run credential (see initialize).
	if err := s.denyInspection(); err != nil {
		s.diagnose("cannot deny process inspection: %v", err)
	} else {
		s.guarded = true
	}

	reader := bufio.NewReaderSize(in, 64<<10)
	for {
		limit := s.lineLimit()
		line, err := readRequestLine(reader, limit)
		switch {
		case errors.Is(err, io.EOF):
			return nil // the engine went away
		case errors.Is(err, errRequestLineTooLong):
			s.diagnose("a request line exceeds %d bytes; ending the session", limit)
			return fmt.Errorf("credential-provider: a request line exceeds %d bytes", limit)
		case err != nil:
			return fmt.Errorf("credential-provider read: %w", err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if s.handle(line) {
			return nil
		}
	}
}

// lineLimit is the bound of the next request line. Only the request loop
// calls it, after initialize set the negotiated capabilities.
func (s *credentialServer) lineLimit() int {
	if slices.Contains(s.negotiated, registry.CapabilityPublicationV1) {
		return registry.MaxPublicationLineBytes
	}
	return registry.MaxCredentialLineBytes
}

var errRequestLineTooLong = errors.New("request line too long")

// readRequestLine reads one line of at most limit bytes without its "\n" or
// "\r\n". A last line without a newline is a line; the end of the stream
// with nothing read is io.EOF. A longer line is errRequestLineTooLong, read
// no further than the bound.
func readRequestLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(line)+len(chunk) > limit+len("\r\n") {
			return nil, errRequestLineTooLong
		}
		line = append(line, chunk...)
		switch {
		case err == nil, errors.Is(err, io.EOF):
			if err != nil && len(line) == 0 {
				return nil, io.EOF
			}
			line = bytes.TrimSuffix(line, []byte("\n"))
			line = bytes.TrimSuffix(line, []byte("\r"))
			if len(line) > limit {
				return nil, errRequestLineTooLong
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
		default:
			return nil, err
		}
	}
}

// stop cancels the exchanges in progress and waits for them; a canceled
// exchange answers nothing.
func (s *credentialServer) stop() {
	s.cancel()
	s.work.Wait()
}

// handle serves one line and reports whether the session ended.
func (s *credentialServer) handle(line []byte) bool {
	request, err := registry.ParseNegotiatedCredentialRequest(line, s.negotiated)
	if err != nil {
		s.refuseMalformed(line, err)
		return false
	}
	switch request.Op {
	case registry.CredentialOpInitialize:
		s.initialize(request)
	case registry.CredentialOpCredential:
		s.credential(request)
	case registry.CredentialOpResolve, registry.CredentialOpOpen, registry.CredentialOpRelease:
		s.publicationOp(request)
	case registry.CredentialOpShutdown:
		s.stop()
		s.write(registry.CredentialResponse{ProtocolVersion: registry.CredentialProtocolVersion, ID: request.ID, OK: true})
		return true
	}
	return false
}

// refuseMalformed answers a line the protocol refuses when it still names a
// request id, so the peer does not wait for the op timeout. The line of an
// initialize may hold the run credential, so neither the answer nor the
// diagnostic quotes the parse error for it.
func (s *credentialServer) refuseMalformed(line []byte, parseErr error) {
	var envelope struct {
		ID      int64           `json:"id"`
		Op      string          `json:"op"`
		Payload json.RawMessage `json:"payload"`
	}
	_ = json.Unmarshal(line, &envelope)
	op := registry.CredentialOp(envelope.Op)
	if op == registry.CredentialOpInitialize {
		s.diagnose("refusing a malformed initialize request")
	} else {
		s.diagnose("refusing a malformed request: %v", parseErr)
	}
	if envelope.ID <= 0 {
		return
	}
	code, message := refusalInvalidRequest, "the request does not follow the credential-provider protocol"
	switch {
	case !op.Valid():
		message = "the request names an op the credential-provider protocol does not define"
	case !op.Allowed(s.negotiated):
		message = fmt.Sprintf("the request names an op of the %s capability, which this session did not negotiate", op.Capability())
	case op == registry.CredentialOpInitialize && invalidRunCredentialMember(envelope.Payload):
		code, message = refusalInvalidRunCredential, fmt.Sprintf("the run credential is not 1 to %d bytes of UTF-8 with no whitespace", registry.MaxRunCredentialBytes)
	}
	s.refuse(envelope.ID, code, message)
}

// invalidRunCredentialMember reports whether an initialize payload carries a
// runCredential member that is not a valid run credential.
func invalidRunCredentialMember(payload json.RawMessage) bool {
	var members map[string]json.RawMessage
	if json.Unmarshal(payload, &members) != nil {
		return false
	}
	raw, present := members["runCredential"]
	if !present {
		return false
	}
	var credential string
	return json.Unmarshal(raw, &credential) != nil || !registry.ValidRunCredential(credential)
}

func (s *credentialServer) initialize(request *registry.CredentialRequest) {
	if s.initialized {
		s.refuse(request.ID, refusalAlreadyInitialized, "the session is already initialized")
		return
	}
	params, err := registry.ParseCredentialInitializeParams(request.Payload)
	if err != nil {
		s.refuse(request.ID, refusalInvalidRequest, "the initialize payload does not follow the credential-provider protocol")
		return
	}
	if params.RunCredential != "" && !s.guarded {
		s.refuse(request.ID, refusalInspectionGuard, "this process cannot deny inspection by other processes, so it does not accept a run credential")
		return
	}
	s.initialized = true
	s.runCredential = params.RunCredential
	echo := []string{registry.CapabilityCredentialV1}
	if s.servesPublication(params.Capabilities) {
		echo = append(echo, registry.CapabilityPublicationV1)
	}
	s.negotiated = registry.NegotiatedCapabilities(params.Capabilities, echo)
	s.answer(request.ID, registry.CredentialInitializeResult{
		ProtocolVersion: registry.CredentialProtocolVersion,
		ProviderName:    providerName,
		Capabilities:    echo,
	})
}

// servesPublication decides the publication-v1 echo, and prepares the
// session's publisher when it is echoed: the engine offered it, the session
// is a hosted run's, and the run gave both a Delivery ingest base and its
// publication origins. Without the echo the engine keeps its own publication
// path, so a hosted run that cannot serve it says why on stderr. A run given
// no publication origin publishes nothing, and says nothing: the initialize
// does not tell whether the engine will ask for the publish purpose.
func (s *credentialServer) servesPublication(offered []string) bool {
	if !slices.Contains(offered, registry.CapabilityPublicationV1) || s.runCredential == "" || s.publication.absent {
		return false
	}
	switch {
	case !s.caller.configured():
		s.diagnose("publication-v1 is not served: %s is not set to an https Delivery ingest base", SessionReporterIngestURLEnv)
		return false
	case s.publication.err != nil:
		s.diagnose("publication-v1 is not served: %v", s.publication.err)
		return false
	}
	publisher, err := newPublisher(s.caller, s.runCredential, s.publication)
	if err != nil {
		s.diagnose("publication-v1 is not served: %v", err)
		return false
	}
	publisher.diagnose = s.diagnose
	publisher.safeText = s.safeText
	publisher.addSecret = s.addSecret
	if s.configurePublisher != nil {
		s.configurePublisher(publisher)
	}
	s.publisher = publisher
	return true
}

func (s *credentialServer) credential(request *registry.CredentialRequest) {
	if !s.initialized {
		s.refuse(request.ID, refusalNotInitialized, "the session is not initialized")
		return
	}
	params, err := registry.ParseCredentialParams(request.Payload)
	if err != nil {
		s.refuse(request.ID, refusalInvalidRequest, "the credential payload does not follow the credential-provider protocol")
		return
	}
	if params.Purpose != registry.PurposeRead {
		if s.publisher == nil {
			// Publishing keeps its own credential path; the engine uses it.
			s.answer(request.ID, registry.CredentialResult{})
			return
		}
		s.publishCredential(request.ID)
		return
	}
	id := request.ID
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		answer, ok := s.shared(registry.PurposeRead, s.resolveRead)
		if !ok {
			return // the session ended first; nobody reads this answer
		}
		if answer.refusal != nil {
			s.refuse(id, answer.refusal.Code, answer.refusal.Message)
			return
		}
		s.answer(id, registry.CredentialResult{Credential: answer.credential})
	}()
}

// publishCredential answers the publish purpose of a publication-v1 session
// from the open plan (publisher.credential).
func (s *credentialServer) publishCredential(id int64) {
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		answer := s.publisher.credential(s.ctx)
		switch {
		case s.ctx.Err() != nil:
			return // the session ended first; nobody reads this answer
		case answer.refusal != nil:
			s.refuse(id, answer.refusal.Code, answer.refusal.Message)
		default:
			s.answer(id, registry.CredentialResult{Credential: answer.credential})
		}
	}()
}

// publicationOp serves resolve, open and release beside the request loop, so
// a second attempt of an op the engine stopped waiting for is read while the
// first is still in progress, and gets its answer.
func (s *credentialServer) publicationOp(request *registry.CredentialRequest) {
	id, op, payload := request.ID, request.Op, request.Payload
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		var answer publicationAnswer
		switch op {
		case registry.CredentialOpResolve:
			answer = s.publisher.resolve(s.ctx, payload)
		case registry.CredentialOpOpen:
			answer = s.publisher.open(s.ctx, payload)
		case registry.CredentialOpRelease:
			answer = s.publisher.release(s.ctx, payload)
		}
		switch {
		case s.ctx.Err() != nil:
			return // the session ended first; nobody reads this answer
		case answer.refusal != nil:
			s.refuse(id, answer.refusal.Code, answer.refusal.Message)
		default:
			s.answerOp(id, op, answer.payload)
		}
	}()
}

// shared runs resolve for purpose unless an exchange for it is already in
// progress, in which case it waits for that exchange's answer. ok is false
// when the session ended first.
func (s *credentialServer) shared(purpose string, resolve func(context.Context) credentialAnswer) (credentialAnswer, bool) {
	s.flightMu.Lock()
	flight := s.flights[purpose]
	if flight == nil {
		flight = &credentialFlight{done: make(chan struct{})}
		s.flights[purpose] = flight
		s.flightMu.Unlock()
		flight.answer = resolve(s.ctx)
		s.flightMu.Lock()
		delete(s.flights, purpose)
		s.flightMu.Unlock()
		close(flight.done)
	} else {
		s.flightMu.Unlock()
		select {
		case <-flight.done:
		case <-s.ctx.Done():
		}
	}
	if s.ctx.Err() != nil {
		return credentialAnswer{}, false
	}
	return flight.answer, true
}

// resolveRead answers the read purpose from the run's install capability on a
// hosted run, else from the local source.
func (s *credentialServer) resolveRead(ctx context.Context) credentialAnswer {
	if s.runCredential != "" {
		return s.resolveHostedRead(ctx)
	}
	return s.resolveLocalRead(ctx)
}

func (s *credentialServer) resolveHostedRead(ctx context.Context) credentialAnswer {
	if !s.caller.configured() {
		s.diagnose("read credential: %s is not set to an https Delivery ingest base", SessionReporterIngestURLEnv)
		return refusal(refusalInstallUnconfigured, fmt.Sprintf("the hosted run gave no https Delivery ingest base (%s), so its install credential cannot be requested", SessionReporterIngestURLEnv))
	}
	answer := s.caller.call(ctx, installCapabilityPath, s.runCredential)
	switch answer.outcome {
	case capabilityGranted:
		credential, err := installCredential(answer.body)
		if err != nil {
			s.diagnose("read credential: Delivery answered HTTP %d with an install credential this provider cannot use: %v", answer.status, err)
			return refusal(refusalInstallUnavailable, "Delivery answered the install capability with a credential that does not follow the protocol")
		}
		return credentialAnswer{credential: credential}
	case capabilityAbsent:
		return credentialAnswer{}
	case capabilityRefused:
		code, message := capabilityRefusal(answer.body)
		if !registryRefusalCode.MatchString(code) {
			code = refusalInstallRefused
		}
		message = boundedDiagnosticText(message, registry.MaxRefusalMessageBytes, s.runCredential)
		if message == "" {
			message = fmt.Sprintf("Delivery refused the install capability (HTTP %d)", answer.status)
		}
		s.diagnose("read credential: Delivery refused the install capability (HTTP %d, %s)", answer.status, code)
		return refusal(code, message)
	default:
		if ctx.Err() != nil {
			return credentialAnswer{}
		}
		message := "Delivery did not answer the install capability"
		if answer.status != 0 {
			message = fmt.Sprintf("Delivery did not answer the install capability (last answer HTTP %d)", answer.status)
		}
		s.diagnose("read credential: %s", message)
		return refusal(refusalInstallUnavailable, message)
	}
}

// resolveLocalRead asks the local source within localTimeout. Every failure is
// absence: on a developer machine the engine's native credentials still apply.
//
// The source runs under a context that ends at the timeout or when the
// session ends, so a source that honors it stops. It holds localSlot until it
// returns, and the session waits for it before it exits.
func (s *credentialServer) resolveLocalRead(session context.Context) credentialAnswer {
	if s.local == nil {
		return credentialAnswer{}
	}
	ctx, cancel := context.WithTimeout(session, s.localTimeout)
	defer cancel()
	select {
	case s.localSlot <- struct{}{}:
	case <-ctx.Done():
		if session.Err() == nil {
			s.diagnose("read credential: an earlier read of this machine's sign-in did not stop within %s; using native credentials", s.localTimeout)
		}
		return credentialAnswer{}
	}
	type result struct {
		credential *registry.Credential
		err        error
	}
	done := make(chan result, 1)
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		defer func() { <-s.localSlot }()
		credential, err := s.local(ctx)
		done <- result{credential, err}
	}()
	var got result
	select {
	case got = <-done:
	case <-ctx.Done():
		if session.Err() == nil {
			s.diagnose("read credential: this machine's sign-in did not answer within %s; using native credentials", s.localTimeout)
		}
		return credentialAnswer{}
	}
	switch {
	case got.err != nil:
		s.diagnose("read credential: %v; using native credentials", got.err)
		return credentialAnswer{}
	case got.credential == nil:
		return credentialAnswer{}
	}
	if err := registry.ValidateCredential(*got.credential); err != nil {
		s.diagnose("read credential: this machine's credential does not follow the protocol (%v); using native credentials", err)
		return credentialAnswer{}
	}
	return credentialAnswer{credential: got.credential}
}

// installCredential reads Delivery's granted install capability leniently and
// returns it as a validated protocol credential. Hosts are lowercased, sorted
// and made unique, and expiresAt is restated as an RFC 3339 UTC instant.
func installCredential(body []byte) (*registry.Credential, error) {
	var granted struct {
		ProtocolVersion int      `json:"protocolVersion"`
		Bearer          string   `json:"bearer"`
		ExpiresAt       string   `json:"expiresAt"`
		Hosts           []string `json:"hosts"`
	}
	if err := json.Unmarshal(body, &granted); err != nil {
		return nil, errors.New("the body is not a JSON object")
	}
	if granted.ProtocolVersion != capabilityProtocolVersion {
		return nil, fmt.Errorf("protocolVersion %d is not %d", granted.ProtocolVersion, capabilityProtocolVersion)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(granted.ExpiresAt))
	if err != nil {
		return nil, errors.New("expiresAt is not an RFC 3339 instant")
	}
	hosts := make([]string, 0, len(granted.Hosts))
	for _, host := range granted.Hosts {
		hosts = append(hosts, strings.ToLower(strings.TrimSpace(host)))
	}
	slices.Sort(hosts)
	credential := &registry.Credential{
		Bearer:    granted.Bearer,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		Hosts:     slices.Compact(hosts),
	}
	if err := registry.ValidateCredential(*credential); err != nil {
		return nil, err
	}
	return credential, nil
}

var registryRefusalCode = regexp.MustCompile(registry.RefusalCodePattern)

// tokenLike matches a JWT or an api key, so a diagnostic built from another
// component's error never carries one.
var tokenLike = regexp.MustCompile(`[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}|pkt_[A-Za-z0-9_-]+`)

func refusal(code, message string) credentialAnswer {
	return credentialAnswer{refusal: &registry.CredentialRefusal{Code: code, Message: message}}
}

// diagnose writes one bounded stderr line with every known secret and every
// token-like value replaced.
func (s *credentialServer) diagnose(format string, args ...any) {
	line := tokenLike.ReplaceAllString(fmt.Sprintf(format, args...), "<redacted>")
	line = boundedDiagnosticText(line, credentialDiagnosticBytes, s.knownSecrets()...)
	fmt.Fprintln(s.logw, "putnami-cloud credential-provider: "+line) //nolint:gosec // G705: logw is the provider's stderr, never an HTTP response
}

// safeText makes text from another component fit a refusal message: every
// known secret and every token-like value replaced, bounded like a refusal.
func (s *credentialServer) safeText(text string) string {
	text = tokenLike.ReplaceAllString(text, "<redacted>")
	return boundedDiagnosticText(text, registry.MaxRefusalMessageBytes, s.knownSecrets()...)
}

// addSecret records a bearer this session must never print.
func (s *credentialServer) addSecret(secret string) {
	if secret == "" {
		return
	}
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	if !slices.Contains(s.secrets, secret) {
		s.secrets = append(s.secrets, secret)
	}
}

func (s *credentialServer) knownSecrets() []string {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	return append([]string{s.runCredential}, s.secrets...)
}

func (s *credentialServer) answer(id int64, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		s.refuse(id, refusalInvalidRequest, "the provider could not encode its answer")
		return
	}
	s.write(registry.CredentialResponse{ProtocolVersion: registry.CredentialProtocolVersion, ID: id, OK: true, Payload: encoded})
}

// answerOp answers a publication op whose answer fits the op's line bound,
// and refuses one that does not: the engine would refuse the line.
func (s *credentialServer) answerOp(id int64, op registry.CredentialOp, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		s.refuse(id, refusalInvalidRequest, "the provider could not encode its answer")
		return
	}
	response := registry.CredentialResponse{ProtocolVersion: registry.CredentialProtocolVersion, ID: id, OK: true, Payload: encoded}
	line, err := json.Marshal(response)
	if err != nil || len(line) > op.MaxLineBytes() {
		s.diagnose("%s: the answer exceeds %d bytes", op, op.MaxLineBytes())
		s.refuse(id, refusalPublicationUnavailable, fmt.Sprintf("the %s answer exceeds the protocol's line bound", op))
		return
	}
	s.write(response)
}

func (s *credentialServer) refuse(id int64, code, message string) {
	refusal := registry.CredentialRefusal{Code: code, Message: message}
	if registry.ValidateRefusal(refusal) != nil {
		refusal.Message = ""
	}
	s.write(registry.CredentialResponse{ProtocolVersion: registry.CredentialProtocolVersion, ID: id, Error: &refusal})
}

// write sends one response line. Answers come from the request loop and from
// exchange goroutines, so every write is serialized and flushed whole.
func (s *credentialServer) write(response registry.CredentialResponse) {
	encoded, err := json.Marshal(response)
	if err != nil {
		s.diagnose("cannot encode the answer to request %d", response.ID)
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.out.Write(append(encoded, '\n')); err != nil {
		return
	}
	_ = s.out.Flush()
}
