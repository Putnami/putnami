package deliverycli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/procguard"
)

// Session reporter environment. A hosted run (--credential-fd) hands the run
// credential over the protocol 2 handshake (reportingHandshake) and places no
// token in the environment. Otherwise the engine places the token in this
// process's environment only (protocolcli.SessionReporterTokenEnv, captured
// before any repository hook runs). The ingest URL is a non-secret destination
// the trusted launcher exports beside the reporter selector.
const (
	// SessionReporterIngestURLEnv names the Delivery ingest base the reporter
	// POSTs to. The chunk route is RecordsRunSessionChunkIngestPath's `/sessions`
	// suffix under it, the same route the retired shell shipper used.
	SessionReporterIngestURLEnv = "PUTNAMI_CLOUD_INGEST_URL"

	sessionReporterChunkPath = "/sessions"
	// sessionReporterRequestTimeout stays under the engine's five-second RPC
	// deadline so a slow receiver produces a retryable NACK instead of a
	// silent frame the engine has to time out on.
	sessionReporterRequestTimeout = 4 * time.Second
	// sessionReporterLineBytes bounds one stdin frame: the protocol line cap plus
	// the base64 expansion of a full chunk, so a canonical frame always fits.
	sessionReporterLineBytes = protocolcli.SessionReportingLineBytes + 2*protocolcli.SessionReportingChunkBytes
)

// The recorded plan, plan.json, which the engine's session reporter sends
// before any other frame.
const (
	planArtifact = "plan.json"
	// sessionReporterPlanRetryableNacks is the retryable NACK on a plan.json
	// frame that this process answers without retry instead. The engine omits
	// a plan its reporter refuses without retry and delivers the rest, but a
	// refusal with retry on its third and last attempt at a frame fails the
	// whole session's delivery. Every attempt at a frame reaches the same
	// process unless one fails in transport, so a fault only the plan meets
	// costs the plan, never the session. The count spans the whole plan, so
	// the plan delays the first events frame by at most three requests.
	sessionReporterPlanRetryableNacks = 3
)

// Delivery's run-session ingest headers, mirrored from the delivery API
// (delivery-api) records contract (the extension does not import server packages).
const (
	sessionPartHeader  = "X-Putnami-Session-Part"
	sessionSeqHeader   = "X-Putnami-Session-Seq"
	sessionFinalHeader = "X-Putnami-Session-Final"
)

// Bounded machine codes carried on a negative acknowledgement. The wire forbids
// human text, so each receiver answer class maps to exactly one of these.
const (
	ackCodeSessionMismatch     = "session_mismatch"
	ackCodeNotConfigured       = "reporter_not_configured"
	ackCodeUnauthorized        = "unauthorized"
	ackCodeInvalidChunk        = "invalid_chunk"
	ackCodeChunkTooLarge       = "chunk_too_large"
	ackCodeIngestUnavailable   = "ingest_unavailable"
	ackCodeSequenceGap         = "sequence_gap"
	ackCodeReplayConflict      = "replay_conflict"
	ackCodeReceiverMismatch    = "receiver_mismatch"
	ackCodeReceiverUnavailable = "receiver_unavailable"
	// ackCodeInspectionGuard is a reporter that could not deny inspection, so
	// it did not use its credential (see newReporterConfig).
	ackCodeInspectionGuard = "inspection_guard_unavailable"
)

// SessionReporter runs the extension binary as the engine's session
// reporter: the provider half of the protocol/cli session-reporting wire. Core
// spawns it once per engine session (or once per `putnami sessions replay`),
// writes one SessionReportingChunk per line on stdin and reads one
// SessionReportingAck per line from stdout. Each frame becomes one POST of the
// original artifact bytes to Delivery's run-session ingest under the run
// credential, so the receiver — not this process — owns durability, replay
// detection and derivation. On a hosted run the chunks follow the protocol 2
// handshake that hands this process the run credential (reportingHandshake);
// elsewhere the credential is the token in its environment.
//
// The reporter is stateless on purpose. The run credential binds the
// destination: Delivery derives the workspace/run from the bearer and keeps a
// dense per-part sequence, so a replay under a credential for another run
// cannot resume mid-stream — it is refused as a sequence gap. Within one
// process the first frame's session id is bound and a different id is refused.
//
// Because the protocol owns stdout, diagnostics go to stderr; core discards
// them, which is exactly why no ACK ever carries receiver text or the URL.
func SessionReporter(_ map[string]any, _ []string, _ string, env map[string]string, _ clicore.IO) error {
	return serveSessionReporter(env, os.Stdin, os.Stdout, os.Stderr)
}

// serveSessionReporter denies inspection, reads the reporter's configuration
// from env, then serves frames until in closes.
func serveSessionReporter(env map[string]string, in io.Reader, out, logw io.Writer) error {
	cfg := newReporterConfig(env, protocolcli.SessionReporterCommand, protocolcli.SessionReporterTokenEnv, logw)
	return runSessionReporter(context.Background(), in, out, logw, cfg, &http.Client{Timeout: sessionReporterRequestTimeout})
}

type sessionReporterConfig struct {
	ingestURL string
	// token is the bearer of every chunk: the run credential an accepted
	// authenticate handed over, or else the token from the environment.
	token string
	// inspectionGuardFailed records that the process could not deny
	// inspection, so it holds no token, refuses initialize and answers every
	// frame with ackCodeInspectionGuard.
	inspectionGuardFailed bool
}

// Format renders the configuration for every verb without its bearer, so no
// diagnostic can print the run credential.
func (c sessionReporterConfig) Format(f fmt.State, _ rune) {
	bearer := ""
	if c.token != "" {
		bearer = "<redacted>"
	}
	fmt.Fprintf(f, "{ingestURL:%s token:%s inspectionGuardFailed:%t}", c.ingestURL, bearer, c.inspectionGuardFailed)
}

// reporterDenyInspection marks a reporter process non-dumpable before it reads
// its run credential (decision D20). Tests replace it.
var reporterDenyInspection = procguard.DenyInspection

// newReporterConfig is the first thing a reporter does: it denies inspection,
// then reads the ingest base and any token the engine placed in its
// environment. Denying inspection first means the process is guarded before
// it reads stdin, so before it answers initialize and receives the run
// credential over the protocol 2 handshake. A token from the environment, the
// protocol 1 path, arrives at exec, so the window between exec and this call
// stays open for it (D20). A process that cannot deny inspection keeps serving
// without a credential, as the cache provider keeps its session without a run
// credential it cannot protect: it refuses initialize, and every frame gets a
// terminal inspection_guard_unavailable NACK, which the engine records, and
// the run's verdict does not change.
func newReporterConfig(env map[string]string, command, tokenEnv string, logw io.Writer) sessionReporterConfig {
	if err := reporterDenyInspection(); err != nil {
		fmt.Fprintf(logw, "putnami-cloud %s: cannot deny process inspection (%v); not using the run credential\n", command, err)
		return sessionReporterConfig{inspectionGuardFailed: true}
	}
	return sessionReporterConfig{
		ingestURL: strings.TrimSpace(env[SessionReporterIngestURLEnv]),
		token:     env[tokenEnv],
	}
}

// runSessionReporter serves frames until stdin closes. It returns an error only
// for a frame it cannot answer at all (a malformed line has no identity to
// echo); every receiver outcome is an ACK.
func runSessionReporter(ctx context.Context, in io.Reader, out, logw io.Writer, cfg sessionReporterConfig, client *http.Client) error {
	sender := &sessionReporterSender{cfg: &cfg, client: client}
	return serveReportingFrames(ctx, in, out, logw, protocolcli.SessionReporterCommand, &cfg, sender.deliver)
}

// reportingDeliverer answers one frame of the session-reporting wire. Every
// receiver outcome is an ACK, positive or negative.
type reportingDeliverer func(ctx context.Context, chunk *protocolcli.SessionReportingChunk) protocolcli.SessionReportingAck

// serveReportingFrames is the wire loop the session reporter and the log
// reporter share: one frame per stdin line, one ACK per stdout line, until
// stdin closes. command names the provider in its stderr diagnostics.
//
// Before the first chunk it also answers the protocol 2 handshake
// (reportingHandshake), one answer per line, which sets cfg's bearer. From the
// first chunk on, every line is a chunk, as in protocol 1.
func serveReportingFrames(ctx context.Context, in io.Reader, out, logw io.Writer, command string, cfg *sessionReporterConfig, deliver reportingDeliverer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), sessionReporterLineBytes)
	w := bufio.NewWriter(out)
	handshake := &reportingHandshake{cfg: cfg}
	streaming := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !streaming {
			if op, ok := handshakeOp(line); ok {
				result := handshake.answer(op, line)
				if !result.OK {
					// The code only: the line may hold the run credential. op and
					// the code are protocol constants (handshakeOp, answer).
					fmt.Fprintf(logw, "putnami-cloud %s: refused %s: %s\n", command, op, result.Code) //nolint:gosec // G705: logw is the reporter's stderr, never an HTTP response, and every value is a constant
				}
				if err := writeReportingLine(w, result); err != nil {
					return err
				}
				continue
			}
		}
		chunk, err := protocolcli.ParseSessionReportingChunk(line)
		if err != nil {
			fmt.Fprintf(logw, "putnami-cloud %s: malformed frame: %v\n", command, err)
			return fmt.Errorf("malformed session reporting frame")
		}
		streaming = true
		if err := writeReportingLine(w, deliver(ctx, chunk)); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("session reporting frame exceeds %d bytes", sessionReporterLineBytes)
		}
		return err
	}
	return nil
}

// writeReportingLine writes one JSON line on the protocol's stdout and flushes
// it: the engine waits for each answer before it sends the next line.
func writeReportingLine(w *bufio.Writer, v any) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return w.Flush()
}

type sessionReporterSender struct {
	// cfg is shared with the frame loop, whose handshake sets its bearer
	// before the first chunk.
	cfg    *sessionReporterConfig
	client *http.Client
	// sessionID is bound by the first frame: one reporter process serves one
	// engine session, and the receiver stores one session per run.
	sessionID string
	// planRetryableNacks counts the retryable NACKs this process answered
	// plan.json frames (sessionReporterPlanRetryableNacks).
	planRetryableNacks int
}

func nack(chunk *protocolcli.SessionReportingChunk, code string, retryable bool) protocolcli.SessionReportingAck {
	ack := chunk.Ack()
	ack.OK = false
	ack.Code = code
	ack.Retryable = retryable
	return ack
}

// deliver answers one frame (post), and answers the plan's
// sessionReporterPlanRetryableNacks-th retryable NACK without retry.
func (s *sessionReporterSender) deliver(ctx context.Context, chunk *protocolcli.SessionReportingChunk) protocolcli.SessionReportingAck {
	ack := s.post(ctx, chunk)
	if chunk.Artifact == planArtifact && !ack.OK && ack.Retryable {
		s.planRetryableNacks++
		if s.planRetryableNacks >= sessionReporterPlanRetryableNacks {
			ack.Retryable = false
		}
	}
	return ack
}

func (s *sessionReporterSender) post(ctx context.Context, chunk *protocolcli.SessionReportingChunk) protocolcli.SessionReportingAck {
	if s.sessionID == "" {
		s.sessionID = chunk.SessionID
	}
	if chunk.SessionID != s.sessionID {
		return nack(chunk, ackCodeSessionMismatch, false)
	}
	if s.cfg.inspectionGuardFailed {
		return nack(chunk, ackCodeInspectionGuard, false)
	}
	if s.cfg.ingestURL == "" || s.cfg.token == "" {
		return nack(chunk, ackCodeNotConfigured, false)
	}
	part, ok := sessionPartFor(chunk.Artifact)
	if !ok {
		return nack(chunk, ackCodeInvalidChunk, false)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.ingestURL, "/")+sessionReporterChunkPath, bytes.NewReader(chunk.Data))
	if err != nil {
		return nack(chunk, ackCodeNotConfigured, false)
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(sessionPartHeader, part)
	req.Header.Set(sessionSeqHeader, strconv.FormatInt(chunk.Sequence, 10))
	req.Header.Set(sessionFinalHeader, strconv.FormatBool(chunk.Final))
	req.ContentLength = int64(len(chunk.Data))

	resp, err := s.client.Do(req) //nolint:gosec // G704: the destination is the launcher-exported ingest base, never frame content
	if err != nil {
		// Transport failures (dial, reset, deadline) are the class worth another
		// bounded attempt; the receiver may never have seen the bytes.
		return nack(chunk, ackCodeReceiverUnavailable, true)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	switch {
	case resp.StatusCode == http.StatusOK:
		var accepted struct {
			Part string `json:"part"`
			Seq  int64  `json:"seq"`
		}
		if json.Unmarshal(body, &accepted) != nil || accepted.Part != part || accepted.Seq != chunk.Sequence {
			// A 200 that does not name this exact frame is not durable acceptance
			// of it. Not retryable: the same request would get the same answer.
			return nack(chunk, ackCodeReceiverMismatch, false)
		}
		return chunk.Ack()
	case resp.StatusCode == http.StatusConflict:
		var conflict struct {
			ExpectedSeq *int64 `json:"expectedSeq"`
		}
		if json.Unmarshal(body, &conflict) == nil && conflict.ExpectedSeq != nil {
			// The receiver is behind the engine's cursor. The engine never rewinds
			// on a NACK, so this is terminal for the run rather than retryable.
			return nack(chunk, ackCodeSequenceGap, false)
		}
		return nack(chunk, ackCodeReplayConflict, false)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nack(chunk, ackCodeUnauthorized, false)
	case resp.StatusCode == http.StatusRequestEntityTooLarge:
		return nack(chunk, ackCodeChunkTooLarge, false)
	case resp.StatusCode == http.StatusNotFound:
		// A dormant ingest: the route does not exist on this host. Terminal, like
		// the shell shipper's dormantIngestRefusal — a retry cannot grow a route.
		return nack(chunk, ackCodeIngestUnavailable, false)
	case platformShedRequest(resp.StatusCode):
		return nack(chunk, ackCodeReceiverUnavailable, true)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return nack(chunk, ackCodeInvalidChunk, false)
	default:
		return nack(chunk, ackCodeReceiverUnavailable, true)
	}
}

// platformShedRequest reports whether the platform refused the request before
// the ingest judged the frame: no instance was free or a rate limit applied
// (429), or the request timed out upstream (408). The same frame can land on
// the next attempt, so both reporters answer it with a retryable
// receiver_unavailable NACK. The reporter makes one POST per frame and keeps
// no retry state: the engine resends a retryable frame and bounds the
// attempts.
func platformShedRequest(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout
}

// sessionPartFor maps the wire's canonical artifact name onto Delivery's closed
// part vocabulary. Both sides already agree on the three documents; only the
// spelling differs. A Delivery that predates the `plan` part answers 400, which
// post answers as a terminal invalid_chunk: the engine omits the plan and
// delivers the rest, so this receiver and delivery-api deploy in either order.
func sessionPartFor(artifact string) (string, bool) {
	switch artifact {
	case "session.json":
		return "session", true
	case "events.jsonl":
		return "events", true
	case planArtifact:
		return "plan", true
	}
	return "", false
}
