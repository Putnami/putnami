package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/protocol/telemetry/cliusage"
	"go.putnami.dev/tooling/cli/internal/hometest"
)

func TestResolvePrecedence(t *testing.T) {
	tests := []struct {
		name     string
		config   *Config
		env      map[string]string
		expected State
	}{
		{
			name:     "explicit config wins over every environment guard",
			config:   &Config{Enabled: boolPtr(true)},
			env:      map[string]string{"DO_NOT_TRACK": "1", "PUTNAMI_TELEMETRY": "off", "CI": "true"},
			expected: State{Enabled: true, Decision: DecisionConfig},
		},
		{
			name:     "explicit opt-out wins over force enable",
			config:   &Config{Enabled: boolPtr(false)},
			env:      map[string]string{"PUTNAMI_TELEMETRY": "on"},
			expected: State{Enabled: false, Decision: DecisionConfig},
		},
		{
			name:     "do not track wins within environment",
			env:      map[string]string{"DO_NOT_TRACK": "1", "PUTNAMI_TELEMETRY": "on"},
			expected: State{Enabled: false, Decision: DecisionEnv},
		},
		{
			name:     "environment force enable precedes CI",
			env:      map[string]string{"PUTNAMI_TELEMETRY": "on", "CI": "true"},
			expected: State{Enabled: true, Decision: DecisionEnv},
		},
		{
			name:     "CI disables when no higher rule applies",
			env:      map[string]string{"CI": ""},
			expected: State{Enabled: false, Decision: DecisionCI},
		},
		{
			name:     "absence defaults on",
			expected: State{Enabled: true, Decision: DecisionDefault},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				value, present := tt.env[key]
				return value, present
			}
			if got := resolveWithEnv(tt.config, lookup); got != tt.expected {
				t.Fatalf("resolveWithEnv() = %+v, want %+v", got, tt.expected)
			}
		})
	}
}

func TestPrepareRunNonInteractiveWaitsForPriorNotice(t *testing.T) {
	dir := hometest.Temp(t)
	t.Setenv("DO_NOT_TRACK", "0")
	t.Setenv("PUTNAMI_TELEMETRY", "on")

	client := NewClient()
	if client.PrepareRun(false) {
		t.Fatal("non-interactive run displayed a notice")
	}
	if client.CanCollect() {
		t.Fatal("non-interactive run collected before the notice")
	}
	client.TrackSessionEnd(SessionEnd{ExitCode: 0, DurationMS: 1, Interactive: false})
	if len(client.events) != 0 {
		t.Fatalf("non-interactive pre-notice events = %#v, want none", client.events)
	}
	if _, err := os.Stat(filepath.Join(dir, configFileName)); !os.IsNotExist(err) {
		t.Fatalf("non-interactive pre-notice wrote config: %v", err)
	}

	if err := writeConfig(filepath.Join(dir, configFileName), &Config{
		NoticeShownAt: "2026-07-24T00:00:00Z",
	}); err != nil {
		t.Fatalf("write noticed config: %v", err)
	}
	client = NewClient()
	if client.PrepareRun(false) {
		t.Fatal("post-notice automation displayed a notice")
	}
	if !client.CanCollect() || !client.CanDrainPrevious() {
		t.Fatal("post-notice automation was not eligible")
	}
	client.TrackSessionEnd(SessionEnd{ExitCode: 0, DurationMS: 1, Interactive: false})
	if len(client.events) != 1 {
		t.Fatalf("post-notice events = %d, want 1", len(client.events))
	}
	if interactive, _ := client.events[0].Data[cliusage.AttrInteractive].(bool); interactive {
		t.Fatalf("post-notice automation event interactive = %v, want false", interactive)
	}
}

func TestPrepareRunShowsOneNoticeDropsLegacyBufferAndDefersDrain(t *testing.T) {
	dir := hometest.Temp(t)
	t.Setenv("DO_NOT_TRACK", "0")
	t.Setenv("PUTNAMI_TELEMETRY", "on")
	bufferPath := filepath.Join(dir, bufferFileName)
	if err := os.WriteFile(bufferPath, []byte(`{"name":"session:end","timestamp":"2026-01-01T00:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write legacy buffer: %v", err)
	}

	client := NewClient()
	if !client.PrepareRun(true) {
		t.Fatal("first interactive run did not request the notice")
	}
	if !client.CanCollect() {
		t.Fatal("notice run did not collect locally")
	}
	if client.CanDrainPrevious() {
		t.Fatal("notice run was allowed to drain")
	}
	if _, err := os.Stat(bufferPath); !os.IsNotExist(err) {
		t.Fatalf("legacy buffer survived notice migration: %v", err)
	}
	if cfg := Status(); cfg.NoticeShownAt == "" {
		t.Fatal("notice timestamp was not persisted")
	}

	client.TrackSessionEnd(SessionEnd{ExitCode: 0, DurationMS: 1, Interactive: true})
	if err := client.Flush(); err != nil {
		t.Fatalf("flush notice run: %v", err)
	}

	next := NewClient()
	if next.PrepareRun(true) {
		t.Fatal("notice was requested more than once")
	}
	if !next.CanDrainPrevious() {
		t.Fatal("run after the notice was not allowed to drain")
	}
}

func TestClientDisabled(t *testing.T) {
	c := &Client{config: &Config{Enabled: boolPtr(false)}}
	if c.IsEnabled() {
		t.Fatal("expected disabled")
	}

	// Track should be no-op
	c.TrackSessionStart(SessionStart{})
	if len(c.events) != 0 {
		t.Fatal("expected no events when disabled")
	}

	// Flush should be no-op
	if err := c.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func TestClientEnabled(t *testing.T) {
	dir := t.TempDir()
	bufferPath := filepath.Join(dir, "buffer.jsonl")
	configPath := filepath.Join(dir, "config.json")
	if err := writeConfig(configPath, &Config{Enabled: boolPtr(true), DeviceID: "test"}); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c := &Client{
		config:     &Config{Enabled: boolPtr(true), DeviceID: "test"},
		configPath: configPath,
		bufferPath: bufferPath,
	}

	if !c.IsEnabled() {
		t.Fatal("expected enabled")
	}

	c.TrackSessionStart(SessionStart{Commands: []string{"build"}, Projects: 5, Jobs: 2})
	c.TrackSessionEnd(SessionEnd{DurationMS: 42})

	if len(c.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(c.events))
	}

	// Flush
	if err := c.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Events should be cleared
	if len(c.events) != 0 {
		t.Fatalf("expected 0 events after flush, got %d", len(c.events))
	}

	// Buffer file should exist with 2 lines
	data, err := os.ReadFile(bufferPath)
	if err != nil {
		t.Fatalf("read buffer: %v", err)
	}

	lines := bytes.Split(data, []byte("\n"))
	nonEmpty := 0
	for _, l := range lines {
		if len(l) > 0 {
			nonEmpty++
		}
	}
	if nonEmpty != 2 {
		t.Fatalf("expected 2 lines in buffer, got %d", nonEmpty)
	}

	// Parse first line
	var ev Event
	if err := json.Unmarshal(lines[0], &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Name != cliusage.EventSessionStart {
		t.Errorf("expected %q, got %q", cliusage.EventSessionStart, ev.Name)
	}
	if ev.Data[cliusage.AttrProjects] != float64(5) {
		t.Errorf("expected projects=5, got %v", ev.Data[cliusage.AttrProjects])
	}
}

func TestTrackStampsCurrentMonthlyDeviceID(t *testing.T) {
	dir := hometest.Temp(t)
	deviceID := "11111111111111111111111111111111"
	if err := writeConfig(filepath.Join(dir, configFileName), &Config{
		Enabled:       boolPtr(true),
		DeviceID:      deviceID,
		DeviceIDMonth: time.Now().UTC().Format("2006-01"),
	}); err != nil {
		t.Fatalf("write telemetry config: %v", err)
	}

	client := NewClient()
	client.TrackSessionEnd(SessionEnd{ExitCode: 0, DurationMS: 1, Interactive: false})
	if got := len(client.events); got != 1 {
		t.Fatalf("tracked events = %d, want 1", got)
	}
	if got := client.events[0].DeviceID; got != deviceID {
		t.Errorf("event device ID = %q, want %q", got, deviceID)
	}
}

func TestFlushBoundsBufferFile(t *testing.T) {
	dir := t.TempDir()
	bufferPath := filepath.Join(dir, "buffer.jsonl")
	configPath := filepath.Join(dir, "config.json")
	if err := writeConfig(configPath, &Config{Enabled: boolPtr(true), DeviceID: "test"}); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c := &Client{
		config:     &Config{Enabled: boolPtr(true), DeviceID: "test"},
		configPath: configPath,
		bufferPath: bufferPath,
	}

	// Record well past the bound across several flushes and confirm the file
	// never retains more than maxBufferEvents records.
	const total = maxBufferEvents + 250
	for i := 0; i < total; i++ {
		c.TrackSessionEnd(SessionEnd{DurationMS: int64(i)})
		if i%100 == 0 {
			if err := c.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
		}
	}
	if err := c.Flush(); err != nil {
		t.Fatalf("final flush: %v", err)
	}

	data, err := os.ReadFile(bufferPath)
	if err != nil {
		t.Fatalf("read buffer: %v", err)
	}
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) != maxBufferEvents {
		t.Fatalf("buffer holds %d records, want bound of %d", len(lines), maxBufferEvents)
	}

	// The retained records must be the most recent ones (oldest dropped).
	var first Event
	if err := json.Unmarshal(lines[0], &first); err != nil {
		t.Fatalf("unmarshal first: %v", err)
	}
	if first.Data[cliusage.AttrDuration] != float64(total-maxBufferEvents) {
		t.Errorf("first retained record duration=%v, want %d (oldest dropped)", first.Data[cliusage.AttrDuration], total-maxBufferEvents)
	}

	var last Event
	if err := json.Unmarshal(lines[len(lines)-1], &last); err != nil {
		t.Fatalf("unmarshal last: %v", err)
	}
	if last.Data[cliusage.AttrDuration] != float64(total-1) {
		t.Errorf("last retained record duration=%v, want %d (newest kept)", last.Data[cliusage.AttrDuration], total-1)
	}
}

func TestConfigReadWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg := &Config{
		Enabled:       boolPtr(true),
		NoticeShownAt: "2026-01-01T00:00:00Z",
		DeviceID:      "abc123",
		DeviceIDMonth: "2026-01",
	}

	if err := writeConfig(path, cfg); err != nil {
		t.Fatalf("write: %v", err)
	}

	read := readConfig(path)
	if read.Enabled == nil || !*read.Enabled {
		t.Error("expected enabled")
	}
	if read.NoticeShownAt != "2026-01-01T00:00:00Z" {
		t.Errorf("expected notice time, got %q", read.NoticeShownAt)
	}
	if read.DeviceID != "abc123" {
		t.Errorf("expected device ID, got %q", read.DeviceID)
	}
	if read.DeviceIDMonth != "2026-01" {
		t.Errorf("expected device ID month, got %q", read.DeviceIDMonth)
	}
}

func TestDeviceIDRotatesAtMonthBoundary(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	c := &Client{
		config: &Config{
			Enabled:       boolPtr(true),
			DeviceID:      "january-id",
			DeviceIDMonth: "2026-01",
		},
		configPath: configPath,
	}

	if got := c.deviceIDAt(time.Date(2026, time.January, 31, 23, 59, 59, 0, time.UTC)); got != "january-id" {
		t.Fatalf("device ID before month boundary = %q, want %q", got, "january-id")
	}

	februaryID := c.deviceIDAt(time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC))
	if februaryID == "" || februaryID == "january-id" {
		t.Fatalf("device ID after month boundary = %q, want a new ID", februaryID)
	}
	if got := c.config.DeviceIDMonth; got != "2026-02" {
		t.Errorf("device ID month = %q, want %q", got, "2026-02")
	}
	if got := c.deviceIDAt(time.Date(2026, time.February, 28, 23, 59, 59, 0, time.UTC)); got != februaryID {
		t.Errorf("device ID later in February = %q, want %q", got, februaryID)
	}

	persisted := readConfig(configPath)
	if persisted.DeviceID == "january-id" {
		t.Error("persisted config re-emitted the January device ID")
	}
	if persisted.DeviceID != februaryID || persisted.DeviceIDMonth != "2026-02" {
		t.Errorf("persisted config = %+v, want February ID and month", persisted)
	}
}

func TestDeviceIDRotatesLegacyConfigOnFirstAccess(t *testing.T) {
	dir := hometest.Temp(t)
	configPath := filepath.Join(dir, configFileName)
	if err := writeConfig(configPath, &Config{Enabled: boolPtr(true), DeviceID: "legacy-id"}); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	c := NewClient()
	got := c.deviceIDAt(time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC))
	if got == "" || got == "legacy-id" {
		t.Fatalf("legacy device ID was re-emitted: %q", got)
	}
	if c.config.DeviceIDMonth != "2026-02" {
		t.Errorf("device ID month = %q, want %q", c.config.DeviceIDMonth, "2026-02")
	}

	persisted := readConfig(configPath)
	if persisted.DeviceID == "legacy-id" || persisted.DeviceIDMonth != "2026-02" {
		t.Errorf("persisted legacy config = %+v, want rotated February ID", persisted)
	}
}

func TestDisableRemovesDeviceIDBufferAndHandoffTemporary(t *testing.T) {
	dir := hometest.Temp(t)
	configPath := filepath.Join(dir, configFileName)
	bufferPath := filepath.Join(dir, bufferFileName)
	if err := writeConfig(configPath, &Config{
		Enabled:       boolPtr(true),
		DeviceID:      "device-id",
		DeviceIDMonth: "2026-01",
	}); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(bufferPath, []byte("buffered event\n"), 0o644); err != nil {
		t.Fatalf("write buffer: %v", err)
	}
	if err := os.WriteFile(handoffTempPath(bufferPath), []byte("handoff copy\n"), 0o644); err != nil {
		t.Fatalf("write handoff temporary: %v", err)
	}

	if err := Disable(); err != nil {
		t.Fatalf("disable: %v", err)
	}

	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(configBytes, &raw); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if _, ok := raw["deviceId"]; ok {
		t.Error("disabled config retains deviceId")
	}
	if _, ok := raw["deviceIdMonth"]; ok {
		t.Error("disabled config retains deviceIdMonth")
	}
	if _, err := os.Stat(bufferPath); !os.IsNotExist(err) {
		t.Fatalf("buffer still exists after disable: %v", err)
	}
	if _, err := os.Stat(handoffTempPath(bufferPath)); !os.IsNotExist(err) {
		t.Fatalf("handoff temporary still exists after disable: %v", err)
	}
}

func TestDeviceIDDoesNotRestoreDisabledConsent(t *testing.T) {
	dir := hometest.Temp(t)
	configPath := filepath.Join(dir, configFileName)
	if err := writeConfig(configPath, &Config{
		Enabled:       boolPtr(true),
		DeviceID:      "old-device-id",
		DeviceIDMonth: "2000-01",
	}); err != nil {
		t.Fatalf("write enabled config: %v", err)
	}

	staleClient := NewClient()
	if err := Disable(); err != nil {
		t.Fatalf("disable telemetry: %v", err)
	}
	if got := staleClient.DeviceID(); got != "" {
		t.Fatalf("stale client device ID = %q, want empty after opt-out", got)
	}

	cfg := readConfig(configPath)
	if cfg.Enabled == nil || *cfg.Enabled || cfg.DeviceID != "" || cfg.DeviceIDMonth != "" {
		t.Fatalf("opt-out config was restored by stale device client: %+v", cfg)
	}
}

func TestEnableStampsCurrentDeviceIDMonth(t *testing.T) {
	hometest.Temp(t)

	if err := Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}

	cfg := Status()
	if cfg.DeviceID != "" || cfg.DeviceIDMonth != "" {
		t.Fatalf("enabled config eagerly created a device ID: %+v", cfg)
	}
}

func TestSessionEventVocabularyGuard(t *testing.T) {
	c := &Client{config: &Config{Enabled: boolPtr(true)}}
	c.TrackSessionStart(SessionStart{
		Commands:    []string{"build", "test", "unregistered-command"},
		Projects:    2,
		Jobs:        3,
		Flags:       FlagPresence{Impacted: true, Coverage: true, Output: true, NoCache: true, Projects: true, Watch: true},
		Interactive: true,
	})
	for _, exitCode := range []int{0, 1, 2, 3, 4, 130} {
		c.TrackSessionEnd(SessionEnd{ExitCode: exitCode, DurationMS: int64(exitCode), Interactive: true})
	}

	if got, want := len(c.events), 7; got != want {
		t.Fatalf("recorded events = %d, want %d", got, want)
	}
	for _, event := range c.events {
		if err := validateEventVocabulary(event); err != nil {
			t.Fatalf("emitted event %#v escaped vocabulary: %v", event, err)
		}
	}

	commands, ok := c.events[0].Data[cliusage.AttrCommands].([]string)
	if !ok {
		t.Fatalf("commands type = %T, want []string", c.events[0].Data[cliusage.AttrCommands])
	}
	if len(commands) != 2 || commands[0] != "build" || commands[1] != "test" {
		t.Fatalf("commands = %v, want [build test]", commands)
	}

	for _, event := range c.events[1:] {
		success, _ := event.Data[cliusage.AttrSuccess].(bool)
		_, hasCategory := event.Data[cliusage.AttrErrorCategory]
		if success && hasCategory {
			t.Fatalf("successful session:end has error category: %#v", event.Data)
		}
		if !success && !hasCategory {
			t.Fatalf("failed session:end lacks error category: %#v", event.Data)
		}
	}
}

func TestSessionEventVocabularyGuardRejectsUnapprovedValues(t *testing.T) {
	tests := []struct {
		name  string
		event Event
	}{
		{
			name:  "unknown attribute",
			event: Event{Name: cliusage.EventSessionStart, Data: map[string]any{"path": "/private/workspace"}},
		},
		{
			name:  "unknown command",
			event: Event{Name: cliusage.EventSessionStart, Data: map[string]any{cliusage.AttrCommands: []string{"private-command"}}},
		},
		{
			name:  "unknown error category",
			event: Event{Name: cliusage.EventSessionEnd, Data: map[string]any{cliusage.AttrErrorCategory: "network timeout"}},
		},
		{
			name: "successful session with error category",
			event: Event{Name: cliusage.EventSessionEnd, Data: map[string]any{
				cliusage.AttrSuccess:       true,
				cliusage.AttrDuration:      int64(1),
				cliusage.AttrInteractive:   false,
				cliusage.AttrErrorCategory: cliusage.ErrorCategoryFailure,
			}},
		},
		{
			name: "failed session without error category",
			event: Event{Name: cliusage.EventSessionEnd, Data: map[string]any{
				cliusage.AttrSuccess:     false,
				cliusage.AttrDuration:    int64(1),
				cliusage.AttrInteractive: false,
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateEventVocabulary(tt.event); err == nil {
				t.Fatalf("validateEventVocabulary(%#v) succeeded for out-of-vocabulary data", tt.event)
			}
		})
	}
}

func TestConfigMissing(t *testing.T) {
	cfg := readConfig("/nonexistent/path")
	if cfg.Enabled != nil {
		t.Error("missing config should leave the explicit state unset")
	}
}

func TestGenerateDeviceID(t *testing.T) {
	id := generateDeviceID()
	if id == "" {
		t.Fatal("generateDeviceID returned empty string")
	}
	// Must be valid hex characters only
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("generateDeviceID returned non-hex character %q in %q", string(c), id)
		}
	}
	// Two calls should return different values (time-based)
	id2 := generateDeviceID()
	if id == id2 {
		t.Error("two consecutive generateDeviceID calls returned the same value")
	}
}

func TestShowWithBufferedEvents(t *testing.T) {
	dir := t.TempDir()
	bufferPath := filepath.Join(dir, bufferFileName)

	// Write JSONL events to the buffer file
	events := []Event{
		{Name: "cmd:build", Timestamp: "2026-01-01T00:00:00Z", Data: map[string]any{"count": float64(3)}},
		{Name: "cmd:test", Timestamp: "2026-01-01T00:01:00Z", Data: nil},
	}

	f, err := os.Create(bufferPath)
	if err != nil {
		t.Fatalf("create buffer: %v", err)
	}
	enc := json.NewEncoder(f)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	f.Close()

	// Override HOME so Show() reads from our temp dir
	hometest.Set(t, dir)

	got, err := Show()
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Show returned %d events, want 2", len(got))
	}
	if got[0].Name != "cmd:build" {
		t.Errorf("event[0].Name = %q, want %q", got[0].Name, "cmd:build")
	}
	if got[0].Data["count"] != float64(3) {
		t.Errorf("event[0].Data[count] = %v, want 3", got[0].Data["count"])
	}
	if got[1].Name != "cmd:test" {
		t.Errorf("event[1].Name = %q, want %q", got[1].Name, "cmd:test")
	}
}

func TestShowNoBufferFile(t *testing.T) {
	hometest.Temp(t)

	got, err := Show()
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if got != nil {
		t.Fatalf("Show returned %d events, want nil", len(got))
	}
}

func TestShowSkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	bufferPath := filepath.Join(dir, bufferFileName)

	// Write a mix of valid and invalid JSONL
	content := `{"name":"good","timestamp":"2026-01-01T00:00:00Z"}
not-valid-json
{"name":"also-good","timestamp":"2026-01-02T00:00:00Z"}
`
	if err := os.WriteFile(bufferPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write buffer: %v", err)
	}

	hometest.Set(t, dir)

	got, err := Show()
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Show returned %d events, want 2 (malformed line skipped)", len(got))
	}
	if got[0].Name != "good" {
		t.Errorf("event[0].Name = %q, want %q", got[0].Name, "good")
	}
	if got[1].Name != "also-good" {
		t.Errorf("event[1].Name = %q, want %q", got[1].Name, "also-good")
	}
}

func TestNewClient(t *testing.T) {
	dir := hometest.Temp(t)
	t.Setenv("DO_NOT_TRACK", "0")
	t.Setenv("PUTNAMI_TELEMETRY", "on")

	// No config file — telemetry defaults on until an explicit opt-out.
	c := NewClient()
	if !c.IsEnabled() {
		t.Error("expected enabled when no config file exists")
	}

	// Write an enabled config and create a new client
	cfg := &Config{Enabled: boolPtr(true), DeviceID: "dev123"}
	configPath := filepath.Join(dir, configFileName)
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c = NewClient()
	if !c.IsEnabled() {
		t.Error("expected enabled after writing config")
	}
	if c.config.DeviceID != "dev123" {
		t.Errorf("DeviceID = %q, want %q", c.config.DeviceID, "dev123")
	}
}

func TestStatus(t *testing.T) {
	dir := hometest.Temp(t)

	// No config — explicit state remains unset.
	cfg := Status()
	if cfg.Enabled != nil {
		t.Error("expected unset config when no config exists")
	}

	// Write config
	if err := writeConfig(filepath.Join(dir, configFileName), &Config{
		Enabled:       boolPtr(true),
		NoticeShownAt: "2026-03-08T00:00:00Z",
		DeviceID:      "status-test",
	}); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg = Status()
	if cfg.Enabled == nil || !*cfg.Enabled {
		t.Error("expected enabled")
	}
	if cfg.DeviceID != "status-test" {
		t.Errorf("DeviceID = %q, want %q", cfg.DeviceID, "status-test")
	}
	if cfg.NoticeShownAt != "2026-03-08T00:00:00Z" {
		t.Errorf("NoticeShownAt = %q, want %q", cfg.NoticeShownAt, "2026-03-08T00:00:00Z")
	}
}
