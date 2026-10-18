// Package hostrpc is the extension side of the host RPC channel: a client an
// extension command uses to call into the putnami process that launched it,
// instead of running putnami again as a subprocess.
//
// putnami opens the channel for an interactive command-group subcommand it
// launches, and names it in two environment variables. Dial reads them:
//
//	client, err := hostrpc.Dial()
//	if err != nil {
//		return err // a *NoHostError when putnami opened no channel
//	}
//	defer client.Close()
//	envelope, err := client.CallCollaboration(ctx, "tasks", "find", json.RawMessage(`{}`))
//
// The wire contract, its methods and its error codes are defined in
// go.putnami.dev/protocol/extension/hostrpc.
package hostrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	wire "go.putnami.dev/protocol/extension/hostrpc"
)

// NoHostError is the error of Dial when the environment names no channel.
type NoHostError struct {
	// Missing names the variables that are not set.
	Missing []string
}

// Error says which variables are missing and why putnami may not have set
// them.
func (e *NoHostError) Error() string {
	return fmt.Sprintf("no putnami host RPC channel: %s not set. putnami sets them only for an interactive "+
		"command-group subcommand it launches; a build job, a provider tool, a hosted run (--credential-fd) "+
		"and a process putnami did not launch receive none, and a launch whose channel did not open prints "+
		"a warning on stderr", strings.Join(e.Missing, " and "))
}

// Client is one connection to the host. Its methods are safe for concurrent
// use; the host answers them one at a time. A call that fails on the
// connection, or whose context ends before the host's whole answer arrives,
// breaks the client: every later call returns the same error, and the
// extension dials again. An answer that arrived whole stands, even when the
// context ends right after.
type Client struct {
	conn  net.Conn
	token string

	mu     sync.Mutex
	reader *bufio.Reader
	nextID uint64
	broken error
}

// Dial connects to the channel the environment names in wire.SocketEnv and
// wire.TokenEnv. It returns a *NoHostError when either is unset.
func Dial() (*Client, error) {
	socket, token := os.Getenv(wire.SocketEnv), os.Getenv(wire.TokenEnv)
	var missing []string
	if socket == "" {
		missing = append(missing, wire.SocketEnv)
	}
	if token == "" {
		missing = append(missing, wire.TokenEnv)
	}
	if len(missing) > 0 {
		return nil, &NoHostError{Missing: missing}
	}
	return Connect(socket, token)
}

// Connect connects to the channel at socket with token.
func Connect(socket, token string) (*Client, error) {
	conn, err := net.Dial("unix", socket) //nolint:gosec // G704: the destination is a local Unix socket, the one putnami named for this process or the one the caller names; connecting to it is this function's purpose
	if err != nil {
		return nil, fmt.Errorf("connect to the putnami host RPC channel %s: %w", socket, err)
	}
	return newClient(conn, token), nil
}

func newClient(conn net.Conn, token string) *Client {
	return &Client{conn: conn, token: token, reader: bufio.NewReader(conn)}
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Hello returns the host's protocol version and the methods it serves.
func (c *Client) Hello(ctx context.Context) (wire.HelloResult, error) {
	var hello wire.HelloResult
	err := c.call(ctx, wire.MethodHello, nil, &hello)
	return hello, err
}

// CallCollaboration routes one collaboration contract operation and returns
// its envelope, byte for byte what `putnami <contract> <operation>
// --output=json` prints for the same input. A nil input is the empty request.
// An operation that fails at the provider is not an error: the envelope
// reports it.
func (c *Client) CallCollaboration(ctx context.Context, contract, operation string, input json.RawMessage) (json.RawMessage, error) {
	var envelope json.RawMessage
	err := c.call(ctx, wire.MethodCollaborationCall,
		wire.CollaborationCallParams{Contract: contract, Operation: operation, Input: input}, &envelope)
	return envelope, err
}

// VerificationReport returns the bytes of the verification report ref names,
// once the host's cache corroborates them. A report the cache does not
// corroborate is a *wire.Error with wire.CodeNotVerified.
func (c *Client) VerificationReport(ctx context.Context, ref wire.VerificationReportParams) ([]byte, error) {
	var report wire.VerificationReportResult
	if err := c.call(ctx, wire.MethodVerificationReport, ref, &report); err != nil {
		return nil, err
	}
	return report.Report, nil
}

// call sends one request and decodes its result into result. The host's
// refusal is returned as a *wire.Error.
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken != nil {
		return c.broken
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.nextID++
	id := strconv.FormatUint(c.nextID, 10)
	request := wire.Request{JSONRPC: wire.JSONRPCVersion, ID: json.RawMessage(id), Method: method, Token: c.token}
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("encode the %s params: %w", method, err)
		}
		request.Params = encoded
	}
	frame, err := wire.EncodeFrame(request)
	if err != nil {
		return fmt.Errorf("encode the %s request: %w", method, err)
	}

	deadline, _ := ctx.Deadline()
	if err := c.conn.SetDeadline(deadline); err != nil {
		return c.breakWith(ctx, method, err)
	}
	// A context that ends mid-call moves the deadline into the past, which
	// unblocks the write or the read below.
	ended := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = c.conn.SetDeadline(time.Unix(1, 0))
		close(ended)
	})
	defer stop()
	if _, err := c.conn.Write(frame); err != nil {
		return c.breakWith(ctx, method, err)
	}
	answer, err := wire.ReadFrame(c.reader, wire.MaxFrameBytes)
	if err != nil {
		return c.breakWith(ctx, method, err)
	}
	if !stop() {
		// The context ended once the whole answer had arrived, so the answer
		// stands. Wait for the past deadline to land: the next call's
		// SetDeadline then replaces it.
		<-ended
	}

	// An error object may carry members this client does not know, such as
	// JSON-RPC's optional data, so the response decodes leniently.
	var response wire.Response
	if err := json.Unmarshal(answer, &response); err != nil {
		return c.breakWith(ctx, method, fmt.Errorf("the host answered a frame that is not a response: %w", err))
	}
	if response.JSONRPC != wire.JSONRPCVersion || string(response.ID) != id {
		return c.breakWith(ctx, method, fmt.Errorf("the host answered request %s, not %s", response.ID, id))
	}
	if response.Error != nil {
		return response.Error
	}
	if len(response.Result) == 0 {
		return fmt.Errorf("the host answered %s with neither a result nor an error", method)
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("decode the %s result: %w", method, err)
	}
	return nil
}

// breakWith marks the client broken by a failure of the connection. When ctx
// ended, its error is the cause.
func (c *Client) breakWith(ctx context.Context, method string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	if errors.Is(err, wire.ErrFrameTooLarge) {
		err = fmt.Errorf("the host's answer exceeds %d bytes: %w", wire.MaxFrameBytes, err)
	}
	c.broken = fmt.Errorf("putnami host RPC %s: %w", method, err)
	_ = c.conn.Close()
	return c.broken
}
