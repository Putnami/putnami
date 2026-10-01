package client

import (
	stderrors "errors"
	"net/http"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// serverStreamCandidates lists, in declared order, the transports this runtime
// can carry for a server stream.
//
// A protocol declared twice is a single candidate for SSE and WebSocket: each
// opener selects the first transport of its own protocol, so a second
// declaration of the same protocol would re-open the same wire. Connect is the
// exception — its encodings are distinct wires, and the runtime carries each on
// its own — so a `connect+proto` and a `connect+json` declaration are two
// candidates.
func serverStreamCandidates(operation Operation) []clientcontract.Transport {
	if operation.Contract.Stream != clientcontract.StreamServer {
		return nil
	}
	candidates := make([]clientcontract.Transport, 0, len(operation.Contract.Transports))
	seen := make(map[clientcontract.TransportProtocol]bool, len(operation.Contract.Transports))
	for _, candidate := range operation.Contract.Transports {
		if !supportedServerStreamTransport(candidate) {
			continue
		}
		if candidate.Protocol != clientcontract.TransportConnect {
			if seen[candidate.Protocol] {
				continue
			}
			seen[candidate.Protocol] = true
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

// streamFallbackAllowed reports whether this operation may be opened a second
// time on another declared transport.
//
// Only a declared-replayable operation may: a fallback is a second opening, and
// an operation whose provider did not state that re-running it is harmless must
// never be re-run because a wire looked absent. A client or bidirectional
// stream is never eligible — the caller's own messages would travel twice — and
// only server streams reach this dispatcher for that reason.
func streamFallbackAllowed(operation Operation) bool {
	switch operation.Contract.Idempotency.Kind {
	case clientcontract.IdempotencySafe, clientcontract.IdempotencyIdempotent:
		return true
	default:
		return false
	}
}

// streamTransportUnavailableStatus reports the three HTTP answers that mean
// "this transport is not served at this path". They are the only statuses a
// declared fallback acts on: a refused credential, a server fault, a rate limit
// or a timeout all say something about the call, not about the wire.
func streamTransportUnavailableStatus(status int) bool {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUpgradeRequired:
		return true
	default:
		return false
	}
}

// streamTransportUnavailable reports the one failure class a declared fallback
// may act on: the provider answered, and its answer says this transport is not
// served at this path.
//
// 404, 405 and 426 are the three HTTP ways to say it, gRPC status 12
// `UNIMPLEMENTED` is the Connect one, and a completed RFC 6455 handshake that
// did not select the first-party subprotocol is the WebSocket one. Everything
// else — a refused credential, a server fault, a rate limit, a budget, a
// contract violation, a caller withdrawal — is a fact about the call, and
// asking a second wire would only ask it again.
func streamTransportUnavailable(err error) bool {
	if stderrors.Is(err, errStreamWireAbsent) {
		return true
	}
	var remote *RemoteError
	if !stderrors.As(err, &remote) {
		return false
	}
	if remote.GRPCCode != nil && *remote.GRPCCode == connectUnimplementedCode {
		return true
	}
	return streamTransportUnavailableStatus(remote.StatusCode)
}

// connectUnimplementedCode is the gRPC status a Connect route reports for a
// method it does not serve.
const connectUnimplementedCode = 12

// errStreamWireAbsent marks a terminal whose cause was the provider saying this
// wire is not served at this path, where no status carries that fact — the
// WebSocket handshake that completed without selecting the first-party
// subprotocol.
var errStreamWireAbsent = stderrors.New("declared transport is not served at this path")

// absentWire carries the terminal the caller reads and, beside it, the fact
// that a declared fallback may act on it. Its message is the surfaced error's
// own: the classification is for the dispatcher, never for the caller.
type absentWire struct{ surfaced error }

func (e *absentWire) Error() string { return e.surfaced.Error() }

func (e *absentWire) Unwrap() []error { return []error{e.surfaced, errStreamWireAbsent} }

// markAbsentWire tags a terminal the dispatcher may fall back on, leaving the
// error the caller reads exactly as it was.
func markAbsentWire(err error, absent bool) error {
	if err == nil || !absent {
		return err
	}
	return &absentWire{surfaced: err}
}
