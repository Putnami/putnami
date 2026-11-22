package itemsclient_test

// TS→Go cell for the client and bidirectional Connect streams over HTTP/2:
// the real TypeScript provider runs in its own process (the D0.5 harness of
// foreign_provider_test.go), and a plain Connect client — the Go standard
// library's HTTP/2 in cleartext with prior knowledge, writing and reading
// Connect envelopes itself — calls the two stock conversations on the
// provider's Connect route. The generated Go client keeps carrying these two
// shapes over WebSocket; this helper is the proof that the Connect route
// serves them to any Connect client that speaks HTTP/2.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const (
	duplexRequirement = "duplex-streams-ride-connect-over-http2"
	adjustRPC         = "/catalog.items.v1.ItemsService/ListItemsByIdAdjust"
	negotiateRPC      = "/catalog.items.v1.ItemsService/ListItemsByIdNegotiate"
	endStreamFlag     = 0x02
)

// stockTotal is the provider's stockTotalSchema.
type stockTotal struct {
	ID      string  `json:"id"`
	Applied float64 `json:"applied"`
	Total   float64 `json:"total"`
}

// connectFrame is one Connect envelope: its flags and its JSON payload.
type connectFrame struct {
	Flags   byte
	Payload []byte
}

func envelope(t *testing.T, message any) []byte {
	t.Helper()
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func readFrame(t *testing.T, body io.Reader) connectFrame {
	t.Helper()
	header := make([]byte, 5)
	if _, err := io.ReadFull(body, header); err != nil {
		t.Fatalf("read envelope header: %v", err)
	}
	payload := make([]byte, binary.BigEndian.Uint32(header[1:5]))
	if _, err := io.ReadFull(body, payload); err != nil {
		t.Fatalf("read envelope payload: %v", err)
	}
	return connectFrame{Flags: header[0], Payload: payload}
}

func readTotal(t *testing.T, body io.Reader) stockTotal {
	t.Helper()
	frame := readFrame(t, body)
	if frame.Flags != 0 {
		t.Fatalf("frame flags = %#x (%s), want a message", frame.Flags, frame.Payload)
	}
	var total stockTotal
	if err := json.Unmarshal(frame.Payload, &total); err != nil {
		t.Fatalf("decode %s: %v", frame.Payload, err)
	}
	return total
}

func expectEndOfStream(t *testing.T, body io.Reader) {
	t.Helper()
	frame := readFrame(t, body)
	if frame.Flags != endStreamFlag || string(frame.Payload) != "{}" {
		t.Fatalf("terminal = flags %#x %s, want a successful end-of-stream message", frame.Flags, frame.Payload)
	}
	if extra, err := io.ReadAll(body); err != nil || len(extra) != 0 {
		t.Fatalf("after the end-of-stream message: %d bytes, %v", len(extra), err)
	}
}

// h2cClient speaks HTTP/2 in cleartext with prior knowledge, and nothing else.
func h2cClient() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: protocols}}
}

// connectDuplex is one open Connect stream: the request side is a pipe the
// test writes envelopes into while the response is read. A provider may hold
// its response headers until it has something to say — a client stream until
// the caller ended — so the response is awaited only when the test reads it.
type connectDuplex struct {
	send     *io.PipeWriter
	answered chan connectAnswer
	answer   *http.Response
}

type connectAnswer struct {
	response *http.Response
	err      error
}

// openConnectDuplex opens a Connect stream and writes its first message.
func openConnectDuplex(t *testing.T, baseURL, rpc string, first any) *connectDuplex {
	t.Helper()
	reader, writer := io.Pipe()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+rpc, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/connect+json")
	request.Header.Set("Connect-Protocol-Version", "1")
	request.Header.Set("X-Catalog-Key", catalogAPIKey)

	stream := &connectDuplex{send: writer, answered: make(chan connectAnswer, 1)}
	go func() {
		response, err := h2cClient().Do(request)
		stream.answered <- connectAnswer{response, err}
	}()
	t.Cleanup(func() {
		_ = writer.Close()
		if stream.answer != nil {
			_ = stream.answer.Body.Close()
		}
	})
	stream.write(t, first)
	return stream
}

// body awaits the response headers once and returns the response body.
func (stream *connectDuplex) body(t *testing.T) io.Reader {
	t.Helper()
	if stream.answer == nil {
		got := <-stream.answered
		if got.err != nil {
			t.Fatalf("open: %v", got.err)
		}
		stream.answer = got.response
		if got.response.ProtoMajor != 2 {
			t.Fatalf("answered over %s, want HTTP/2", got.response.Proto)
		}
		if got.response.StatusCode != http.StatusOK || got.response.Header.Get("Content-Type") != "application/connect+json" {
			t.Fatalf("answered %d %q", got.response.StatusCode, got.response.Header.Get("Content-Type"))
		}
	}
	return stream.answer.Body
}

func (stream *connectDuplex) write(t *testing.T, message any) {
	t.Helper()
	if _, err := stream.send.Write(envelope(t, message)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestTypeScriptProviderServesDuplexConnectStreamsOverH2C(t *testing.T) {
	spectest.Proves(t, matrixFeature, duplexRequirement,
		"the-go-consumer-calls-a-client-and-a-bidirectional-rpc-over-h2c")
	providerURL := startForeignProvider(t)

	t.Run("a client stream answers its one total after the consumer ended", func(t *testing.T) {
		stream := openConnectDuplex(t, providerURL, adjustRPC, map[string]any{
			"params": map[string]string{"id": "item-1"},
			"body":   map[string]float64{"delta": 5},
		})
		stream.write(t, map[string]any{"body": map[string]float64{"delta": -2}})
		stream.write(t, map[string]any{"body": map[string]float64{"delta": 7}})
		if err := stream.send.Close(); err != nil {
			t.Fatal(err)
		}
		if total := readTotal(t, stream.body(t)); total != (stockTotal{ID: "item-1", Applied: 3, Total: 10}) {
			t.Fatalf("result = %+v", total)
		}
		expectEndOfStream(t, stream.body(t))
	})

	t.Run("a bidirectional stream answers each delta before the next is sent", func(t *testing.T) {
		stream := openConnectDuplex(t, providerURL, negotiateRPC, map[string]any{
			"params": map[string]string{"id": "item-1"},
			"body":   map[string]float64{"delta": 4},
		})
		// Each running total is read before the next delta is written: a
		// provider that buffered the request until its end would never answer.
		if total := readTotal(t, stream.body(t)); total != (stockTotal{ID: "item-1", Applied: 1, Total: 4}) {
			t.Fatalf("first running total = %+v", total)
		}
		stream.write(t, map[string]any{"body": map[string]float64{"delta": 6}})
		if total := readTotal(t, stream.body(t)); total != (stockTotal{ID: "item-1", Applied: 2, Total: 10}) {
			t.Fatalf("second running total = %+v", total)
		}
		if err := stream.send.Close(); err != nil {
			t.Fatal(err)
		}
		// The handler's returned value is the last message before the end.
		if total := readTotal(t, stream.body(t)); total != (stockTotal{ID: "item-1", Applied: 2, Total: 10}) {
			t.Fatalf("declared result = %+v", total)
		}
		expectEndOfStream(t, stream.body(t))
	})

	t.Run("a declared error ends the stream in its terminal", func(t *testing.T) {
		stream := openConnectDuplex(t, providerURL, adjustRPC, map[string]any{
			"params": map[string]string{"id": "missing"},
		})
		if err := stream.send.Close(); err != nil {
			t.Fatal(err)
		}
		frame := readFrame(t, stream.body(t))
		var terminal struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(frame.Payload, &terminal); err != nil {
			t.Fatal(err)
		}
		if frame.Flags != endStreamFlag || terminal.Error.Code != "not_found" {
			t.Fatalf("terminal = flags %#x %s, want not_found in the end-of-stream message", frame.Flags, frame.Payload)
		}
	})
}

func TestTypeScriptProviderAnswersDuplexConnectStreamsUnimplementedOverHTTP1(t *testing.T) {
	spectest.Proves(t, matrixFeature, duplexRequirement, "the-same-duplex-rpcs-over-http1-answer-unimplemented")
	providerURL := startForeignProvider(t)

	for _, rpc := range []string{adjustRPC, negotiateRPC} {
		t.Run(rpc, func(t *testing.T) {
			body := envelope(t, map[string]any{
				"params": map[string]string{"id": "item-1"},
				"body":   map[string]float64{"delta": 1},
			})
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, providerURL+rpc, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/connect+json")
			request.Header.Set("Connect-Protocol-Version", "1")
			request.Header.Set("X-Catalog-Key", catalogAPIKey)
			response, err := (&http.Client{Transport: &http.Transport{}}).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.ProtoMajor != 1 {
				t.Fatalf("answered over %s, want HTTP/1.1", response.Proto)
			}
			var failure struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusNotImplemented || failure.Code != "unimplemented" {
				t.Fatalf("%s over HTTP/1.1 = %d %s, want 501 unimplemented", rpc, response.StatusCode, fmt.Sprint(failure))
			}
		})
	}
}
