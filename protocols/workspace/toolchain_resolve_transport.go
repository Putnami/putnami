// Toolchain-resolve transport: how the core ASKS an extension for its
// toolchain pins and how the extension ANSWERS. The reasoning is recorded in
// doc/adr/0003-extensions-answer-toolchain-pins.md.
//
// toolchain_resolve.go fixes the shapes; this file fixes the exchange. It is
// the probe's exchange (probe_transport.go) under another control command:
//
//   - ONE reserved control invocation, `<exe> __putnami toolchain-resolve`, in
//     the namespace every core-initiated control call shares, so no task an
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

// ToolchainResolveControlCommand is the control command that asks an
// extension for its toolchain pins, under ProbeControlVerb.
const ToolchainResolveControlCommand = "toolchain-resolve"

// ToolchainResolveControlArgs is the argv tail that invokes an extension's
// toolchain resolution. Callers spawn `<executable>` with exactly these
// arguments.
func ToolchainResolveControlArgs() []string {
	return []string{ProbeControlVerb, ToolchainResolveControlCommand}
}

// IsToolchainResolveInvocation reports whether args (the executable's
// arguments, excluding argv[0]) name the reserved toolchain-resolve control
// call.
func IsToolchainResolveInvocation(args []string) bool {
	return len(args) == 2 && args[0] == ProbeControlVerb && args[1] == ToolchainResolveControlCommand
}

// ToolchainResolveFunc is an extension's answer to one request. Returning an
// error means the extension could not answer; ServeToolchainResolve then
// writes NOTHING to stdout, so the core sees "no result document" rather than
// a partial answer it might record in the lock.
type ToolchainResolveFunc func(ToolchainResolveRequest) (ToolchainResolveResult, error)

// EncodeToolchainResolveRequest writes one request document. It is the core
// side of the exchange: it stamps the protocol version, sorts the asked
// identities and drops repeats, and validates the request before a byte is
// written, so a request the extension would reject is reported at its source.
func EncodeToolchainResolveRequest(w io.Writer, request ToolchainResolveRequest) error {
	request.Version = ToolchainResolveProtocolVersion
	normalizeToolchainResolveRequest(&request)
	if diags := ValidateToolchainResolveRequest(&request); diag.HasErrors(diags) {
		return fmt.Errorf("toolchain-resolve request rejected by the v%d contract: %s",
			ToolchainResolveProtocolVersion, formatProbeDiagnostics(diags))
	}
	data, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode toolchain-resolve request: %w", err)
	}
	if len(data) > MaxProbeDocumentBytes {
		return fmt.Errorf("toolchain-resolve request is %d bytes; the contract caps it at %d", len(data), MaxProbeDocumentBytes)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write toolchain-resolve request: %w", err)
	}
	return nil
}

// DecodeToolchainResolveDocument reads exactly one bounded JSON document from
// r, with the rules of DecodeProbeDocument: trailing data and an empty or
// oversized document are errors.
func DecodeToolchainResolveDocument(r io.Reader) ([]byte, error) {
	return decodeControlDocument(r, "toolchain-resolve document")
}

// ServeToolchainResolve is the extension side of the exchange.
//
// It reports handled=false when args do not name the toolchain-resolve control
// call, so an extension binary can call it before its ordinary subcommand
// dispatch, as it calls ServeProbe. When it handles the call, the request is
// strict-parsed and validated first: an extension must never answer a
// question it could not fully read. The answer function receives the request
// normalized, with the asked identities sorted.
//
// The answer is attributed, then validated on its own and against the request
// BEFORE a byte reaches stdout, so an answer the core would reject is reported
// to the extension author here. On any error stdout stays empty.
func ServeToolchainResolve(args []string, stdin io.Reader, stdout io.Writer, answer ToolchainResolveFunc) (bool, error) {
	if !IsToolchainResolveInvocation(args) {
		return false, nil
	}
	if answer == nil {
		return true, errors.New("workspace: toolchain-resolve invoked with no answer function")
	}

	data, err := DecodeToolchainResolveDocument(stdin)
	if err != nil {
		return true, err
	}
	request, diags := ParseAndValidateToolchainResolveRequest(data)
	if diag.HasErrors(diags) {
		return true, fmt.Errorf("toolchain-resolve request rejected: %s", formatProbeDiagnostics(diags))
	}
	normalizeToolchainResolveRequest(request)

	result, err := answer(cloneToolchainResolveRequest(*request))
	if err != nil {
		return true, err
	}
	result.Version = ToolchainResolveProtocolVersion
	if result.Extension == "" {
		result.Extension = request.Extension
	}
	diags = append(ValidateToolchainResolveResult(&result), ValidateToolchainResolveAnswer(request, &result)...)
	if diag.HasErrors(diags) {
		return true, fmt.Errorf("toolchain-resolve answer rejected by the v%d contract: %s",
			ToolchainResolveProtocolVersion, formatProbeDiagnostics(diags))
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return true, fmt.Errorf("encode toolchain-resolve result: %w", err)
	}
	if len(encoded) > MaxProbeDocumentBytes {
		return true, fmt.Errorf("toolchain-resolve result is %d bytes; the contract caps it at %d",
			len(encoded), MaxProbeDocumentBytes)
	}
	if _, err := stdout.Write(append(encoded, '\n')); err != nil {
		return true, fmt.Errorf("write toolchain-resolve result: %w", err)
	}
	return true, nil
}
