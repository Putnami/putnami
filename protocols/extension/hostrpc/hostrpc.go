// Package hostrpc is the wire contract of the host RPC channel: the calls an
// extension command makes into the running putnami process that launched it.
//
// The channel is JSON-RPC 2.0 over a Unix domain socket. Each frame is one
// JSON object followed by a newline, at most MaxFrameBytes long. A connection
// serves its requests in order, one at a time. Every request carries the
// launch's token in the top-level "token" member; a request with any other
// token is refused with CodeUnauthorized, and the host closes the connection.
//
// The host names the socket in SocketEnv and the token in TokenEnv. It sets
// both only for an interactive command-group subcommand it launches. A build
// job, a provider and a hosted run (--credential-fd) never receive them.
//
// The extension side is go.putnami.dev/sdk/extension/hostrpc; the host side
// is internal to the CLI. Both read this package, so neither defines a frame,
// a method or an error code of its own.
package hostrpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
)

const (
	// ProtocolVersion is the version host.hello reports. A change that an
	// existing client cannot read moves it.
	ProtocolVersion = 1

	// SocketEnv names the path of the Unix socket the host listens on.
	SocketEnv = "PUTNAMI_HOST_RPC"
	// TokenEnv names the token every request carries: TokenBytes random
	// bytes, hex-encoded.
	TokenEnv = "PUTNAMI_HOST_RPC_TOKEN" // #nosec G101 -- environment variable name, not a credential
	// TokenBytes is the number of random bytes in a token.
	TokenBytes = 32

	// JSONRPCVersion is the value of the jsonrpc member of every frame.
	JSONRPCVersion = "2.0"

	// MaxFrameBytes bounds one frame, without its newline, in both
	// directions. It holds the largest verification report the host returns
	// (16 MiB, base64-encoded) and the largest collaboration envelope.
	MaxFrameBytes = 32 << 20
)

// The methods the host serves.
const (
	// MethodHello answers with a HelloResult. It takes no params.
	MethodHello = "host.hello"
	// MethodCollaborationCall routes one collaboration contract operation and
	// answers with the envelope `putnami <contract> <operation> --output=json`
	// prints for the same request. Params: CollaborationCallParams.
	MethodCollaborationCall = "collaboration.call"
	// MethodVerificationReport returns the bytes of a verification report
	// that a successful cached test run of the workspace produced. Params:
	// VerificationReportParams. Result: VerificationReportResult.
	MethodVerificationReport = "verification.report"
	// MethodVersionLines reports the workspace's version lines as the core
	// computes them: for each line, the version `putnami version get` prints,
	// and what `putnami version tag` needs to release it. Params:
	// VersionLinesParams. Result: VersionLinesResult.
	MethodVersionLines = "version.lines"
	// MethodWorkspaceOwners answers which workspace projects own each path.
	// A project owns a path when impact analysis treats it as an owner: the
	// nearest project whose directory holds the path, every project that
	// claims the path as an asset, and, for a file at the workspace root, every
	// project whose provider lists that file as watched. The CLI applies this
	// rule to a changed file before it follows the dependency graph. Params:
	// WorkspaceOwnersParams. Result: WorkspaceOwnersResult.
	MethodWorkspaceOwners = "workspace.owners"
)

// Methods lists every method the host serves, sorted.
func Methods() []string {
	methods := []string{MethodHello, MethodCollaborationCall, MethodVerificationReport, MethodVersionLines, MethodWorkspaceOwners}
	slices.Sort(methods)
	return methods
}

// The error codes of an Error. The first five are JSON-RPC 2.0's own.
const (
	// CodeParseError answers a frame that is not JSON.
	CodeParseError = -32700
	// CodeInvalidRequest answers a frame that is not a request object, and a
	// frame longer than MaxFrameBytes.
	CodeInvalidRequest = -32600
	// CodeMethodNotFound answers a method the host does not serve.
	CodeMethodNotFound = -32601
	// CodeInvalidParams answers params the method refuses.
	CodeInvalidParams = -32602
	// CodeInternalError answers a failure of the host itself.
	CodeInternalError = -32603
	// CodeUnauthorized answers a request whose token is not the launch's.
	CodeUnauthorized = -32001
	// CodeUnavailable answers a method the launch cannot serve, such as a
	// workspace method outside any workspace.
	CodeUnavailable = -32002
	// CodeNotVerified answers a verification report the cache does not
	// corroborate.
	CodeNotVerified = -32003
	// CodeInvalidConfig answers a request the host refuses because the
	// workspace's configuration or the host's environment is invalid, such as
	// an unparsable support catalog or a malformed PUTNAMI_SOURCE_REVISION.
	// The core exits 2 for the same refusal, where it exits 1 for a failure
	// of the host (CodeInternalError). Its message is the one the core prints.
	CodeInvalidConfig = -32004
)

// Request is one JSON-RPC request frame.
type Request struct {
	// JSONRPC is JSONRPCVersion.
	JSONRPC string `json:"jsonrpc"`
	// ID is a JSON string or number that the response repeats. A request
	// without one is a notification, which no method accepts: the host
	// answers nothing.
	ID json.RawMessage `json:"id,omitempty"`
	// Method names the method.
	Method string `json:"method"`
	// Params is the method's params object.
	Params json.RawMessage `json:"params,omitempty"`
	// Token is the value of TokenEnv.
	Token string `json:"token"`
}

// Response is one JSON-RPC response frame: exactly one of Result and Error.
type Response struct {
	// JSONRPC is JSONRPCVersion.
	JSONRPC string `json:"jsonrpc"`
	// ID repeats the request's id, or is null when the request had none
	// the host could read.
	ID json.RawMessage `json:"id"`
	// Result is the method's result.
	Result json.RawMessage `json:"result,omitempty"`
	// Error is the reason the method returned no result.
	Error *Error `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	// Code is one of the Code constants.
	Code int `json:"code"`
	// Message says what failed, in one sentence.
	Message string `json:"message"`
}

// Error returns the message with its code.
func (e *Error) Error() string {
	return fmt.Sprintf("putnami host RPC error %d: %s", e.Code, e.Message)
}

// HelloResult is the result of host.hello.
type HelloResult struct {
	// Protocol is the host's ProtocolVersion.
	Protocol int `json:"protocol"`
	// Methods lists the methods the host serves, sorted.
	Methods []string `json:"methods"`
}

// CollaborationCallParams are the params of collaboration.call.
type CollaborationCallParams struct {
	// Contract is the collaboration contract, such as "tasks".
	Contract string `json:"contract"`
	// Operation is the contract operation, such as "find".
	Operation string `json:"operation"`
	// Input is the request document `--input` would carry. Absent is the
	// empty request.
	Input json.RawMessage `json:"input,omitempty"`
}

// VerificationReportParams are the params of verification.report. They name
// one report the way a recorded verdict does (features.VerificationReportRef).
type VerificationReportParams struct {
	// Project is the reporting project's canonical ID.
	Project string `json:"project"`
	// Task is the producing task's canonical key.
	Task string `json:"task"`
	// Path is the workspace-relative path the report was read from.
	Path string `json:"path"`
	// Digest is the lower-case sha256:<hex> content address of the report.
	Digest string `json:"digest"`
}

// VerificationReportResult is the result of verification.report.
type VerificationReportResult struct {
	// Report holds the report bytes, base64-encoded on the wire.
	Report []byte `json:"report"`
}

// VersionLinesParams are the params of version.lines. The empty object asks
// for every line.
type VersionLinesParams struct {
	// Scope names one version line by its scope path, as the --scope flag of
	// `putnami version get` does, for example "tooling". Leading and trailing
	// slashes are ignored. Absent or empty asks for every line.
	Scope string `json:"scope,omitempty"`
}

// VersionLinesResult is the result of version.lines.
type VersionLinesResult struct {
	// SourceRevision is the full commit ID the host's run is bound to: the
	// value of PUTNAMI_SOURCE_REVISION when the host's environment sets it,
	// else HEAD's. A release commit and its tag go on HEAD, so a caller that
	// releases refuses when SourceRevision is not HEAD.
	SourceRevision string `json:"sourceRevision"`
	// RecordedGraph is present when the host computed the versions from a
	// dependency graph it recorded earlier, because it could not refresh the
	// graph a support catalog reads. A caller that releases refuses then.
	RecordedGraph *RecordedGraph `json:"recordedGraph,omitempty"`
	// Lines holds one entry for each version line, sorted by Line, or only
	// the line Scope names.
	Lines []VersionLine `json:"lines"`
}

// RecordedGraph names the recorded dependency graph a version was computed
// from after a refresh failed.
type RecordedGraph struct {
	// ObservedAt is when the host recorded the graph, in RFC 3339.
	ObservedAt string `json:"observedAt"`
	// RefreshError says why the refresh failed.
	RefreshError string `json:"refreshError"`
}

// VersionLine is one version line at HEAD.
type VersionLine struct {
	// Line is the line's scope path, slash-separated. "" is the root line.
	Line string `json:"line"`
	// TagPattern spells the line's release tags: it carries one {version}
	// placeholder, for example "tooling/v{version}".
	TagPattern string `json:"tagPattern"`
	// Version is the version `putnami version get` prints for the line: the
	// release tag's version when Tagged, else the next version followed by the
	// build suffix.
	Version string `json:"version"`
	// Tagged reports whether HEAD carries one of the line's release tags.
	Tagged bool `json:"tagged"`
	// Tag is that release tag when Tagged, else empty.
	Tag string `json:"tag,omitempty"`
	// Release is the version and tag the line's next release takes when the
	// caller names no tag. It is absent when Tagged: HEAD is a release already.
	Release *LineRelease `json:"release,omitempty"`
	// Commits are the commits that touch the line since its last release, or
	// since the first commit when it has none, newest first: the commits a
	// release's changelog lists. It is an empty array, never null.
	Commits []LineCommit `json:"commits"`
}

// LineRelease is the release the host proposes for a line.
type LineRelease struct {
	// Version is the release's MAJOR.MINOR.PATCH version.
	Version string `json:"version"`
	// Tag is the line's tag pattern rendered with Version.
	Tag string `json:"tag"`
}

// LineCommit is one commit of a line's history.
type LineCommit struct {
	// SHA is the commit's full ID.
	SHA string `json:"sha"`
	// Subject is the first line of the commit message.
	Subject string `json:"subject"`
	// Body is the rest of the commit message, without trailing newlines.
	Body string `json:"body"`
}

// WorkspaceOwnersParams are the params of workspace.owners.
type WorkspaceOwnersParams struct {
	// Paths are workspace-relative paths in io/fs form (ValidWorkspacePath),
	// for example "tooling/cli/main.go". A path may repeat.
	Paths []string `json:"paths"`
}

// WorkspaceOwnersResult is the result of workspace.owners.
type WorkspaceOwnersResult struct {
	// Owners holds exactly one entry for each requested path, in request
	// order.
	Owners []PathOwners `json:"owners"`
}

// PathOwners lists the projects that own one requested path.
type PathOwners struct {
	// Path repeats the requested path.
	Path string `json:"path"`
	// Projects are the owners of Path under the rule OwningProject states,
	// sorted by ID. It is an empty array, never null, when no project owns the
	// path.
	Projects []OwningProject `json:"projects"`
}

// OwningProject names one project that owns a path. A project owns a path when
// impact analysis treats it as an owner: it is the nearest project whose
// directory holds the path, it claims the path as an asset, or, for a file at
// the workspace root, its provider lists that file as watched.
type OwningProject struct {
	// ID is the project's canonical ID, as the workspace model holds it.
	ID string `json:"id"`
	// Path is the project's workspace-relative directory, as the workspace
	// model holds it.
	Path string `json:"path"`
}

// ValidWorkspacePath reports whether p is a workspace-relative path in io/fs
// form that names something below the workspace root. It refuses ".", the
// root itself. A backslash is an ordinary character of a name, not a
// separator. The host and the client both apply it to the paths of
// workspace.owners.
func ValidWorkspacePath(p string) bool {
	return fs.ValidPath(p) && p != "."
}

// ErrFrameTooLarge is the error ReadFrame returns for a frame longer than its
// limit. The stream holds the rest of that frame, so the reader closes the
// connection.
var ErrFrameTooLarge = errors.New("the frame exceeds the host RPC frame limit")

// ReadFrame reads one frame from r and returns it without its newline. It
// returns ErrFrameTooLarge as soon as more than limit bytes of the frame have
// arrived, without waiting for more, and io.ErrUnexpectedEOF for a stream that
// ends inside a frame.
func ReadFrame(r *bufio.Reader, limit int) ([]byte, error) {
	var frame []byte
	for {
		// Peek(1) waits for one byte at most, so the bytes that arrived are
		// judged before the next wait. ReadSlice would wait for a newline or a
		// full buffer: a frame one byte over a limit that is a multiple of the
		// buffer size would then wait for bytes that never come.
		if _, err := r.Peek(1); err != nil {
			if errors.Is(err, io.EOF) && len(frame) > 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		arrived, _ := r.Peek(r.Buffered())
		if end := bytes.IndexByte(arrived, '\n'); end >= 0 {
			if len(frame)+end > limit {
				return nil, ErrFrameTooLarge
			}
			frame = append(frame, arrived[:end]...)
			_, _ = r.Discard(end + 1)
			return frame, nil
		}
		if len(frame)+len(arrived) > limit {
			return nil, ErrFrameTooLarge
		}
		frame = append(frame, arrived...)
		_, _ = r.Discard(len(arrived))
	}
}

// EncodeFrame encodes v as one frame, with its newline. It refuses a value
// whose encoding exceeds MaxFrameBytes.
func EncodeFrame(v any) ([]byte, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaxFrameBytes {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrFrameTooLarge, len(encoded), MaxFrameBytes)
	}
	return append(encoded, '\n'), nil
}

// DecodeStrict decodes one JSON value into v and refuses an unknown member
// and trailing data, so that a misspelled param fails instead of being
// dropped.
func DecodeStrict(data []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}
