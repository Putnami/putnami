package client

import (
	"bufio"
	"bytes"
	stderrors "errors"
	"io"
	"net"
	"strings"
	"testing"
)

// wsPipe returns a framed client and provider pair over an in-memory
// connection, so framing is exercised without a listener.
func wsPipe(t *testing.T, maxMessageBytes int64) (*wsConn, *wsConn) {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	t.Cleanup(func() {
		_ = clientSide.Close()
		_ = serverSide.Close()
	})
	return newWSConn(clientSide, nil, wsRoleClient, maxMessageBytes),
		newWSConn(serverSide, nil, wsRoleServer, maxMessageBytes)
}

// TestWebSocketAcceptTokenMatchesTheRFCExample pins the handshake digest to the
// published RFC 6455 section 1.3 vector rather than to this package's own
// arithmetic.
func TestWebSocketAcceptTokenMatchesTheRFCExample(t *testing.T) {
	if got := webSocketAcceptToken("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept token = %q", got)
	}
}

// TestWebSocketClientFramesAreMaskedAndMinimallyEncoded checks the bytes a
// client puts on the wire at every length boundary of RFC 6455 section 5.2.
func TestWebSocketClientFramesAreMaskedAndMinimallyEncoded(t *testing.T) {
	tests := []struct {
		name       string
		size       int
		wantHeader int
		wantLength byte
	}{
		{name: "empty", size: 0, wantHeader: 2, wantLength: 0},
		{name: "short", size: 125, wantHeader: 2, wantLength: 125},
		{name: "sixteen-bit-floor", size: 126, wantHeader: 4, wantLength: 126},
		{name: "sixteen-bit-ceiling", size: 0xFFFF, wantHeader: 4, wantLength: 126},
		{name: "sixty-four-bit-floor", size: 0x10000, wantHeader: 10, wantLength: 127},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, provider := wsPipe(t, 1<<20)
			payload := bytes.Repeat([]byte("p"), test.size)
			go func() {
				if err := client.writeText(payload); err != nil {
					t.Errorf("write failed: %v", err)
				}
			}()
			reader := bufio.NewReader(provider.conn)
			header := make([]byte, test.wantHeader)
			if _, err := io.ReadFull(reader, header); err != nil {
				t.Fatal(err)
			}
			if header[0] != 0x80|byte(wsOpcodeText) {
				t.Fatalf("first header byte = %#x", header[0])
			}
			if header[1]&0x80 == 0 {
				t.Fatal("a client frame must set the mask bit")
			}
			if header[1]&0x7F != test.wantLength {
				t.Fatalf("length indicator = %d, want %d", header[1]&0x7F, test.wantLength)
			}
			var key [4]byte
			if _, err := io.ReadFull(reader, key[:]); err != nil {
				t.Fatal(err)
			}
			body := make([]byte, test.size)
			if _, err := io.ReadFull(reader, body); err != nil {
				t.Fatal(err)
			}
			wsApplyMask(body, key)
			if !bytes.Equal(body, payload) {
				t.Fatal("unmasked payload does not match the written payload")
			}
		})
	}
}

// TestWebSocketMaskingKeyIsFreshPerFrame proves the mask is not a constant.
func TestWebSocketMaskingKeyIsFreshPerFrame(t *testing.T) {
	client, provider := wsPipe(t, 1<<20)
	go func() {
		for range 2 {
			if err := client.writeText([]byte("same")); err != nil {
				t.Errorf("write failed: %v", err)
			}
		}
	}()
	reader := bufio.NewReader(provider.conn)
	keys := make([][4]byte, 0, 2)
	for range 2 {
		header := make([]byte, 2)
		if _, err := io.ReadFull(reader, header); err != nil {
			t.Fatal(err)
		}
		var key [4]byte
		if _, err := io.ReadFull(reader, key[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(reader, make([]byte, header[1]&0x7F)); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	if keys[0] == keys[1] {
		t.Fatal("two frames reused one masking key")
	}
}

// TestWebSocketReaderReassemblesContinuations checks that a message split into
// continuation frames arrives whole, which the first-party protocol needs
// because a provider may fragment any frame.
func TestWebSocketReaderReassemblesContinuations(t *testing.T) {
	client, provider := wsPipe(t, 1<<20)
	payload := []byte(strings.Repeat("fragment-", 40))
	go func() {
		offset, first := 0, true
		for offset < len(payload) {
			end := min(offset+17, len(payload))
			opcode := wsOpcodeContinuation
			if first {
				opcode = wsOpcodeText
			}
			if err := writeRawWSFrame(provider.conn, end == len(payload), opcode, payload[offset:end]); err != nil {
				t.Errorf("fragment write failed: %v", err)
				return
			}
			first, offset = false, end
		}
	}()
	opcode, message, err := client.readMessage()
	if err != nil {
		t.Fatal(err)
	}
	if opcode != wsOpcodeText || !bytes.Equal(message, payload) {
		t.Fatalf("reassembled opcode %d, %d bytes", opcode, len(message))
	}
}

// TestWebSocketReaderBoundsMessagesAndFrames refuses anything past the declared
// frame budget, whole or fragmented, before the payload becomes a message.
func TestWebSocketReaderBoundsMessagesAndFrames(t *testing.T) {
	tests := []struct {
		name      string
		fragments []int
	}{
		{name: "single-frame", fragments: []int{64}},
		{name: "sum-of-fragments", fragments: []int{20, 20, 20, 20}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, provider := wsPipe(t, 32)
			go func() {
				first := true
				for _, size := range test.fragments {
					opcode := wsOpcodeContinuation
					if first {
						opcode = wsOpcodeText
					}
					_ = writeRawWSFrame(provider.conn, false, opcode, bytes.Repeat([]byte("x"), size))
					first = false
				}
			}()
			_, _, err := client.readMessage()
			var protocolErr *wsProtocolError
			if !stderrors.As(err, &protocolErr) || protocolErr.closeCode != wsCloseMessageTooBig {
				t.Fatalf("error = %T %v", err, err)
			}
		})
	}
}

// TestWebSocketReaderRefusesMalformedFraming walks the framing violations a
// conforming client must refuse instead of guessing an intent.
func TestWebSocketReaderRefusesMalformedFraming(t *testing.T) {
	tests := []struct {
		name  string
		bytes []byte
		want  int
	}{
		{name: "reserved-bit", bytes: []byte{0x80 | 0x40 | 0x01, 0x00}, want: wsCloseProtocolError},
		{name: "reserved-opcode", bytes: []byte{0x80 | 0x03, 0x00}, want: wsCloseProtocolError},
		{name: "masked-provider-frame", bytes: []byte{0x81, 0x80, 1, 2, 3, 4}, want: wsCloseProtocolError},
		{name: "fragmented-control", bytes: []byte{0x09, 0x00}, want: wsCloseProtocolError},
		{name: "oversized-control", bytes: append([]byte{0x89, 126, 0x00, 0x7E}, bytes.Repeat([]byte("x"), 126)...), want: wsCloseProtocolError},
		{name: "non-minimal-sixteen-bit", bytes: []byte{0x81, 126, 0x00, 0x10}, want: wsCloseProtocolError},
		{name: "orphan-continuation", bytes: []byte{0x80, 0x00}, want: wsCloseProtocolError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, provider := wsPipe(t, 1<<20)
			go func() { _, _ = provider.conn.Write(test.bytes) }()
			_, _, err := client.readMessage()
			var protocolErr *wsProtocolError
			if !stderrors.As(err, &protocolErr) || protocolErr.closeCode != test.want {
				t.Fatalf("error = %T %v", err, err)
			}
		})
	}
}

// TestWebSocketReaderRefusesAnInterruptedFragmentedMessage keeps a provider
// from interleaving a second data message inside a fragmented one.
func TestWebSocketReaderRefusesAnInterruptedFragmentedMessage(t *testing.T) {
	client, provider := wsPipe(t, 1<<20)
	go func() {
		_ = writeRawWSFrame(provider.conn, false, wsOpcodeText, []byte("first"))
		_ = writeRawWSFrame(provider.conn, true, wsOpcodeText, []byte("second"))
	}()
	_, _, err := client.readMessage()
	var protocolErr *wsProtocolError
	if !stderrors.As(err, &protocolErr) || protocolErr.closeCode != wsCloseProtocolError {
		t.Fatalf("error = %T %v", err, err)
	}
}

// TestWebSocketReaderAnswersPingAndObservesActivity proves a transport ping is
// answered with its own payload and that every frame refreshes the observer the
// idle budget listens to.
func TestWebSocketReaderAnswersPingAndObservesActivity(t *testing.T) {
	client, provider := wsPipe(t, 1<<20)
	activity := 0
	client.onActivity = func() { activity++ }
	go func() {
		_ = writeRawWSFrame(provider.conn, true, wsOpcodePing, []byte("probe"))
		_ = writeRawWSFrame(provider.conn, true, wsOpcodeText, []byte("payload"))
	}()
	reader := bufio.NewReader(provider.conn)
	done := make(chan struct{})
	var message []byte
	var readErr error
	go func() {
		defer close(done)
		_, message, readErr = client.readMessage()
	}()
	fin, opcode, payload, err := readRawWSFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !fin || opcode != wsOpcodePong || string(payload) != "probe" {
		t.Fatalf("pong frame = fin %t opcode %d payload %q", fin, opcode, payload)
	}
	<-done
	if readErr != nil || string(message) != "payload" {
		t.Fatalf("message = %q, err = %v", message, readErr)
	}
	if activity != 2 {
		t.Fatalf("activity observations = %d, want 2 (the ping and the message)", activity)
	}
}

// TestWebSocketReaderReportsPeerClose projects the peer's close frame, and the
// codeless form, without inventing a payload.
func TestWebSocketReaderReportsPeerClose(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    int
	}{
		{name: "with-code", payload: []byte{0x03, 0xE8}, want: wsCloseNormal},
		{name: "without-code", payload: nil, want: wsCloseNoStatus},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, provider := wsPipe(t, 1<<20)
			go func() { _ = writeRawWSFrame(provider.conn, true, wsOpcodeClose, test.payload) }()
			_, _, err := client.readMessage()
			var closeErr *wsCloseError
			if !stderrors.As(err, &closeErr) || closeErr.Code != test.want {
				t.Fatalf("error = %T %v", err, err)
			}
		})
	}
}

// TestWebSocketCloseFrameCarriesABoundedReason keeps a close reason inside the
// control payload budget.
func TestWebSocketCloseFrameCarriesABoundedReason(t *testing.T) {
	client, provider := wsPipe(t, 1<<20)
	go func() {
		if err := client.writeClose(wsClosePolicyViolation, strings.Repeat("r", 400)); err != nil {
			t.Errorf("close write failed: %v", err)
		}
	}()
	reader := bufio.NewReader(provider.conn)
	_, opcode, payload, err := readRawWSFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	if opcode != wsOpcodeClose || len(payload) != wsControlPayloadMax {
		t.Fatalf("close frame opcode %d, payload %d bytes", opcode, len(payload))
	}
	if int(payload[0])<<8|int(payload[1]) != wsClosePolicyViolation {
		t.Fatalf("close code = %d", int(payload[0])<<8|int(payload[1]))
	}
}

// TestWebSocketWriterRefusesAnOversizedMessage keeps the client from emitting a
// frame past the budget it declared to the provider.
func TestWebSocketWriterRefusesAnOversizedMessage(t *testing.T) {
	client, _ := wsPipe(t, 8)
	err := client.writeText(bytes.Repeat([]byte("x"), 9))
	var protocolErr *wsProtocolError
	if !stderrors.As(err, &protocolErr) || protocolErr.closeCode != wsCloseMessageTooBig {
		t.Fatalf("error = %T %v", err, err)
	}
}

// TestWebSocketReaderRefusesWideAndNonMinimalLengths covers the 64-bit length
// form: it must be minimally encoded and must stay inside the addressable
// range, so a length field alone cannot ask the client for an allocation.
func TestWebSocketReaderRefusesWideAndNonMinimalLengths(t *testing.T) {
	tests := []struct {
		name  string
		bytes []byte
		want  int
	}{
		{
			name:  "sixty-four-bit-restates-a-short-length",
			bytes: []byte{0x81, 127, 0, 0, 0, 0, 0, 0, 0x01, 0x00},
			want:  wsCloseProtocolError,
		},
		{
			name:  "sixty-four-bit-past-the-addressable-range",
			bytes: []byte{0x81, 127, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
			want:  wsCloseMessageTooBig,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, provider := wsPipe(t, 0)
			go func() { _, _ = provider.conn.Write(test.bytes) }()
			_, _, err := client.readMessage()
			var protocolErr *wsProtocolError
			if !stderrors.As(err, &protocolErr) || protocolErr.closeCode != test.want {
				t.Fatalf("error = %T %v", err, err)
			}
		})
	}
}

// TestWebSocketErrorsDescribeThemselvesWithoutFrameMaterial keeps every error
// this layer reports readable and free of wire payload.
func TestWebSocketErrorsDescribeThemselvesWithoutFrameMaterial(t *testing.T) {
	protocolErr := newWSProtocolError(wsCloseProtocolError, "websocket frame is malformed")
	if protocolErr.Error() != "websocket frame is malformed" {
		t.Fatalf("protocol error = %q", protocolErr.Error())
	}
	closeErr := &wsCloseError{Code: wsClosePolicyViolation, Reason: "secret-material"}
	if strings.Contains(closeErr.Error(), "secret-material") {
		t.Fatalf("close error repeats the peer reason: %q", closeErr.Error())
	}
	rejection := &webSocketHandshakeRejection{StatusCode: 401, Body: []byte("Bearer stream-token")}
	if strings.Contains(rejection.Error(), "stream-token") {
		t.Fatalf("handshake rejection repeats the body: %q", rejection.Error())
	}
	if wsErrorCloseCode(stderrors.New("something else")) != wsCloseInternalError {
		t.Fatal("an unclassified failure must close as an internal endpoint error")
	}
	var contractErr *WebSocketContractError
	if contractErr.Error() != "" || contractErr.Unwrap() != nil {
		t.Fatal("a nil contract error must stay describable")
	}
}
