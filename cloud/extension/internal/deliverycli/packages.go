package deliverycli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	protocolcli "go.putnami.dev/protocol/cli"
)

// Session reporting packages (ADR 0135). Once an acknowledgement offers
// packages, the engine sends its sealed packages in put lines, each a batch,
// several at once, and closes the session with one final manifest line. The
// session reporter posts each put's packages as one bundle under its
// destination's route and posts the manifest there last.
const (
	// sessionPackagesRoute and sessionManifestRoute follow the route of a
	// destination: the hosted <ingest>/sessions or the local-sessions route
	// of the session.
	sessionPackagesRoute = "/packages"
	sessionManifestRoute = "/manifest"
	// sessionPackagesHeader names, on every answer of a receiver that stores
	// packages, the version of the package routes it serves, beside the
	// contract header of ADR 0131, which keeps its value.
	sessionPackagesHeader  = "X-Putnami-Session-Packages"
	sessionPackagesVersion = "1"
	// sessionPackagesRebuiltHeader names, beside sessionPackagesHeader, on
	// every answer of a receiver that also stores the task packages whose
	// header names rebuilt records (CLI ADR 0137), the version of that
	// support. sessionPackagesHeader keeps its value, so a reporter that
	// predates it still gets packages.
	sessionPackagesRebuiltHeader  = "X-Putnami-Session-Packages-Rebuilt"
	sessionPackagesRebuiltVersion = "1"
	// putDeadlinePerMiB is what a put's deadline adds to
	// sessionReporterRequestTimeout per MiB of its decoded bundle
	// (putDeadline).
	putDeadlinePerMiB = 2 * time.Second
	// sessionReporterPutsInFlight bounds the puts in flight at once. A put
	// line past it waits for one of them to complete.
	sessionReporterPutsInFlight = 8
	// reportingLineBytes bounds one stdin line: a put of the largest batch.
	// A frame line keeps its own bound, sessionReporterLineBytes.
	reportingLineBytes = max(sessionReporterLineBytes, protocolcli.SessionReportingMaxPackageLineBytes)
)

// ackCodePackagesMissing refuses a manifest that lists a package the receiver
// does not hold. Not retryable: the same manifest gets the same answer.
const ackCodePackagesMissing = "packages_missing"

// packagesRequested reports whether the engine named framing version 4 in the
// session reporter's environment: it then reads packages on every
// acknowledgement and sends put and manifest lines once one offers them.
func packagesRequested(env map[string]string) bool {
	version, err := strconv.Atoi(strings.TrimSpace(env[protocolcli.SessionReportingFramingEnv]))
	return err == nil && version >= protocolcli.SessionReportingPackagesVersion
}

// advertisesPackages reports whether header names version 1 of the package
// routes: its receiver stores packages. It reads nothing of the contract
// header, which advertises the cap of ADR 0131 alone.
func advertisesPackages(header http.Header) bool {
	return strings.TrimSpace(header.Get(sessionPackagesHeader)) == sessionPackagesVersion
}

// advertisesRebuiltPackages reports whether header advertises packages and
// names version 1 of rebuilt records: its receiver stores the task packages
// whose header names rebuilt records too (CLI ADR 0137).
func advertisesRebuiltPackages(header http.Header) bool {
	return advertisesPackages(header) && strings.TrimSpace(header.Get(sessionPackagesRebuiltHeader)) == sessionPackagesRebuiltVersion
}

// rebuiltPackagesRequested reports whether the engine named framing version
// 5 or later in the session reporter's environment: it then reads
// packagesRebuilt on acknowledgements, which an engine of version 4 refuses.
func rebuiltPackagesRequested(env map[string]string) bool {
	version, err := strconv.Atoi(strings.TrimSpace(env[protocolcli.SessionReportingFramingEnv]))
	return err == nil && version >= protocolcli.SessionReportingRebuiltPackagesVersion
}

// putDeadline is the deadline of a put whose decoded bundle is n bytes:
// sessionReporterRequestTimeout, plus putDeadlinePerMiB for each MiB or part
// of one. It stays under the engine's deadline for the same put, 5 s plus
// 2 s per MiB (ADR 0135), so a slow put answers retryable before the engine
// gives up on it.
func putDeadline(n int) time.Duration {
	mib := (n + 1<<20 - 1) >> 20
	return sessionReporterRequestTimeout + time.Duration(mib)*putDeadlinePerMiB
}

// offersPackages reports whether the latest decisive answer of url advertised
// packages.
func (c *frameCap) offersPackages(url string) bool {
	return c.answered(url) && c.packages
}

// offersRebuiltPackages reports whether the latest decisive answer of url
// advertised packages that name rebuilt records.
func (c *frameCap) offersRebuiltPackages(url string) bool {
	return c.answered(url) && c.rebuilt
}

// packageSender is the package half of a reporter. The session reporter has
// one; the log reporter has none, and its lane refuses every package line.
type packageSender interface {
	// bind binds the session of a package line, in stream order, as the first
	// frame does, and reports whether the line belongs to it.
	bind(sessionID string) bool
	// put sends one put's batch of packages and answers it by the put's
	// address. Several run at once.
	put(ctx context.Context, p *protocolcli.SessionReportingPut) protocolcli.SessionReportingResult
	// manifest posts the final manifest and answers it. It runs once no put
	// is in flight.
	manifest(ctx context.Context, m *protocolcli.SessionReportingManifest) protocolcli.SessionReportingResult
}

// reportingOutput is the protocol's stdout. Whichever goroutine answers, each
// answer is one whole JSON line, flushed at once. It keeps the first write
// error and writes nothing after it.
type reportingOutput struct {
	mu  sync.Mutex
	w   *bufio.Writer
	err error
	// failed is set once err is: failure reads it without waiting for a
	// write in progress.
	failed atomic.Bool
}

// write writes v as one line and returns the first write error of the stream.
func (o *reportingOutput) write(v any) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err == nil {
		o.err = writeReportingLine(o.w, v)
		o.failed.Store(o.err != nil)
	}
	return o.err
}

// failure is the first write error of the stream, or nil. It waits for no
// write in progress unless a write already failed.
func (o *reportingOutput) failure() error {
	if !o.failed.Load() {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// packageLane serves the put and manifest lines of one stream. A put leaves
// as soon as a slot is free, at most sessionReporterPutsInFlight at once, and
// its result is written when it completes, so results arrive in any order and
// the engine matches them by address. The loop that reads stdin waits only
// for a free slot, never for a put in flight. A manifest waits for every put
// in flight, so its request leaves after theirs completed and its result is
// written after theirs.
type packageLane struct {
	sender   packageSender
	output   *reportingOutput
	logw     io.Writer
	command  string
	slots    chan struct{}
	inflight sync.WaitGroup
}

func newPackageLane(sender packageSender, output *reportingOutput, logw io.Writer, command string) *packageLane {
	return &packageLane{sender: sender, output: output, logw: logw, command: command, slots: make(chan struct{}, sessionReporterPutsInFlight)}
}

// drain waits for every put in flight to complete and its result to be
// written.
func (l *packageLane) drain() {
	l.inflight.Wait()
}

// serve answers one line whose op is put or manifest. It returns an error for
// a line it cannot answer, which has no identity to echo, as a malformed
// frame, or once a write to stdout failed.
func (l *packageLane) serve(ctx context.Context, op string, line []byte) error {
	if op == protocolcli.SessionReportingOpManifest {
		m, err := protocolcli.ParseSessionReportingManifest(line)
		if err != nil {
			return l.malformed(op, err)
		}
		l.drain()
		accepted := m.Accept()
		switch {
		case l.sender == nil:
			return l.output.write(accepted.Refuse(ackCodeInvalidChunk, false))
		case !l.sender.bind(m.SessionID):
			return l.output.write(accepted.Refuse(ackCodeSessionMismatch, false))
		}
		return l.output.write(l.sender.manifest(ctx, m))
	}
	p, err := protocolcli.ParseSessionReportingPut(line)
	if err != nil {
		return l.malformed(op, err)
	}
	accepted := p.Accept()
	switch {
	case l.sender == nil:
		// The log reporter's route stores no packages, and the engine never
		// sends it one.
		return l.output.write(accepted.Refuse(ackCodeInvalidChunk, false))
	case !l.sender.bind(p.SessionID):
		return l.output.write(accepted.Refuse(ackCodeSessionMismatch, false))
	}
	l.slots <- struct{}{}
	l.inflight.Add(1)
	go func() {
		defer func() {
			<-l.slots
			l.inflight.Done()
		}()
		_ = l.output.write(l.sender.put(ctx, p))
	}()
	return l.output.failure()
}

// malformed reports a package line the protocol's parser refused. It waits
// for the puts in flight first, so no other diagnostic is written beside it.
func (l *packageLane) malformed(op string, err error) error {
	l.drain()
	fmt.Fprintf(l.logw, "%s %s: malformed %s line: %v\n", reporterBinary, l.command, op, err)
	return fmt.Errorf("malformed session reporting %s line", op)
}

// bind binds the session of a package line as post binds a frame's: the first
// line of the stream binds it, and a line of another session is refused.
func (s *sessionReporterSender) bind(sessionID string) bool {
	if s.sessionID == "" {
		s.sessionID = sessionID
	}
	return sessionID == s.sessionID
}

// put sends one put's packages as one request: POST <route>/packages, body
// their bundle (protocolcli.EncodeSessionReportingBundle), gzip-encoded once
// its destination advertised gzip. The receiver stores each package under the
// digest of its decoded bytes, answers 200 once it holds every package of the
// bundle, and refuses a bundle whose digests do not match, so an accepted put
// is durable acceptance of each of its packages whatever its coding. The
// result names the put's address. The put, a re-minted attempt included,
// ends by its deadline (putDeadline), which grows with the bundle's size; a
// put past it is a retryable receiver_unavailable.
func (s *sessionReporterSender) put(ctx context.Context, p *protocolcli.SessionReportingPut) protocolcli.SessionReportingResult {
	accepted := p.Accept()
	dest, encoder, code := s.packageDestination(p.SessionID)
	if code != "" {
		return accepted.Refuse(code, false)
	}
	bundle := protocolcli.EncodeSessionReportingBundle(p.Packages)
	ctx, cancel := context.WithTimeout(ctx, putDeadline(len(bundle)))
	defer cancel()
	payload, coding := encoder.body(dest.url, bundle)
	status, failure := s.packageExchange(ctx, s.putClient(), dest, sessionPackagesRoute, payload, func(req *http.Request) {
		req.Header.Set("Content-Type", "application/octet-stream")
		setContentEncoding(req, coding)
	})
	if failure != "" {
		return accepted.Refuse(failure, failure == ackCodeReceiverUnavailable)
	}
	if status == http.StatusOK || status == http.StatusCreated || status == http.StatusNoContent {
		return accepted
	}
	code, retryable := packageRefusal(status)
	return accepted.Refuse(code, retryable)
}

// manifest posts the final manifest, {"addresses": [...], "final": true}, to
// <route>/manifest. The receiver answers 200 once it holds every listed
// package, and 409 when it misses one.
func (s *sessionReporterSender) manifest(ctx context.Context, m *protocolcli.SessionReportingManifest) protocolcli.SessionReportingResult {
	accepted := m.Accept()
	dest, _, code := s.packageDestination(m.SessionID)
	if code != "" {
		return accepted.Refuse(code, false)
	}
	addresses := m.Addresses
	if addresses == nil {
		addresses = []string{}
	}
	payload, err := json.Marshal(struct {
		Addresses []string `json:"addresses"`
		Final     bool     `json:"final"`
	}{addresses, true})
	if err != nil {
		return accepted.Refuse(ackCodeInvalidChunk, false)
	}
	status, failure := s.packageExchange(ctx, s.client, dest, sessionManifestRoute, payload, func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
	})
	switch {
	case failure != "":
		return accepted.Refuse(failure, failure == ackCodeReceiverUnavailable)
	case status == http.StatusOK:
		return accepted
	case status == http.StatusConflict:
		return accepted.Refuse(ackCodePackagesMissing, false)
	}
	code, retryable := packageRefusal(status)
	return accepted.Refuse(code, retryable)
}

// packageRefusal maps a receiver's status that does not accept a put or a
// manifest onto a refusal, as post maps a frame's.
func packageRefusal(status int) (string, bool) {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ackCodeUnauthorized, false
	case status == http.StatusRequestEntityTooLarge:
		return ackCodeChunkTooLarge, false
	case status == http.StatusNotFound:
		// The route does not exist on this host: a retry cannot grow it.
		return ackCodeIngestUnavailable, false
	case platformShedRequest(status):
		return ackCodeReceiverUnavailable, true
	case status >= 400 && status < 500:
		// Includes a bundle with a package whose digest is not its address,
		// or a malformed bundle (400).
		return ackCodeInvalidChunk, false
	default:
		return ackCodeReceiverUnavailable, true
	}
}

// packageDestination resolves where a package line of sessionID goes, as a
// frame's (destination), and a copy of the encoder's latest advertisement,
// which compresses this package with a writer of its own. It returns the
// refusal code of a reporter that cannot send it.
func (s *sessionReporterSender) packageDestination(sessionID string) (sessionDestination, chunkEncoder, string) {
	s.packageMu.Lock()
	defer s.packageMu.Unlock()
	if s.cfg.inspectionGuardFailed {
		return sessionDestination{}, chunkEncoder{}, ackCodeInspectionGuard
	}
	dest, ok := s.destination(sessionID)
	if !ok {
		return sessionDestination{}, chunkEncoder{}, ackCodeNotConfigured
	}
	return dest, chunkEncoder{advertised: s.encoder.advertised}, ""
}

// packageExchange makes one POST of payload to path under dest's route and
// returns the receiver's status, or the refusal code of a request that got no
// answer, through client. A local route that answers 401 gets the request
// once more under a re-minted bearer, as post does for a frame: a 401 stored
// nothing.
func (s *sessionReporterSender) packageExchange(ctx context.Context, client *http.Client, dest sessionDestination, path string, payload []byte, header func(*http.Request)) (int, string) {
	status, failure := s.packageRequest(ctx, client, dest, path, payload, header)
	if failure == "" && status == http.StatusUnauthorized && dest.local != nil && dest.local.RefreshAuth != nil {
		fresh, reminted := s.remintPackageBearer(dest)
		if !reminted {
			return 0, ackCodeNotConfigured
		}
		dest.bearer = fresh
		status, failure = s.packageRequest(ctx, client, dest, path, payload, header)
	}
	return status, failure
}

// packageRequest sends one POST through sessionRequest and records its
// answer as the destination's latest, as a frame's: it sets whether the next
// put goes compressed, the cap and the packages offer.
func (s *sessionReporterSender) packageRequest(ctx context.Context, client *http.Client, dest sessionDestination, path string, payload []byte, header func(*http.Request)) (int, string) {
	target := dest
	target.url = dest.url + path
	req, err := sessionRequest(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return 0, ackCodeNotConfigured
	}
	header(req)
	req.ContentLength = int64(len(payload))
	resp, err := client.Do(req)
	if err != nil {
		// The receiver may never have seen the bytes, or the put passed its
		// deadline: another attempt may land.
		s.observePackageAnswer(dest.url, 0, nil)
		return 0, ackCodeReceiverUnavailable
	}
	defer resp.Body.Close()
	s.observePackageAnswer(dest.url, resp.StatusCode, resp.Header)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	return resp.StatusCode, ""
}

// putClient is the reporter's client without its timeout, on the same
// transport: a put is bounded by its context's deadline (putDeadline). A
// frame and the manifest keep the client's timeout.
func (s *sessionReporterSender) putClient() *http.Client {
	client := *s.client
	client.Timeout = 0
	return &client
}

// observePackageAnswer records the answer route gave a package request, or no
// answer when header is nil.
func (s *sessionReporterSender) observePackageAnswer(route string, status int, header http.Header) {
	s.packageMu.Lock()
	defer s.packageMu.Unlock()
	s.encoder.observe(route, header)
	s.frameCap.observe(route, status, header)
}

// remintPackageBearer is the bearer a put retries under after a 401 of the
// local route. Concurrent puts share one re-mint: a put whose bearer another
// put already replaced retries under the replacement. It reports false once
// the local mode ended, after a failed re-mint.
func (s *sessionReporterSender) remintPackageBearer(dest sessionDestination) (clicore.Bearer, bool) {
	s.packageMu.Lock()
	defer s.packageMu.Unlock()
	if s.local != dest.local {
		return clicore.Bearer{}, false
	}
	if dest.local.AuthToken != dest.bearer {
		return dest.local.AuthToken, true
	}
	return s.remint(dest.local)
}
