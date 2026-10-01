package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/logger"
	telemetry "go.putnami.dev/protocol/telemetry"
)

// Emitter persists sanitized CLI-usage log records into the workspace's own
// collector. Emit must never block the request path, allocate unboundedly, or
// surface an error: it is best-effort, exactly as the telemetry protocol's
// ExportContract requires (bounded queue, drop-on-error, periodic flush). An
// unauthenticated caller therefore gets an identical 202 whether the collector
// is healthy, wedged, or unconfigured.
type Emitter interface {
	Emit(resourceLogs []telemetry.ResourceLogs)
}

// noopEmitter drops everything. It is used when no collector endpoint is
// configured so the workload still runs and accepts (then silently drops)
// traffic in any environment.
type noopEmitter struct{}

func (noopEmitter) Emit([]telemetry.ResourceLogs) {}

// bearerTokenSource resolves the bearer sent on each collector push. It is the
// framework's OTLPConfig.BearerTokenSource shape; buildEmitter passes
// telemetry.NewGCPIDTokenSource so the re-emit authenticates with the
// workload's own GCP identity. Nil sends unauthenticated (tests, local runs).
type bearerTokenSource func(context.Context) (string, error)

// otlpExporter re-emits sanitized records as OTLP/JSON over HTTP to the
// workspace collector. It buffers into a bounded queue and flushes on a
// background ticker; a wedged or unreachable collector drops batches rather than
// growing memory or propagating an error. This is ordinary workload telemetry
// plumbing — the same OTLP/JSON wire every Putnami runtime pushes — with the
// endpoint configured by the workload itself (app-framework telemetry stays
// opt-in).
type otlpExporter struct {
	endpoint string
	client   *http.Client
	log      *logger.Logger
	maxQueue int
	timeout  time.Duration
	tokens   bearerTokenSource

	mu  sync.Mutex
	buf []telemetry.ResourceLogs

	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

func newOTLPExporter(endpoint string, flushInterval, timeout time.Duration, tokens bearerTokenSource, log *logger.Logger) *otlpExporter {
	contract := telemetry.DefaultContract().Export
	if flushInterval <= 0 {
		flushInterval = time.Duration(contract.DefaultFlushIntervalMS) * time.Millisecond
	}
	if timeout <= 0 {
		timeout = time.Duration(contract.DefaultTimeoutMS) * time.Millisecond
	}
	e := &otlpExporter{
		endpoint: strings.TrimRight(endpoint, "/"),
		client:   &http.Client{Timeout: timeout},
		log:      log,
		maxQueue: contract.MaxQueueRecords,
		timeout:  timeout,
		tokens:   tokens,
		done:     make(chan struct{}),
	}
	e.wg.Add(1)
	go e.flushLoop(flushInterval)
	return e
}

// Emit buffers the sanitized resource logs. It is non-blocking; when the queue
// is full the oldest entries are dropped (bounded memory under a wedged
// collector).
func (e *otlpExporter) Emit(resourceLogs []telemetry.ResourceLogs) {
	if len(resourceLogs) == 0 {
		return
	}
	e.mu.Lock()
	e.buf = append(e.buf, resourceLogs...)
	if len(e.buf) > e.maxQueue {
		e.buf = e.buf[len(e.buf)-e.maxQueue:]
	}
	e.mu.Unlock()
}

// Flush drains the queue and POSTs it as one OTLP/JSON logs request. Transport
// and non-2xx responses are dropped (best-effort): telemetry never affects the
// request path. Returns nil always for the same reason.
func (e *otlpExporter) Flush() error {
	e.mu.Lock()
	if len(e.buf) == 0 {
		e.mu.Unlock()
		return nil
	}
	batch := e.buf
	e.buf = nil
	e.mu.Unlock()

	if e.endpoint == "" {
		return nil // nowhere to send; drop (still best-effort)
	}

	body, err := telemetry.MarshalCanonical(telemetry.LogsRequest{ResourceLogs: batch})
	if err != nil {
		e.log.Warn("dropped telemetry batch: marshal failed: " + err.Error())
		return nil
	}

	ctx := context.Background()
	if e.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}
	if err := e.post(ctx, body); err != nil {
		e.log.Warn("dropped telemetry batch: " + err.Error())
	}
	return nil
}

func (e *otlpExporter) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint+telemetry.PathLogs, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", telemetry.ContentType)
	if e.tokens != nil {
		token, err := e.tokens(ctx)
		if err != nil {
			return err
		}
		if strings.TrimSpace(token) == "" {
			return errors.New("collector token source returned an empty token")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// The destination is operator-controlled workload configuration, not request
	// input. Private collector endpoints are an intentional deployment mode.
	resp, err := e.client.Do(req) //nolint:gosec // G704: trusted operational destination
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &collectorError{status: resp.StatusCode}
	}
	return nil
}

func (e *otlpExporter) flushLoop(interval time.Duration) {
	defer e.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			e.Flush() //nolint:errcheck // best-effort periodic flush
		case <-e.done:
			return
		}
	}
}

// Close stops the background flusher and performs a final flush, honoring the
// protocol's FinalFlushOnShutdown so a short-lived container does not silently
// drop its last batch.
func (e *otlpExporter) Close() error {
	e.closeOnce.Do(func() {
		close(e.done)
		e.wg.Wait()
	})
	return e.Flush()
}

type collectorError struct{ status int }

func (e *collectorError) Error() string {
	return "collector returned status " + http.StatusText(e.status)
}
