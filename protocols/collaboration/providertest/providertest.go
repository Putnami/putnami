// Package providertest runs the shared contract scenarios of the tasks,
// proposals and memory collaboration contracts against one provider.
//
// A provider's test builds a Target — its handlers, the settings a binding
// passes, the workspace root and name, the branches proposal scenarios may
// use, and for memory a second instance on the same store — and calls
// RunTasks, RunProposals or RunMemory. Every call goes through
// collaboration.Serve with the request a routed call carries, so every answer
// has passed the request and result validation a real call passes; the
// scenarios then check what validation cannot: that an answer is about the
// item asked for, that an idempotent repeat returns the first result instead
// of a second item, that a stale revision is a conflict, that pages cover a
// result set exactly once, and that a reference another source issued is not
// found. The memory scenarios check that a checkpoint lands only against the
// revision it names, that of concurrent writers at one revision exactly one
// lands, that a repeated idempotency key replays the write that landed, that
// another provider instance resumes from the store alone, that a record
// stays within its workspace, and that the largest checkpoint the contract
// accepts (MaximalCheckpoint) is read back unchanged.
//
// The scenarios name nothing a particular backend defines: they scope what
// they create with labels, titles and mission names unique to the run, so
// they can run against a shared, non-empty backend, and they never assume an
// identifier or revision format.
package providertest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	extension "go.putnami.dev/protocol/extension"
)

// Target is one provider under test.
type Target struct {
	// Handlers is the provider's dispatch table, as collaboration.Serve takes it.
	Handlers map[collab.OperationKey]collab.Handler
	// WorkspaceRoot is the workspace the routed request names.
	WorkspaceRoot string
	// Settings is the binding's settings object, passed verbatim.
	Settings json.RawMessage
	// Base and Heads name the branches the proposal scenarios use: two
	// distinct heads that can each carry an open proposal against Base, and
	// that carry none when the scenario starts.
	Base  string
	Heads [2]string
	// Settle is how long a read may take to observe a write the provider
	// acknowledged, for a backend whose search index is eventually consistent.
	// Zero means every read observes every acknowledged write at once.
	Settle time.Duration
	// Track, when set, is told every item a scenario creates, so a test
	// against a real backend can clean up after it.
	Track func(contract string, ref collab.Ref)
	// Workspace is the workspace name the orchestrator fills into a memory
	// identity that names none. RunMemory requires it.
	Workspace string
	// Reopen returns another instance of the provider on the same store: it
	// shares nothing with Handlers but the stored state, as a second provider
	// process does. RunMemory requires it.
	Reopen func(t *testing.T) map[collab.OperationKey]collab.Handler
}

// Session drives one Target from one test.
type Session struct {
	t      *testing.T
	target Target
}

// Open starts a session on a target.
func Open(t *testing.T, target Target) *Session {
	t.Helper()
	return &Session{t: t, target: target}
}

// Call sends one routed request and returns the provider's Response. Like the
// orchestrator, it gives every list request an explicit page size and every
// memory identity that names no workspace the target's workspace.
func (s *Session) Call(contract, operation, arguments string) collab.Response {
	s.t.Helper()
	response, err := s.do(contract, operation, arguments)
	if err != nil {
		s.t.Fatal(err)
	}
	return response
}

// do is Call reporting a transport or contract failure as an error instead of
// failing the test, so that concurrent callers can collect it.
func (s *Session) do(contract, operation, arguments string) (collab.Response, error) {
	request := extension.ToolCallRequest{
		Name:          "providertest." + contract + "." + operation,
		Arguments:     normalize(contract, operation, arguments, s.target.Workspace),
		WorkspaceRoot: s.target.WorkspaceRoot,
		Provider:      &extension.ToolProviderCall{Contract: contract, Version: 1, Operation: operation},
	}
	if len(s.target.Settings) > 0 {
		request.Provider.Settings = s.target.Settings
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return collab.Response{}, err
	}
	var out bytes.Buffer
	if err := collab.Serve(context.Background(), bytes.NewReader(payload), &out, s.target.Handlers); err != nil {
		return collab.Response{}, err
	}
	var result extension.ToolCallResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return collab.Response{}, fmt.Errorf("the provider wrote no tool result: %w", err)
	}
	if len(result.Content) != 1 {
		return collab.Response{}, fmt.Errorf("the provider wrote %d content blocks; a collaboration result is exactly one", len(result.Content))
	}
	response, diags := collab.ParseResponse([]byte(result.Content[0].Text))
	if diags != nil {
		return collab.Response{}, fmt.Errorf("%s.%s answered a response the contract refuses: %s", contract, operation, collab.FormatDiagnostics(diags))
	}
	if result.IsError != (response.Outcome != collab.OutcomeOK) {
		return collab.Response{}, fmt.Errorf("%s.%s: isError %v disagrees with outcome %s", contract, operation, result.IsError, response.Outcome)
	}
	return *response, nil
}

// OK sends a request that must succeed and decodes its result into into.
func (s *Session) OK(contract, operation, arguments string, into any) {
	s.t.Helper()
	response := s.Call(contract, operation, arguments)
	if response.Outcome != collab.OutcomeOK {
		s.t.Fatalf("%s.%s %s: outcome %s %+v", contract, operation, arguments, response.Outcome, response.Error)
	}
	if err := json.Unmarshal(response.Result, into); err != nil {
		s.t.Fatal(err)
	}
}

// Fails sends a request that must fail with outcome and returns its error.
func (s *Session) Fails(contract, operation, arguments string, outcome collab.Outcome) collab.Error {
	s.t.Helper()
	response := s.Call(contract, operation, arguments)
	if response.Outcome != outcome {
		s.t.Fatalf("%s.%s %s: outcome %s, want %s (%+v)", contract, operation, arguments, response.Outcome, outcome, response.Error)
	}
	return *response.Error
}

// Eventually repeats a read until check accepts its result or the target's
// Settle time has passed, and fails the test with the last result then.
func (s *Session) Eventually(contract, operation, arguments string, into any, check func() string) {
	s.t.Helper()
	deadline := time.Now().Add(s.target.Settle)
	for {
		s.OK(contract, operation, arguments, into)
		problem := check()
		if problem == "" {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("%s.%s %s: %s", contract, operation, arguments, problem)
		}
		time.Sleep(time.Second)
	}
}

func (s *Session) track(contract string, ref collab.Ref) {
	if s.target.Track != nil {
		s.target.Track(contract, ref)
	}
}

// normalize applies the orchestrator's defaults: a list request always
// carries an explicit page size, and a memory identity that names no
// workspace names the one served. A request the contract refuses is passed on
// unchanged, for the provider to refuse.
func normalize(contract, operation, arguments, workspace string) json.RawMessage {
	input, diags := collab.ParseRequest(contract, 1, operation, []byte(arguments))
	if diags != nil {
		return json.RawMessage(arguments)
	}
	page := func(p **collab.PageRequest) {
		if *p == nil {
			*p = &collab.PageRequest{}
		}
		if (*p).Size == 0 {
			(*p).Size = collab.DefaultPageSize
		}
	}
	identity := func(id *collab.MemoryIdentity) {
		if id.Workspace == "" {
			id.Workspace = workspace
		}
	}
	switch in := input.(type) {
	case *collab.TaskFindInput:
		page(&in.Page)
	case *collab.ProposalFindInput:
		page(&in.Page)
	case *collab.MemoryContextInput:
		page(&in.Page)
		identity(&in.Identity)
	case *collab.MemorySearchInput:
		page(&in.Page)
		identity(&in.Identity)
	case *collab.MemoryMissionInput:
		identity(&in.Identity)
	case *collab.MemoryCheckpointInput:
		identity(&in.Identity)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return json.RawMessage(arguments)
	}
	return encoded
}

// Unique returns a token no earlier run produced, for labels, titles and
// idempotency keys that scope a scenario to what it created.
func Unique(t *testing.T) string {
	t.Helper()
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(random[:])
}

func quote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func refJSON(ref collab.Ref) string {
	encoded, _ := json.Marshal(ref)
	return string(encoded)
}

// foreignRef is a reference no provider under test issued.
var foreignRef = collab.Ref{Source: "providertest:elsewhere", ID: "1"}
