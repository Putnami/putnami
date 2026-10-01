package api

import (
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"

	phttp "go.putnami.dev/http"
)

type streamMsg struct {
	Message string `json:"message"`
}

// newTestStreamContext builds a transport StreamContext wired to send and an
// inbound raw-message channel, using the http package's test helper. Either may
// be nil to model a one-way stream.
func newTestStreamContext(send func(any) error, raw <-chan []byte) *phttp.StreamContext {
	ctx := phttp.NewContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/stream", nil))
	return phttp.NewTestStreamContext(ctx, send, raw)
}

func rawChan(frames ...string) <-chan []byte {
	ch := make(chan []byte, len(frames))
	for _, f := range frames {
		ch <- []byte(f)
	}
	close(ch)
	return ch
}

func TestServerStreamContext_SendWritesTypedFrame(t *testing.T) {
	var got []streamMsg
	sc := &ServerStreamContext[streamMsg]{StreamContext: newTestStreamContext(func(v any) error {
		got = append(got, v.(streamMsg))
		return nil
	}, nil)}

	if err := sc.Send(streamMsg{Message: "ready"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(got) != 1 || got[0].Message != "ready" {
		t.Fatalf("captured = %+v, want one {ready}", got)
	}
}

func TestServerStreamContext_SendUnavailable(t *testing.T) {
	sc := &ServerStreamContext[streamMsg]{StreamContext: newTestStreamContext(nil, nil)}
	if err := sc.Send(streamMsg{Message: "x"}); err == nil {
		t.Fatal("expected error when the stream has no send transport")
	}
}

func TestClientStreamContext_MessagesDecodesSequence(t *testing.T) {
	cs := &ClientStreamContext[streamMsg, streamMsg]{StreamContext: newTestStreamContext(nil,
		rawChan(`{"message":"a"}`, `{"message":"b"}`, `{"message":"c"}`))}

	got := make([]string, 0, 3)
	for m := range cs.Messages() {
		got = append(got, m.Message)
	}
	if err := cs.Err(); err != nil {
		t.Fatalf("Err = %v, want nil", err)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded = %v, want %v", got, want)
	}
}

func TestClientStreamContext_MessagesSurfacesDecodeError(t *testing.T) {
	cs := &ClientStreamContext[streamMsg, streamMsg]{StreamContext: newTestStreamContext(nil, rawChan(`{not valid json`))}

	for range cs.Messages() {
		t.Fatal("expected no decoded messages from a malformed frame")
	}
	if cs.Err() == nil {
		t.Fatal("expected the decode error to surface through Err()")
	}
}

func TestClientStreamContext_EmptyStreamClosesCleanly(t *testing.T) {
	cs := &ClientStreamContext[streamMsg, streamMsg]{StreamContext: newTestStreamContext(nil, nil)}
	for range cs.Messages() {
		t.Fatal("expected no messages from an empty stream")
	}
	if cs.Err() != nil {
		t.Fatalf("Err = %v, want nil", cs.Err())
	}
}

func TestClientStreamContext_SetErrKeepsFirst(t *testing.T) {
	cs := &ClientStreamContext[streamMsg, streamMsg]{StreamContext: newTestStreamContext(nil, nil)}
	first := errors.New("first")
	cs.setErr(first)
	cs.setErr(errors.New("second"))
	cs.setErr(nil)
	if cs.Err() != first {
		t.Fatalf("Err = %v, want the first error", cs.Err())
	}
}

func TestBidiStreamContext_RoundTrip(t *testing.T) {
	var sent []streamMsg
	bc := &BidiStreamContext[streamMsg, streamMsg]{StreamContext: newTestStreamContext(func(v any) error {
		sent = append(sent, v.(streamMsg))
		return nil
	}, rawChan(`{"message":"ping"}`, `{"message":"pong"}`))}

	for m := range bc.Messages() {
		if err := bc.Send(streamMsg{Message: "echo:" + m.Message}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if err := bc.Err(); err != nil {
		t.Fatalf("Err = %v, want nil", err)
	}
	want := []streamMsg{{Message: "echo:ping"}, {Message: "echo:pong"}}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent = %+v, want %+v", sent, want)
	}
}

func TestBidiStreamContext_SurfacesDecodeError(t *testing.T) {
	bc := &BidiStreamContext[streamMsg, streamMsg]{StreamContext: newTestStreamContext(nil, rawChan(`nope`))}
	for range bc.Messages() {
		t.Fatal("expected no messages from a malformed frame")
	}
	if bc.Err() == nil {
		t.Fatal("expected the decode error to surface through Err()")
	}
}

func TestStreamAdapters_DriveTypedContexts(t *testing.T) {
	var serverSent []streamMsg
	sh := ServerStream(func(c *ServerStreamContext[streamMsg]) error {
		return c.Send(streamMsg{Message: "s"})
	})
	if got := sh.streamMode(); got != StreamModeServer {
		t.Errorf("server mode = %q, want %q", got, StreamModeServer)
	}
	if err := sh.handleStream(newTestStreamContext(func(v any) error {
		serverSent = append(serverSent, v.(streamMsg))
		return nil
	}, nil)); err != nil {
		t.Fatalf("server handleStream: %v", err)
	}
	if len(serverSent) != 1 || serverSent[0].Message != "s" {
		t.Errorf("server sent = %+v", serverSent)
	}

	var clientGot []string
	ch := ClientStream(func(c *ClientStreamContext[streamMsg, streamMsg]) error {
		for m := range c.Messages() {
			clientGot = append(clientGot, m.Message)
		}
		return c.Err()
	})
	if got := ch.streamMode(); got != StreamModeClient {
		t.Errorf("client mode = %q, want %q", got, StreamModeClient)
	}
	if err := ch.handleStream(newTestStreamContext(nil, rawChan(`{"message":"c"}`))); err != nil {
		t.Fatalf("client handleStream: %v", err)
	}
	if len(clientGot) != 1 || clientGot[0] != "c" {
		t.Errorf("client got = %v", clientGot)
	}

	var bidiSent []streamMsg
	bh := BidiStream(func(c *BidiStreamContext[streamMsg, streamMsg]) error {
		for m := range c.Messages() {
			if err := c.Send(streamMsg{Message: "r:" + m.Message}); err != nil {
				return err
			}
		}
		return c.Err()
	})
	if got := bh.streamMode(); got != StreamModeBidirectional {
		t.Errorf("bidi mode = %q, want %q", got, StreamModeBidirectional)
	}
	if err := bh.handleStream(newTestStreamContext(func(v any) error {
		bidiSent = append(bidiSent, v.(streamMsg))
		return nil
	}, rawChan(`{"message":"b"}`))); err != nil {
		t.Fatalf("bidi handleStream: %v", err)
	}
	if len(bidiSent) != 1 || bidiSent[0].Message != "r:b" {
		t.Errorf("bidi sent = %+v", bidiSent)
	}
}

func TestStreamAdapters_NilHandlerReturnsError(t *testing.T) {
	if err := (serverStreamHandler[streamMsg]{}).handleStream(newTestStreamContext(nil, nil)); err == nil {
		t.Error("server adapter with nil fn should return an error")
	}
	if err := (clientStreamHandler[streamMsg, streamMsg]{}).handleStream(newTestStreamContext(nil, nil)); err == nil {
		t.Error("client adapter with nil fn should return an error")
	}
	if err := (bidiStreamHandler[streamMsg, streamMsg]{}).handleStream(newTestStreamContext(nil, nil)); err == nil {
		t.Error("bidi adapter with nil fn should return an error")
	}
}
