package configcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/hometest"
)

// telemetryHome points HOME at a temp dir so telemetry config/buffer files are
// isolated from the developer's real home directory.
func telemetryHome(t *testing.T) string {
	t.Helper()
	home := hometest.Temp(t)
	return home
}

func TestTelemetryOn_EnablesAndPrints(t *testing.T) {
	home := telemetryHome(t)

	out, err := sharedtest.CaptureStdout(t, TelemetryOn)
	if err != nil {
		t.Fatalf("TelemetryOn: %v", err)
	}

	if !strings.Contains(out, "Telemetry enabled.") {
		t.Errorf("output = %q, want enabled message", out)
	}
	if !strings.Contains(out, "never code, paths, or personal details") {
		t.Errorf("output = %q, want data-minimization disclaimer", out)
	}
	if !strings.Contains(out, "https://putnami.dev/docs/concepts/cli-telemetry") {
		t.Errorf("output = %q, want public disclosure link", out)
	}

	// Config file should record an explicit opt-in without creating an ID before
	// an eligible run records an event.
	data, readErr := os.ReadFile(filepath.Join(home, ".putnami-telemetry.json"))
	if readErr != nil {
		t.Fatalf("read config: %v", readErr)
	}
	var cfg struct {
		Enabled  *bool  `json:"enabled"`
		DeviceID string `json:"deviceId"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.Enabled == nil || !*cfg.Enabled {
		t.Error("expected enabled=true in config")
	}
	if cfg.DeviceID != "" {
		t.Error("device ID should remain lazy before an eligible run")
	}
}

func TestTelemetryOff_DisablesAndRemovesBuffer(t *testing.T) {
	home := telemetryHome(t)

	// Seed an enabled config and a buffer file.
	if err := os.WriteFile(filepath.Join(home, ".putnami-telemetry.json"),
		[]byte(`{"enabled":true,"deviceId":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bufferPath := filepath.Join(home, ".putnami-telemetry-buffer.jsonl")
	if err := os.WriteFile(bufferPath, []byte(`{"name":"x","timestamp":"t"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sharedtest.CaptureStdout(t, TelemetryOff)
	if err != nil {
		t.Fatalf("TelemetryOff: %v", err)
	}
	if !strings.Contains(out, "Telemetry disabled") {
		t.Errorf("output = %q, want disabled message", out)
	}

	if _, err := os.Stat(bufferPath); !os.IsNotExist(err) {
		t.Errorf("buffer file should be removed, stat err = %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(home, ".putnami-telemetry.json"))
	if strings.Contains(string(data), `"enabled":true`) {
		t.Errorf("config should be disabled, got %q", string(data))
	}
}

func TestTelemetryStatus_Enabled(t *testing.T) {
	home := telemetryHome(t)
	if err := os.WriteFile(filepath.Join(home, ".putnami-telemetry.json"),
		[]byte(`{"enabled":true,"noticeShownAt":"2026-01-01T00:00:00Z","deviceIdMonth":"2026-01"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sharedtest.CaptureStdout(t, TelemetryStatus)
	if err != nil {
		t.Fatalf("TelemetryStatus: %v", err)
	}
	if !strings.Contains(out, "Telemetry: enabled") {
		t.Errorf("output = %q, want enabled status", out)
	}
	if !strings.Contains(out, "Effective rule: config") || !strings.Contains(out, "Device ID month: 2026-01") {
		t.Errorf("output = %q, want config rule and device ID month", out)
	}
}

func TestTelemetryStatus_EnvEnabled(t *testing.T) {
	telemetryHome(t) // no config file → environment can force-enable
	t.Setenv("DO_NOT_TRACK", "0")
	t.Setenv("PUTNAMI_TELEMETRY", "on")

	out, err := sharedtest.CaptureStdout(t, TelemetryStatus)
	if err != nil {
		t.Fatalf("TelemetryStatus: %v", err)
	}
	if !strings.Contains(out, "Telemetry: enabled") || !strings.Contains(out, "Effective rule: env") {
		t.Errorf("output = %q, want environment-enabled status", out)
	}
}

func TestTelemetryShow_NoEvents(t *testing.T) {
	telemetryHome(t)

	out, err := sharedtest.CaptureStdout(t, func() error { return TelemetryShow("") })
	if err != nil {
		t.Fatalf("TelemetryShow: %v", err)
	}
	if !strings.Contains(out, "No buffered telemetry events.") {
		t.Errorf("output = %q, want no-events message", out)
	}
}

func TestTelemetryShow_TextFormat(t *testing.T) {
	home := telemetryHome(t)
	buffer := `{"name":"build","timestamp":"2026-01-01T00:00:00Z","data":{"duration":42}}` + "\n"
	if err := os.WriteFile(filepath.Join(home, ".putnami-telemetry-buffer.jsonl"),
		[]byte(buffer), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sharedtest.CaptureStdout(t, func() error { return TelemetryShow("") })
	if err != nil {
		t.Fatalf("TelemetryShow: %v", err)
	}
	if !strings.Contains(out, "Buffered events: 1") {
		t.Errorf("output = %q, want event count", out)
	}
	if !strings.Contains(out, "build") {
		t.Errorf("output = %q, want event name", out)
	}
	if !strings.Contains(out, "duration: 42") {
		t.Errorf("output = %q, want event data", out)
	}
}

func TestTelemetryShow_JSONLFormat(t *testing.T) {
	home := telemetryHome(t)
	buffer := `{"name":"test","timestamp":"2026-01-01T00:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(home, ".putnami-telemetry-buffer.jsonl"),
		[]byte(buffer), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sharedtest.CaptureStdout(t, func() error { return TelemetryShow("jsonl") })
	if err != nil {
		t.Fatalf("TelemetryShow: %v", err)
	}
	line := strings.TrimSpace(out)
	var ev struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("parse JSONL output %q: %v", line, err)
	}
	if ev.Name != "test" {
		t.Errorf("event name = %q, want test", ev.Name)
	}
}
