package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/url"
	"strings"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// openNegotiatedSSE runs an admitted SSE session on the negotiated wire of
// clientcontract ADR 0013.
//
// The wire ends only at an explicit terminal: `complete` ends the session
// successfully and a typed error ends it with that error. An end of body or a
// broken socket before a terminal is an interruption. When the operation
// declares reconnect, an interruption reopens the same declared SSE operation,
// in the same session, at most maxStreamResumeAttempts times. It never falls
// back to another transport and never changes mode.
//
// Cursor mode reopens after the position of the last message the caller
// received and drops the messages it had not received yet, which the provider
// sends again. Best-effort mode reopens with the original query and keeps its
// queue: nothing it drops could be asked for again.
func openNegotiatedSSE[T any](opener *sseOpener, first *sseConnection, query url.Values, reconnect bool,
	release func()) *Stream[T] {
	session := opener.session
	// The bridge stops only when the caller closes the stream. The session
	// context also ends at the declared duration and after the terminal, and
	// neither may drop a value the provider already sent.
	closed := make(chan struct{})
	delivery := newSSEDelivery[T](session.config.Budgets.MaxBufferedMessages, closed, session.Activate)
	reader := &negotiatedSSEReader[T]{
		operation:    opener.operation,
		schemas:      opener.runtime.descriptor.Schemas,
		session:      session,
		carryMessage: opener.runtime.binding.CarryRemoteMessage,
		delivery:     delivery,
	}
	if opener.continuation.Mode == clientcontract.SSEContinuationCursor {
		reader.cursor = opener.continuation.Cursor
	}
	done := make(chan struct{})
	// Stream.Close runs cancel at most once.
	cancel := func() {
		close(closed)
		session.Cancel()
	}
	stream := &Stream[T]{messages: delivery.out, done: done, cancel: cancel}
	go func() {
		terminal := runNegotiatedSSE(opener, reader, first, query, reconnect)
		if terminal != nil {
			stream.setErr(session.Fail(terminal))
		} else {
			session.Complete()
		}
		// The terminal is decided, so Done and Err answer now, and the
		// session emits its single measurement and leaves the registry
		// without waiting for the caller. The values queued before the
		// terminal still reach the caller, in order, until it has taken them
		// all or closes the stream; then Messages closes.
		close(done)
		session.Close() //nolint:errcheck // Close always returns nil; the terminal error reaches the caller through Stream.Err
		release()
		delivery.finish()
		close(delivery.out)
	}()
	return stream
}

// runNegotiatedSSE reads the session's connections in turn and returns its
// single terminal: nil for `complete`, the error otherwise.
func runNegotiatedSSE[T any](opener *sseOpener, reader *negotiatedSSEReader[T], connection *sseConnection,
	query url.Values, reconnect bool) error {
	continuations := 0
	for {
		interrupted, err := reader.read(opener.ctx, connection.response.Body, connection.applied.secrets)
		_ = connection.response.Body.Close() //nolint:errcheck // the connection's outcome is authoritative
		if !interrupted {
			return err
		}
		if opener.ctx.Err() != nil || opener.session.idleBudgetExpired() {
			return sessionEndError(opener.ctx, opener.session)
		}
		// The sixth break ends the session with the break as its error.
		if !reconnect || continuations >= maxStreamResumeAttempts {
			return err
		}
		continuations++
		delivered := ""
		if reader.cursor != nil {
			position, live := reader.delivery.reset()
			if !live {
				return sessionEndError(opener.ctx, opener.session)
			}
			delivered = position
		}
		next, openErr := opener.open(clientcontract.SSEReopenQuery(*opener.continuation, query, delivered), true)
		if openErr != nil {
			// The idle budget and the declared duration keep running while a
			// reopening dials; when one of them ended the session, it is the
			// reason, not the handshake it interrupted.
			if opener.ctx.Err() != nil {
				return sessionEndError(opener.ctx, opener.session)
			}
			return openErr
		}
		connection = next
	}
}

// sessionEndError names why a session that is no longer live ended: its idle
// budget, or the caller or the declared duration through its context.
func sessionEndError(ctx context.Context, session *StreamSession) error {
	if session.idleBudgetExpired() {
		return errors.New(CodeClientDeadline, "service stream idle timeout")
	}
	return normalizedCallError(ctx, ctx.Err())
}

// errSSEInterrupted is the terminal of a negotiated stream whose connection
// ended before a terminal event and that is not continued.
func errSSEInterrupted() error {
	return errors.New(CodeClientResponse, "service stream was interrupted before its terminal event")
}

// negotiatedSSEReader reads connections of the negotiated wire.
type negotiatedSSEReader[T any] struct {
	operation Operation
	schemas   map[string]clientcontract.Schema
	session   *StreamSession
	// cursor is the declared position carrier in cursor mode, nil in
	// best-effort mode.
	cursor       *clientcontract.SSECursor
	carryMessage bool
	delivery     *sseDelivery[T]
}

// read reads one connection. It reports an interruption — the connection
// ended before a terminal while the session was live, the one outcome a
// continuation may follow — or the connection's terminal: nil for `complete`,
// the typed or contract error otherwise.
//
// An event the break cut short is discarded: it was never delivered, so the
// position does not include it. A complete event that is malformed, outside
// the negotiated vocabulary, oversize, or without a valid position is a
// contract error and never an interruption.
func (reader *negotiatedSSEReader[T]) read(ctx context.Context, body io.Reader, secrets []string) (bool, error) {
	budgets := reader.session.config.Budgets
	buffered := bufio.NewReaderSize(body, int(min(budgets.MaxFrameBytes, 64<<10)))
	var data bytes.Buffer
	eventType := ""
	eventBytes := int64(0)
	for {
		line, err := readBoundedSSELine(buffered, budgets.MaxFrameBytes-eventBytes, reader.session.touchIdle)
		if stderrors.Is(err, errSSEFrameTooLarge) {
			return false, errors.New(CodeClientResponse, "service stream frame exceeds maximum size")
		}
		eventBytes += int64(len(line))
		if len(line) > 0 {
			trimmed := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
			switch {
			case trimmed == "" && bytes.HasSuffix(line, []byte("\n")):
				if terminal, dispatchErr := reader.dispatch(ctx, eventType, &data, secrets); terminal {
					return false, dispatchErr
				}
				data.Reset()
				eventType = ""
				eventBytes = 0
			case strings.HasPrefix(trimmed, ":"):
				// A comment keeps the connection alive and never advances the
				// position.
			case strings.HasPrefix(trimmed, "event:"):
				eventType = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			case strings.HasPrefix(trimmed, "data:"):
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " "))
				data.WriteByte('\n')
			}
		}
		if reader.session.idleBudgetExpired() {
			return false, errors.New(CodeClientDeadline, "service stream idle timeout")
		}
		if err != nil {
			if ctx.Err() != nil {
				return false, normalizedCallError(ctx, err)
			}
			return true, errSSEInterrupted()
		}
	}
}

// dispatch acts on one complete event block. It reports whether the event
// ended the connection and, if so, with which error (nil for `complete`).
func (reader *negotiatedSSEReader[T]) dispatch(ctx context.Context, eventType string, data *bytes.Buffer,
	secrets []string) (bool, error) {
	if data.Len() == 0 {
		// A comment-only block keeps the connection alive. A typed block with
		// no data — `complete` without its payload among them — is outside the
		// closed vocabulary.
		if eventType != "" {
			return true, errors.New(CodeClientResponse, "service stream event carries no data")
		}
		return false, nil
	}
	payload := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
	kind, err := clientcontract.ClassifySSEEvent(eventType, string(payload), true)
	switch {
	case err != nil:
		// The classification names what the provider sent; the caller reads a
		// fixed message, like every other contract error of this reader.
		return true, errors.New(CodeClientResponse, "service stream event is outside the negotiated wire vocabulary")
	case kind == clientcontract.SSEEventKindComplete:
		return true, nil
	case kind == clientcontract.SSEEventKindError:
		return true, decodeSSEError(payload, reader.operation, reader.schemas, secrets, reader.carryMessage)
	}
	projected, ok := projectResponseJSON(payload, reader.operation.Contract.Messages.Output, reader.schemas, nil, false)
	if !ok {
		return true, errors.New(CodeClientResponse, "service stream message does not match the generated contract")
	}
	var message T
	if decodeErr := json.Unmarshal(projected, &message); decodeErr != nil {
		return true, errors.New(CodeClientResponse, "service stream message cannot be decoded")
	}
	position := ""
	if reader.cursor != nil {
		// The position is copied verbatim: never parsed, compared or
		// incremented. A message that cannot say where it is ends the stream,
		// because a continuation after it could not be placed.
		value, positionErr := clientcontract.SSECursorValue(payload, reader.cursor.OutputField)
		if positionErr != nil {
			return true, errors.New(CodeClientResponse, "service stream message carries no valid position in its declared cursor field")
		}
		position = value
	}
	if err := reader.delivery.enqueue(ctx, reader.session, ssePending[T]{value: message, position: position}); err != nil {
		return true, err
	}
	return false, nil
}

// ssePending is one decoded message waiting for the caller, with the position
// it carries (empty in best-effort mode).
type ssePending[T any] struct {
	value    T
	position string
}

// sseDelivery is the delivery bridge of one negotiated SSE session.
//
// The reader decodes into a bounded internal queue. One goroutine offers the
// head of that queue to the caller over the unbuffered public channel, so a
// completed send is an observable handoff: the caller has taken the value.
// Only then is the value delivered, and only then does its position become
// the one a continuation resumes after. A send into a buffered channel could
// not say whether the caller had taken anything.
//
// The queue and the value being offered together hold at most
// MaxBufferedMessages values, the bound the legacy buffered channel has.
type sseDelivery[T any] struct {
	out      chan T
	queue    chan ssePending[T]
	resets   chan chan string
	stop     <-chan struct{}
	exited   chan struct{}
	activate func()
	// delivered is the position of the last value the caller took. Only the
	// handoff goroutine touches it; reset hands it over.
	delivered string
}

// newSSEDelivery starts the handoff goroutine. stop ends it, dropping what the
// caller has not taken: it is the caller closing the stream, never the end of
// the session, so a value sent before the terminal stays readable after it.
// activate runs after every completed handoff.
func newSSEDelivery[T any](capacity int, stop <-chan struct{}, activate func()) *sseDelivery[T] {
	delivery := &sseDelivery[T]{
		out: make(chan T),
		// The value being offered is the last slot of the bound.
		queue:    make(chan ssePending[T], max(capacity-1, 0)),
		resets:   make(chan chan string),
		stop:     stop,
		exited:   make(chan struct{}),
		activate: activate,
	}
	go delivery.run()
	return delivery
}

func (delivery *sseDelivery[T]) run() {
	defer close(delivery.exited)
	var head *ssePending[T]
	for {
		// A nil channel disables its case: the queue is read only when no
		// value is being offered, and the caller is offered one only when
		// there is one.
		queue, out := delivery.queue, chan T(nil)
		var value T
		if head != nil {
			queue, out, value = nil, delivery.out, head.value
		}
		select {
		case pending, ok := <-queue:
			if !ok {
				return
			}
			head = &pending
		case out <- value:
			if head.position != "" {
				delivery.delivered = head.position
			}
			head = nil
			delivery.activate()
		case reply := <-delivery.resets:
			head = nil
			delivery.discardQueued()
			reply <- delivery.delivered
		case <-delivery.stop:
			return
		}
	}
}

// discardQueued empties the queue. Only reset calls it, while the reader that
// fills the queue waits for its answer, so nothing is added meanwhile.
func (delivery *sseDelivery[T]) discardQueued() {
	for {
		select {
		case <-delivery.queue:
		default:
			return
		}
	}
}

// enqueue queues one decoded value. A full queue blocks the reader rather than
// dropping a value; the session's end and its idle budget still release it.
func (delivery *sseDelivery[T]) enqueue(ctx context.Context, session *StreamSession, pending ssePending[T]) error {
	select {
	case delivery.queue <- pending:
		return nil
	case <-ctx.Done():
		return sessionEndError(ctx, session)
	case <-session.idleExpired():
		return errors.New(CodeClientDeadline, "service stream idle timeout")
	}
}

// reset settles, before a cursor-mode reopening, every value the caller has
// not taken: it drops them, the one being offered included, and returns the
// position of the last value the caller did take. The reopened connection asks
// for everything after that position, so a dropped value is sent again and
// none arrives twice. The handoff goroutine performs the reset itself, between
// two sends, so no send can complete unrecorded while it runs.
//
// It reports false when the caller has already closed the stream.
func (delivery *sseDelivery[T]) reset() (string, bool) {
	reply := make(chan string, 1)
	select {
	case delivery.resets <- reply:
		return <-reply, true
	case <-delivery.exited:
		return "", false
	}
}

// finish hands the caller what is still queued, in order, and returns once it
// has taken everything or has closed the stream. The reader calls it once,
// after its last enqueue; the public channel is its owner's to close after it.
func (delivery *sseDelivery[T]) finish() {
	close(delivery.queue)
	<-delivery.exited
}
