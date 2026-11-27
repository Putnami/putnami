package deliverycli

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
)

// A receiver that stores frames larger than 64 KiB names its decoded cap on
// every answer of its session routes (ADR 0131).
const (
	sessionContractHeader   = "X-Putnami-Session-Contract"
	sessionChunkBytesHeader = "X-Putnami-Session-Chunk-Bytes"
	sessionLargeContract    = "2"
)

// ackCodeFrameUnsupported refuses a version 3 frame the destination does not
// accept; the receiver stored nothing of it.
const ackCodeFrameUnsupported = protocolcli.SessionReportingCodeFrameUnsupported

// framingRequested reports whether the engine named framing version 3 in the
// session reporter's environment: it then reads chunkBytes on every
// acknowledgement and may send version 3 frames.
func framingRequested(env map[string]string) bool {
	version, err := strconv.Atoi(strings.TrimSpace(env[protocolcli.SessionReportingFramingEnv]))
	return err == nil && version >= protocolcli.SessionReportingLargeFrameVersion
}

// frameCap tracks the decoded cap the latest answer of one destination
// advertised. Like chunkEncoder, the latest exchange decides: an answer
// without the headers, or a request without an answer, advertises nothing.
// One frameCap serves one reporter, whose frames arrive one at a time.
type frameCap struct {
	// url is the destination whose latest exchange got an answer, or "".
	url string
	// bytes is the cap that answer advertised, or 0 when it advertised none.
	bytes int
}

// observe records the answer url gave with status, or no answer when header
// is nil. An answer that a gateway or the platform may have given instead of
// the receiver (decisiveAnswer) changes nothing: it says nothing of the
// receiver's cap.
func (c *frameCap) observe(url string, status int, header http.Header) {
	if header == nil {
		*c = frameCap{}
		return
	}
	if !decisiveAnswer(status) {
		return
	}
	*c = frameCap{url: url, bytes: advertisedChunkBytes(header)}
}

// decisiveAnswer reports whether an answer of status comes from the receiver
// that judged the request: not a 5xx, a timeout (408) or a shed request (429),
// which a gateway in front of it may give whether or not it stored the frame.
func decisiveAnswer(status int) bool {
	return status < http.StatusInternalServerError && !platformShedRequest(status)
}

// answered reports whether the latest exchange with url got an answer.
func (c *frameCap) answered(url string) bool {
	return url != "" && c.url == url
}

// accepts reports whether the latest answer of url advertised a cap of at
// least n decoded bytes.
func (c *frameCap) accepts(url string, n int) bool {
	return c.answered(url) && c.bytes >= n
}

// ackBytes is the cap an acknowledgement names for url: the advertised one,
// or 64 KiB when url advertised none.
func (c *frameCap) ackBytes(url string) int {
	if c.answered(url) && c.bytes > protocolcli.SessionReportingChunkBytes {
		return c.bytes
	}
	return protocolcli.SessionReportingChunkBytes
}

// advertisedChunkBytes is the cap header advertises: the contract header names
// version 2 and the cap header a number above 64 KiB and at most the protocol's
// maximum. Anything else advertises nothing, 0.
func advertisedChunkBytes(header http.Header) int {
	if strings.TrimSpace(header.Get(sessionContractHeader)) != sessionLargeContract {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(header.Get(sessionChunkBytesHeader)))
	if err != nil || n <= protocolcli.SessionReportingChunkBytes || n > protocolcli.SessionReportingMaxChunkBytes {
		return 0
	}
	return n
}

// probe asks dest for its advertisement with one OPTIONS request, which
// stores nothing, and reports whether it answered. A version 3 frame to a
// destination that has not answered in this process, after a restart or in a
// replay, waits for it.
func (s *sessionReporterSender) probe(ctx context.Context, dest sessionDestination) bool {
	req, err := sessionRequest(ctx, http.MethodOptions, dest, nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.encoder.observe(dest.url, nil)
		s.frameCap.observe(dest.url, 0, nil)
		return false
	}
	_ = resp.Body.Close()
	// The probe's answer also says whether the receiver reads gzip, so the
	// frame that follows it goes compressed.
	s.encoder.observe(dest.url, resp.Header)
	s.frameCap.observe(dest.url, resp.StatusCode, resp.Header)
	return s.frameCap.answered(dest.url)
}

// admitLarge decides whether chunk may be posted to dest: a frame of 64 KiB
// or less always may; a version 3 frame only once the latest answer of dest
// advertised a cap at least its size, probing dest first when it has not
// answered. It returns the NACK code that refuses the frame, or "".
//
// It refuses a frame frame_unsupported only when no earlier attempt at it is
// unresolved (unsure): that code tells the engine the receiver holds nothing
// of the frame, and the engine then cuts its sequence again. An attempt that
// got no decisive answer may have been stored, so a frame after one gets a
// retryable receiver_unavailable instead, and its sequence keeps its bytes.
func (s *sessionReporterSender) admitLarge(ctx context.Context, dest sessionDestination, chunk *protocolcli.SessionReportingChunk) (code string, retryable bool) {
	if chunk.ProtocolVersion != protocolcli.SessionReportingLargeFrameVersion {
		return "", false
	}
	if !s.frameCap.answered(dest.url) && !s.probe(ctx, dest) {
		// Nothing was posted: the frame's state at the receiver is what it
		// was before this attempt.
		return ackCodeReceiverUnavailable, true
	}
	if !s.frameCap.accepts(dest.url, len(chunk.Data)) {
		return s.unsupported(chunk)
	}
	return "", false
}

// unsupported is the refusal of a version 3 frame its receiver does not take:
// frame_unsupported, or a retryable receiver_unavailable when an earlier
// attempt at the frame is unresolved.
func (s *sessionReporterSender) unsupported(chunk *protocolcli.SessionReportingChunk) (string, bool) {
	if s.unsure[frameKey(chunk)] {
		return ackCodeReceiverUnavailable, true
	}
	return ackCodeFrameUnsupported, false
}

// frameKey names a frame's identity within the session.
func frameKey(chunk *protocolcli.SessionReportingChunk) string {
	return chunk.Artifact + "/" + strconv.FormatInt(chunk.Sequence, 10)
}

// markUnsure records that an attempt at a version 3 frame ended without a
// decisive answer, so the receiver may hold it.
func (s *sessionReporterSender) markUnsure(chunk *protocolcli.SessionReportingChunk) {
	if chunk.ProtocolVersion != protocolcli.SessionReportingLargeFrameVersion {
		return
	}
	if s.unsure == nil {
		s.unsure = map[string]bool{}
	}
	s.unsure[frameKey(chunk)] = true
}
