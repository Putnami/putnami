// Probe transport: how core ASKS and how a provider ANSWERS. The reasoning is
// recorded in doc/adr/0001-core-owns-identity-providers-own-language.md.
//
// probe.go fixes the shapes and the digest; this file fixes the exchange, in
// the protocol rather than in either consumer, because the two ends are written
// in different repositories and would otherwise agree only by coincidence.
//
// The exchange is deliberately the smallest thing that can work:
//
//   - ONE reserved control invocation. The provider executable is spawned as
//     `<exe> __putnami workspace-probe`, the same reserved-verb shape the
//     runtime handshake uses. A reserved verb cannot collide with a task name,
//     so an extension that later declares a `workspace-probe` task does not
//     shadow the probe.
//   - ONE JSON document in, ONE JSON document out. The request arrives on
//     stdin, the result leaves on stdout, and stdout carries NOTHING else — a
//     provider that logs to stdout produces a document with trailing data,
//     which is rejected rather than truncated at the first brace. Human output
//     belongs on stderr.
//   - BOUNDED. Both directions are capped at MaxProbeDocumentBytes. An
//     unbounded read is how a provider that never terminates becomes an
//     orchestrator that never terminates.
//
// The provider answers the SAME facts for the same tree regardless of how it
// was asked: ProbeRequest.Reason is advisory and never enters the digest, and
// the Paths/Files hints may be ignored without changing the answer.

package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	// ProbeControlVerb is the reserved executable-control verb. It matches the
	// runtime handshake's verb on purpose: an extension executable has exactly
	// one reserved namespace, and every core-initiated control call lives in it.
	ProbeControlVerb = "__putnami"
	// ProbeControlCommand is the control command that runs one workspace probe.
	ProbeControlCommand = "workspace-probe"
)

// MaxProbeDocumentBytes bounds each direction of the exchange. It is generous
// for the payload (a workspace of thousands of projects encodes well under it)
// and small enough that a runaway provider fails fast instead of exhausting the
// orchestrator's memory.
const MaxProbeDocumentBytes = 8 << 20

// ProbeControlArgs is the argv tail that invokes a provider's probe. Callers
// spawn `<executable>` with exactly these arguments.
func ProbeControlArgs() []string {
	return []string{ProbeControlVerb, ProbeControlCommand}
}

// IsProbeInvocation reports whether args (the executable's arguments, excluding
// argv[0]) name the reserved probe control call.
func IsProbeInvocation(args []string) bool {
	return len(args) == 2 && args[0] == ProbeControlVerb && args[1] == ProbeControlCommand
}

// ProbeAnswerFunc is a provider's answer to one request. Returning an error
// means the provider could not answer; the caller writes NOTHING to stdout in
// that case, so core sees "no result document" rather than a half-truth it
// might merge.
type ProbeAnswerFunc func(ProbeRequest) (ProbeResult, error)

// EncodeProbeRequest writes one request document. It is the core side of the
// exchange, spelled here so the two ends cannot drift on framing.
func EncodeProbeRequest(w io.Writer, request ProbeRequest) error {
	request.Version = ProbeProtocolVersion
	data, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode probe request: %w", err)
	}
	if len(data) > MaxProbeDocumentBytes {
		return fmt.Errorf("probe request is %d bytes; the contract caps it at %d", len(data), MaxProbeDocumentBytes)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write probe request: %w", err)
	}
	return nil
}

// DecodeProbeDocument reads exactly one bounded JSON document from r.
//
// "Exactly one" is enforced: trailing data is an error, because a provider that
// prints a log line after its result would otherwise have that line silently
// discarded — and a provider that prints one BEFORE would have its result
// silently replaced by the log line's parse failure. Both are extension bugs
// that must be reported as such.
func DecodeProbeDocument(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, MaxProbeDocumentBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read probe document: %w", err)
	}
	if len(data) > MaxProbeDocumentBytes {
		return nil, fmt.Errorf("probe document exceeds the %d byte contract cap", MaxProbeDocumentBytes)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("probe document is empty")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	var probe json.RawMessage
	if err := decoder.Decode(&probe); err != nil {
		return nil, fmt.Errorf("probe document is not one JSON value: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("probe document carries trailing data; stdout must carry the result and nothing else")
	}
	return data, nil
}

// ServeProbe is the provider side of the exchange.
//
// It reports handled=false when args do not name the probe control call, so an
// extension binary can call it before its ordinary subcommand dispatch exactly
// as it calls the runtime handshake. When it does handle the call, the request
// is strict-parsed and validated first: a provider must never answer a question
// it could not fully read, because the members it skipped are the ones core
// added.
//
// The answer is normalized and validated BEFORE a byte reaches stdout. A result
// core would reject is therefore reported to the extension author here, at its
// source, instead of arriving as an opaque "invalid-result" two processes away.
func ServeProbe(args []string, stdin io.Reader, stdout io.Writer, answer ProbeAnswerFunc) (bool, error) {
	if !IsProbeInvocation(args) {
		return false, nil
	}
	if answer == nil {
		return true, errors.New("workspace: probe invoked with no answer function")
	}

	data, err := DecodeProbeDocument(stdin)
	if err != nil {
		return true, err
	}
	request, diags := ParseAndValidateProbeRequest(data)
	if diag.HasErrors(diags) {
		return true, fmt.Errorf("probe request rejected: %s", formatProbeDiagnostics(diags))
	}

	result, err := answer(*request)
	if err != nil {
		return true, err
	}
	// A negotiated member leaves only when it was asked for. A core that did
	// not ask may predate it, and it strict-decodes this answer: one unknown
	// member and every graph-dependent command it runs refuses the workspace.
	if !request.DependencySources {
		for i := range result.Projects {
			result.Projects[i].DependencySources = nil
		}
	}
	result.Version = ProbeProtocolVersion
	if result.Extension == "" {
		result.Extension = request.Extension
	}
	NormalizeProbeResult(&result)
	if diags := ValidateProbeResult(&result); diag.HasErrors(diags) {
		return true, fmt.Errorf("probe answer rejected by the v%d contract: %s",
			ProbeProtocolVersion, formatProbeDiagnostics(diags))
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return true, fmt.Errorf("encode probe result: %w", err)
	}
	if len(encoded) > MaxProbeDocumentBytes {
		return true, fmt.Errorf("probe result is %d bytes; the contract caps it at %d",
			len(encoded), MaxProbeDocumentBytes)
	}
	if _, err := stdout.Write(append(encoded, '\n')); err != nil {
		return true, fmt.Errorf("write probe result: %w", err)
	}
	return true, nil
}

// formatProbeDiagnostics renders error diagnostics in a stable, single-line
// form so a failure message is identical for identical inputs.
func formatProbeDiagnostics(diags []diag.Diagnostic) string {
	errs := diag.Errors(diags)
	if len(errs) == 0 {
		return "no diagnostics"
	}
	var buf bytes.Buffer
	for i, d := range errs {
		if i > 0 {
			buf.WriteString("; ")
		}
		buf.WriteString(d.String())
	}
	return buf.String()
}
