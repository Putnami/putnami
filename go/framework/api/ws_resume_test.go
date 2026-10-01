package api

import (
	"strconv"
	"sync"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// historyFeedMessages is how many revisions one connection of the test feed
// delivers. Two is enough to see a sequence carry on across a continuation.
const historyFeedMessages = 2

// resumableWatchEndpoint declares the one endpoint these tests are about: a
// safe server stream whose provider states it can be continued without a gap.
func resumableWatchEndpoint(handle func(*ServerStreamContext[wsEvent]) error) EndpointDefinition {
	return Endpoint("GET", "/watch").
		Returns(StreamOf[wsEvent]()).
		Client(ClientOperationOptions{Resume: true}).
		Handle(ServerStream(handle))
}

// countingWatch sends two events numbered from the position the stream
// continues after, so a gap and a duplicate are both visible in the sequences.
func countingWatch(stream *ServerStreamContext[wsEvent]) error {
	from, _ := StreamResumeFrom(stream.Context.Context())
	for offset := uint64(1); offset <= historyFeedMessages; offset++ {
		if err := stream.Send(wsEvent{ID: "event-" + strconv.FormatUint(from+offset, 10), Payload: []byte{}}); err != nil {
			return err
		}
	}
	return nil
}

// wsResumeReady reads the admission frame of one connection.
func wsResumeReady(t *testing.T, peer *wsPeer) *clientcontract.WebSocketReadyFrameV1 {
	t.Helper()
	ready, ok := peer.expect(clientcontract.WebSocketFrameReady).(*clientcontract.WebSocketReadyFrameV1)
	if !ok {
		t.Fatal("the provider did not admit the conversation")
	}
	return ready
}

// wsResumeSequences reads the two messages one connection of the feed delivers
// and returns their sequences and identifiers.
func wsResumeSequences(t *testing.T, peer *wsPeer) ([]string, []string) {
	t.Helper()
	sequences := make([]string, 0, historyFeedMessages)
	identifiers := make([]string, 0, historyFeedMessages)
	for range historyFeedMessages {
		message, ok := peer.expect(clientcontract.WebSocketFrameMessage).(*clientcontract.WebSocketMessageFrameV1)
		if !ok {
			t.Fatal("the provider did not deliver a message")
		}
		sequences = append(sequences, message.Sequence)
		identifiers = append(identifiers, wsDecode[wsEvent](t, message.Payload).ID)
	}
	return sequences, identifiers
}

// A provider that declares a resumable server stream issues a grant with its
// admission, honors it once, and continues the stream where the consumer left:
// the sequences carry on, and neither a value nor a number is repeated.
func TestWebSocketProvider_ContinuesAResumedStreamWithoutAGapOrADuplicate(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-stream-resume",
		"a-resumed-server-stream-continues-the-sequence-it-left")

	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(resumableWatchEndpoint(countingWatch))
	}})

	first := dialWSPeer(t, provider, "/watch")
	first.send(wsInit("getWatch"))
	opening := wsResumeReady(t, first)
	if opening.Resumed == nil || *opening.Resumed {
		t.Fatalf("first ready = %#v, want a fresh admission", opening)
	}
	if opening.ResumeToken == "" {
		t.Fatal("a resumable stream was admitted without a resume grant")
	}
	sequences, identifiers := wsResumeSequences(t, first)
	if sequences[0] != "1" || sequences[1] != "2" || identifiers[0] != "event-1" || identifiers[1] != "event-2" {
		t.Fatalf("first connection delivered %v / %v", sequences, identifiers)
	}

	second := dialWSPeer(t, provider, "/watch")
	second.send(wsInit("getWatch", func(frame *clientcontract.WebSocketInitFrameV1) {
		frame.Resume = &clientcontract.WebSocketResumeRequestV1{Token: opening.ResumeToken, AfterSequence: "2"}
	}))
	continued := wsResumeReady(t, second)
	if continued.Resumed == nil || !*continued.Resumed {
		t.Fatalf("continued ready = %#v, want resumed", continued)
	}
	if continued.ResumeToken == "" || continued.ResumeToken == opening.ResumeToken {
		t.Fatalf("resume token = %q; want a rotated grant", continued.ResumeToken)
	}
	sequences, identifiers = wsResumeSequences(t, second)
	if sequences[0] != "3" || sequences[1] != "4" {
		t.Fatalf("continued sequences = %v; want the stream to carry on from 2", sequences)
	}
	if identifiers[0] != "event-3" || identifiers[1] != "event-4" {
		t.Fatalf("continued values = %v; want no value delivered twice", identifiers)
	}
}

// The grant is spent by its single redemption. A token replayed after the
// connection it opened is a refusal, whoever holds it.
func TestWebSocketProvider_RefusesAResumeTokenItAlreadySpent(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-stream-resume",
		"a-resume-grant-is-spent-by-its-single-redemption")

	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(resumableWatchEndpoint(countingWatch))
	}})
	first := dialWSPeer(t, provider, "/watch")
	first.send(wsInit("getWatch"))
	token := wsResumeReady(t, first).ResumeToken
	wsResumeSequences(t, first)

	second := dialWSPeer(t, provider, "/watch")
	second.send(wsInit("getWatch", func(frame *clientcontract.WebSocketInitFrameV1) {
		frame.Resume = &clientcontract.WebSocketResumeRequestV1{Token: token, AfterSequence: "2"}
	}))
	wsResumeReady(t, second)

	replay := dialWSPeer(t, provider, "/watch")
	replay.send(wsInit("getWatch", func(frame *clientcontract.WebSocketInitFrameV1) {
		frame.Resume = &clientcontract.WebSocketResumeRequestV1{Token: token, AfterSequence: "2"}
	}))
	failure := replay.expectError()
	if failure.Code != clientcontract.ErrorCodeInvalidResilience {
		t.Fatalf("replay refusal code = %q, want %q", failure.Code, clientcontract.ErrorCodeInvalidResilience)
	}
	requireContains(t, failure.Message, "already spent")
}

// A grant names the identity that earned it. Presented by another client, it
// buys nothing — an observed token is not a way into someone else's stream.
func TestWebSocketProvider_RefusesAResumeTokenPresentedByAnotherClient(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-stream-resume",
		"a-resume-grant-is-bound-to-the-operation-and-identity-that-earned-it")

	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(resumableWatchEndpoint(countingWatch))
	}})
	first := dialWSPeer(t, provider, "/watch")
	first.send(wsInit("getWatch"))
	token := wsResumeReady(t, first).ResumeToken

	stolen := dialWSPeer(t, provider, "/watch")
	stolen.send(wsInit("getWatch", func(frame *clientcontract.WebSocketInitFrameV1) {
		frame.ClientID = "another-consumer"
		frame.Resume = &clientcontract.WebSocketResumeRequestV1{Token: token, AfterSequence: "1"}
	}))
	failure := stolen.expectError()
	if failure.Code != clientcontract.ErrorCodeInvalidResilience {
		t.Fatalf("refusal code = %q, want %q", failure.Code, clientcontract.ErrorCodeInvalidResilience)
	}
	requireContains(t, failure.Message, "not issued for this operation and client")
}

// A consumer cannot claim to have received more than this provider sent.
// Continuing past the cursor would skip values nobody delivered.
func TestWebSocketProvider_RefusesAResumePositionAheadOfWhatItDelivered(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-stream-resume",
		"a-resume-position-ahead-of-what-the-provider-delivered-is-refused")

	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(resumableWatchEndpoint(countingWatch))
	}})
	first := dialWSPeer(t, provider, "/watch")
	first.send(wsInit("getWatch"))
	token := wsResumeReady(t, first).ResumeToken
	wsResumeSequences(t, first)

	ahead := dialWSPeer(t, provider, "/watch")
	ahead.send(wsInit("getWatch", func(frame *clientcontract.WebSocketInitFrameV1) {
		frame.Resume = &clientcontract.WebSocketResumeRequestV1{Token: token, AfterSequence: "9"}
	}))
	failure := ahead.expectError()
	if failure.Code != clientcontract.ErrorCodeInvalidResilience {
		t.Fatalf("refusal code = %q, want %q", failure.Code, clientcontract.ErrorCodeInvalidResilience)
	}
	requireContains(t, failure.Message, "ahead of the sequence")
}

// A stream nobody declared resumable issues nothing to continue it with, and
// refuses a continuation with the wire's own rule.
func TestWebSocketProvider_IssuesNoGrantForAStreamItDidNotDeclareResumable(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-stream-resume",
		"a-stream-that-is-not-declared-resumable-issues-no-grant")

	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(watchEndpoint(countingWatch))
	}})
	peer := dialWSPeer(t, provider, "/watch")
	peer.send(wsInit("getWatch"))
	ready := wsResumeReady(t, peer)
	if ready.ResumeToken != "" {
		t.Fatalf("ready frame = %#v; want no grant on a stream that declares no resume", ready)
	}
}

// A continuation is an admission: it runs the endpoint's own security chain
// again. A credential this session can no longer prove ends the stream with a
// typed terminal instead of continuing on the strength of an old check.
func TestWebSocketProvider_RunsTheSecurityChainAgainOnEveryContinuation(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-stream-resume",
		"a-continuation-runs-the-endpoint-security-chain-again")

	var mu sync.Mutex
	accepted := map[string]bool{"Bearer harness-token": true}
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(Endpoint("GET", "/watch").
			Returns(StreamOf[wsEvent]()).
			Client(ClientOperationOptions{Resume: true}).
			Use(func(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
				mu.Lock()
				allowed := accepted[ctx.Request.Header.Get("Authorization")]
				mu.Unlock()
				if !allowed {
					return phttp.ErrorResponse(perrors.Unauthorized("service credential is no longer valid"))
				}
				return next()
			}).
			Handle(ServerStream(countingWatch)))
	}})
	first := dialWSPeer(t, provider, "/watch")
	first.send(wsInit("getWatch"))
	token := wsResumeReady(t, first).ResumeToken
	wsResumeSequences(t, first)

	// The credential the first socket carried is no longer accepted.
	mu.Lock()
	delete(accepted, "Bearer harness-token")
	mu.Unlock()
	second := dialWSPeer(t, provider, "/watch")
	second.send(wsInit("getWatch", func(frame *clientcontract.WebSocketInitFrameV1) {
		frame.Resume = &clientcontract.WebSocketResumeRequestV1{Token: token, AfterSequence: "2"}
	}))
	failure := second.expectError()
	if failure.Status != 401 {
		t.Fatalf("continuation status = %d, want 401", failure.Status)
	}
}

// --- the grant store's own bounds, on a controlled clock --------------------

func TestWebSocketResumeStore_ExpiresAGrantAtItsDeclaredTTL(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-stream-resume",
		"a-resume-grant-expires-at-its-bound")

	instant := time.Date(2026, time.September, 10, 12, 0, 0, 0, time.UTC)
	store := newWebSocketResumeStore()
	store.now = func() time.Time { return instant }
	token, _, err := store.issue("getWatch", "consumer", 4, defaultWebSocketResumeBudget)
	if err != nil {
		t.Fatal(err)
	}
	instant = instant.Add(defaultWebSocketResumeTTL + time.Second)
	if _, err := store.redeem(token, "getWatch", "consumer", 4); err == nil {
		t.Fatal("an expired grant was redeemed")
	}
}

func TestWebSocketResumeStore_StopsIssuingOnceTheBudgetIsSpent(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-stream-resume",
		"a-stream-stops-being-resumable-once-its-budget-is-spent")

	store := newWebSocketResumeStore()
	token, _, err := store.issue("getWatch", "consumer", 0, defaultWebSocketResumeBudget)
	if err != nil {
		t.Fatal(err)
	}
	budget := defaultWebSocketResumeBudget
	for continuation := 1; continuation <= defaultWebSocketResumeBudget; continuation++ {
		grant, redeemErr := store.redeem(token, "getWatch", "consumer", 0)
		if redeemErr != nil {
			t.Fatalf("continuation %d: %v", continuation, redeemErr)
		}
		budget = grant.budget - 1
		token, _, err = store.issue("getWatch", "consumer", 0, budget)
		if err != nil {
			t.Fatal(err)
		}
	}
	if budget != 0 || token != "" {
		t.Fatalf("budget = %d, token = %q; want an exhausted stream to issue nothing", budget, token)
	}
}

func TestWebSocketResumeStore_BoundsTheGrantsItHolds(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-stream-resume",
		"the-live-grants-of-one-endpoint-are-bounded")

	store := newWebSocketResumeStore()
	first, _, err := store.issue("getWatch", "consumer", 0, defaultWebSocketResumeBudget)
	if err != nil {
		t.Fatal(err)
	}
	for range defaultWebSocketResumeGrants {
		if _, _, err := store.issue("getWatch", "consumer", 0, defaultWebSocketResumeBudget); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.grants) > defaultWebSocketResumeGrants {
		t.Fatalf("live grants = %d, want at most %d", len(store.grants), defaultWebSocketResumeGrants)
	}
	if _, err := store.redeem(first, "getWatch", "consumer", 0); err == nil {
		t.Fatal("the oldest grant survived the bound")
	}
}

func TestWebSocketResumeStore_MintsAnUnguessableTokenPerAdmission(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-stream-resume",
		"every-admission-mints-its-own-grant")

	store := newWebSocketResumeStore()
	seen := map[string]bool{}
	for range 64 {
		token, _, err := store.issue("getWatch", "consumer", 0, defaultWebSocketResumeBudget)
		if err != nil {
			t.Fatal(err)
		}
		if len(token) < 40 {
			t.Fatalf("token %q is shorter than its declared entropy", token)
		}
		if seen[token] {
			t.Fatalf("token %q was minted twice", token)
		}
		seen[token] = true
	}
}
