package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	otlp "go.putnami.dev/protocol/telemetry"
	"go.putnami.dev/protocol/telemetry/cliusage"
)

const (
	telemetryEndpointEnv     = "PUTNAMI_TELEMETRY_ENDPOINT"
	defaultTelemetryEndpoint = "https://telemetry.putnami.dev"
	telemetryRequestTimeout  = 2 * time.Second
)

var (
	telemetryVersionMu sync.RWMutex
	telemetryVersion   = "dev"
)

// SetCLIVersion supplies the ldflags-stamped CLI version used by Drain. It is
// called during CLI startup instead of importing the parent cli package here,
// which would create an import cycle.
func SetCLIVersion(version string) {
	telemetryVersionMu.Lock()
	defer telemetryVersionMu.Unlock()
	telemetryVersion = version
}

func currentCLIVersion() string {
	telemetryVersionMu.RLock()
	defer telemetryVersionMu.RUnlock()
	return telemetryVersion
}

// Drain best-effort POSTs the local telemetry buffer as one OTLP/JSON logs
// request. It is deliberately fail-silent: a failure before the local handoff
// leaves the buffer intact, while a timeout, transport failure, or non-2xx
// response after handoff drops that bounded batch and never changes the
// caller's result. This is intentionally at-most-once delivery: a process that
// exits after the receiver accepts a request cannot replay that batch later.
func Drain() {
	DrainContext(context.Background())
}

// DrainContext behaves like Drain but is canceled with its caller. The
// two-second limit applies to the complete background operation, including
// waiting for the local buffer lock, so a CLI run never outlives its drain.
func DrainContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, telemetryRequestTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return
	}

	endpoint := telemetryEndpoint()

	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		return
	}
	bufferPath := filepath.Join(homeDir, bufferFileName)
	consentLock, err := acquireConsentLockContext(ctx, bufferPath)
	if err != nil {
		return
	}
	// Hold the same cross-process lock used by Disable through local handoff and
	// request dispatch. Buffer locking remains short-lived, so a foreground Flush
	// can append while a slow collector request is in flight.
	defer consentLock.Release() //nolint:errcheck // background draining is best-effort

	data, deviceID, deviceIDMonth, ok := drainSnapshot(ctx, bufferPath)
	if !ok {
		return
	}

	events := decodeBufferedEvents(data)
	if len(events) == 0 {
		return
	}
	body, err := encodeBufferedLogs(events, deviceID, deviceIDMonth, currentCLIVersion())
	if err != nil {
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+otlp.PathLogs, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", otlp.ContentType)

	// Atomically hand off only the exact prefix that was encoded, immediately
	// before dispatch. If the process exits after the receiver accepts the
	// request but before it can read a response, a later run sees no old prefix
	// to replay. A concurrent Flush can only append a suffix under the same lock;
	// handoff preserves that suffix. The intentional tradeoff is bounded loss if
	// the handoff succeeds but the request cannot be delivered.
	if !handoffSentBufferContext(ctx, bufferPath, data) {
		return
	}

	clientHTTP := &http.Client{
		Timeout: telemetryRequestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// The destination is the fixed Putnami endpoint or an explicit local
	// process environment override; no remote/request data reaches this URL.
	response, err := clientHTTP.Do(req) //nolint:gosec // G704: trusted process configuration
	if err != nil {
		return
	}
	defer response.Body.Close() //nolint:errcheck // telemetry is best-effort
	// The local handoff is the delivery boundary. Any HTTP status remains
	// fail-silent and deliberately cannot restore the batch for replay.
}

func telemetryEndpoint() string {
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv(telemetryEndpointEnv)), "/")
	if endpoint != "" {
		return endpoint
	}
	return defaultTelemetryEndpoint
}

// drainSnapshot takes a stable buffer snapshot while holding the same lock as
// Flush and Disable. The caller holds the consent lock across snapshot, local
// handoff, and request dispatch, so an opt-out cannot complete between them.
func drainSnapshot(ctx context.Context, bufferPath string) ([]byte, string, string, bool) {
	lock, err := acquireBufferLockContext(ctx, bufferPath)
	if err != nil {
		return nil, "", "", false
	}
	defer lock.Release() //nolint:errcheck // a read-only snapshot is best-effort

	// A stale handoff temporary can only be from a pre-rename process exit: the
	// request dispatch happens after rename. Drop it before reading the retained
	// buffer so raw telemetry is not stranded and the original prefix can safely
	// be retried.
	_ = os.Remove(handoffTempPath(bufferPath))

	client := NewClient()
	if !client.IsEnabled() || client.config == nil || client.config.NoticeShownAt == "" {
		return nil, "", "", false
	}
	now := time.Now().UTC()
	deviceID := client.deviceIDWithFreshConsentAt(now)
	if deviceID == "" {
		return nil, "", "", false
	}
	data, err := os.ReadFile(bufferPath)
	if err != nil || len(data) == 0 {
		return nil, "", "", false
	}
	return data, deviceID, now.Format("2006-01"), true
}

func handoffSentBufferContext(ctx context.Context, bufferPath string, sent []byte) bool {
	lock, err := acquireBufferLockContext(ctx, bufferPath)
	if err != nil {
		return false
	}
	defer lock.Release() //nolint:errcheck // handoff is best-effort
	return handoffSentBufferLocked(bufferPath, sent)
}

// handoffSentBufferLocked atomically removes the sent prefix while preserving
// events appended after the snapshot. The caller holds the buffer lock.
func handoffSentBufferLocked(bufferPath string, sent []byte) bool {
	current, err := os.ReadFile(bufferPath)
	if err != nil || !bytes.HasPrefix(current, sent) {
		return false
	}

	// Rename makes the handoff visible all at once to readers. The lock keeps a
	// concurrent Flush from appending between the prefix check and replacement.
	tmp := handoffTempPath(bufferPath)
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return false
	}
	if err := os.WriteFile(tmp, current[len(sent):], 0o644); err != nil {
		return false
	}
	if err := os.Rename(tmp, bufferPath); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

func decodeBufferedEvents(data []byte) []Event {
	events := make([]Event, 0)
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		var event Event
		if err := decoder.Decode(&event); err != nil {
			continue
		}
		if event, ok := sanitizeBufferedEvent(event); ok {
			events = append(events, event)
		}
	}
	return events
}

// sanitizeBufferedEvent restores the concrete types lost to JSON decoding and
// applies the same closed vocabulary guard used before events enter the local
// buffer. This prevents a buffer produced by an older binary, or modified on
// disk, from bypassing the data-minimization boundary during export.
func sanitizeBufferedEvent(event Event) (Event, bool) {
	allowed, knownEvent := cliusage.AllowedAttributes[event.Name]
	if !knownEvent {
		return Event{}, false
	}
	data := make(map[string]any, len(allowed))
	for key := range allowed {
		value, present := event.Data[key]
		if !present {
			continue
		}
		normalized, ok := normalizeBufferedValue(key, value)
		if !ok {
			return Event{}, false
		}
		data[key] = normalized
	}
	event.Data = data
	if event.DeviceID != "" && !isDeviceID(event.DeviceID) {
		event.DeviceID = ""
	}
	if err := validateEventVocabulary(event); err != nil {
		return Event{}, false
	}
	return event, true
}

func normalizeBufferedValue(key string, value any) (any, bool) {
	switch key {
	case cliusage.AttrCommands:
		commands, ok := value.([]any)
		if !ok {
			return nil, false
		}
		out := make([]string, 0, len(commands))
		for _, command := range commands {
			value, ok := command.(string)
			if !ok {
				return nil, false
			}
			out = append(out, value)
		}
		return out, true
	case cliusage.AttrProjects, cliusage.AttrJobs:
		number, ok := value.(json.Number)
		if !ok {
			return nil, false
		}
		parsed, err := number.Int64()
		if err != nil || int64(int(parsed)) != parsed {
			return nil, false
		}
		return int(parsed), true
	case cliusage.AttrDuration:
		number, ok := value.(json.Number)
		if !ok {
			return nil, false
		}
		parsed, err := number.Int64()
		if err != nil {
			return nil, false
		}
		return parsed, true
	case cliusage.AttrSuccess, cliusage.AttrInteractive,
		cliusage.AttrFlagImpacted, cliusage.AttrFlagCoverage, cliusage.AttrFlagOutput,
		cliusage.AttrFlagNoCache, cliusage.AttrFlagProjects, cliusage.AttrFlagWatch:
		parsed, ok := value.(bool)
		return parsed, ok
	case cliusage.AttrErrorCategory:
		parsed, ok := value.(string)
		return parsed, ok
	default:
		return nil, false
	}
}
