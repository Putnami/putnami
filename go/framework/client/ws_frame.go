package client

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// wsOpcode is an RFC 6455 frame opcode. The first-party service protocol only
// ever sends text frames and a provider-owned byte stream binary ones; the
// other opcodes exist because a conforming client must still read, answer and
// refuse them correctly.
type wsOpcode byte

const (
	wsOpcodeContinuation wsOpcode = 0x0
	wsOpcodeText         wsOpcode = 0x1
	wsOpcodeBinary       wsOpcode = 0x2
	wsOpcodeClose        wsOpcode = 0x8
	wsOpcodePing         wsOpcode = 0x9
	wsOpcodePong         wsOpcode = 0xA
)

// RFC 6455 section 7.4.1 close codes this runtime sends or recognizes.
const (
	wsCloseNormal          = 1000
	wsCloseGoingAway       = 1001
	wsCloseProtocolError   = 1002
	wsCloseUnsupportedData = 1003
	wsCloseNoStatus        = 1005
	wsCloseAbnormal        = 1006
	wsCloseInvalidPayload  = 1007
	wsClosePolicyViolation = 1008
	wsCloseMessageTooBig   = 1009
	wsCloseInternalError   = 1011
)

// wsControlPayloadMax is the RFC 6455 bound on a control frame payload.
const wsControlPayloadMax = 125

// wsRole selects the masking rules of RFC 6455 section 5.3: a client masks
// every frame it sends and refuses a masked frame; a server does the opposite.
type wsRole uint8

const (
	wsRoleClient wsRole = iota
	wsRoleServer
)

// wsProtocolError is a framing violation. It carries the close code the peer
// must be told, and never carries frame material: a close reason travels on the
// wire in clear text, so only fixed framework text may reach it.
type wsProtocolError struct {
	closeCode int
	message   string
}

func (err *wsProtocolError) Error() string { return err.message }

func newWSProtocolError(closeCode int, message string) *wsProtocolError {
	return &wsProtocolError{closeCode: closeCode, message: message}
}

// wsCloseError reports the peer's close frame.
type wsCloseError struct {
	Code   int
	Reason string
}

func (err *wsCloseError) Error() string {
	return fmt.Sprintf("websocket connection closed with code %d", err.Code)
}

// wsConn is a hand-written RFC 6455 connection over one net.Conn. It owns
// masking, continuation reassembly, control-frame answering and the frame and
// message size bounds. It carries no first-party protocol knowledge.
type wsConn struct {
	conn   net.Conn
	reader *bufio.Reader
	role   wsRole
	// maxMessageBytes bounds one reassembled application message, and therefore
	// every fragment of it too. Zero leaves the message unbounded, which only a
	// caller that has already bounded its own reads may ask for.
	maxMessageBytes int64
	// onActivity observes every frame read from the peer, including control
	// frames, so an idle budget is refreshed by a heartbeat as well as by data.
	onActivity func()

	writeMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

// newWSConn wraps an established connection. reader may already hold bytes the
// handshake response read ahead of the first frame.
func newWSConn(conn net.Conn, reader *bufio.Reader, role wsRole, maxMessageBytes int64) *wsConn {
	if reader == nil {
		reader = bufio.NewReader(conn)
	}
	return &wsConn{conn: conn, reader: reader, role: role, maxMessageBytes: maxMessageBytes}
}

// close releases the connection once.
func (connection *wsConn) close() error {
	connection.closeOnce.Do(func() { connection.closeErr = connection.conn.Close() })
	return connection.closeErr
}

// setReadDeadline bounds the next read. The zero time removes the bound.
func (connection *wsConn) setReadDeadline(deadline time.Time) error {
	return connection.conn.SetReadDeadline(deadline)
}

// setWriteDeadline bounds the next write. The zero time removes the bound.
func (connection *wsConn) setWriteDeadline(deadline time.Time) error {
	return connection.conn.SetWriteDeadline(deadline)
}

// writeText sends one unfragmented text message. The first-party protocol is
// JSON, so text is the only data opcode this client ever writes.
func (connection *wsConn) writeText(payload []byte) error {
	if connection.maxMessageBytes > 0 && int64(len(payload)) > connection.maxMessageBytes {
		return newWSProtocolError(wsCloseMessageTooBig, "outgoing websocket message exceeds the declared frame budget")
	}
	return connection.writeFrame(wsOpcodeText, payload)
}

// writeBinary sends one unfragmented binary message. Only a provider-owned
// byte stream writes one; the frame budget applies exactly as it does to text.
func (connection *wsConn) writeBinary(payload []byte) error {
	if connection.maxMessageBytes > 0 && int64(len(payload)) > connection.maxMessageBytes {
		return newWSProtocolError(wsCloseMessageTooBig, "outgoing websocket message exceeds the declared frame budget")
	}
	return connection.writeFrame(wsOpcodeBinary, payload)
}

// writeControl sends one control frame. RFC 6455 bounds its payload to 125
// bytes and forbids fragmenting it.
func (connection *wsConn) writeControl(opcode wsOpcode, payload []byte) error {
	if len(payload) > wsControlPayloadMax {
		return newWSProtocolError(wsCloseProtocolError, "websocket control payload exceeds 125 bytes")
	}
	return connection.writeFrame(opcode, payload)
}

// writeClose sends a close frame. The reason is framework text only and is
// truncated to the bytes a control frame can carry beside the code.
func (connection *wsConn) writeClose(code int, reason string) error {
	payload := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(payload, uint16(code)) //nolint:gosec // close codes are the closed set above, all below 5000
	if len(reason) > wsControlPayloadMax-2 {
		reason = reason[:wsControlPayloadMax-2]
	}
	payload = append(payload, reason...)
	return connection.writeFrame(wsOpcodeClose, payload)
}

// writeFrame emits one complete frame, masked when this side is the client.
func (connection *wsConn) writeFrame(opcode wsOpcode, payload []byte) error {
	header := make([]byte, 0, 14)
	header = append(header, 0x80|byte(opcode))
	masked := connection.role == wsRoleClient
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	length := len(payload)
	switch {
	case length <= 125:
		// The switch bounds every conversion below: 125 fits a byte and 0xFFFF
		// fits the sixteen-bit form, so neither narrowing can overflow.
		header = append(header, maskBit|byte(length)) //nolint:gosec // bounded by the case above
	case length <= 0xFFFF:
		header = append(header, maskBit|126, byte(length>>8), byte(length)) //nolint:gosec // bounded by the case above
	default:
		header = append(header, maskBit|127)
		var extended [8]byte
		binary.BigEndian.PutUint64(extended[:], uint64(length)) //nolint:gosec // a Go slice length is never negative
		header = append(header, extended[:]...)
	}

	body := payload
	if masked {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return newWSProtocolError(wsCloseInternalError, "websocket masking key could not be generated")
		}
		header = append(header, key[:]...)
		body = make([]byte, len(payload))
		copy(body, payload)
		wsApplyMask(body, key)
	}

	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	if _, err := connection.conn.Write(header); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	_, err := connection.conn.Write(body)
	return err
}

// wsApplyMask applies the RFC 6455 section 5.3 masking transform in place. The
// transform is its own inverse.
func wsApplyMask(payload []byte, key [4]byte) {
	for i := range payload {
		payload[i] ^= key[i%4]
	}
}

// readMessage returns the next reassembled application message. Control frames
// are handled inline: a ping is answered with a pong carrying the same payload,
// a pong is observed, and a close frame is reported as *wsCloseError.
func (connection *wsConn) readMessage() (wsOpcode, []byte, error) {
	var message []byte
	messageOpcode := wsOpcodeContinuation
	fragmented := false
	for {
		fin, opcode, payload, err := connection.readFrame()
		if err != nil {
			return 0, nil, err
		}
		if connection.onActivity != nil {
			connection.onActivity()
		}
		if opcode >= wsOpcodeClose {
			if !fin {
				return 0, nil, newWSProtocolError(wsCloseProtocolError, "websocket control frame is fragmented")
			}
			if err := connection.handleControlFrame(opcode, payload); err != nil {
				return 0, nil, err
			}
			continue
		}
		if opcode == wsOpcodeContinuation {
			if !fragmented {
				return 0, nil, newWSProtocolError(wsCloseProtocolError, "websocket continuation frame has no message to continue")
			}
		} else {
			if fragmented {
				return 0, nil, newWSProtocolError(wsCloseProtocolError, "websocket data frame interrupts a fragmented message")
			}
			messageOpcode = opcode
			fragmented = true
		}
		if connection.maxMessageBytes > 0 && int64(len(message))+int64(len(payload)) > connection.maxMessageBytes {
			return 0, nil, newWSProtocolError(wsCloseMessageTooBig, "websocket message exceeds the declared frame budget")
		}
		message = append(message, payload...)
		if fin {
			return messageOpcode, message, nil
		}
	}
}

// handleControlFrame answers a ping and reports a close.
func (connection *wsConn) handleControlFrame(opcode wsOpcode, payload []byte) error {
	switch opcode {
	case wsOpcodeClose:
		return newWSCloseError(payload)
	case wsOpcodePing:
		return connection.writeControl(wsOpcodePong, payload)
	case wsOpcodePong:
		return nil
	default:
		return newWSProtocolError(wsCloseProtocolError, "websocket control opcode is not defined")
	}
}

// newWSCloseError projects a close payload. A close frame carries either no
// payload or a two-byte code and a UTF-8 reason.
func newWSCloseError(payload []byte) error {
	if len(payload) == 0 {
		return &wsCloseError{Code: wsCloseNoStatus}
	}
	if len(payload) == 1 {
		return newWSProtocolError(wsCloseProtocolError, "websocket close payload is truncated")
	}
	return &wsCloseError{Code: int(binary.BigEndian.Uint16(payload[:2])), Reason: string(payload[2:])}
}

// readFrame reads exactly one frame header and its payload.
func (connection *wsConn) readFrame() (bool, wsOpcode, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(connection.reader, header[:]); err != nil {
		return false, 0, nil, err
	}
	if header[0]&0x70 != 0 {
		return false, 0, nil, newWSProtocolError(wsCloseProtocolError, "websocket frame sets a reserved bit no extension negotiated")
	}
	fin := header[0]&0x80 != 0
	opcode := wsOpcode(header[0] & 0x0F)
	if opcode > wsOpcodeBinary && opcode < wsOpcodeClose {
		return false, 0, nil, newWSProtocolError(wsCloseProtocolError, "websocket frame uses a reserved opcode")
	}
	masked := header[1]&0x80 != 0
	if masked != (connection.role == wsRoleServer) {
		return false, 0, nil, newWSProtocolError(wsCloseProtocolError, "websocket frame masking does not match the peer role")
	}
	length, err := connection.readPayloadLength(header[1] & 0x7F)
	if err != nil {
		return false, 0, nil, err
	}
	if opcode >= wsOpcodeClose && length > wsControlPayloadMax {
		return false, 0, nil, newWSProtocolError(wsCloseProtocolError, "websocket control payload exceeds 125 bytes")
	}
	if connection.maxMessageBytes > 0 && length > connection.maxMessageBytes {
		return false, 0, nil, newWSProtocolError(wsCloseMessageTooBig, "websocket frame exceeds the declared frame budget")
	}
	var key [4]byte
	if masked {
		if _, err := io.ReadFull(connection.reader, key[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(connection.reader, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		wsApplyMask(payload, key)
	}
	return fin, opcode, payload, nil
}

// readPayloadLength decodes the one, three or nine byte length field. The
// 64-bit form must have its most significant bit clear and must not restate a
// length a shorter form could have carried.
func (connection *wsConn) readPayloadLength(indicator byte) (int64, error) {
	switch indicator {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(connection.reader, extended[:]); err != nil {
			return 0, err
		}
		length := int64(binary.BigEndian.Uint16(extended[:]))
		if length < 126 {
			return 0, newWSProtocolError(wsCloseProtocolError, "websocket frame length is not minimally encoded")
		}
		return length, nil
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(connection.reader, extended[:]); err != nil {
			return 0, err
		}
		value := binary.BigEndian.Uint64(extended[:])
		if value > uint64(1)<<62 {
			return 0, newWSProtocolError(wsCloseMessageTooBig, "websocket frame length exceeds the addressable range")
		}
		length := int64(value)
		if length <= 0xFFFF {
			return 0, newWSProtocolError(wsCloseProtocolError, "websocket frame length is not minimally encoded")
		}
		return length, nil
	default:
		return int64(indicator), nil
	}
}

// wsErrorCloseCode is the code a peer must be told about err. A framing
// violation names its own; anything else is an internal endpoint failure.
func wsErrorCloseCode(err error) int {
	var protocolErr *wsProtocolError
	if stderrors.As(err, &protocolErr) {
		return protocolErr.closeCode
	}
	return wsCloseInternalError
}
