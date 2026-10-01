package client

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.putnami.dev/errors"
)

// wsHandshakeGUID is the fixed RFC 6455 section 1.3 accept salt.
const wsHandshakeGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsHandshakeRejectionMaxBytes bounds the body read from a provider that
// answers the upgrade with an ordinary HTTP response instead of accepting it.
const wsHandshakeRejectionMaxBytes = 64 << 10

// webSocketHandshakeRejection is a provider answer that is a complete HTTP
// response rather than a protocol switch. It is projected into the operation's
// declared errors by the caller, exactly like a unary non-2xx response.
type webSocketHandshakeRejection struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func (rejection *webSocketHandshakeRejection) Error() string {
	return "service refused the websocket upgrade"
}

// errWebSocketNotNegotiated is a completed RFC 6455 handshake that did not
// select the first-party subprotocol. The socket is open and useless: the
// provider serves WebSocket at this path, but not this wire. It is one of the
// answers a declared transport fallback acts on, which is why it is a single
// identifiable value rather than a message.
var errWebSocketNotNegotiated = errors.New(CodeClientResponse,
	"service did not negotiate the first-party websocket subprotocol")

// webSocketDialRequest is everything the RFC 6455 opening handshake needs. On
// the first-party conversation Header stays empty: admission material travels
// in the first application frame, never in the handshake, so a browser and a
// server client negotiate the same way. A provider-owned wire has no such
// frame, so its upgrade request carries what a unary call would.
type webSocketDialRequest struct {
	// URL is the absolute http or https URL of the upgraded route.
	URL string
	// Subprotocol is the single token offered in Sec-WebSocket-Protocol. Empty
	// offers none, and then the provider must select none.
	Subprotocol string
	// Header carries the upgrade request's own headers on a provider-owned
	// wire. The handshake members are always the framework's: a header here
	// never replaces Upgrade, Connection or a Sec-WebSocket-* value.
	Header http.Header
	// Deadline bounds the whole opening handshake. The zero time leaves it
	// bounded only by the context.
	Deadline time.Time
	// MaxMessageBytes bounds one reassembled message on the returned connection.
	MaxMessageBytes int64
}

// dialWebSocket performs the RFC 6455 opening handshake by hand over a plain
// or TLS connection and returns the framed connection on success.
func dialWebSocket(ctx context.Context, request webSocketDialRequest) (*wsConn, error) {
	target, err := url.Parse(request.URL)
	if err != nil || target.Host == "" {
		return nil, errors.New(CodeClientConfig, "service websocket URL is not an absolute URL")
	}
	secure, err := webSocketSchemeIsSecure(target.Scheme)
	if err != nil {
		return nil, err
	}

	conn, err := dialWebSocketConn(ctx, target, secure, request.Deadline)
	if err != nil {
		return nil, err
	}
	closeOnFailure := true
	defer func() {
		if closeOnFailure {
			_ = conn.Close() //nolint:errcheck // the returned error is authoritative
		}
	}()

	if !request.Deadline.IsZero() {
		if err := conn.SetDeadline(request.Deadline); err != nil {
			return nil, errors.Wrap(err, CodeClientRequest)
		}
	}

	key, err := newWebSocketKey()
	if err != nil {
		return nil, err
	}
	upgrade, err := http.NewRequestWithContext(ctx, http.MethodGet, request.URL, nil)
	if err != nil {
		return nil, errors.Wrap(err, CodeClientRequest)
	}
	for name, values := range request.Header {
		if isWebSocketHandshakeHeader(name) {
			continue
		}
		for _, value := range values {
			upgrade.Header.Add(name, value)
		}
	}
	upgrade.Header.Set("Upgrade", "websocket")
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Sec-WebSocket-Version", "13")
	upgrade.Header.Set("Sec-WebSocket-Key", key)
	if request.Subprotocol != "" {
		upgrade.Header.Set("Sec-WebSocket-Protocol", request.Subprotocol)
	}
	if err := upgrade.Write(conn); err != nil {
		return nil, errors.Wrap(err, CodeClientRequest)
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, upgrade)
	if err != nil {
		return nil, errors.Wrap(err, CodeClientResponse)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		return nil, readWebSocketRejection(response)
	}
	defer func() {
		_ = response.Body.Close() //nolint:errcheck // a 101 response has no body to fail on
	}()
	if err := verifyWebSocketAccept(response, key, request.Subprotocol); err != nil {
		return nil, err
	}
	if request.Deadline.IsZero() {
		if err := conn.SetDeadline(time.Time{}); err != nil {
			return nil, errors.Wrap(err, CodeClientRequest)
		}
	}
	closeOnFailure = false
	return newWSConn(conn, reader, wsRoleClient, request.MaxMessageBytes), nil
}

// isWebSocketHandshakeHeader names the headers the opening handshake owns. A
// caller header of the same name would change what the upgrade negotiates.
func isWebSocketHandshakeHeader(name string) bool {
	canonical := http.CanonicalHeaderKey(name)
	switch canonical {
	case "Upgrade", "Connection", "Host", "Content-Length", "Transfer-Encoding":
		return true
	default:
		return strings.HasPrefix(canonical, "Sec-Websocket-")
	}
}

// webSocketSchemeIsSecure maps the bound service scheme onto the transport. A
// generated client dials the same origin its unary calls use, so the binding's
// own scheme rules already decided whether plaintext is allowed.
func webSocketSchemeIsSecure(scheme string) (bool, error) {
	switch strings.ToLower(scheme) {
	case "https", "wss":
		return true, nil
	case "http", "ws":
		return false, nil
	default:
		return false, errors.New(CodeClientConfig, "service websocket URL scheme is not http or https")
	}
}

// dialWebSocketConn opens the transport connection under the handshake budget.
func dialWebSocketConn(ctx context.Context, target *url.URL, secure bool, deadline time.Time) (net.Conn, error) {
	address := target.Host
	if target.Port() == "" {
		if secure {
			address = net.JoinHostPort(target.Hostname(), "443")
		} else {
			address = net.JoinHostPort(target.Hostname(), "80")
		}
	}
	dialCtx := ctx
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	dialer := &net.Dialer{}
	if !secure {
		conn, err := dialer.DialContext(dialCtx, "tcp", address)
		if err != nil {
			return nil, errors.Wrap(err, CodeClientRequest)
		}
		return conn, nil
	}
	tlsDialer := &tls.Dialer{
		NetDialer: dialer,
		Config:    &tls.Config{ServerName: target.Hostname(), MinVersion: tls.VersionTLS12},
	}
	conn, err := tlsDialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return nil, errors.Wrap(err, CodeClientRequest)
	}
	return conn, nil
}

// newWebSocketKey returns the base64 of 16 fresh random bytes, as RFC 6455
// section 4.1 requires.
func newWebSocketKey() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", errors.Wrap(err, CodeClientRequest)
	}
	return base64.StdEncoding.EncodeToString(nonce[:]), nil
}

// webSocketAcceptToken is the RFC 6455 section 4.2.2 accept value for a key.
func webSocketAcceptToken(key string) string {
	digest := sha1.Sum([]byte(key + wsHandshakeGUID))
	return base64.StdEncoding.EncodeToString(digest[:])
}

// verifyWebSocketAccept refuses every switch the RFC or the first-party
// subprotocol does not allow. A provider that does not echo the subprotocol has
// not agreed to the first-party frame vocabulary, so the socket is unusable.
func verifyWebSocketAccept(response *http.Response, key, subprotocol string) error {
	if !strings.EqualFold(strings.TrimSpace(response.Header.Get("Upgrade")), "websocket") {
		return errors.New(CodeClientResponse, "service did not upgrade the connection to websocket")
	}
	if !headerContainsToken(response.Header, "Connection", "upgrade") {
		return errors.New(CodeClientResponse, "service upgrade response does not carry the upgrade connection token")
	}
	if response.Header.Get("Sec-WebSocket-Accept") != webSocketAcceptToken(key) {
		return errors.New(CodeClientResponse, "service upgrade response does not prove it read the handshake key")
	}
	if response.Header.Get("Sec-WebSocket-Protocol") != subprotocol {
		return errWebSocketNotNegotiated
	}
	if strings.TrimSpace(response.Header.Get("Sec-WebSocket-Extensions")) != "" {
		return errors.New(CodeClientResponse, "service selected a websocket extension the client never offered")
	}
	return nil
}

// headerContainsToken reports whether a comma-separated header lists a token.
func headerContainsToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, candidate := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(candidate), token) {
				return true
			}
		}
	}
	return false
}

// readWebSocketRejection captures a bounded ordinary HTTP refusal so the caller
// can project it through the operation's declared errors.
func readWebSocketRejection(response *http.Response) error {
	defer func() {
		_ = response.Body.Close() //nolint:errcheck // the rejection is the authoritative result
	}()
	body, err := io.ReadAll(io.LimitReader(response.Body, wsHandshakeRejectionMaxBytes+1))
	if err != nil || int64(len(body)) > wsHandshakeRejectionMaxBytes {
		return errors.New(CodeClientResponse, "service websocket upgrade response failed")
	}
	return &webSocketHandshakeRejection{StatusCode: response.StatusCode, Header: response.Header, Body: body}
}
