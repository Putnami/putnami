// Package hostrpc serves the host RPC channel of one extension command
// launch: the calls the launched process makes into this putnami process,
// over the wire contract of go.putnami.dev/protocol/extension/hostrpc.
//
// A Server listens on a fresh Unix socket in a directory only the user can
// open, answers only requests that carry its token, and serves one call at a
// time. Stop closes the socket, cancels the call in flight, and removes the
// directory. A process killed before Stop leaves the directory behind; the
// next Start reclaims it, because the directory is a scratch directory
// (go.putnami.dev/sdk/extension/scratch) whose owner lock dies with its
// process.
package hostrpc

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/sdk/extension/ownerperm"
	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

const (
	// dirPrefix names every socket directory, so a sweep finds the ones a
	// killed process left.
	dirPrefix = scratch.Namespace + "hostrpc-"
	// socketName is the socket's name inside its directory.
	socketName = "host.sock"
	// maxSocketPath is the longest socket path every supported platform
	// accepts: macOS holds 104 bytes in sun_path, its terminating NUL
	// included.
	maxSocketPath = 103
	// firstAcceptWait and maxAcceptWait bound the wait before the accept
	// loop tries again after a failed accept.
	firstAcceptWait = 5 * time.Millisecond
	maxAcceptWait   = time.Second
)

// ErrHosted is the refusal of Start in a hosted run (--credential-fd): a
// process that holds the run credential opens no channel that repository code
// could call.
var ErrHosted = errors.New("a hosted run opens no host RPC channel")

// Methods are the core calls a Server answers besides host.hello. A nil method
// answers wire.CodeUnavailable. A method returns a *wire.Error to choose the
// code; any other error answers wire.CodeInternalError.
type Methods struct {
	// Collaboration answers collaboration.call with the envelope the CLI
	// prints for the same request.
	Collaboration func(context.Context, wire.CollaborationCallParams) (json.RawMessage, error)
	// VerificationReport answers verification.report with the report bytes.
	VerificationReport func(context.Context, wire.VerificationReportParams) ([]byte, error)
	// VersionLines answers version.lines with the workspace's version lines.
	// It refuses a scope that names no line with wire.CodeInvalidParams.
	VersionLines func(context.Context, wire.VersionLinesParams) (wire.VersionLinesResult, error)
	// WorkspaceOwners answers workspace.owners with the projects that own
	// each path. The server has already checked that the paths are not empty
	// and are valid workspace paths.
	WorkspaceOwners func(context.Context, wire.WorkspaceOwnersParams) (wire.WorkspaceOwnersResult, error)
}

// Server is one open host RPC channel.
type Server struct {
	dir      *scratch.Dir
	socket   string
	token    string
	listener net.Listener
	methods  Methods

	// ctx is canceled by Stop, which cancels the call in flight.
	ctx    context.Context
	cancel context.CancelFunc
	// callMu serializes the calls of every connection: the core state the
	// methods read is not built for concurrent use.
	callMu sync.Mutex

	connMu  sync.Mutex
	conns   map[net.Conn]struct{}
	stopped bool
	// serving counts the accept loop and the connection goroutines.
	serving sync.WaitGroup
	// stopping is closed by Stop, which ends the accept loop's wait between
	// two failed accepts.
	stopping chan struct{}

	stopOnce sync.Once
	stopErr  error
}

// Start opens a channel in a fresh socket directory and serves it until Stop.
// Canceling ctx cancels the call in flight, like Stop, but leaves the socket
// open. A hosted run gets ErrHosted.
func Start(ctx context.Context, methods Methods) (*Server, error) {
	if runcredential.Hosted() {
		return nil, ErrHosted
	}
	return startIn(ctx, methods, socketRoots())
}

// socketRoots lists where a socket directory may go, in order: the
// temporary directory, then /tmp where a long temporary directory would make
// the socket path too long.
func socketRoots() []string {
	roots := []string{os.TempDir()}
	if runtime.GOOS != "windows" && os.TempDir() != "/tmp" {
		roots = append(roots, "/tmp")
	}
	return roots
}

func startIn(ctx context.Context, methods Methods, roots []string) (*Server, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	dir, socket, err := openSocketDir(roots)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_ = dir.Remove()
		return nil, fmt.Errorf("listen on the host RPC socket %s: %w", socket, err)
	}
	return serveOn(ctx, methods, dir, socket, token, listener), nil
}

// serveOn serves listener, which listens on socket in dir, until Stop.
func serveOn(ctx context.Context, methods Methods, dir *scratch.Dir, socket, token string, listener net.Listener) *Server {
	server := &Server{
		dir:      dir,
		socket:   socket,
		token:    token,
		listener: listener,
		methods:  methods,
		conns:    map[net.Conn]struct{}{},
		stopping: make(chan struct{}),
	}
	server.ctx, server.cancel = context.WithCancel(ctx)
	server.serving.Add(1)
	go server.accept()
	return server
}

// newToken returns wire.TokenBytes random bytes, hex-encoded.
func newToken() (string, error) {
	raw := make([]byte, wire.TokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("draw the host RPC token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// openSocketDir creates the socket directory under the first root whose
// socket path fits maxSocketPath, and restricts it to the user.
func openSocketDir(roots []string) (*scratch.Dir, string, error) {
	var errs []error
	for _, root := range roots {
		dir, err := scratch.NewIn(root, dirPrefix)
		if err != nil {
			errs = append(errs, fmt.Errorf("create a host RPC directory under %s: %w", root, err))
			continue
		}
		socket := filepath.Join(dir.Path(), socketName)
		if len(socket) > maxSocketPath {
			_ = dir.Remove()
			errs = append(errs, fmt.Errorf("the socket path %s is %d bytes, longer than the %d a socket path holds", socket, len(socket), maxSocketPath))
			continue
		}
		if err := ownerperm.Restrict(dir.Path(), 0o700); err != nil {
			_ = dir.Remove()
			errs = append(errs, fmt.Errorf("restrict the host RPC directory %s: %w", dir.Path(), err))
			continue
		}
		return dir, socket, nil
	}
	return nil, "", errors.Join(errs...)
}

// Socket returns the socket path.
func (s *Server) Socket() string {
	return s.socket
}

// Env returns the variables the launched process receives.
func (s *Server) Env() []string {
	return []string{wire.SocketEnv + "=" + s.socket, wire.TokenEnv + "=" + s.token}
}

// Stop closes the channel: it cancels the call in flight, closes every
// connection and the socket, waits for the connection goroutines, and removes
// the socket directory. It returns the first Stop's error on every call.
func (s *Server) Stop() error {
	s.stopOnce.Do(func() {
		close(s.stopping)
		s.cancel()
		closeErr := s.listener.Close()
		if errors.Is(closeErr, net.ErrClosed) {
			closeErr = nil
		}
		s.connMu.Lock()
		s.stopped = true
		for conn := range s.conns {
			_ = conn.Close()
		}
		s.connMu.Unlock()
		s.serving.Wait()
		s.stopErr = errors.Join(closeErr, s.dir.Remove())
	})
	return s.stopErr
}

// accept serves each connection the socket receives until Stop. A failed
// accept, such as one past the open-file limit, is tried again after a wait
// that doubles from firstAcceptWait up to maxAcceptWait, as
// net/http.Server.Serve does, so the socket serves again once the cause
// passes. Only a closed listener or Stop ends the loop.
func (s *Server) accept() {
	defer s.serving.Done()
	var wait time.Duration
	for {
		conn, err := s.listener.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			wait = min(max(2*wait, firstAcceptWait), maxAcceptWait)
			timer := time.NewTimer(wait)
			select {
			case <-s.stopping:
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		wait = 0
		s.connMu.Lock()
		if s.stopped {
			s.connMu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.serving.Add(1)
		s.connMu.Unlock()
		go s.serve(conn)
	}
}

// serve answers the requests of one connection in order. It returns when the
// peer closes the connection, when a frame exceeds the limit, after a refused
// token, and when Stop closes the connection.
func (s *Server) serve(conn net.Conn) {
	defer s.serving.Done()
	defer func() {
		s.connMu.Lock()
		delete(s.conns, conn)
		s.connMu.Unlock()
		_ = conn.Close()
	}()
	reader := bufio.NewReader(conn)
	for {
		frame, err := wire.ReadFrame(reader, wire.MaxFrameBytes)
		if errors.Is(err, wire.ErrFrameTooLarge) {
			_ = s.write(conn, failure(nil, wire.CodeInvalidRequest,
				fmt.Sprintf("the frame exceeds %d bytes", wire.MaxFrameBytes)))
			return
		}
		if err != nil {
			return
		}
		response, keep := s.handle(frame)
		if response != nil {
			if err := s.write(conn, response); err != nil {
				return
			}
		}
		if !keep {
			return
		}
	}
}

func (s *Server) write(conn net.Conn, response *wire.Response) error {
	frame, err := wire.EncodeFrame(response)
	if err != nil {
		frame, err = wire.EncodeFrame(failure(response.ID, wire.CodeInternalError,
			fmt.Sprintf("the result does not fit one frame: %v", err)))
		if err != nil {
			return err
		}
	}
	_, err = conn.Write(frame)
	return err
}

// handle answers one frame. It returns a nil response for a notification,
// and keep false when the connection must close: after a frame with another
// token, which gets -32001 when it is a request and no answer when it is a
// notification.
func (s *Server) handle(frame []byte) (*wire.Response, bool) {
	if !json.Valid(frame) {
		return failure(nil, wire.CodeParseError, "the frame is not JSON"), true
	}
	var request wire.Request
	if err := json.Unmarshal(frame, &request); err != nil {
		return failure(nil, wire.CodeInvalidRequest, "a request is a JSON object"), true
	}
	id := request.ID
	if !validID(id) {
		id = nil
	}
	if subtle.ConstantTimeCompare([]byte(request.Token), []byte(s.token)) != 1 {
		if len(request.ID) == 0 {
			return nil, false // a notification gets no answer, not even a refusal
		}
		return failure(id, wire.CodeUnauthorized, "the token is not this launch's "+wire.TokenEnv), false
	}
	if len(request.ID) == 0 {
		return nil, true // a notification: no method accepts one
	}
	if id == nil || request.JSONRPC != wire.JSONRPCVersion || request.Method == "" {
		return failure(id, wire.CodeInvalidRequest,
			`a request carries "jsonrpc":"2.0", a string or number id, and a method`), true
	}
	result, err := s.dispatch(request.Method, request.Params)
	if err != nil {
		var refusal *wire.Error
		if !errors.As(err, &refusal) {
			// The core exits 2 for an invalid configuration and 1 for any
			// other failure, so the two keep their own codes on the wire.
			code := wire.CodeInternalError
			if errors.Is(err, cmderr.ErrInvalidConfig) {
				code = wire.CodeInvalidConfig
			}
			refusal = &wire.Error{Code: code, Message: err.Error()}
		}
		return &wire.Response{JSONRPC: wire.JSONRPCVersion, ID: id, Error: refusal}, true
	}
	return &wire.Response{JSONRPC: wire.JSONRPCVersion, ID: id, Result: result}, true
}

// validID reports whether id is a JSON string, number or null.
func validID(id json.RawMessage) bool {
	if len(id) == 0 {
		return false
	}
	switch id[0] {
	case '"', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	}
	return string(id) == "null"
}

func (s *Server) dispatch(method string, params json.RawMessage) (json.RawMessage, error) {
	switch method {
	case wire.MethodHello:
		if err := noParams(params); err != nil {
			return nil, err
		}
		return json.Marshal(wire.HelloResult{Protocol: wire.ProtocolVersion, Methods: wire.Methods()})
	case wire.MethodCollaborationCall:
		var call wire.CollaborationCallParams
		if err := decodeParams(params, &call); err != nil {
			return nil, err
		}
		if call.Contract == "" || call.Operation == "" {
			return nil, invalidParams("collaboration.call needs a contract and an operation")
		}
		if s.methods.Collaboration == nil {
			return nil, unavailable(method)
		}
		return s.call(func(ctx context.Context) (json.RawMessage, error) {
			return s.methods.Collaboration(ctx, call)
		})
	case wire.MethodVerificationReport:
		var report wire.VerificationReportParams
		if err := decodeParams(params, &report); err != nil {
			return nil, err
		}
		if report.Project == "" || report.Task == "" || report.Path == "" || report.Digest == "" {
			return nil, invalidParams("verification.report needs a project, a task, a path and a digest")
		}
		if s.methods.VerificationReport == nil {
			return nil, unavailable(method)
		}
		return s.call(func(ctx context.Context) (json.RawMessage, error) {
			data, err := s.methods.VerificationReport(ctx, report)
			if err != nil {
				return nil, err
			}
			return json.Marshal(wire.VerificationReportResult{Report: data})
		})
	case wire.MethodVersionLines:
		var lines wire.VersionLinesParams
		if err := decodeParams(params, &lines); err != nil {
			return nil, err
		}
		if s.methods.VersionLines == nil {
			return nil, unavailable(method)
		}
		return s.call(func(ctx context.Context) (json.RawMessage, error) {
			result, err := s.methods.VersionLines(ctx, lines)
			if err != nil {
				return nil, err
			}
			return json.Marshal(result)
		})
	case wire.MethodWorkspaceOwners:
		var owners wire.WorkspaceOwnersParams
		if err := decodeParams(params, &owners); err != nil {
			return nil, err
		}
		if len(owners.Paths) == 0 {
			return nil, invalidParams("workspace.owners needs one or more paths")
		}
		for _, path := range owners.Paths {
			if !wire.ValidWorkspacePath(path) {
				return nil, invalidParams(fmt.Sprintf("workspace.owners takes workspace-relative paths in io/fs form, such as dir/file.go; %q is not one", path))
			}
		}
		if s.methods.WorkspaceOwners == nil {
			return nil, unavailable(method)
		}
		return s.call(func(ctx context.Context) (json.RawMessage, error) {
			result, err := s.methods.WorkspaceOwners(ctx, owners)
			if err != nil {
				return nil, err
			}
			return json.Marshal(result)
		})
	default:
		return nil, &wire.Error{Code: wire.CodeMethodNotFound,
			Message: fmt.Sprintf("the host does not serve %q; %s lists the methods it serves", method, wire.MethodHello)}
	}
}

// call runs one method under callMu, and returns when it answers or when
// Stop cancels the channel, whichever comes first. A method that ignores its
// context then finishes on its own, and its answer is dropped.
func (s *Server) call(method func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	type answer struct {
		result json.RawMessage
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		s.callMu.Lock()
		defer s.callMu.Unlock()
		if err := s.ctx.Err(); err != nil {
			answered <- answer{err: err}
			return
		}
		result, err := method(s.ctx)
		answered <- answer{result: result, err: err}
	}()
	select {
	case got := <-answered:
		if got.err != nil && s.ctx.Err() != nil {
			return nil, &wire.Error{Code: wire.CodeUnavailable, Message: "the host stopped while the call ran"}
		}
		return got.result, got.err
	case <-s.ctx.Done():
		return nil, &wire.Error{Code: wire.CodeUnavailable, Message: "the host stopped while the call ran"}
	}
}

func noParams(params json.RawMessage) error {
	if len(params) == 0 || string(params) == "null" {
		return nil
	}
	var none struct{}
	if err := wire.DecodeStrict(params, &none); err != nil {
		return invalidParams(wire.MethodHello + " takes no params")
	}
	return nil
}

func decodeParams(params json.RawMessage, into any) error {
	if len(params) == 0 || string(params) == "null" {
		return invalidParams("the method needs a params object")
	}
	if err := wire.DecodeStrict(params, into); err != nil {
		return invalidParams(fmt.Sprintf("the params do not decode: %v", err))
	}
	return nil
}

func invalidParams(message string) *wire.Error {
	return &wire.Error{Code: wire.CodeInvalidParams, Message: message}
}

func unavailable(method string) *wire.Error {
	return &wire.Error{Code: wire.CodeUnavailable, Message: fmt.Sprintf("this launch does not serve %s", method)}
}

func failure(id json.RawMessage, code int, message string) *wire.Response {
	return &wire.Response{JSONRPC: wire.JSONRPCVersion, ID: id, Error: &wire.Error{Code: code, Message: message}}
}
