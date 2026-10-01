// Package mcp implements a minimal Model Context Protocol (MCP) server that
// exposes the Putnami workspace to AI agent harnesses. It speaks JSON-RPC 2.0
// over a newline-delimited byte stream (the MCP "stdio" transport): the agent
// client spawns `putnami mcp`, exchanges messages over stdin/stdout, and the
// server dies with the session. There is no hosting, port, or core auth —
// execution stays inside the existing local trust boundary. Extensions may
// contribute tools whose own subprocesses make outbound authenticated calls;
// those credentials and network boundaries remain extension-owned, at the same
// trust level as the CLI's existing extension commands.
//
// The implementation is deliberately hand-rolled and stdlib-only, matching the
// framework's zero-dependency posture. It is an adapter over existing CLI
// internals (workspace, jobs, impact analysis) rather than a reimplementation,
// so tool results stay consistent with the `putnami` CLI.
//
// This package is a timeboxed spike (see tooling/cli/doc/mcp-spike.md). It is
// read-heavy on purpose: the only write operation is running build/test/lint
// jobs, which the CLI already exposes.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	proto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/useragent"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const (
	// AgentIdentityEnabledEnv is the explicit opt-in for propagating MCP agent
	// provenance to outbound HTTP clients and extension tool subprocesses.
	AgentIdentityEnabledEnv = "PUTNAMI_AGENT_IDENTITY"
	// AgentModelEnv lets a harness explicitly name the active model because MCP
	// initialize has no standard model identifier.
	AgentModelEnv = proto.AgentModelEnv
)

// maxFrameBytes bounds one inbound JSON-RPC frame, terminating newline
// included. The stdio framing carries no length prefix, so without a cap a
// client that streams a line and never terminates it makes the reader allocate
// until the host is out of memory — the frame has to be bounded while it is
// being read, not after (see readFrame).
//
// The number is sized off the largest request this server can legitimately
// receive. Core tool arguments are selectors and flags: the biggest is a
// `run_jobs` call listing every project in a workspace, tens of KiB even for a
// workspace an order of magnitude larger than this one. Extension tool
// arguments are agent-authored JSON, not file payloads. 4 MiB leaves roughly
// three orders of magnitude of headroom over that while keeping the worst case
// a single-digit-MiB allocation. It is deliberately tighter than the 16 MiB the
// CLI allows for machine-generated JSONL job events (internal/jobs), which
// carry whole asset manifests; nothing on the MCP request path does.
const maxFrameBytes = 4 << 20

// errFrameTooLarge rejects a frame that crossed maxFrameBytes. It is a
// recoverable transport error rather than a session-ending one, and its message
// is fixed-length so the reply can never be amplified by the request that
// caused it.
var errFrameTooLarge = fmt.Errorf("request exceeds the %d-byte maximum frame size", maxFrameBytes)

// ProjectSelection is the project narrowing the two catalog tools accept,
// already resolved into canonical project ids by the adapter. It is declared
// here — not imported from the command subtree — for the same reason the
// builders are injected as closures: this package holds no edge into
// internal/commands, so the CLI shell maps it onto the selection value the
// catalog builders take.
type ProjectSelection struct {
	// Projects is a comma-separated list of canonical project ids.
	Projects string
	// Impacted asks for the projects the current change reaches.
	Impacted bool
	// Baseline is the git ref backing Impacted.
	Baseline string
}

// Options configures a Server.
type Options struct {
	// WorkspaceRoot is the resolved workspace root the tools operate on.
	WorkspaceRoot string
	// Config is the loaded, alias-resolved workspace configuration.
	Config *wsproto.Config
	// ServerVersion is reported as serverInfo.version in initialize (the CLI
	// build version).
	ServerVersion string
	// ExtensionGuidance carries exact AI.md bytes read from lock-pinned
	// extension artifacts before the server was constructed. The bytes are
	// exposed verbatim as versioned resources and appended to initialize
	// instructions after a short, self-contained Putnami routing preamble.
	ExtensionGuidance []ExtensionGuidance
	// GuidanceIssues records declared guidance that bounded read preparation
	// could not make available. It is surfaced as an explicit resource status;
	// the server never substitutes guidance from an ambient or latest version.
	GuidanceIssues []GuidanceIssue
	// PreparedExtensions maps registry names to exact stable roots selected by
	// the workspace lock. Discovery replaces node_modules and other ambient
	// copies with these roots before registering any tool.
	PreparedExtensions map[string]string
	// UnavailableExtensions are registry extension names whose exact locked
	// artifact preparation failed or whose lock pin is absent. Their ambient
	// stable links must not leak tools into this read session.
	UnavailableExtensions []string
	// PrepareWorkspaceView lazily creates the recorded provider index before a
	// request reads graph-derived workspace facts. The CLI injects a bounded,
	// lock-faithful preparation; nil preserves the fail-closed recorded-view
	// behavior for embedders that cannot prepare one.
	//
	// Failures are not memoized. A later request may retry after an interrupted
	// runtime build, a transient artifact failure, or another process repairing
	// the workspace.
	PrepareWorkspaceView func(context.Context) error
	// PropagateAgentIdentity opts this MCP session into request provenance.
	// When false, captured client/model metadata stays session-local and all
	// outbound behavior remains unchanged.
	PropagateAgentIdentity bool
	// AgentModel is an explicit harness-provided model identifier. MCP itself
	// has no standard model field, so this value must never be inferred.
	AgentModel string
	// AgentContext returns one project's freshly aggregated agent-context result
	// (a commands.AgentContextResult, typed as any so this package needs no
	// dependency on internal/commands). It is dependency-injected by the CLI
	// shell, which is the one package that already depends on every adapter, so
	// this package keeps no edge into the command subtree. When nil, the
	// agent_context tool reports itself unavailable.
	AgentContext func(ctx context.Context, projectSelector string) (any, error)
	// WorkspaceMap returns the workspace orientation map (a
	// commands.WorkspaceMapResult, typed as any for the same reason
	// AgentContext is), rebuilt in memory and scoped to the requested section
	// and optional project. When nil, the workspace_map tool reports itself
	// unavailable.
	WorkspaceMap func(ctx context.Context, section, projectSelector string) (any, error)
	// ResolveSelection turns an already-id-resolved narrowing into the wire
	// block a manifest-declared tool that declares `workspaceSelection`
	// receives. It is injected for the same reason the closures above are:
	// the canonical resolver lives in internal/commands/shared, and this package
	// holds no edge into the command subtree.
	//
	// It takes the loaded workspace rather than a root so one tool call reads the
	// tree once — the membership on the request is projected from the SAME
	// value, which is what stops a tool from being told about a selection and a
	// membership resolved a moment apart.
	//
	// Nil is not "resolve nothing": a declaring tool then fails with a message
	// naming the missing wiring, because handing it an absent selection would
	// make it answer for the whole workspace when the caller asked for one
	// project.
	ResolveSelection func(ws *workspace.Workspace, selection ProjectSelection) (*proto.ToolSelection, error)
	// Preflight is the production doctor gate every `run_jobs` execution passes
	// through. It is injected for the same reason AgentContext is — the
	// evaluator lives in internal/commands — and an agent-initiated run must be
	// gated exactly like a human's, so leaving it nil does not disable the gate:
	// it fails a production run CLOSED (see engine.PreflightGate).
	Preflight engine.PreflightGate
}

// Server is a single-session MCP server. One Server handles one stdio
// connection; construct a fresh one per `putnami mcp` invocation.
type Server struct {
	opts   Options
	tools  []toolEntry
	byName map[string]toolEntry

	// out serializes all writes to the client; the read loop is single
	// goroutine but tool handlers could one day emit notifications, so the
	// writer is mutex-guarded from the start.
	out     *bufio.Writer
	writeMu sync.Mutex

	// identityMu guards the bounded identity captured during initialize. It is
	// retained for the full server session and read when an extension tool is
	// invoked.
	identityMu sync.RWMutex
	identity   proto.AgentIdentity

	// mu guards lastRun, the cached result of the most recent run_jobs call,
	// which get_diagnostics reads back without re-running anything.
	mu      sync.Mutex
	lastRun *runRecord

	// preparationMu guards the most recent lazy workspace-view preparation
	// failure. Graph handlers still build their bounded partial payload after a
	// failure; the recorded-view verdict joins this cause to that payload.
	preparationMu  sync.Mutex
	preparationErr error

	// notices captures what the engine writes to its human stdout stream during
	// a run_jobs call (engine.Request.Stdout). It is NOT guarded by a mutex on
	// purpose: the stdio loop dispatches one message at a time, so exactly one
	// run_jobs call is ever in flight, and the engine writes to it only from the
	// calling goroutine's stages. Buffering it is what keeps "No jobs matched.
	// Nothing to do." off the JSON-RPC transport while still letting the tool
	// explain why nothing ran.
	notices bytes.Buffer
}

// NewServer builds a Server with the standard tool set registered.
func NewServer(opts Options) *Server {
	s := &Server{opts: opts, byName: make(map[string]toolEntry)}
	s.registerTools()
	s.registerExtensionTools()
	return s
}

// loadWorkspace attempts to prepare graph facts before loading them. A failed
// preparation is retained rather than returned here: graph tools must still
// return their bounded partial payload and workspaceView=absent, as the public
// fail-closed contract promises. recordedView joins the retained cause into the
// error beside that payload.
func (s *Server) loadWorkspace(ctx context.Context) (*workspace.Workspace, error) {
	var preparationErr error
	if s.opts.PrepareWorkspaceView != nil {
		preparationErr = s.opts.PrepareWorkspaceView(ctx)
	}
	s.preparationMu.Lock()
	s.preparationErr = preparationErr
	s.preparationMu.Unlock()
	return workspace.Load(s.opts.WorkspaceRoot)
}

func (s *Server) recordedView(ws *workspace.Workspace) (workspace.RecordedView, error) {
	view, err := recordedView(ws)
	if err == nil {
		return view, nil
	}
	s.preparationMu.Lock()
	preparationErr := s.preparationErr
	s.preparationMu.Unlock()
	if preparationErr != nil {
		err = errors.Join(err, fmt.Errorf("workspace preparation failed: %w", preparationErr))
	}
	return view, err
}

// Serve runs the JSON-RPC loop until the input stream closes (client exit) or
// the context is canceled (process shutdown). Each input line is one JSON-RPC
// message; each response is one line. Reads happen on a helper goroutine so a
// canceled context unblocks the loop even while blocked on stdin.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	// A putnami mcp process owns one stdio session. Reset process-global HTTP
	// provenance at both boundaries so identity cannot survive a disconnected
	// client in tests or in a future in-process embedding.
	useragent.ClearAgentIdentity()
	defer useragent.ClearAgentIdentity()

	s.out = bufio.NewWriter(out)
	defer func() { _ = s.out.Flush() }()

	frames := make(chan inboundFrame)
	readErr := make(chan error, 1)
	go readFrames(in, frames, readErr)

	for {
		select {
		case <-ctx.Done():
			return nil
		case f := <-frames:
			if f.err != nil {
				s.rejectFrame(f.err)
				continue
			}
			s.handleLine(ctx, f.data)
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// inboundFrame carries one frame from the reader goroutine to the serve loop.
// err is set only for a frame the transport rejected but recovered from
// (currently errFrameTooLarge); fatal read errors travel on readErr because
// they end the session.
type inboundFrame struct {
	data []byte
	err  error
}

// readFrames feeds newline-delimited frames to the server loop. It sends the
// frame before the terminating error so a final line arriving together with
// EOF is still processed (the reader returns data and io.EOF in one call). An
// oversized frame is reported and the loop continues: the reader has already
// resynchronized on the next frame boundary, so the session survives it.
func readFrames(in io.Reader, frames chan<- inboundFrame, readErr chan<- error) {
	reader := bufio.NewReader(in)
	for {
		line, err := readFrame(reader)
		if errors.Is(err, errFrameTooLarge) {
			frames <- inboundFrame{err: err}
			continue
		}
		if len(line) > 0 {
			frames <- inboundFrame{data: line}
		}
		if err != nil {
			readErr <- err
			return
		}
	}
}

// readFrame reads one newline-terminated frame, enforcing maxFrameBytes WHILE
// reading rather than after the line is already in memory — buffering the whole
// line first and measuring it afterwards is the unbounded-allocation bug this
// guard exists to close. ReadSlice hands back a view into the reader's fixed
// internal buffer, so the only growing allocation is the returned frame and its
// growth stops at the cap. That returned slice is freshly allocated and never
// aliases the reader buffer, so the caller needs no defensive copy (which used
// to double peak frame memory).
func readFrame(r *bufio.Reader) ([]byte, error) {
	var frame []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(frame)+len(chunk) > maxFrameBytes {
			// Over the cap: drop what was accumulated, and when the frame is
			// still open, discard the rest of it through the fixed reader buffer
			// so the next frame boundary is reached without ever buffering the
			// oversized one. When err is nil or terminal the frame is already
			// fully consumed and there is nothing left to skip.
			if errors.Is(err, bufio.ErrBufferFull) {
				discardFrame(r)
			}
			return nil, errFrameTooLarge
		}
		frame = append(frame, chunk...)
		if err == nil {
			return frame, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return frame, err
	}
}

// discardFrame drops bytes up to and including the next newline without
// retaining any of them, resynchronizing the stream after an oversized frame.
// It stops at any other read error; the caller's next read surfaces it (an
// unterminated oversized frame therefore ends in EOF, not a hang).
func discardFrame(r *bufio.Reader) {
	for {
		if _, err := r.ReadSlice('\n'); !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
	}
}

// rejectFrame answers a frame the transport refused to buffer. The posture is
// recover-and-continue, matching how handleLine already answers an unparseable
// frame: a null id, one JSON-RPC error, session intact. Killing the session
// would be simpler, but it would put a second, contradictory recovery rule in
// the same loop for the same class of client mistake. The id is null because
// the frame was never parsed, so there is no id to echo back (JSON-RPC 2.0
// prescribes exactly this for a request whose id could not be determined).
func (s *Server) rejectFrame(err error) {
	s.write(&rpcResponse{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage("null"),
		Error:   &rpcError{Code: codeInvalidRequest, Message: err.Error()},
	})
}

// handleLine parses one frame and dispatches it, writing a response unless the
// frame was a notification or unparseable notification.
func (s *Server) handleLine(ctx context.Context, line []byte) {
	// Blank/keepalive lines are not messages; the stdio framing is one JSON
	// object per non-empty line.
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		// id is unknowable for an unparseable frame, so it is null per spec.
		s.write(&rpcResponse{
			JSONRPC: jsonRPCVersion,
			ID:      json.RawMessage("null"),
			Error:   &rpcError{Code: codeParseError, Message: "parse error: " + err.Error()},
		})
		return
	}

	resp := s.dispatch(ctx, &msg)
	if resp == nil {
		return // notification: no reply
	}
	s.write(resp)
}

// dispatch routes a message to its handler. It returns nil for notifications
// and otherwise a response to write.
func (s *Server) dispatch(ctx context.Context, msg *rpcMessage) *rpcResponse {
	switch msg.Method {
	case "initialize":
		return s.handleInitialize(msg)
	case "notifications/initialized", "initialized":
		return nil
	case "ping":
		return s.ok(msg.ID, struct{}{})
	case "tools/list":
		return s.ok(msg.ID, toolsListResult{Tools: s.toolDefs()})
	case "tools/call":
		return s.handleToolsCall(ctx, msg)
	case "resources/list":
		return s.ok(msg.ID, resourcesListResult{Resources: s.resourceDefs()})
	case "resources/read":
		return s.handleResourcesRead(ctx, msg)
	default:
		// Every other notification (no id) is ignored per JSON-RPC, including
		// the MCP cancellation notification — this server's tool calls are
		// short and synchronous, so there is nothing to cancel mid-flight.
		if msg.isNotification() {
			return nil
		}
		return s.fail(msg.ID, codeMethodNotFound, "method not found: "+msg.Method)
	}
}

func (s *Server) handleInitialize(msg *rpcMessage) *rpcResponse {
	var p initializeParams
	if len(msg.Params) > 0 {
		_ = json.Unmarshal(msg.Params, &p)
	}
	identity := useragent.MCPIdentity(s.opts.ServerVersion, p.ClientInfo.Name, p.ClientInfo.Version, s.opts.AgentModel)
	s.identityMu.Lock()
	s.identity = identity
	s.identityMu.Unlock()
	if s.opts.PropagateAgentIdentity {
		useragent.SetAgentIdentity(identity)
	}
	version := p.ProtocolVersion
	if version == "" {
		version = defaultProtocolVersion
	}
	return s.ok(msg.ID, initializeResult{
		ProtocolVersion: version,
		Capabilities: serverCaps{
			Tools:     &toolsCapability{ListChanged: false},
			Resources: &resourcesCapability{Subscribe: false, ListChanged: false},
		},
		ServerInfo:   serverInfo{Name: serverName, Version: s.opts.ServerVersion},
		Instructions: s.initializeInstructions(),
	})
}

// ok builds a success response.
func (s *Server) ok(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: jsonRPCVersion, ID: id, Result: result}
}

// fail builds an error response.
func (s *Server) fail(id json.RawMessage, code int, msg string) *rpcResponse {
	return &rpcResponse{JSONRPC: jsonRPCVersion, ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// write serializes one message as a single newline-delimited line and flushes,
// so the client can read it immediately.
func (s *Server) write(resp *rpcResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		// Marshaling a response should never fail; degrade to a generic error
		// rather than dropping the reply silently.
		data, _ = json.Marshal(&rpcResponse{
			JSONRPC: jsonRPCVersion,
			ID:      resp.ID,
			Error:   &rpcError{Code: codeInternalError, Message: "encode response failed"},
		})
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.out.Write(data)
	_ = s.out.WriteByte('\n')
	_ = s.out.Flush()
}
