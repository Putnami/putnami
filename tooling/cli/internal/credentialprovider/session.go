// Package credentialprovider asks the workspace's credential provider for one
// credential per purpose over the credential-provider RPC defined in
// protocol/registry.
//
// The provider is the one loaded extension that declares the reserved command
// registry.CredentialProviderCommand. The engine consults it only for the
// purposes the process enabled (`--providers`), starts it on the first
// credential a consumer needs, asks it at most once per purpose while the
// answer it holds is valid, and sends a bearer only to the hosts the
// credential names. This package never writes a bearer to any output.
package credentialprovider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/proctree"
)

// shutdownGrace bounds the graceful close before the provider tree is killed.
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

// RefusalError is a provider's refusal to issue a purpose's credential. The
// work that needed the credential fails with it; nothing retries on another
// credential.
type RefusalError struct {
	Purpose string
	Code    string
	Message string
}

func (e *RefusalError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the credential provider refused the %s credential (%s)", e.Purpose, e.Code)
	}
	return fmt.Sprintf("the credential provider refused the %s credential (%s): %s", e.Purpose, e.Code, e.Message)
}

type pendingCall struct {
	op registry.CredentialOp
	ch chan *registry.CredentialResponse
}

// Session is a live connection to one provider process. Requests are written
// one per line; responses are matched to requests by correlation id.
type Session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	tree   *proctree.Tree

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]pendingCall
	dead    bool
	deadErr error
	closing bool

	waitCh   chan error
	readDone chan struct{}
}

func newSession() *Session {
	return &Session{
		pending:  make(map[int64]pendingCall),
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

// Spawn starts the provider subprocess as the root of its own process tree.
// The process is not bound to ctx: only Close ends it, after a shutdown.
func Spawn(ctx context.Context, spec LaunchSpec, stderr io.Writer) (*Session, error) {
	s := newSession()
	cmd := exec.CommandContext(context.WithoutCancel(ctx), spec.Command, spec.Args...)
	cmd.Dir, cmd.Env, cmd.Stderr = spec.Dir, spec.Env, stderr
	tree := proctree.New(cmd)
	cmd.WaitDelay = shutdownGrace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("credential provider stdin pipe: %w", err)
	}
	// Stdout is a pipe this session owns, not cmd.StdoutPipe: Wait never
	// waits on it, so the provider's exit is observed even when a process it
	// started still holds the write end.
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("credential provider stdout pipe: %w", err)
	}
	cmd.Stdout = stdoutWriter
	if err := tree.Start(); err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return nil, fmt.Errorf("start credential provider %s: %w", spec.Command, err)
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
			s.markDead(fmt.Errorf("credential provider exited and its output stayed open for %s after; a process it started may still hold it", exitDrainGrace))
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
	scanner.Buffer(make([]byte, 0, 4<<10), registry.MaxCredentialLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := s.deliver(line); err != nil {
			s.markDead(err)
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	s.markDead(fmt.Errorf("credential provider session ended: %w", err))
}

// deliver routes one response line to the call waiting for it. A line that
// answers no request this session sent, or that the protocol refuses, ends
// the session: a provider out of step with its caller is never trusted again.
func (s *Session) deliver(line []byte) error {
	var envelope struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return errors.New("credential provider sent a malformed response")
	}
	s.mu.Lock()
	call, waiting := s.pending[envelope.ID]
	issued := envelope.ID > 0 && envelope.ID < s.nextID
	s.mu.Unlock()
	if !issued {
		return fmt.Errorf("credential provider answered request %d, which was never sent", envelope.ID)
	}
	if !waiting {
		// The caller stopped waiting (its context ended); the late answer is dropped.
		return nil
	}
	response, err := registry.ParseCredentialResponse(line, call.op)
	if err != nil {
		// The call stays pending, so markDead wakes it with the session's error.
		return fmt.Errorf("credential provider sent an invalid %s response: %w", call.op, err)
	}
	s.mu.Lock()
	_, waiting = s.pending[envelope.ID]
	delete(s.pending, envelope.ID)
	s.mu.Unlock()
	if waiting {
		call.ch <- response
	}
	return nil
}

func (s *Session) markDead(err error) {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return
	}
	s.dead, s.deadErr = true, err
	pending := s.pending
	s.pending = make(map[int64]pendingCall)
	s.mu.Unlock()
	for _, call := range pending {
		call.ch <- nil
	}
}

// ended reports whether the session ended: the provider exited, closed its
// output, broke the protocol or overstayed an op timeout. An ended session
// never serves again.
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
	return errors.New("credential provider session ended")
}

// call writes one request and waits for its response, the context, or death.
func (s *Session) call(ctx context.Context, op registry.CredentialOp, params any) (*registry.CredentialResponse, error) {
	var payload json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("encode %s params: %w", op, err)
		}
		payload = encoded
	}
	s.mu.Lock()
	if s.dead {
		err := s.deadErr
		s.mu.Unlock()
		return nil, err
	}
	id := s.nextID
	s.nextID++
	ch := make(chan *registry.CredentialResponse, 1)
	s.pending[id] = pendingCall{op: op, ch: ch}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	line, err := json.Marshal(&registry.CredentialRequest{ProtocolVersion: registry.CredentialProtocolVersion, ID: id, Op: op, Payload: payload})
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", op, err)
	}
	s.writeMu.Lock()
	_, err = s.stdin.Write(append(line, '\n'))
	s.writeMu.Unlock()
	if err != nil {
		s.markDead(fmt.Errorf("credential provider write failed: %w", err))
		return nil, s.deathErr()
	}
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("credential provider op %s: %w", op, ctx.Err())
	case response := <-ch:
		if response == nil {
			return nil, s.deathErr()
		}
		return response, nil
	}
}

// Initialize negotiates the protocol. A provider that answers with another
// version or without the credential-v1 capability cannot serve this engine.
//
// A non-empty runCredential is the hosted run's bearer, handed to the provider
// in the initialize line and nowhere else. An empty one leaves the line as it
// is without a run. A malformed one sends nothing and fails, so a hosted run
// never falls back to the provider's own credentials.
func (s *Session) Initialize(ctx context.Context, runCredential string) (*registry.CredentialInitializeResult, error) {
	if runCredential != "" && !registry.ValidRunCredential(runCredential) {
		return nil, fmt.Errorf("the run credential is not 1 to %d bytes of UTF-8 with no whitespace", registry.MaxRunCredentialBytes)
	}
	response, err := s.call(ctx, registry.CredentialOpInitialize, &registry.CredentialInitializeParams{
		ProtocolVersion: registry.CredentialProtocolVersion,
		Capabilities:    []string{registry.CapabilityCredentialV1},
		RunCredential:   runCredential,
	})
	if err != nil {
		return nil, err
	}
	if !response.OK {
		message := response.Error.Message
		if runCredential != "" {
			message = strings.ReplaceAll(message, runCredential, "<redacted>")
		}
		return nil, fmt.Errorf("the credential provider refused to initialize (%s): %s", response.Error.Code, message)
	}
	result, err := registry.ParseCredentialInitializeResult(response.Payload)
	if err != nil {
		return nil, err
	}
	if result.ProtocolVersion != registry.CredentialProtocolVersion {
		return nil, fmt.Errorf("the credential provider speaks protocol version %d; this CLI requires %d", result.ProtocolVersion, registry.CredentialProtocolVersion)
	}
	if !slices.Contains(result.Capabilities, registry.CapabilityCredentialV1) {
		return nil, fmt.Errorf("the credential provider does not support %s", registry.CapabilityCredentialV1)
	}
	return result, nil
}

// Credential asks for the credential of one purpose. A nil credential with a
// nil error is absence; a refusal is a *RefusalError.
func (s *Session) Credential(ctx context.Context, purpose string) (*registry.Credential, error) {
	response, err := s.call(ctx, registry.CredentialOpCredential, &registry.CredentialParams{Purpose: purpose})
	if err != nil {
		return nil, err
	}
	if !response.OK {
		return nil, &RefusalError{Purpose: purpose, Code: response.Error.Code, Message: response.Error.Message}
	}
	result, err := registry.ParseCredentialResult(response.Payload)
	if err != nil {
		return nil, err
	}
	return result.Credential, nil
}

// Close sends a best-effort shutdown, closes stdin and waits for the process,
// killing its tree if it overstays the grace period. A process outside the
// killed tree that still holds the provider's output open gets another grace
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
		_, _ = s.call(ctx, registry.CredentialOpShutdown, nil)
		cancel()
	}
	_ = s.stdin.Close()
	select {
	case <-s.waitCh:
	case <-time.After(shutdownGrace):
		if s.tree != nil {
			_ = s.tree.Kill()
		}
		select {
		case <-s.waitCh:
		case <-time.After(shutdownGrace):
			_ = s.stdout.Close()
			<-s.waitCh
		}
	}
	if s.tree != nil {
		_ = s.tree.Close()
	}
	<-s.readDone
	return nil
}
