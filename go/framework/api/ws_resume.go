package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"sync"
	"sync/atomic"
	"time"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// Bounds this provider applies to every resume grant it issues. They are
// framework ceilings, not defaults that enable anything: a stream resumes only
// because its endpoint declared resume.
const (
	// defaultWebSocketResumeTTL is how long a grant stays redeemable. A
	// consumer that reconnects later starts a fresh stream instead of being
	// handed a position nobody can still prove is gap-free.
	defaultWebSocketResumeTTL = 60 * time.Second
	// defaultWebSocketResumeBudget bounds how many times one stream may be
	// continued. Without it a socket that keeps breaking keeps one position
	// alive forever.
	defaultWebSocketResumeBudget = 5
	// defaultWebSocketResumeGrants bounds the live grants of one endpoint. The
	// oldest is dropped first, so a flood of abandoned streams cannot grow this
	// map without limit.
	defaultWebSocketResumeGrants = 256
	// webSocketResumeTokenBytes is the entropy of one token. The token is the
	// only thing standing between a stolen position and a replayed stream, so
	// it is generated, never derived from anything a peer can see.
	webSocketResumeTokenBytes = 32
)

// webSocketResumeGrant is one live permission to continue one stream.
//
// It is bound to the operation and the client identity that earned it, spent by
// its single redemption, and replaced by a fresh grant on every ready frame.
// That rotation is what makes an observed token useless after the connection
// that carried it ends.
type webSocketResumeGrant struct {
	operationID string
	clientID    string
	// cursor is the highest sequence this provider has put on the wire for the
	// stream this grant continues. A consumer cannot ask to continue past it.
	cursor atomic.Uint64
	// budget is the number of continuations still allowed on this stream.
	budget  int
	expires time.Time
}

// webSocketResumeStore holds the live resume grants of one stream endpoint.
//
// A grant lives in memory only: a provider that restarts cannot prove any
// position is still gap-free, so it refuses the continuation and the consumer
// learns the stream ended instead of silently receiving a second copy.
type webSocketResumeStore struct {
	mu     sync.Mutex
	grants map[string]*webSocketResumeGrant
	order  []string
	// now is the clock. Tests drive it so a bound is proved by expiry, not by
	// waiting for one.
	now func() time.Time
	// mint generates one token. Tests replace it to observe rotation exactly.
	mint func() (string, error)
}

func newWebSocketResumeStore() *webSocketResumeStore {
	return &webSocketResumeStore{
		grants: map[string]*webSocketResumeGrant{},
		now:    time.Now,
		mint:   newWebSocketResumeToken,
	}
}

// newWebSocketResumeToken returns one unguessable, URL-safe token.
func newWebSocketResumeToken() (string, error) {
	raw := make([]byte, webSocketResumeTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// issue mints the grant one ready frame carries. budget is the number of
// continuations the new grant still allows, so a resumed stream inherits what
// the grant it spent had left rather than starting over.
func (store *webSocketResumeStore) issue(operationID, clientID string, cursor uint64,
	budget int) (string, *webSocketResumeGrant, error) {
	if budget <= 0 {
		return "", nil, nil
	}
	token, err := store.mint()
	if err != nil {
		return "", nil, err
	}
	grant := &webSocketResumeGrant{
		operationID: operationID, clientID: clientID,
		budget: budget, expires: store.now().Add(defaultWebSocketResumeTTL),
	}
	grant.cursor.Store(cursor)
	store.mu.Lock()
	defer store.mu.Unlock()
	store.evictExpiredLocked()
	for len(store.order) >= defaultWebSocketResumeGrants {
		oldest := store.order[0]
		store.order = store.order[1:]
		delete(store.grants, oldest)
	}
	store.grants[token] = grant
	store.order = append(store.order, token)
	return token, grant, nil
}

// redeem spends one grant. A grant is single use: the token that opened this
// connection can never open another, whoever holds it.
func (store *webSocketResumeStore) redeem(token, operationID, clientID string,
	afterSequence uint64) (*webSocketResumeGrant, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.evictExpiredLocked()
	grant, live := store.grants[token]
	if !live {
		return nil, contractRefusal(clientcontract.ErrorCodeInvalidResilience,
			"resume token is unknown, expired or already spent")
	}
	delete(store.grants, token)
	for index, candidate := range store.order {
		if candidate == token {
			store.order = append(store.order[:index], store.order[index+1:]...)
			break
		}
	}
	// The grant names the operation and the identity that earned it. A token
	// presented on another operation, or by another client, buys nothing.
	if grant.operationID != operationID || grant.clientID != clientID {
		return nil, contractRefusal(clientcontract.ErrorCodeInvalidResilience,
			"resume token was not issued for this operation and client")
	}
	if grant.budget <= 0 {
		return nil, contractRefusal(clientcontract.ErrorCodeInvalidResilience,
			"resume budget for this stream is exhausted")
	}
	// A consumer cannot claim to have received more than this provider sent:
	// continuing past the cursor would skip values nobody delivered.
	if afterSequence > grant.cursor.Load() {
		return nil, contractRefusal(clientcontract.ErrorCodeInvalidResilience,
			"resume position is ahead of the sequence this provider delivered")
	}
	return grant, nil
}

// evictExpiredLocked drops grants past their TTL. The caller holds the lock.
func (store *webSocketResumeStore) evictExpiredLocked() {
	now := store.now()
	live := store.order[:0]
	for _, token := range store.order {
		grant, present := store.grants[token]
		if !present {
			continue
		}
		if now.After(grant.expires) {
			delete(store.grants, token)
			continue
		}
		live = append(live, token)
	}
	store.order = live
}

// streamResumeContextKey carries the position a resumed stream continues after.
type streamResumeContextKey struct{}

// withStreamResume returns a context that reports the stream continues after
// the given sequence. The framework sets it on an admitted resumed stream; a
// handler reads it with StreamResumeFrom.
func withStreamResume(ctx context.Context, afterSequence uint64) context.Context {
	return context.WithValue(ctx, streamResumeContextKey{}, afterSequence)
}

// StreamResumeFrom reports the sequence a resumed server stream continues
// after. It is false for a fresh stream, so a handler that ignores it produces
// the whole stream, and a handler that reads it produces only what the consumer
// has not received.
func StreamResumeFrom(ctx context.Context) (uint64, bool) {
	value, ok := ctx.Value(streamResumeContextKey{}).(uint64)
	return value, ok
}

// resumeDeclared reports whether this endpoint's published transport states the
// stream can be continued without a gap.
func (s *webSocketService) resumeDeclared() bool {
	return s.transport.WebSocket != nil && s.transport.WebSocket.Resume
}

// admitResume redeems the grant an init frame presents and reports the position
// the stream continues after. It runs before this provider's security chain:
// a token that buys nothing must not cost a credential check.
func (s *webSocketSession) admitResume(frame *clientcontract.WebSocketInitFrameV1) (uint64, error) {
	if frame.Resume == nil {
		return 0, nil
	}
	if !s.service.resumeDeclared() {
		return 0, contractRefusal(clientcontract.ErrorCodeInvalidResilience,
			"selected websocket transport does not support resume")
	}
	if s.service.stream != clientcontract.StreamServer {
		return 0, contractRefusal(clientcontract.ErrorCodeInvalidResilience,
			"only a server stream can be resumed")
	}
	after, ok := webSocketSequence(frame.Resume.AfterSequence)
	if !ok {
		return 0, contractRefusal(clientcontract.ErrorCodeInvalidTransport,
			"resume position is not a sequence this provider issued")
	}
	grant, err := s.service.resume.redeem(frame.Resume.Token, s.service.operationID, frame.ClientID, after)
	if err != nil {
		return 0, err
	}
	s.resumeBudget = grant.budget - 1
	return after, nil
}

// issueResumeGrant mints the token the ready frame carries and binds it to this
// session, so every message this provider sends moves the position the token
// buys. A stream the endpoint did not declare resumable issues nothing.
func (s *webSocketSession) issueResumeGrant(clientID string, cursor uint64) (string, error) {
	if !s.service.resumeDeclared() || s.service.stream != clientcontract.StreamServer {
		return "", nil
	}
	token, grant, err := s.service.resume.issue(s.service.operationID, clientID, cursor, s.resumeBudget)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.grant = grant
	s.mu.Unlock()
	return token, nil
}
