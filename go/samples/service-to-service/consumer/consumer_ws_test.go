package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"time"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	itemsclient "go.putnami.dev/examples/service-to-service/clients/go"
	"go.putnami.dev/examples/service-to-service/service"
)

// The server stream over the published WebSocket wire, end to end.
//
// The provider declares `websocket` before `sse` for this operation, so the
// runtime opens the socket; the consuming application below says nothing about
// either. The revisions arrive in order, exactly once each, and the stream
// completes on the provider's terminal frame.
func TestReadItemHistoryFollowsTheDeclaredWebSocketFirstOrder(t *testing.T) {
	stream, err := ReadItemHistory(t.Context(), generatedClientAgainstRealProvider(t), "1")
	if err != nil {
		t.Fatalf("ReadItemHistory: %v", err)
	}
	defer func() { _ = stream.Close() }()
	revisions := make([]string, 0, 3)
	for revision := range stream.Messages() {
		if revision.Id != "1" {
			t.Fatalf("revision = %+v, want item 1", revision)
		}
		revisions = append(revisions, revision.Revision)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream terminal: %v", err)
	}
	if fmt.Sprint(revisions) != "[1 2 3]" {
		t.Fatalf("revisions = %v, want the feed in order with nothing repeated", revisions)
	}
	// The declared order is what put this stream on a socket. It travels in the
	// contract the generated client embeds, so a provider that reordered its
	// transports would move this consumer without a line of consumer change.
	assertWebSocketIsDeclaredFirst(t, "getItems_Id_History")
}

// assertWebSocketIsDeclaredFirst reads the committed provider document and
// checks the transport order the generated client dispatches on.
func assertWebSocketIsDeclaredFirst(t *testing.T, operationID string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "schema", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Client      struct {
				Transports []struct {
					Protocol  string `json:"protocol"`
					WebSocket *struct {
						Resume bool `json:"resume"`
					} `json:"websocket"`
				} `json:"transports"`
			} `json:"x-putnami-client"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	for _, methods := range document.Paths {
		for _, operation := range methods {
			if operation.OperationID != operationID {
				continue
			}
			transports := operation.Client.Transports
			if len(transports) != 2 || transports[0].Protocol != "websocket" || transports[1].Protocol != "sse" {
				t.Fatalf("declared transports = %+v, want websocket then sse", transports)
			}
			if transports[0].WebSocket == nil || !transports[0].WebSocket.Resume {
				t.Fatalf("declared websocket transport = %+v, want a resumable one", transports[0].WebSocket)
			}
			return
		}
	}
	t.Fatalf("the committed provider document does not declare %s", operationID)
}

// The client stream, end to end: the consumer sends typed deltas through the
// generated method, ends its own direction, and reads the single declared
// result. Nothing in this test names a socket, a frame or a header — that is
// the point of the generated client.
func TestAdjustStockCarriesAClientStreamToItsDeclaredResult(t *testing.T) {
	stream, err := AdjustStock(t.Context(), generatedClientAgainstRealProvider(t), "1")
	if err != nil {
		t.Fatalf("AdjustStock: %v", err)
	}
	defer func() { _ = stream.Close() }()
	for _, delta := range []int64{5, -2, 7} {
		if err := stream.Send(t.Context(), itemsclient.StockDelta{Delta: delta}); err != nil {
			t.Fatalf("Send(%d): %v", delta, err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	// Idempotent by contract: a consumer that half-closes on its own exit path
	// as well as at the end of its loop must not be punished for it.
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("second CloseSend: %v", err)
	}
	total, err := stream.Result(t.Context())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if total.Id != "1" || total.Applied != 3 || total.Total != 10 {
		t.Fatalf("result = %+v", total)
	}
}

// The bidirectional conversation: both directions run at once, the half-close
// ends only the consumer's side, and the last value read before io.EOF is the
// declared result.
func TestNegotiateStockOutlivesTheConsumerHalfClose(t *testing.T) {
	stream, err := NegotiateStock(t.Context(), generatedClientAgainstRealProvider(t), "1")
	if err != nil {
		t.Fatalf("NegotiateStock: %v", err)
	}
	defer func() { _ = stream.Close() }()
	for index, delta := range []int64{4, 6} {
		if err := stream.Send(t.Context(), itemsclient.StockDelta{Delta: delta}); err != nil {
			t.Fatalf("Send(%d): %v", delta, err)
		}
		running, recvErr := stream.Recv(t.Context())
		if recvErr != nil {
			t.Fatalf("Recv after send %d: %v", index, recvErr)
		}
		if running.Applied != int64(index+1) {
			t.Fatalf("running total = %+v", running)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	terminal, err := stream.Recv(t.Context())
	if err != nil {
		t.Fatalf("terminal Recv: %v", err)
	}
	if terminal.Id != "1" || terminal.Applied != 2 || terminal.Total != 10 {
		t.Fatalf("terminal value = %+v", terminal)
	}
	// The terminal value arrives before the end of the stream, never as an
	// error: io.EOF is how a bidirectional conversation ends cleanly.
	if _, err := stream.Recv(t.Context()); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream = %T %v", err, err)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("clean bidi terminal: %v", err)
	}
}

// A credential the provider refuses is an admission refusal: it reaches the
// consumer before any frame, no handle is returned, and the next open
// reacquires rather than replaying the conversation that was refused.
func TestAdjustStockRefusedAdmissionReachesTheConsumerBeforeAnyMessage(t *testing.T) {
	var acquisitions atomic.Int32
	generated := boundClient(t, sampleBindingWithTokens(realProvider(t),
		[]string{"not-the-workload-token", service.WorkloadToken}, &acquisitions))

	stream, err := AdjustStock(t.Context(), generated, "1")
	if err == nil {
		t.Fatalf("a refused credential opened a stream: %+v", stream)
	}
	var remote *client.RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admission error = %T %v", err, err)
	}
	if stream != nil {
		t.Fatal("a refused admission returned a stream handle")
	}
	rejected := acquisitions.Load()

	refreshed, err := AdjustStock(t.Context(), generated, "1")
	if err != nil {
		t.Fatalf("reacquisition after a refused credential: %v", err)
	}
	defer func() { _ = refreshed.Close() }()
	if err := refreshed.CloseSend(); err != nil {
		t.Fatalf("CloseSend after refresh: %v", err)
	}
	if _, err := refreshed.Result(t.Context()); err != nil {
		t.Fatalf("result after refresh: %v", err)
	}
	if got := acquisitions.Load(); got != rejected+1 {
		t.Fatalf("token acquisitions = %d, want %d", got, rejected+1)
	}
}

// Admission is refused the same way on every stream shape, not only on the
// client stream: a server stream over the same WebSocket wire is refused before
// its first revision, and the consumer gets no stream handle to read.
func TestReadItemHistoryRefusedAdmissionReachesTheConsumerBeforeAnyMessage(t *testing.T) {
	spectest.Proves(t, matrixFeature, "identity-is-declared-and-never-downgraded", "a-refused-stream-credential-arrives-before-any-message")
	var acquisitions atomic.Int32
	generated := boundClient(t, sampleBindingWithTokens(realProvider(t),
		[]string{"not-the-workload-token", service.WorkloadToken}, &acquisitions))

	stream, err := ReadItemHistory(t.Context(), generated, "1")
	if err == nil {
		t.Fatalf("a refused credential opened a stream: %+v", stream)
	}
	var remote *client.RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admission error = %T %v", err, err)
	}
	if stream != nil {
		t.Fatal("a refused admission returned a stream handle")
	}

	// The refused credential is dropped once, and the next open acquires a new
	// one and reads the feed — the refusal is not sticky.
	refreshed, err := ReadItemHistory(t.Context(), generated, "1")
	if err != nil {
		t.Fatalf("reacquisition after a refused credential: %v", err)
	}
	defer func() { _ = refreshed.Close() }()
	revisions := 0
	for range refreshed.Messages() {
		revisions++
	}
	if err := refreshed.Err(); err != nil {
		t.Fatalf("stream terminal after refresh: %v", err)
	}
	if revisions != 3 {
		t.Fatalf("revisions after refresh = %d, want the whole feed", revisions)
	}
}

// The bidirectional conversation admits before the first frame in either
// direction: a refused credential never opens it, and an authenticated but
// unauthorized client is refused with 403.
func TestNegotiateStockRefusedAdmissionReachesTheConsumerBeforeAnyMessage(t *testing.T) {
	var acquisitions atomic.Int32
	refused, err := NegotiateStock(t.Context(), boundClient(t, sampleBindingWithTokens(realProvider(t),
		[]string{"not-the-workload-token", service.WorkloadToken}, &acquisitions)), "1")
	if err == nil {
		t.Fatalf("a refused credential opened a conversation: %+v", refused)
	}
	var remote *client.RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admission error = %T %v", err, err)
	}
	if refused != nil {
		t.Fatal("a refused admission returned a stream handle")
	}

	unauthorized := sampleBinding(realProvider(t))
	unauthorized.ClientID = "other.consumer"
	stream, err := NegotiateStock(t.Context(), boundClient(t, unauthorized), "1")
	if err == nil {
		t.Fatalf("an unauthorized client opened a conversation: %+v", stream)
	}
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusForbidden {
		t.Fatalf("admission error = %T %v", err, err)
	}
}

// An identity the provider authenticates but does not authorize is refused with
// 403, again before any frame. The consumer changes one binding value; it never
// writes a header.
func TestAdjustStockUnauthorizedClientIsRefusedBeforeAnyMessage(t *testing.T) {
	options := sampleBinding(realProvider(t))
	options.ClientID = "other.consumer"
	stream, err := AdjustStock(t.Context(), boundClient(t, options), "1")
	if err == nil {
		t.Fatalf("an unauthorized client opened a stream: %+v", stream)
	}
	var remote *client.RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusForbidden {
		t.Fatalf("admission error = %T %v", err, err)
	}
}

// A declared provider error arrives as the generated type, not as an opaque
// remote failure, and it arrives before the consumer sends anything.
func TestStockStreamsMapTheProviderTerminalErrorToGeneratedTypes(t *testing.T) {
	generated := generatedClientAgainstRealProvider(t)

	adjust, err := AdjustStock(t.Context(), generated, "missing")
	if err != nil {
		t.Fatalf("AdjustStock open: %v", err)
	}
	defer func() { _ = adjust.Close() }()
	if err := adjust.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if _, err := adjust.Result(t.Context()); err == nil {
		t.Fatal("a missing item returned a result")
	} else {
		var notFound *itemsclient.GetItemsAdjustNotFoundError
		if !errors.As(err, &notFound) {
			t.Fatalf("client stream terminal = %T %v", err, err)
		}
	}

	negotiate, err := NegotiateStock(t.Context(), generated, "missing")
	if err != nil {
		t.Fatalf("NegotiateStock open: %v", err)
	}
	defer func() { _ = negotiate.Close() }()
	if _, err := negotiate.Recv(t.Context()); err == nil {
		t.Fatal("a missing item delivered a message")
	} else {
		var notFound *itemsclient.GetItemsNegotiateNotFoundError
		if !errors.As(err, &notFound) {
			t.Fatalf("bidi terminal = %T %v", err, err)
		}
	}
}

// The consumer cancels mid-conversation. Exactly one terminal reaches the
// caller and it is the typed cancellation, not a transport failure.
func TestNegotiateStockEndsOnConsumerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := NegotiateStock(ctx, generatedClientAgainstRealProvider(t), "1")
	if err != nil {
		t.Fatalf("NegotiateStock: %v", err)
	}
	if err := stream.Send(ctx, itemsclient.StockDelta{Delta: 1}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := stream.Recv(ctx); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	cancel()
	select {
	case <-stream.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation left the conversation open")
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, client.CodeClientCanceled) {
		t.Fatalf("terminal after cancellation = %T %v", err, err)
	}
}

// Closing the handle is the caller's other exit: it ends the conversation
// without the caller ever canceling its own context, and the terminal stays the
// typed cancellation so the call measurement keeps its code.
func TestNegotiateStockIsReleasedByClose(t *testing.T) {
	stream, err := NegotiateStock(t.Context(), generatedClientAgainstRealProvider(t), "1")
	if err != nil {
		t.Fatalf("NegotiateStock: %v", err)
	}
	if err := stream.Send(t.Context(), itemsclient.StockDelta{Delta: 3}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := stream.Recv(t.Context()); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close left the conversation open")
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, client.CodeClientCanceled) {
		t.Fatalf("terminal after Close = %T %v", err, err)
	}
}

// Backpressure is not message loss: the provider's declared queue depth is 4,
// and a consumer that sends more than that in one burst still has every delta
// counted in the declared result.
func TestAdjustStockAppliesBackpressureWithoutLosingAMessage(t *testing.T) {
	stream, err := AdjustStock(t.Context(), generatedClientAgainstRealProvider(t), "1")
	if err != nil {
		t.Fatalf("AdjustStock: %v", err)
	}
	defer func() { _ = stream.Close() }()
	const burst = 32
	for index := range burst {
		if err := stream.Send(t.Context(), itemsclient.StockDelta{Delta: 1}); err != nil {
			t.Fatalf("Send %d of %d past the declared queue depth: %v", index, burst, err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	total, err := stream.Result(t.Context())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if total.Applied != burst || total.Total != burst {
		t.Fatalf("result = %+v, want every one of the %d messages applied", total, burst)
	}
}
