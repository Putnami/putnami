// Input-resolve transport: how the core ASKS an extension to expand its input
// selectors and how the extension ANSWERS. The reasoning is recorded in
// doc/adr/0004-extensions-expand-their-input-selectors.md.
//
// input_resolve.go fixes the shapes; this file fixes the exchange. It is the
// probe's exchange (probe_transport.go) under another control command:
//
//   - ONE reserved control invocation, `<exe> __putnami input-resolve`, in the
//     namespace every core-initiated control call shares, so no task an
//     extension declares can shadow it.
//   - ONE JSON document in on stdin, ONE out on stdout, and stdout carries
//     nothing else. Human output belongs on stderr.
//   - BOUNDED. Both directions are capped at MaxProbeDocumentBytes.
//
// The core runs the executable with the workspace root as its working
// directory and the probe's timeout.

package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	diag "go.putnami.dev/protocol/diagnostic"
)

// InputResolveControlCommand is the control command that asks an extension to
// expand its input selectors, under ProbeControlVerb.
const InputResolveControlCommand = "input-resolve"

// InputResolveControlArgs is the argv tail that invokes an extension's input
// resolution. Callers spawn `<executable>` with exactly these arguments.
func InputResolveControlArgs() []string {
	return []string{ProbeControlVerb, InputResolveControlCommand}
}

// IsInputResolveInvocation reports whether args (the executable's arguments,
// excluding argv[0]) name the reserved input-resolve control call.
func IsInputResolveInvocation(args []string) bool {
	return len(args) == 2 && args[0] == ProbeControlVerb && args[1] == InputResolveControlCommand
}

// InputResolveFunc is an extension's answer to one request. Returning an
// error means the extension could not answer; ServeInputResolve then writes
// NOTHING to stdout, so the core sees "no result document" rather than a
// partial answer it might key a task on. A selection that is invalid is not
// such an error: the extension answers it as the input's Error.
type InputResolveFunc func(InputResolveRequest) (InputResolveResult, error)

// EncodeInputResolveRequest writes one request document. It is the core side
// of the exchange: it stamps the protocol version and validates the request
// before a byte is written, so a request the extension would reject is
// reported at its source. The inputs keep the caller's order, which the
// answer follows.
func EncodeInputResolveRequest(w io.Writer, request InputResolveRequest) error {
	request.Version = InputResolveProtocolVersion
	if diags := ValidateInputResolveRequest(&request); diag.HasErrors(diags) {
		return fmt.Errorf("input-resolve request rejected by the v%d contract: %s",
			InputResolveProtocolVersion, formatProbeDiagnostics(diags))
	}
	data, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode input-resolve request: %w", err)
	}
	if len(data) > MaxProbeDocumentBytes {
		return fmt.Errorf("input-resolve request is %d bytes; the contract caps it at %d", len(data), MaxProbeDocumentBytes)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write input-resolve request: %w", err)
	}
	return nil
}

// DecodeInputResolveDocument reads exactly one bounded JSON document from r,
// with the rules of DecodeProbeDocument: trailing data and an empty or
// oversized document are errors.
func DecodeInputResolveDocument(r io.Reader) ([]byte, error) {
	return decodeControlDocument(r, "input-resolve document")
}

// ServeInputResolve is the extension side of the exchange.
//
// It reports handled=false when args do not name the input-resolve control
// call, so an extension binary can call it before its ordinary subcommand
// dispatch, as it calls ServeProbe. When it handles the call, the request is
// strict-parsed and validated first: an extension must never answer a
// question it could not fully read. The answer function receives a copy of
// the request.
//
// The answer is attributed, then validated on its own and against the
// request BEFORE a byte reaches stdout, so an answer the core would reject is
// reported to the extension author here. On any error stdout stays empty.
func ServeInputResolve(args []string, stdin io.Reader, stdout io.Writer, answer InputResolveFunc) (bool, error) {
	if !IsInputResolveInvocation(args) {
		return false, nil
	}
	if answer == nil {
		return true, errors.New("workspace: input-resolve invoked with no answer function")
	}

	data, err := DecodeInputResolveDocument(stdin)
	if err != nil {
		return true, err
	}
	request, diags := ParseAndValidateInputResolveRequest(data)
	if diag.HasErrors(diags) {
		return true, fmt.Errorf("input-resolve request rejected: %s", formatProbeDiagnostics(diags))
	}

	result, err := answer(cloneInputResolveRequest(*request))
	if err != nil {
		return true, err
	}
	result.Version = InputResolveProtocolVersion
	if result.Extension == "" {
		result.Extension = request.Extension
	}
	diags = append(ValidateInputResolveResult(&result), ValidateInputResolveAnswer(request, &result)...)
	if diag.HasErrors(diags) {
		return true, fmt.Errorf("input-resolve answer rejected by the v%d contract: %s",
			InputResolveProtocolVersion, formatProbeDiagnostics(diags))
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return true, fmt.Errorf("encode input-resolve result: %w", err)
	}
	if len(encoded) > MaxProbeDocumentBytes {
		return true, fmt.Errorf("input-resolve result is %d bytes; the contract caps it at %d",
			len(encoded), MaxProbeDocumentBytes)
	}
	if _, err := stdout.Write(append(encoded, '\n')); err != nil {
		return true, fmt.Errorf("write input-resolve result: %w", err)
	}
	return true, nil
}
