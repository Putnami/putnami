// Package telemetry provides anonymous CLI usage telemetry.
//
// No PII is collected. Metrics are recorded to a local JSONL buffer file in the
// user's home directory. Interactive users receive a one-time notice before a
// previous buffer can be sent; non-interactive runs remain silent until that
// notice has been shown on the machine. The anonymous device ID rotates monthly.
// Buffered events retain the ID from their recorded UTC month, and each job
// session starts one fail-silent drain of the previous buffer: one POST and a
// two-second timeout after an atomic, at-most-once local handoff. That avoids
// replay after a fast process exit at the cost of bounded post-handoff loss.
// The buffer is bounded to maxBufferEvents; `telemetry off` removes it entirely;
// and `telemetry show` prints what has been recorded locally.
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/tooling/cli/internal/flock"
)

// configFileName is stored in the user's home config directory.
const configFileName = ".putnami-telemetry.json"

// bufferFileName stores recorded events locally.
const bufferFileName = ".putnami-telemetry-buffer.jsonl"

// handoffTempSuffix names the private, short-lived file used to atomically
// replace the buffer during a delivery handoff. It is never a delivery source:
// a stale copy means dispatch did not begin, because dispatch follows rename.
const handoffTempSuffix = ".handoff.tmp"

func handoffTempPath(bufferPath string) string { return bufferPath + handoffTempSuffix }

// bufferLockFileName serializes buffer reads, appends, handoffs, and opt-out
// removal across concurrent CLI processes.
const bufferLockFileName = ".putnami-telemetry-buffer.lock"

// consentLockFileName serializes opt-out with in-flight telemetry dispatches.
const consentLockFileName = ".putnami-telemetry-consent.lock"

// maxBufferEvents bounds the local buffer file. Once an append pushes the
// record count past this limit, the oldest records are dropped so the file
// cannot grow unbounded for long-lived opted-in installs.
const maxBufferEvents = 1000

// FirstRunNotice is emitted once, on stderr, before telemetry can send any
// buffered data from a machine. Keep it short: it appears in the normal CLI
// output path and is the public page's exact link target.
const FirstRunNotice = "putnami collects minimal usage data to improve the CLI — command names, counts, durations, and a monthly-rotating random ID; never code, paths, or personal details. Opt out anytime: putnami telemetry off · details & your rights: https://putnami.dev/docs/concepts/cli-telemetry"

// Decision identifies the rule that set telemetry's effective state.
type Decision string

const (
	DecisionConfig  Decision = "config"
	DecisionEnv     Decision = "env"
	DecisionCI      Decision = "CI guard"
	DecisionDefault Decision = "default"
)

// State is the resolved telemetry state, including the rule that won
// the precedence order. It is used by `telemetry status` instead of exposing
// implementation details of the tri-state config field.
type State struct {
	Enabled  bool
	Decision Decision
}

// Config holds the user's telemetry preferences. Enabled is deliberately a
// pointer: nil means the user has not made an explicit on/off choice, while
// false means `putnami telemetry off` was run. That distinction is what allows
// default-on telemetry without overriding an opt-out.
type Config struct {
	Enabled       *bool  `json:"enabled,omitempty"`
	NoticeShownAt string `json:"noticeShownAt,omitempty"` // ISO 8601 timestamp of the interactive notice
	DeviceID      string `json:"deviceId,omitempty"`      // anonymous random ID
	DeviceIDMonth string `json:"deviceIdMonth,omitempty"` // UTC YYYY-MM for the anonymous ID
}

// Event is a single anonymous telemetry event.
type Event struct {
	DeviceID  string         `json:"deviceId,omitempty"`
	Name      string         `json:"name"`
	Timestamp string         `json:"timestamp"`
	Data      map[string]any `json:"data,omitempty"`
}

// Client manages telemetry collection and persistence.
type Client struct {
	mu                   sync.Mutex
	config               *Config
	configPath           string
	bufferPath           string
	events               []Event
	runPrepared          bool
	runCollectionAllowed bool
	previousDrainAllowed bool
}

// NewClient creates a telemetry client. It reads the config from the user's
// home directory to determine if telemetry is enabled.
func NewClient() *Client {
	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		return &Client{config: &Config{}}
	}

	configPath := filepath.Join(homeDir, configFileName)
	bufferPath := filepath.Join(homeDir, bufferFileName)

	cfg := readConfig(configPath)
	return &Client{
		config:     cfg,
		configPath: configPath,
		bufferPath: bufferPath,
	}
}

// IsEnabled returns whether telemetry collection is active.
func (c *Client) IsEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshConfigLocked()
	return resolve(c.config).Enabled
}

// EffectiveState resolves config, environment, CI, and default precedence for
// the current process. Config always wins so an explicit opt-in or opt-out is
// predictable even in an inherited shell environment.
func EffectiveState() State {
	return resolve(Status())
}

func resolve(cfg *Config) State {
	return resolveWithEnv(cfg, os.LookupEnv)
}

func resolveWithEnv(cfg *Config, lookup func(string) (string, bool)) State {
	if cfg != nil && cfg.Enabled != nil {
		return State{Enabled: *cfg.Enabled, Decision: DecisionConfig}
	}
	if value, _ := lookup("DO_NOT_TRACK"); value == "1" {
		return State{Enabled: false, Decision: DecisionEnv}
	}
	telemetryValue, _ := lookup("PUTNAMI_TELEMETRY")
	switch strings.ToLower(strings.TrimSpace(telemetryValue)) {
	case "off", "0", "false":
		return State{Enabled: false, Decision: DecisionEnv}
	case "on", "1", "true":
		return State{Enabled: true, Decision: DecisionEnv}
	}
	if _, present := lookup("CI"); present {
		return State{Enabled: false, Decision: DecisionCI}
	}
	return State{Enabled: true, Decision: DecisionDefault}
}

// PrepareRun establishes the collection and drain gates for one job run. It
// returns true exactly when the caller must print FirstRunNotice. An interactive
// notice run records locally but cannot send; an automation run before a prior
// interactive notice does neither.
func (c *Client) PrepareRun(interactive bool) bool {
	c.mu.Lock()
	c.refreshConfigLocked()
	state := resolve(c.config)
	noticeShown := c.config != nil && c.config.NoticeShownAt != ""
	c.runPrepared = true
	c.runCollectionAllowed = state.Enabled && (interactive || noticeShown)
	c.previousDrainAllowed = state.Enabled && noticeShown
	c.mu.Unlock()

	if !state.Enabled || !interactive || noticeShown || c.bufferPath == "" {
		return false
	}

	// Serialize the migration with Disable and Drain. A pre-S3 buffer was
	// created under copy promising that it would never be transmitted, so remove
	// it before persisting the notice that makes delivery eligible on later runs.
	consentLock, err := acquireConsentLock(c.bufferPath)
	if err != nil {
		c.disablePreparedRun()
		return false
	}
	defer consentLock.Release() //nolint:errcheck // telemetry is fail-silent

	bufferLock, err := acquireBufferLock(c.bufferPath)
	if err != nil {
		c.disablePreparedRun()
		return false
	}
	defer bufferLock.Release() //nolint:errcheck // telemetry is fail-silent

	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshConfigLocked()
	state = resolve(c.config)
	if !state.Enabled {
		c.runCollectionAllowed = false
		c.previousDrainAllowed = false
		return false
	}
	if c.config.NoticeShownAt != "" {
		// Another interactive process displayed the notice while this process
		// waited for the lock. It remains a notice run here, so it must not drain.
		c.runCollectionAllowed = true
		c.previousDrainAllowed = false
		return false
	}

	_ = os.Remove(c.bufferPath)
	c.config.NoticeShownAt = time.Now().UTC().Format(time.RFC3339)
	if c.configPath == "" || writeConfig(c.configPath, c.config) != nil {
		c.runCollectionAllowed = false
		c.previousDrainAllowed = false
		return false
	}
	c.runCollectionAllowed = true
	c.previousDrainAllowed = false
	return true
}

func (c *Client) disablePreparedRun() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runCollectionAllowed = false
	c.previousDrainAllowed = false
}

// CanCollect reports whether this client may collect for the active run. It
// re-reads the config so a lifecycle hook's `telemetry off` still wins.
func (c *Client) CanCollect() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshConfigLocked()
	if !resolve(c.config).Enabled {
		return false
	}
	return !c.runPrepared || c.runCollectionAllowed
}

// CanDrainPrevious reports whether events buffered by an earlier job may be
// sent during this run. It deliberately remains false for the notice run.
func (c *Client) CanDrainPrevious() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshConfigLocked()
	return c.previousDrainAllowed && resolve(c.config).Enabled
}

func (c *Client) refreshConfigLocked() {
	// The consent file can be changed by another CLI process (including a
	// lifecycle hook that runs `putnami telemetry off`). Refresh it before
	// touching the buffer so a stale client cannot recreate opted-out data.
	if c.configPath != "" {
		c.config = readConfig(c.configPath)
	}
}

// DeviceID returns the anonymous device ID for the current UTC calendar month.
// IDs rotate monthly to bound cross-session linkability. It returns an empty
// string while telemetry is disabled.
func (c *Client) DeviceID() string {
	return c.deviceIDAtTimestamp(time.Now())
}

// deviceIDAtTimestamp returns the rotating ID that belongs to now. Its buffer
// lock serializes month rotation with Flush and Drain, so an event can retain
// the ID from the month in which it was recorded even if it drains later.
func (c *Client) deviceIDAtTimestamp(now time.Time) string {
	if c.bufferPath == "" {
		return c.deviceIDWithFreshConsentAt(now)
	}
	lock, err := acquireBufferLock(c.bufferPath)
	if err != nil {
		return ""
	}
	defer lock.Release() //nolint:errcheck // telemetry is best-effort
	return c.deviceIDWithFreshConsentAt(now)
}

func (c *Client) deviceIDWithFreshConsentAt(now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshConfigLocked()
	return c.deviceIDAt(now)
}

// deviceIDAt returns the current month's device ID while c.mu is held. Keeping
// the clock explicit lets tests exercise the calendar boundary without making
// production time mutable.
func (c *Client) deviceIDAt(now time.Time) string {
	if !resolve(c.config).Enabled {
		return ""
	}

	month := now.UTC().Format("2006-01")
	if c.config.DeviceID != "" && c.config.DeviceIDMonth == month {
		return c.config.DeviceID
	}

	c.config.DeviceID = generateDeviceID()
	c.config.DeviceIDMonth = month
	if c.configPath != "" {
		_ = writeConfig(c.configPath, c.config)
	}
	return c.config.DeviceID
}

// track records a curated anonymous event. No-op if telemetry is disabled or
// the event falls outside the closed telemetry vocabulary.
func (c *Client) track(event Event) {
	if !c.CanCollect() {
		return
	}
	if err := validateEventVocabulary(event); err != nil {
		return
	}
	now := time.Now().UTC()
	deviceID := c.deviceIDAtTimestamp(now)
	if deviceID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	event.DeviceID = deviceID
	event.Timestamp = now.Format(time.RFC3339)
	c.events = append(c.events, event)
}

// Flush appends collected events to the local buffer file. Despite the name
// (kept for API compatibility), there is no network flush: events are only
// persisted locally. After appending, the buffer is trimmed to its size bound.
func (c *Client) Flush() error {
	if !c.CanCollect() {
		return nil
	}
	lock, err := acquireBufferLock(c.bufferPath)
	if err != nil {
		return err
	}
	defer lock.Release() //nolint:errcheck // write errors are returned below
	if !c.CanCollect() {
		c.Discard()
		return nil
	}

	c.mu.Lock()
	events := make([]Event, len(c.events))
	copy(events, c.events)
	c.events = c.events[:0]
	c.mu.Unlock()

	if len(events) == 0 {
		return nil
	}

	f, err := os.OpenFile(c.bufferPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(f)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			f.Close() //nolint:errcheck // returning the encode error, which is primary
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}

	enforceBufferLimit(c.bufferPath)
	return nil
}

// Discard drops events that have not yet been persisted. It is used when a
// running command observes that consent was withdrawn after the client was
// created, so no stale event can recreate the removed buffer.
func (c *Client) Discard() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = nil
}

// enforceBufferLimit trims the buffer file to the most recent maxBufferEvents
// records so it cannot grow unbounded. It is best-effort: any read/write error
// leaves the existing file untouched rather than risking data loss.
func enforceBufferLimit(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= maxBufferEvents {
		return
	}

	lines = lines[len(lines)-maxBufferEvents:]
	var buf bytes.Buffer
	for _, line := range lines {
		buf.Write(line)
		buf.WriteByte('\n')
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func acquireBufferLock(bufferPath string) (*flock.Lock, error) {
	return flock.Acquire(filepath.Join(filepath.Dir(bufferPath), bufferLockFileName), true, false)
}

func acquireConsentLock(bufferPath string) (*flock.Lock, error) {
	return flock.Acquire(filepath.Join(filepath.Dir(bufferPath), consentLockFileName), true, false)
}

// acquireBufferLockContext waits for the shared buffer lock only while ctx is
// active. Flush and Disable intentionally use the unbounded variant above;
// background draining must instead stop with its owning CLI run.
func acquireBufferLockContext(ctx context.Context, bufferPath string) (*flock.Lock, error) {
	return acquireLockContext(ctx, filepath.Join(filepath.Dir(bufferPath), bufferLockFileName))
}

func acquireConsentLockContext(ctx context.Context, bufferPath string) (*flock.Lock, error) {
	return acquireLockContext(ctx, filepath.Join(filepath.Dir(bufferPath), consentLockFileName))
}

func acquireLockContext(ctx context.Context, lockPath string) (*flock.Lock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := flock.Acquire(lockPath, true, true)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, flock.ErrBusy) {
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Enable records an explicit opt-in. The first device ID remains lazy: it is
// created only when an eligible job actually records an event.
func Enable() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}

	path := filepath.Join(homeDir, configFileName)
	cfg := readConfig(path)
	cfg.Enabled = boolPtr(true)
	return writeConfig(path, cfg)
}

// Disable turns off telemetry and removes buffered data.
func Disable() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}

	bufferPath := filepath.Join(homeDir, bufferFileName)
	consentLock, err := acquireConsentLock(bufferPath)
	if err != nil {
		return fmt.Errorf("lock telemetry consent: %w", err)
	}
	defer consentLock.Release() //nolint:errcheck // disable errors are reported below

	lock, err := acquireBufferLock(bufferPath)
	if err != nil {
		return fmt.Errorf("lock telemetry buffer: %w", err)
	}
	defer lock.Release() //nolint:errcheck // disable errors are reported below

	configPath := filepath.Join(homeDir, configFileName)
	cfg := readConfig(configPath)
	cfg.Enabled = boolPtr(false)
	cfg.DeviceID = ""
	cfg.DeviceIDMonth = ""
	if err := writeConfig(configPath, cfg); err != nil {
		return err
	}

	// Remove every local raw-event copy, including a handoff temporary left by a
	// process that exited before its atomic rename completed.
	_ = os.Remove(bufferPath)
	_ = os.Remove(handoffTempPath(bufferPath))
	return nil
}

// Status returns the current telemetry configuration.
func Status() *Config {
	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		return &Config{}
	}
	return readConfig(filepath.Join(homeDir, configFileName))
}

// Show returns the events recorded in the local buffer file.
func Show() ([]Event, error) {
	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		return nil, nil
	}

	bufferPath := filepath.Join(homeDir, bufferFileName)
	data, err := os.ReadFile(bufferPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var events []Event
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}
	return events, nil
}

func readConfig(path string) *Config {
	data, err := os.ReadFile(path)
	if err != nil {
		return &Config{}
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return &Config{}
	}
	return &cfg
}

func boolPtr(value bool) *bool {
	return &value
}

func writeConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func generateDeviceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func isDeviceID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
