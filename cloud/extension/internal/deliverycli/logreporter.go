package deliverycli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	protocolcli "go.putnami.dev/protocol/cli"
)

// logReporterChunkPath is the run-log chunk route under the ingest base
// (SessionReporterIngestURLEnv): Delivery's RecordsRunLogChunkIngestPath.
const logReporterChunkPath = "/logs"

// Delivery's run-log ingest headers, mirrored from the delivery API
// (delivery-api) records contract (the extension does not import server packages).
const (
	runLogSeqHeader    = "X-Putnami-Log-Seq"
	runLogFinalHeader  = "X-Putnami-Log-Final"
	runLogOffsetHeader = "X-Putnami-Log-Offset"
	runLogSHA256Header = "X-Putnami-Log-Sha256"
)

// LogReporter runs the extension binary as the engine's log reporter:
// the second capability of the protocol/cli session-reporting wire.
// The engine spawns it once per session when PUTNAMI_LOG_REPORTER selects this
// extension, and streams events.jsonl to it while tasks run: one frame at
// 64 KiB or every 2 seconds, then an empty final marker.
//
// Each frame becomes one POST of its bytes to Delivery's run-log ingest under
// the run credential. That chunk log is what `putnami cloud ci logs`, the live
// tail and the console read, so a run's log is the engine's own event stream
// and no script on the runner tails a file.
//
// The reporter is stateless, like SessionReporter. The engine owns the
// checkpoint, the pending frame and the retries. The frame's offset and digest
// travel with it, so a frame resent after a lost acknowledgement is recognized
// by its bytes and answered as already stored, and a frame that contradicts the
// committed log is refused instead of spliced in.
//
// Reporting never changes the run's verdict: every outcome here is an ACK, and
// the engine records a refused or lost stream as evidence, not as a failure.
func LogReporter(_ map[string]any, _ []string, _ string, env map[string]string, _ clicore.IO) error {
	return serveLogReporter(env, os.Stdin, os.Stdout, os.Stderr)
}

// serveLogReporter denies inspection, reads the reporter's configuration from
// env, then serves frames until in closes, like serveSessionReporter.
func serveLogReporter(env map[string]string, in io.Reader, out, logw io.Writer) error {
	cfg := newReporterConfig(env, protocolcli.LogReporterCommand, protocolcli.LogReporterTokenEnv, logw)
	return runLogReporter(context.Background(), in, out, logw, cfg, &http.Client{Timeout: sessionReporterRequestTimeout})
}

// runLogReporter serves frames until stdin closes, like runSessionReporter.
func runLogReporter(ctx context.Context, in io.Reader, out, logw io.Writer, cfg sessionReporterConfig, client *http.Client) error {
	sender := &logReporterSender{cfg: &cfg, client: client}
	return serveReportingFrames(ctx, in, out, logw, protocolcli.LogReporterCommand, &cfg, sender.deliver)
}

type logReporterSender struct {
	// cfg is shared with the frame loop, whose handshake sets its bearer
	// before the first chunk.
	cfg    *sessionReporterConfig
	client *http.Client
	// sessionID is bound by the first frame: one reporter process serves one
	// engine session, and the run credential binds one run's log.
	sessionID string
}

func (s *logReporterSender) deliver(ctx context.Context, chunk *protocolcli.SessionReportingChunk) protocolcli.SessionReportingAck {
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
	// The log is the event stream alone. session.json and plan.json belong to
	// the session reporter; appending one here would put a second document in
	// the run's log.
	if !slices.Contains(protocolcli.SessionReportingArtifacts(protocolcli.LogReporterCommand), chunk.Artifact) {
		return nack(chunk, ackCodeInvalidChunk, false)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.ingestURL, "/")+logReporterChunkPath, bytes.NewReader(chunk.Data))
	if err != nil {
		return nack(chunk, ackCodeNotConfigured, false)
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(runLogSeqHeader, strconv.FormatInt(chunk.Sequence, 10))
	req.Header.Set(runLogFinalHeader, strconv.FormatBool(chunk.Final))
	req.Header.Set(runLogOffsetHeader, strconv.FormatInt(chunk.Offset, 10))
	req.Header.Set(runLogSHA256Header, chunk.SHA256)
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
			Seq *int64 `json:"seq"`
		}
		if json.Unmarshal(body, &accepted) != nil || accepted.Seq == nil || *accepted.Seq != chunk.Sequence {
			// A 200 that does not name this exact frame is not durable acceptance
			// of it. Not retryable: the same request would get the same answer.
			return nack(chunk, ackCodeReceiverMismatch, false)
		}
		// applied:false is also acceptance: the receiver verified the offset and
		// the digest, so these bytes are the ones it already stored.
		return chunk.Ack()
	case resp.StatusCode == http.StatusConflict:
		var conflict struct {
			ExpectedSeq *int64 `json:"expectedSeq"`
		}
		if json.Unmarshal(body, &conflict) == nil && conflict.ExpectedSeq != nil {
			// The receiver is behind the engine's cursor. The engine never rewinds
			// on a NACK, so this is terminal for the run's log rather than retryable.
			return nack(chunk, ackCodeSequenceGap, false)
		}
		return nack(chunk, ackCodeReplayConflict, false)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// A caller that is not a run: a laptop with the selector set and a
		// workspace token. The ingest accepts the run credential only.
		return nack(chunk, ackCodeUnauthorized, false)
	case resp.StatusCode == http.StatusRequestEntityTooLarge:
		return nack(chunk, ackCodeChunkTooLarge, false)
	case resp.StatusCode == http.StatusNotFound:
		// A dormant ingest: the route does not exist on this host. A retry cannot
		// grow a route.
		return nack(chunk, ackCodeIngestUnavailable, false)
	case platformShedRequest(resp.StatusCode):
		return nack(chunk, ackCodeReceiverUnavailable, true)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// Includes a run past its log caps (400 invalid_chunk).
		return nack(chunk, ackCodeInvalidChunk, false)
	default:
		return nack(chunk, ackCodeReceiverUnavailable, true)
	}
}
