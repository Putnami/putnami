package runner

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// Provider RPC. A runner provider is an out-of-process extension command
// (ProviderCommandName) that speaks one JSONL request per line on stdin and
// one JSONL response per line on stdout, exactly like the cache provider
// pattern. Bytes never cross the pipe: source blobs and session bundle files
// move through the content-addressed exchange directory named at initialize.
//
// The provider owns transport, isolation and lifecycle. It never plans,
// schedules or judges: the executing engine inside the isolated snapshot
// produces the canonical session, and the bundle returns it unchanged.
const (
	// ProviderProtocolVersion is the runner provider RPC version.
	ProviderProtocolVersion = 1
	// CapabilityExecutionRequestV1 must be echoed by a provider that accepts
	// this package's execution request; without it the client refuses to submit.
	CapabilityExecutionRequestV1 = "execution-request-v1"
	// CapabilitySessionBundleV1 must be echoed by a provider that returns the
	// canonical session bundle described here.
	CapabilitySessionBundleV1 = "session-bundle-v1"
	// CapabilityInvocationProvidersV1 is echoed by a provider that accepts
	// invocation.providers. A client offers it, and submits a request that
	// carries providers, only to a provider that echoed it.
	CapabilityInvocationProvidersV1 = "invocation-providers-v1"
	// MaxOutputRecordBytes bounds one forwarded output line.
	MaxOutputRecordBytes = 1 << 20
	// MaxFollowRecords bounds the records one follow answer carries. A provider
	// answers with at most this many records after the cursor and the client
	// asks again immediately, so a long stream is drained in bounded steps and
	// never buffered whole on either side.
	MaxFollowRecords = 1024
	// MaxBundleFiles bounds the files of one bundled session.
	MaxBundleFiles = 64
	// MaxBundleSessions bounds the sessions one attempt returns.
	MaxBundleSessions = 1024
)

// BoundRequestEnv names the file holding the bound execution request an
// executing engine consumes. It is an execution-only channel: the provider
// exports it into the pinned entrypoint it launches and nothing else. The
// executing engine removes it from its own environment before any task runs,
// so a nested CLI invocation is never hijacked into a second bound execution.
//
// The entrypoint takes no argument, with one exception: a provider that holds
// a run credential for the execution passes `--credential-fd <n>`, where n is
// a descriptor of 3 or more the entrypoint inherits, holding the bearer on one
// line. The engine reads and closes it before it starts any process, and never
// puts the credential in an environment. A provider never passes a credential
// through this or any other variable.
const BoundRequestEnv = "PUTNAMI_RUNNER_REQUEST"

// ProviderOp identifies a provider RPC method.
type ProviderOp string

const (
	// OpInitialize negotiates protocol and capabilities. It is sent first, once.
	OpInitialize ProviderOp = "initialize"
	// OpPrepare offers the source manifest and returns the blobs the provider
	// still needs. The client copies exactly those into the exchange directory.
	OpPrepare ProviderOp = "prepare"
	// OpSubmit submits the bound request. Acceptance is not completion.
	OpSubmit ProviderOp = "submit"
	// OpLookup resolves the attempt an idempotency key already names. A client
	// asks before every submit, so a lost acknowledgement never creates a
	// second attempt for the same key.
	OpLookup ProviderOp = "lookup"
	// OpFollow reads forwarded output records and lifecycle state from a cursor.
	OpFollow ProviderOp = "follow"
	// OpCancel asks the provider to terminate an attempt's process tree and
	// release what it leased. The answer is the state observed once the
	// request was applied, never a promise.
	OpCancel ProviderOp = "cancel"
	// OpFetch returns the final session bundle of a terminal attempt.
	OpFetch ProviderOp = "fetch"
	// OpShutdown ends the session; the provider releases leased resources.
	OpShutdown ProviderOp = "shutdown"
)

// Valid reports whether op is a recognized provider op.
func (op ProviderOp) Valid() bool {
	switch op {
	case OpInitialize, OpPrepare, OpSubmit, OpLookup, OpFollow, OpCancel, OpFetch, OpShutdown:
		return true
	}
	return false
}

// Attempt lifecycle states. Acceptance and completion are distinct; a client's
// observation may be disconnected without changing the provider's state.
const (
	StateQueued    = "queued"
	StatePreparing = "preparing"
	StateRunning   = "running"
	StateCompleted = "completed"
	StateFailed    = "failed"
	StateCanceled  = "canceled"
)

// TerminalState reports whether an attempt state can no longer change.
func TerminalState(state string) bool {
	return state == StateCompleted || state == StateFailed || state == StateCanceled
}

// KnownState reports whether state belongs to this protocol version. A client
// treats any other spelling as a protocol error instead of waiting on it.
func KnownState(state string) bool {
	switch state {
	case StateQueued, StatePreparing, StateRunning, StateCompleted, StateFailed, StateCanceled:
		return true
	}
	return false
}

// ProviderRequest is one client → provider message.
type ProviderRequest struct {
	// ProtocolVersion is the runner provider RPC version of this envelope (1).
	ProtocolVersion int `json:"protocolVersion"`
	// ID is the positive correlation id, unique and increasing within a session.
	ID int64 `json:"id"`
	// Op is the provider method to invoke.
	Op ProviderOp `json:"op"`
	// Payload is the op-specific request body, dispatched by Op before decoding.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ProviderResponse is one provider → client message.
type ProviderResponse struct {
	// ProtocolVersion is the runner provider RPC version of this envelope (1).
	ProtocolVersion int `json:"protocolVersion"`
	// ID echoes the ProviderRequest.ID this response answers.
	ID int64 `json:"id"`
	// OK reports whether the op succeeded; false requires Error.
	OK bool `json:"ok"`
	// Error carries the failure reason when OK is false.
	Error *ProviderError `json:"error,omitempty"`
	// Payload is the op-specific result body.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ProviderError describes a failed op. The client never turns it into a local
// fallback run.
type ProviderError struct {
	// Code is the machine-readable failure code.
	Code string `json:"code"`
	// Message is the human-readable failure description.
	Message string `json:"message,omitempty"`
}

// InitializeParams opens a provider session.
type InitializeParams struct {
	// ProtocolVersion is the RPC version the client speaks (1).
	ProtocolVersion int `json:"protocolVersion"`
	// ExchangeDir is the absolute content-addressed directory bytes move through.
	ExchangeDir string `json:"exchangeDir"`
	// Capabilities are the capability names the client supports.
	Capabilities []string `json:"capabilities"`
	// Workspace is the opaque client-derived workspace identity, empty when unknown.
	Workspace string `json:"workspace,omitempty"`
}

// InitializeResult answers OpInitialize. Ready=false with a Reason is a
// precise configuration failure, never permission to run locally.
type InitializeResult struct {
	// ProtocolVersion is the RPC version the provider selected; the client requires 1.
	ProtocolVersion int `json:"protocolVersion"`
	// ProviderName identifies the provider implementation, for notices.
	ProviderName string `json:"providerName,omitempty"`
	// ProviderVersion is the provider's own version, for diagnostics only.
	ProviderVersion string `json:"providerVersion,omitempty"`
	// Capabilities are the capability names the provider echoes as supported.
	Capabilities []string `json:"capabilities"`
	// Ready reports whether the provider can execute; false is a configuration error.
	Ready bool `json:"ready"`
	// Reason explains a not-ready answer.
	Reason string `json:"reason,omitempty"`
}

// PrepareParams offers a source manifest before submission.
type PrepareParams struct {
	// SourceDigest is the canonical digest of Manifest.
	SourceDigest string `json:"sourceDigest"`
	// Manifest is the source manifest the client intends to submit.
	Manifest SourceManifest `json:"manifest"`
}

// PrepareResult lists the blob digests the provider does not hold yet.
type PrepareResult struct {
	// MissingBlobs are the sorted file digests the provider does not hold yet.
	MissingBlobs []string `json:"missingBlobs"`
}

// SubmitParams carries the bound request with the manifest it names.
type SubmitParams struct {
	// Request is the bound execution request.
	Request ExecutionRequest `json:"request"`
	// Manifest is the source manifest whose digest Request.Source names.
	Manifest SourceManifest `json:"manifest"`
}

// SubmitResult acknowledges a submission with its opaque attempt reference.
// A submission whose idempotency key the provider already accepted answers
// with that earlier attempt: one key is one attempt, however many times the
// submit envelope reaches the provider.
type SubmitResult struct {
	// Attempt is the provider's opaque attempt reference, never empty.
	Attempt string `json:"attempt"`
	// State is the attempt state at acknowledgement.
	State string `json:"state"`
}

// LookupParams resolves a submission by the idempotency key its request
// carried in its control block.
type LookupParams struct {
	// IdempotencyKey is the request control key whose attempt is sought.
	IdempotencyKey string `json:"idempotencyKey"`
}

// LookupResult names the attempt an idempotency key already produced. Every
// member is absent when the key was never accepted.
type LookupResult struct {
	// Attempt is the attempt reference the key names; empty when the provider
	// never accepted a submission under that key.
	Attempt string `json:"attempt,omitempty"`
	// State is the attempt's current lifecycle state, present with Attempt.
	State string `json:"state,omitempty"`
	// ExecutionInputDigest is the execution-input digest of the request the
	// provider accepted under the key, present with Attempt, so a client can
	// refuse to adopt an attempt that ran different inputs.
	ExecutionInputDigest string `json:"executionInputDigest,omitempty"`
}

// FollowParams reads from a cursor. Cursor 0 starts at the beginning. A
// client that reconnects passes the last cursor it persisted and drops any
// record at or below it, so a replay never duplicates a line.
type FollowParams struct {
	// Attempt is the attempt reference to follow.
	Attempt string `json:"attempt"`
	// Cursor is the last record cursor already received; 0 starts at the beginning.
	Cursor int64 `json:"cursor"`
}

// OutputRecord is one forwarded line of the executing engine's stdout or
// stderr. The line is the canonical record unchanged; the envelope adds only
// the cursor and the stream.
type OutputRecord struct {
	// Cursor is the record's strictly increasing position.
	Cursor int64 `json:"cursor"`
	// Stream is stdout or stderr.
	Stream string `json:"stream"`
	// Line is the forwarded line without its trailing newline.
	Line string `json:"line"`
}

// FollowResult returns records after the cursor and the current state.
type FollowResult struct {
	// State is the attempt's current lifecycle state.
	State string `json:"state"`
	// Cursor is the highest cursor covered by this answer.
	Cursor int64 `json:"cursor"`
	// Records are the output records after the requested cursor, in order,
	// at most MaxFollowRecords of them.
	Records []OutputRecord `json:"records"`
	// ExitCode is present only in a terminal state that produced one.
	ExitCode *int `json:"exitCode,omitempty"`
	// Reason explains a failed or canceled state.
	Reason string `json:"reason,omitempty"`
}

// CancelParams asks the provider to terminate an attempt.
type CancelParams struct {
	// Attempt is the attempt reference to cancel.
	Attempt string `json:"attempt"`
}

// CancelResult acknowledges a cancellation. The state is what the provider
// observed once the request was applied: an attempt that had already reached
// a terminal state keeps it, so a cancel racing a completion converges on the
// one outcome the provider recorded; an attempt still terminating reports its
// non-terminal state and the client follows it to the terminal one.
type CancelResult struct {
	// State is the attempt state after the cancellation request was applied.
	State string `json:"state"`
}

// FetchParams requests the bundle of a terminal attempt.
type FetchParams struct {
	// Attempt is the terminal attempt whose bundle is requested.
	Attempt string `json:"attempt"`
}

// FetchResult carries the bundle; its file bytes sit in the exchange directory.
type FetchResult struct {
	// Bundle is the attempt's session bundle.
	Bundle SessionBundle `json:"bundle"`
}

// SessionBundle is the canonical evidence of one attempt: the executing
// engine's exit code and every session it recorded, the gate session first.
type SessionBundle struct {
	// Attempt is the attempt reference the bundle belongs to.
	Attempt string `json:"attempt"`
	// ExitCode is the exit code of the executing engine's process.
	ExitCode int `json:"exitCode"`
	// SessionID is the gate session, or empty when the engine refused before
	// recording one (a rejected plan, a usage error).
	SessionID string `json:"sessionId"`
	// Sessions are the recorded sessions, the gate session first.
	Sessions []BundleSession `json:"sessions"`
}

// BundleSession is one recorded session directory.
type BundleSession struct {
	// ID is the session directory name.
	ID string `json:"id"`
	// ParentID names the session that spawned this one, absent for the gate.
	ParentID string `json:"parentId,omitempty"`
	// Files are the admitted session files, session.json included.
	Files []BundleFile `json:"files"`
}

// BundleFile is one session file by name, digest and size.
type BundleFile struct {
	// Name is the admitted file name inside the session directory.
	Name string `json:"name"`
	// Digest is the sha256 digest of the file bytes in the exchange directory.
	Digest string `json:"digest"`
	// Size is the file length in bytes.
	Size int64 `json:"size"`
}

// Bundle file names the import admits. Reporting checkpoints, locks and
// anything else a session directory may hold stay provider-side.
const (
	BundleSessionFile   = "session.json"
	BundlePlanFile      = "plan.json"
	BundleEventsFile    = "events.jsonl"
	BundleReportFile    = "report.json"
	BundleSpecFile      = "spec-verification.json"
	BundleRunReportFile = "run-report.json"
)

// BundleFileAdmitted reports whether name is an importable session file.
func BundleFileAdmitted(name string) bool {
	switch name {
	case BundleSessionFile, BundlePlanFile, BundleEventsFile, BundleReportFile, BundleSpecFile, BundleRunReportFile:
		return true
	}
	return false
}

// ValidateSessionBundle checks the bundle's shape: unique session ids, the
// gate session listed first when present, admitted file names and digests.
func ValidateSessionBundle(bundle SessionBundle) error {
	if bundle.Attempt == "" {
		return fmt.Errorf("runner: bundle attempt is empty")
	}
	if len(bundle.Sessions) > MaxBundleSessions {
		return fmt.Errorf("runner: bundle exceeds %d sessions", MaxBundleSessions)
	}
	ids := make(map[string]bool, len(bundle.Sessions))
	for index, session := range bundle.Sessions {
		if !ValidSessionID(session.ID) || ids[session.ID] {
			return fmt.Errorf("runner: bundle session %d has an invalid or duplicate id", index)
		}
		if session.ParentID != "" && !ValidSessionID(session.ParentID) {
			return fmt.Errorf("runner: bundle session %s has an invalid parent id", session.ID)
		}
		ids[session.ID] = true
		if len(session.Files) == 0 || len(session.Files) > MaxBundleFiles {
			return fmt.Errorf("runner: bundle session %s lists no files or too many", session.ID)
		}
		names := make(map[string]bool, len(session.Files))
		hasSession := false
		for _, file := range session.Files {
			if !BundleFileAdmitted(file.Name) || names[file.Name] {
				return fmt.Errorf("runner: bundle session %s file %q is not admitted or repeats", session.ID, file.Name)
			}
			if !validDigest(file.Digest) || file.Size < 0 || file.Size > MaxSourceFileBytes {
				return fmt.Errorf("runner: bundle session %s file %q has an invalid digest or size", session.ID, file.Name)
			}
			names[file.Name] = true
			hasSession = hasSession || file.Name == BundleSessionFile
		}
		if !hasSession {
			return fmt.Errorf("runner: bundle session %s has no session.json", session.ID)
		}
	}
	if bundle.SessionID != "" {
		if len(bundle.Sessions) == 0 || bundle.Sessions[0].ID != bundle.SessionID {
			return fmt.Errorf("runner: bundle gate session %s must be listed first", bundle.SessionID)
		}
	}
	for _, session := range bundle.Sessions {
		if session.ParentID != "" && !ids[session.ParentID] {
			return fmt.Errorf("runner: bundle session %s names an absent parent %s", session.ID, session.ParentID)
		}
	}
	return nil
}

// ValidSessionID accepts the CLI's session directory names: a timestamp and
// six hex characters, never a path component that could escape a store.
func ValidSessionID(id string) bool {
	if len(id) != 22 || id[8] != '-' || id[15] != '-' {
		return false
	}
	for index, char := range id {
		switch {
		case index == 8 || index == 15:
		case index < 15:
			if char < '0' || char > '9' {
				return false
			}
		default:
			if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
				return false
			}
		}
	}
	return true
}

// ExchangeBlobPath returns where a blob sits under the exchange directory,
// sharded by its first two hex characters, and whether the digest is valid.
func ExchangeBlobPath(exchangeDir, digest string) (string, bool) {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || !validHex(hex, 64) {
		return "", false
	}
	return filepath.Join(exchangeDir, hex[:2], hex), true
}

// MarshalPayload encodes an op body; nil marshals to an absent payload.
func MarshalPayload(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

// ParseProviderRequest strictly decodes one request envelope. Envelopes carry
// MaxEnvelopeDepth levels: the document budget plus the envelope's own two.
func ParseProviderRequest(line []byte) (*ProviderRequest, error) {
	if err := strictJSONDepth(line, MaxEnvelopeDepth); err != nil {
		return nil, err
	}
	if _, err := strictObject(line, []string{"protocolVersion", "id", "op"}, []string{"payload"}); err != nil {
		return nil, err
	}
	var request ProviderRequest
	if err := json.Unmarshal(line, &request); err != nil {
		return nil, err
	}
	if request.ProtocolVersion != ProviderProtocolVersion || request.ID <= 0 || !request.Op.Valid() {
		return nil, fmt.Errorf("runner: invalid provider request envelope")
	}
	return &request, nil
}

// ParseProviderResponse strictly decodes one response envelope, with the same
// MaxEnvelopeDepth budget as requests.
func ParseProviderResponse(line []byte) (*ProviderResponse, error) {
	if err := strictJSONDepth(line, MaxEnvelopeDepth); err != nil {
		return nil, err
	}
	if _, err := strictObject(line, []string{"protocolVersion", "id", "ok"}, []string{"error", "payload"}); err != nil {
		return nil, err
	}
	var response ProviderResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, err
	}
	if response.ProtocolVersion != ProviderProtocolVersion || response.ID <= 0 {
		return nil, fmt.Errorf("runner: invalid provider response envelope")
	}
	if !response.OK && response.Error == nil {
		return nil, fmt.Errorf("runner: failed response carries no error")
	}
	return &response, nil
}
