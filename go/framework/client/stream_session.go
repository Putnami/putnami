package client

import (
	"context"
	stderrors "errors"
	"sync"
	"time"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// StreamPhase is one of the five phases a stream session traverses, in order
// and without going back: connecting, admitted, active, terminal, closed. A
// session always reaches StreamPhaseClosed whatever path it takes.
type StreamPhase uint8

const (
	// StreamPhaseConnecting covers everything up to and including admission:
	// credential resolution, the handshake, and the transport's acceptance
	// check. Nothing has been delivered to the caller yet.
	StreamPhaseConnecting StreamPhase = iota
	// StreamPhaseAdmitted means the provider accepted the operation. For SSE
	// that is a 2xx response carrying the declared content type; for WebSocket
	// it is the ready frame; for Connect the accepted response headers.
	StreamPhaseAdmitted
	// StreamPhaseActive means the first provider message reached the caller.
	StreamPhaseActive
	// StreamPhaseTerminal means exactly one of complete, error or cancel has
	// been decided. No further terminal is recorded.
	StreamPhaseTerminal
	// StreamPhaseClosed means the session is canceled, its budgets released
	// and its single call measurement emitted.
	StreamPhaseClosed
)

// String names the phase for diagnostics.
func (phase StreamPhase) String() string {
	switch phase {
	case StreamPhaseConnecting:
		return "connecting"
	case StreamPhaseAdmitted:
		return "admitted"
	case StreamPhaseActive:
		return "active"
	case StreamPhaseTerminal:
		return "terminal"
	case StreamPhaseClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// StreamBudgets are the four session bounds plus the declared operation
// duration. Handshake bounds connection opening through admission, Idle bounds
// the interval between two provider frames after admission, MaxFrameBytes
// bounds one frame or event, and MaxBufferedMessages bounds the queue of
// messages the caller has not consumed. Session is the declared operation
// duration (`resilience.timeoutMs`) and leaves the session unbounded in time
// when it is zero; the other budgets still apply.
type StreamBudgets struct {
	Handshake           time.Duration
	Idle                time.Duration
	Session             time.Duration
	MaxFrameBytes       int64
	MaxBufferedMessages int
}

// streamSessionConfig is everything a session needs to own a stream lifecycle
// without knowing which transport carries it. Every hook is optional: a nil
// hook means the session skips that concern rather than substituting a
// permissive default.
type streamSessionConfig struct {
	ServiceID   string
	OperationID string
	Protocol    clientcontract.TransportProtocol
	Idempotency clientcontract.IdempotencyKind
	Budgets     StreamBudgets
	Breaker     *CircuitBreaker
	Telemetry   ServiceCallTelemetry
	// Credentials resolves the credentials for the next frame or request and
	// applies them to the transport's outgoing request. It is called again at
	// the send point, so it must be safe to call more than once.
	Credentials func(context.Context) (appliedCredentials, error)
	// Invalidate drops the service credentials the last resolution applied.
	Invalidate func(appliedCredentials)
	// MapError projects a terminal error through the generated error mapper.
	MapError func(error) error
}

// StreamSession owns one stream lifecycle: the five phases, the four budgets,
// the single breaker observation, the credential re-check at the send point,
// the single terminal, and the single call measurement. Every first-party
// stream transport drives the same session instead of restating those rules.
type StreamSession struct {
	config streamSessionConfig
	caller context.Context
	ctx    context.Context
	cancel context.CancelFunc

	mu                  sync.Mutex
	phase               StreamPhase
	admitted            bool
	dispatched          bool
	attempts            int
	lastStatus          int
	authDuration        time.Duration
	handshakeDeadlineAt time.Time
	credentials         appliedCredentials
	invalidated         bool
	err                 error

	idleActivity  chan struct{}
	idleExpiredCh chan struct{}
	idleStop      chan struct{}
	idleStart     sync.Once
	idleRelease   sync.Once

	finishCall func(ServiceCallResult)
	closeOnce  sync.Once
}

// newStreamSession returns the session and the context every attempt derives
// from. The session owns cancellation: the returned context is canceled when
// the session is closed, when the declared operation duration expires, or when
// the idle budget expires after admission.
func newStreamSession(ctx context.Context, config streamSessionConfig) (*StreamSession, context.Context) {
	session := &StreamSession{config: config, caller: ctx, phase: StreamPhaseConnecting}
	if config.Budgets.Session > 0 {
		session.ctx, session.cancel = context.WithTimeout(ctx, config.Budgets.Session)
	} else {
		session.ctx, session.cancel = context.WithCancel(ctx)
	}
	if config.Telemetry != nil {
		session.ctx, session.finishCall = config.Telemetry.StartServiceCall(session.ctx, session.callInfo())
	}
	if config.Budgets.Idle > 0 {
		session.idleActivity = make(chan struct{}, 1)
		session.idleExpiredCh = make(chan struct{})
		session.idleStop = make(chan struct{})
	}
	return session, session.ctx
}

func (session *StreamSession) callInfo() ServiceCallInfo {
	return ServiceCallInfo{
		ServiceID:   session.config.ServiceID,
		OperationID: session.config.OperationID,
		Protocol:    string(session.config.Protocol),
	}
}

// Phase reports the current phase.
func (session *StreamSession) Phase() StreamPhase {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.phase
}

// Context is the session context every attempt derives from.
func (session *StreamSession) Context() context.Context {
	return session.ctx
}

// Cancel asks the session to stop without deciding its terminal. The component
// that owns the terminal — the transport's reader — still calls Fail or
// Complete and then Close, so the call measurement keeps the terminal code
// instead of reporting an empty one.
func (session *StreamSession) Cancel() {
	session.cancel()
}

// beginAttempt starts one attempt and returns its context plus the single
// finisher for that attempt. The attempt context is bounded by the handshake
// budget; after admission the session, not the attempt, bounds the stream.
func (session *StreamSession) beginAttempt() (context.Context, func(ServiceAttemptResult)) {
	session.mu.Lock()
	session.attempts++
	attempt := session.attempts
	session.mu.Unlock()

	var attemptCtx context.Context
	var attemptCancel context.CancelFunc
	if session.config.Budgets.Handshake > 0 {
		attemptCtx, attemptCancel = context.WithTimeout(session.ctx, session.config.Budgets.Handshake)
	} else {
		attemptCtx, attemptCancel = context.WithCancel(session.ctx)
	}
	deadline, _ := attemptCtx.Deadline()
	session.mu.Lock()
	session.handshakeDeadlineAt = deadline
	session.mu.Unlock()

	var finishAttempt func(ServiceAttemptResult)
	if session.config.Telemetry != nil {
		attemptCtx, finishAttempt = session.config.Telemetry.StartServiceAttempt(attemptCtx, ServiceAttemptInfo{
			ServiceCallInfo: session.callInfo(), Attempt: attempt,
		})
	}
	var once sync.Once
	return attemptCtx, func(result ServiceAttemptResult) {
		once.Do(func() {
			session.mu.Lock()
			session.lastStatus = result.StatusCode
			session.mu.Unlock()
			if finishAttempt != nil {
				finishAttempt(result)
			}
			attemptCancel()
		})
	}
}

// handshakeDeadline is the absolute instant the handshake budget expires. It
// is the zero time when neither the handshake budget nor the declared
// operation duration bounds the current attempt.
func (session *StreamSession) handshakeDeadline() time.Time {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.handshakeDeadlineAt
}

// StreamCredentials is the credential material one resolution applied to the
// outgoing request: the secret values to redact from any error payload, and
// the service credentials to invalidate when the provider rejects them.
type StreamCredentials = appliedCredentials

// Credentials resolves, and re-resolves when an asynchronous acquisition has
// expired, the credentials for the next frame or request.
//
// The resolution runs twice. The first pass is the acquisition; it may block on
// a credential source long enough for the value it returns to be past its
// expiry by the time the session reaches the send point. The second pass is
// that send-point state re-check: the session keeps no expiry clock of its own
// — the credential manager owns freshness — so the re-check asks for the
// credentials again, which returns the held value while it is still fresh and
// acquires a new one when it is not. A resolution that never blocked answers
// the second pass from the same held value, so nothing is acquired twice.
func (session *StreamSession) Credentials(ctx context.Context) (StreamCredentials, error) {
	if session.config.Credentials == nil {
		return appliedCredentials{}, nil
	}
	started := time.Now()
	applied, err := session.config.Credentials(ctx)
	if err == nil {
		applied, err = session.config.Credentials(ctx)
	}
	elapsed := time.Since(started)
	session.mu.Lock()
	session.authDuration += elapsed
	if err == nil {
		session.credentials = applied
	}
	session.mu.Unlock()
	return applied, err
}

// AuthDuration is the total time this session spent resolving credentials,
// including the send-point re-check.
func (session *StreamSession) AuthDuration() time.Duration {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.authDuration
}

// invalidateCredentials drops the resolved service credential exactly once. A
// provider that answers 401 or 403 at admission or after invalidates the
// credential and never triggers a replay.
func (session *StreamSession) invalidateCredentials() {
	session.mu.Lock()
	applied := session.credentials
	already := session.invalidated
	session.invalidated = true
	session.mu.Unlock()
	if already || session.config.Invalidate == nil {
		return
	}
	session.config.Invalidate(applied)
}

// Admit moves Connecting to Admitted and records the single breaker success.
// Calling it twice is a no-op. Admission is the protocol act by which the
// provider accepts the operation, never the first application message.
func (session *StreamSession) Admit() {
	session.mu.Lock()
	if session.phase != StreamPhaseConnecting {
		session.mu.Unlock()
		return
	}
	session.phase = StreamPhaseAdmitted
	session.admitted = true
	breaker := session.config.Breaker
	session.mu.Unlock()
	if breaker != nil {
		breaker.OnSuccess()
	}
	session.startIdle()
}

// Dispatch records that an attempt has left for the provider. Before it, a
// pre-admission failure is local — configuration, credential acquisition, or
// the caller — and writes nothing to the breaker, because nothing about the
// provider's availability has been observed.
func (session *StreamSession) Dispatch() {
	session.mu.Lock()
	session.dispatched = true
	session.mu.Unlock()
}

// Activate moves Admitted to Active on the first delivered message.
func (session *StreamSession) Activate() {
	session.mu.Lock()
	if session.phase == StreamPhaseAdmitted {
		session.phase = StreamPhaseActive
	}
	session.mu.Unlock()
}

// touchIdle resets the idle budget.
func (session *StreamSession) touchIdle() {
	if session.idleActivity == nil {
		return
	}
	select {
	case session.idleActivity <- struct{}{}:
	default:
	}
}

// idleExpired closes when the idle budget expires after admission. It is nil
// when no idle budget is declared, so a select on it never fires.
func (session *StreamSession) idleExpired() <-chan struct{} {
	return session.idleExpiredCh
}

// idleBudgetExpired reports whether the idle budget has already expired.
func (session *StreamSession) idleBudgetExpired() bool {
	if session.idleExpiredCh == nil {
		return false
	}
	select {
	case <-session.idleExpiredCh:
		return true
	default:
		return false
	}
}

func (session *StreamSession) startIdle() {
	if session.idleExpiredCh == nil {
		return
	}
	session.idleStart.Do(func() { go session.watchIdle() })
}

func (session *StreamSession) watchIdle() {
	timer := time.NewTimer(session.config.Budgets.Idle)
	defer timer.Stop()
	for {
		select {
		case <-session.idleActivity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(session.config.Budgets.Idle)
		case <-timer.C:
			close(session.idleExpiredCh)
			// Canceling the session releases the transport read that the idle
			// budget just declared dead; the reader still reports the typed
			// idle deadline rather than a cancellation.
			session.cancel()
			return
		case <-session.idleStop:
			return
		}
	}
}

func (session *StreamSession) stopIdle() {
	if session.idleStop == nil {
		return
	}
	session.idleRelease.Do(func() { close(session.idleStop) })
}

// Fail records the sanitized terminal error and applies the phase breaker
// rule. It returns the error the caller must surface. A second terminal is
// ignored and the first one is returned.
func (session *StreamSession) Fail(err error) error {
	if err == nil {
		return nil
	}
	mapped := err
	if session.config.MapError != nil {
		mapped = session.config.MapError(err)
	}
	session.mu.Lock()
	phase := session.phase
	if phase == StreamPhaseTerminal || phase == StreamPhaseClosed {
		first := session.err
		session.mu.Unlock()
		if first != nil {
			return first
		}
		return mapped
	}
	session.phase = StreamPhaseTerminal
	session.err = mapped
	breaker := session.config.Breaker
	session.mu.Unlock()
	session.stopIdle()
	// After admission the session writes nothing to the breaker: the provider
	// accepted, so a mid-stream break is a session fact, not an availability
	// fact.
	if breaker != nil && phase == StreamPhaseConnecting {
		session.recordPreAdmissionFailure(breaker, err)
	}
	return mapped
}

// recordPreAdmissionFailure applies the pre-admission breaker rule: a rejected
// request records nothing at all, a failure that never reached the provider or
// that the caller caused releases the probe without a verdict, a provider
// answer the declared circuit policy does not count as a failure records a
// success, and everything else records a failure.
func (session *StreamSession) recordPreAdmissionFailure(breaker *CircuitBreaker, err error) {
	session.mu.Lock()
	dispatched := session.dispatched
	session.mu.Unlock()
	var remote *RemoteError
	switch {
	case errors.Is(err, CodeCircuitOpen):
		// The breaker rejected the request, so it never admitted a probe to
		// release and never saw an outcome to record.
	case !dispatched || session.callerDone() || errors.Is(err, CodeClientCanceled):
		breaker.onIgnored()
	case stderrors.As(err, &remote) && !breaker.isFailureStatus(remote.StatusCode):
		breaker.OnSuccess()
	default:
		breaker.OnFailure()
	}
}

func (session *StreamSession) callerDone() bool {
	return session.caller != nil && session.caller.Err() != nil
}

// Complete records the successful terminal.
func (session *StreamSession) Complete() {
	session.mu.Lock()
	if session.phase == StreamPhaseTerminal || session.phase == StreamPhaseClosed {
		session.mu.Unlock()
		return
	}
	preAdmission := session.phase == StreamPhaseConnecting
	session.phase = StreamPhaseTerminal
	breaker := session.config.Breaker
	session.mu.Unlock()
	session.stopIdle()
	if preAdmission && breaker != nil {
		breaker.OnSuccess()
	}
}

// Close moves to Closed, cancels the session and emits the single call
// measurement. It is idempotent. Closing a session that has no terminal yet —
// a caller close, or the service registry stopping — records the cancel
// terminal first, so the breaker probe and the idle watch are released and the
// measurement carries the terminal code.
func (session *StreamSession) Close() error {
	session.closeOnce.Do(func() {
		session.mu.Lock()
		pending := session.phase != StreamPhaseTerminal
		session.mu.Unlock()
		if pending {
			session.Fail(errors.New(CodeClientCanceled, "service stream was closed")) //nolint:errcheck // the recorded terminal is read back from Err
		}
		session.mu.Lock()
		session.phase = StreamPhaseClosed
		result := ServiceCallResult{Attempts: session.attempts, Code: serviceErrorCode(session.err)}
		if session.admitted {
			result.StatusCode = session.lastStatus
		}
		session.mu.Unlock()
		session.stopIdle()
		session.cancel()
		if session.finishCall != nil {
			session.finishCall(result)
		}
	})
	return nil
}

// Err returns the sanitized terminal error, if any.
func (session *StreamSession) Err() error {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.err
}
