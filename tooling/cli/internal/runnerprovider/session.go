// Package runnerprovider drives an out-of-process runner provider over the
// provider RPC defined in protocol/runner, and imports the canonical session
// bundle a remote execution returns into the local session store.
//
// It owns transport, process supervision and import validation. It never
// plans, schedules or reduces results: the executing engine inside the
// isolated snapshot is the only verdict producer, and this package carries
// its records unchanged. A provider failure is reported precisely; it is never
// permission to run the work locally instead.
package runnerprovider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/sdk/extension/proctree"
)

// BoundRequestEnv names the file holding the bound execution request an
// executing engine consumes (runner.BoundRequestEnv). It is an execution-only
// channel: the provider exports it into the pinned entrypoint it launches, the
// adapter removes it before any task runs, and nothing derived from it reaches
// a task parameter, a cache key or a run marker.
const BoundRequestEnv = runner.BoundRequestEnv

// maxResponseBytes bounds one provider response line. Responses carry
// metadata and forwarded output records, never blob bytes.
const maxResponseBytes = 16 << 20

// shutdownGrace bounds the graceful close before the provider is killed. It
// bounds cleanup of a process this package owns, never a verdict.
const shutdownGrace = 5 * time.Second

// exitDrainGrace bounds how long an exited provider's output is still read.
const exitDrainGrace = 2 * time.Second

// LaunchSpec describes how to start the provider subprocess.
type LaunchSpec struct {
	Command string
	Args    []string
	Dir     string
	Env     []string
}

// OpError reports an op the provider answered with a failure.
type OpError struct {
	Op      runner.ProviderOp
	Code    string
	Message string
}

func (e *OpError) Error() string {
	message := e.Message
	if message == "" {
		message = "provider reported failure"
	}
	return fmt.Sprintf("runner provider op %s failed (%s): %s", e.Op, e.Code, message)
}

// Session is a live connection to one provider process. Requests are written
// one per line; responses are demultiplexed by correlation id.
type Session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	tree   *proctree.Tree

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *runner.ProviderResponse
	dead    bool
	deadErr error
	closing bool

	waitCh   chan error
	readDone chan struct{}
}

func newSession() *Session {
	return &Session{
		pending:  make(map[int64]chan *runner.ProviderResponse),
		nextID:   1,
		waitCh:   make(chan error, 1),
		readDone: make(chan struct{}),
	}
}

// Connect wires a Session over caller-supplied pipes; tests use it to speak
// the protocol in-process.
func Connect(stdin io.WriteCloser, stdout io.ReadCloser) *Session {
	s := newSession()
	s.stdin, s.stdout = stdin, stdout
	s.waitCh <- nil
	go s.readLoop()
	return s
}

// Spawn starts the provider subprocess as the root of its own process tree and
// begins reading its responses. The process is deliberately NOT bound to ctx:
// the run's cancellation must still be able to send a cancel request and read
// its acknowledgement through this session, so only Close terminates the tree.
func Spawn(ctx context.Context, spec LaunchSpec, stderr io.Writer) (*Session, error) {
	s := newSession()
	cmd := exec.CommandContext(context.WithoutCancel(ctx), spec.Command, spec.Args...)
	cmd.Dir, cmd.Env, cmd.Stderr = spec.Dir, spec.Env, stderr
	tree := proctree.New(cmd)
	cmd.WaitDelay = shutdownGrace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("runner provider stdin pipe: %w", err)
	}
	// Stdout is a pipe this session owns, not cmd.StdoutPipe: Wait never
	// waits on it, so the provider's exit is observed even when a process it
	// started still holds the write end.
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("runner provider stdout pipe: %w", err)
	}
	cmd.Stdout = stdoutWriter
	if err := tree.Start(); err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return nil, fmt.Errorf("start runner provider %s: %w", spec.Command, err)
	}
	// The provider holds its own copy of the write end.
	_ = stdoutWriter.Close()
	s.cmd, s.stdin, s.stdout, s.tree = cmd, stdin, stdout, tree
	go s.readLoop()
	// Once the provider exits, the reader gets exitDrainGrace to read what
	// the provider wrote before exiting. A process it started that still
	// holds the write end would keep the reader from EOF forever, so after
	// the grace the session ends: the rest of the tree is killed while that
	// process still keeps the group's ID taken, and the read end is closed.
	go func() {
		err := cmd.Wait()
		select {
		case <-s.readDone:
		case <-time.After(exitDrainGrace):
			s.markDead(fmt.Errorf("runner provider exited and its output stayed open for %s after; a process it started may still hold it", exitDrainGrace))
			_ = tree.Kill()
			_ = stdout.Close()
			<-s.readDone
		}
		s.waitCh <- err
	}()
	return s, nil
}

func (s *Session) readLoop() {
	defer close(s.readDone)
	scanner := bufio.NewScanner(s.stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), maxResponseBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		response, err := runner.ParseProviderResponse(line)
		if err != nil {
			s.markDead(fmt.Errorf("runner provider sent a malformed response: %w", err))
			return
		}
		s.mu.Lock()
		ch, ok := s.pending[response.ID]
		if ok {
			delete(s.pending, response.ID)
		}
		s.mu.Unlock()
		if ok {
			ch <- response
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	s.markDead(fmt.Errorf("runner provider session ended: %w", err))
}

func (s *Session) markDead(err error) {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return
	}
	s.dead, s.deadErr = true, err
	pending := s.pending
	s.pending = make(map[int64]chan *runner.ProviderResponse)
	s.mu.Unlock()
	for _, ch := range pending {
		ch <- nil
	}
}

// ended reports whether the transport died; a dead session answers nothing.
func (s *Session) ended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead
}

func (s *Session) deathErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadErr != nil {
		return s.deadErr
	}
	return errors.New("runner provider session ended")
}

// call writes one request and waits for its response, the context, or death.
func (s *Session) call(ctx context.Context, op runner.ProviderOp, params any) (*runner.ProviderResponse, error) {
	payload, err := runner.MarshalPayload(params)
	if err != nil {
		return nil, fmt.Errorf("encode %s params: %w", op, err)
	}
	s.mu.Lock()
	if s.dead {
		err := s.deadErr
		s.mu.Unlock()
		return nil, err
	}
	id := s.nextID
	s.nextID++
	ch := make(chan *runner.ProviderResponse, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	line, err := json.Marshal(&runner.ProviderRequest{ProtocolVersion: runner.ProviderProtocolVersion, ID: id, Op: op, Payload: payload})
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", op, err)
	}
	s.writeMu.Lock()
	_, err = s.stdin.Write(append(line, '\n'))
	s.writeMu.Unlock()
	if err != nil {
		s.markDead(fmt.Errorf("runner provider write failed: %w", err))
		return nil, s.deathErr()
	}
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("runner provider op %s: %w", op, ctx.Err())
	case response := <-ch:
		if response == nil {
			return nil, s.deathErr()
		}
		return response, nil
	}
}

func decode[T any](op runner.ProviderOp, response *runner.ProviderResponse, out *T) error {
	if !response.OK {
		return &OpError{Op: op, Code: response.Error.Code, Message: response.Error.Message}
	}
	if len(response.Payload) == 0 {
		return fmt.Errorf("runner provider op %s returned no result", op)
	}
	if err := json.Unmarshal(response.Payload, out); err != nil {
		return fmt.Errorf("runner provider op %s returned an invalid result: %w", op, err)
	}
	return nil
}

// Initialize negotiates the protocol. A provider that is not ready, that
// speaks another version, or that does not echo the request capability is a
// precise configuration failure.
func (s *Session) Initialize(ctx context.Context, params *runner.InitializeParams) (*runner.InitializeResult, error) {
	response, err := s.call(ctx, runner.OpInitialize, params)
	if err != nil {
		return nil, err
	}
	var result runner.InitializeResult
	if err := decode(runner.OpInitialize, response, &result); err != nil {
		return nil, err
	}
	if result.ProtocolVersion != runner.ProviderProtocolVersion {
		return nil, fmt.Errorf("runner provider %s speaks protocol version %d; this CLI requires %d", result.ProviderName, result.ProtocolVersion, runner.ProviderProtocolVersion)
	}
	for _, capability := range []string{runner.CapabilityExecutionRequestV1, runner.CapabilitySessionBundleV1} {
		if !contains(result.Capabilities, capability) {
			return nil, fmt.Errorf("runner provider %s does not support %s", result.ProviderName, capability)
		}
	}
	if !result.Ready {
		reason := result.Reason
		if reason == "" {
			reason = "provider reported not ready"
		}
		return nil, fmt.Errorf("runner provider %s cannot execute: %s", result.ProviderName, reason)
	}
	return &result, nil
}

// Prepare offers the manifest and returns the blobs still missing remotely.
func (s *Session) Prepare(ctx context.Context, params *runner.PrepareParams) (*runner.PrepareResult, error) {
	response, err := s.call(ctx, runner.OpPrepare, params)
	if err != nil {
		return nil, err
	}
	var result runner.PrepareResult
	return &result, decode(runner.OpPrepare, response, &result)
}

// Submit submits the bound request and returns the attempt reference.
func (s *Session) Submit(ctx context.Context, params *runner.SubmitParams) (*runner.SubmitResult, error) {
	response, err := s.call(ctx, runner.OpSubmit, params)
	if err != nil {
		return nil, err
	}
	var result runner.SubmitResult
	if err := decode(runner.OpSubmit, response, &result); err != nil {
		return nil, err
	}
	if result.Attempt == "" {
		return nil, fmt.Errorf("runner provider accepted the submission without an attempt reference")
	}
	return &result, nil
}

// Lookup resolves the attempt an idempotency key already names. A miss is a
// result with no attempt, not an error: it is the answer that permits a submit.
func (s *Session) Lookup(ctx context.Context, params *runner.LookupParams) (*runner.LookupResult, error) {
	response, err := s.call(ctx, runner.OpLookup, params)
	if err != nil {
		return nil, err
	}
	var result runner.LookupResult
	if err := decode(runner.OpLookup, response, &result); err != nil {
		return nil, err
	}
	if result.Attempt != "" && !runner.KnownState(result.State) {
		return nil, fmt.Errorf("runner provider reported unknown attempt state %q for %s", result.State, result.Attempt)
	}
	return &result, nil
}

// Follow reads records after the cursor. An answer carrying more than
// MaxFollowRecords records is a protocol violation: buffering is bounded on
// both sides by contract, not by trust.
func (s *Session) Follow(ctx context.Context, params *runner.FollowParams) (*runner.FollowResult, error) {
	response, err := s.call(ctx, runner.OpFollow, params)
	if err != nil {
		return nil, err
	}
	var result runner.FollowResult
	if err := decode(runner.OpFollow, response, &result); err != nil {
		return nil, err
	}
	if len(result.Records) > runner.MaxFollowRecords {
		return nil, fmt.Errorf("runner provider answered follow with %d records; the contract bounds an answer at %d", len(result.Records), runner.MaxFollowRecords)
	}
	return &result, nil
}

// Cancel asks the provider to terminate an attempt and returns the state it
// observed once the request was applied.
func (s *Session) Cancel(ctx context.Context, params *runner.CancelParams) (*runner.CancelResult, error) {
	response, err := s.call(ctx, runner.OpCancel, params)
	if err != nil {
		return nil, err
	}
	var result runner.CancelResult
	if err := decode(runner.OpCancel, response, &result); err != nil {
		return nil, err
	}
	if !runner.KnownState(result.State) {
		return nil, fmt.Errorf("runner provider acknowledged cancellation with unknown state %q", result.State)
	}
	return &result, nil
}

// Fetch returns the final bundle of a terminal attempt.
func (s *Session) Fetch(ctx context.Context, params *runner.FetchParams) (*runner.FetchResult, error) {
	response, err := s.call(ctx, runner.OpFetch, params)
	if err != nil {
		return nil, err
	}
	var result runner.FetchResult
	return &result, decode(runner.OpFetch, response, &result)
}

// Close sends a best-effort shutdown, closes stdin and waits for the process,
// killing the group if it overstays the grace period. A process outside the
// killed group that still holds the provider's output open gets another grace
// period; then the output is closed under it. Close is idempotent.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	dead := s.dead
	s.mu.Unlock()
	if !dead {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		_, _ = s.call(ctx, runner.OpShutdown, nil)
		cancel()
	}
	_ = s.stdin.Close()
	select {
	case <-s.waitCh:
	case <-time.After(shutdownGrace):
		_ = s.tree.Kill()
		select {
		case <-s.waitCh:
		case <-time.After(shutdownGrace):
			_ = s.stdout.Close()
			<-s.waitCh
		}
	}
	_ = s.tree.Close()
	<-s.readDone
	return nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
