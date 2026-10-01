package output

import (
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/jobs"
)

func makeLogEvent(level, message string, ctx map[string]any) jobs.RawJobEvent {
	data := map[string]any{
		"level":   level,
		"message": message,
	}
	if ctx != nil {
		data["context"] = ctx
	}
	return jobs.RawJobEvent{
		Version: 1,
		Type:    jobs.EventTypeLog,
		Data:    data,
	}
}

func TestFormatLogEvent_HTTPLog(t *testing.T) {
	event := makeLogEvent("info", "GET /", map[string]any{
		"logger":    "http",
		"method":    "GET",
		"routePath": "/",
		"status":    float64(200),
		"duration":  float64(5),
	})

	line := FormatLogEvent(event, true, "")
	if !strings.Contains(line, "GET") {
		t.Errorf("expected GET in output: %q", line)
	}
	if !strings.Contains(line, "200") {
		t.Errorf("expected 200 in output: %q", line)
	}
	if !strings.Contains(line, "5ms") {
		t.Errorf("expected 5ms in output: %q", line)
	}
	if !strings.Contains(line, "INF") {
		t.Errorf("expected INF tag in no-color output: %q", line)
	}
}

func TestFormatLogEvent_HTTPLogColor(t *testing.T) {
	event := makeLogEvent("info", "POST /api/users", map[string]any{
		"method":   "POST",
		"status":   float64(201),
		"duration": float64(12),
	})

	line := FormatLogEvent(event, false, "")
	if !strings.Contains(line, "POST") {
		t.Errorf("expected POST in output: %q", line)
	}
	if !strings.Contains(line, "201") {
		t.Errorf("expected 201 in output: %q", line)
	}
	if !strings.Contains(line, "●") {
		t.Errorf("expected ● icon in color output: %q", line)
	}
}

func TestFormatLogEvent_GenericLog(t *testing.T) {
	event := makeLogEvent("info", "Server started on :3000", map[string]any{
		"logger": "app",
	})

	line := FormatLogEvent(event, true, "")
	if !strings.Contains(line, "[app]") {
		t.Errorf("expected [app] in output: %q", line)
	}
	if !strings.Contains(line, "Server started on :3000") {
		t.Errorf("expected message in output: %q", line)
	}
}

func TestFormatLogEvent_GenericLogNoLogger(t *testing.T) {
	event := makeLogEvent("warn", "Something happened", nil)

	line := FormatLogEvent(event, true, "")
	if !strings.Contains(line, "WRN") {
		t.Errorf("expected WRN tag in output: %q", line)
	}
	if !strings.Contains(line, "Something happened") {
		t.Errorf("expected message in output: %q", line)
	}
}

func TestFormatLogEvent_ErrorLevel(t *testing.T) {
	event := makeLogEvent("error", "Connection refused", map[string]any{
		"logger": "sql",
	})

	line := FormatLogEvent(event, true, "")
	if !strings.Contains(line, "ERR") {
		t.Errorf("expected ERR tag in output: %q", line)
	}

	lineColor := FormatLogEvent(event, false, "")
	if !strings.Contains(lineColor, "✖") {
		t.Errorf("expected ✖ icon in color output: %q", lineColor)
	}
}

func TestFormatLogEvent_GenericErrorWithDetails(t *testing.T) {
	event := makeLogEvent("error", "failed to bootstrap", map[string]any{
		"logger": "npm-registry",
	})
	event.Data["error"] = map[string]any{
		"message": "connect to registry PostgreSQL (DATABASE_DSN): db.connection: ping: connect: connection refused",
		"stack":   "registry.example.com/core/bootstrap.Bootstrap\n\t/src/registry/core/bootstrap/bootstrap.go:80",
	}

	line := FormatLogEvent(event, true, "serve(npm-registry)")
	if !strings.Contains(line, "failed to bootstrap") {
		t.Errorf("expected base message in output: %q", line)
	}
	if !strings.Contains(line, "connect to registry PostgreSQL") {
		t.Errorf("expected error detail in output: %q", line)
	}
	if !strings.Contains(line, "bootstrap.go:80") {
		t.Errorf("expected stack trace in output: %q", line)
	}

	lineColor := FormatLogEvent(event, false, "serve(npm-registry)")
	if !strings.Contains(lineColor, "connect to registry PostgreSQL") {
		t.Errorf("expected error detail in color output: %q", lineColor)
	}
}

func TestFormatLogEvent_DebugSkipped(t *testing.T) {
	event := makeLogEvent("debug", "Debug trace", nil)

	line := FormatLogEvent(event, true, "")
	if line != "" {
		t.Errorf("expected empty string for debug level, got: %q", line)
	}
}

func TestFormatLogEvent_EmptyMessage(t *testing.T) {
	event := makeLogEvent("info", "", nil)

	line := FormatLogEvent(event, true, "")
	if line != "" {
		t.Errorf("expected empty string for empty message, got: %q", line)
	}
}

func TestFormatLogEvent_WithLabel(t *testing.T) {
	event := makeLogEvent("info", "Server started on :3000", map[string]any{
		"logger": "app",
	})

	line := FormatLogEvent(event, true, "serve(putnami.dev)")
	if !strings.HasPrefix(line, "  serve(putnami.dev) ") {
		t.Errorf("expected label prefix, got: %q", line)
	}
	if !strings.Contains(line, "Server started on :3000") {
		t.Errorf("expected message in output: %q", line)
	}
}

func TestFormatLogDuration(t *testing.T) {
	tests := []struct {
		ms   float64
		want string
	}{
		{5, "5ms"},
		{150, "150ms"},
		{999, "999ms"},
		{1000, "1.0s"},
		{1500, "1.5s"},
		{12345, "12.3s"},
	}
	for _, tt := range tests {
		got := formatLogDuration(tt.ms)
		if got != tt.want {
			t.Errorf("formatLogDuration(%v) = %q, want %q", tt.ms, got, tt.want)
		}
	}
}

func TestFormatLogEvent_GenericLogWithDuration(t *testing.T) {
	event := makeLogEvent("info", "🔥 warmed up", map[string]any{
		"logger":   "putnami",
		"duration": float64(320),
	})

	line := FormatLogEvent(event, true, "serve(putnami.dev)")
	if !strings.Contains(line, "320ms") {
		t.Errorf("expected 320ms in output: %q", line)
	}
	if !strings.Contains(line, "🔥 warmed up") {
		t.Errorf("expected message in output: %q", line)
	}

	lineColor := FormatLogEvent(event, false, "serve(putnami.dev)")
	if !strings.Contains(lineColor, "320ms") {
		t.Errorf("expected 320ms in color output: %q", lineColor)
	}
}

func TestFormatLogEvent_GenericLogNoDuration(t *testing.T) {
	event := makeLogEvent("info", "⚡️ listening http://localhost:3000", map[string]any{
		"logger": "putnami",
	})

	line := FormatLogEvent(event, true, "")
	if strings.Contains(line, "ms") || strings.Contains(line, "s ") {
		t.Errorf("expected no duration in output: %q", line)
	}
}

func TestFormatLogEvent_HTTPStatus5xx(t *testing.T) {
	event := makeLogEvent("error", "GET /fail", map[string]any{
		"method": "GET",
		"status": float64(500),
	})

	line := FormatLogEvent(event, true, "")
	if !strings.Contains(line, "500") {
		t.Errorf("expected 500 in output: %q", line)
	}
}

func TestFormatLogEvent_HTTPErrorWithDetails(t *testing.T) {
	event := makeLogEvent("error", "[GET] /auth/google: token exchange failed", map[string]any{
		"method":    "GET",
		"routePath": "/auth/google",
		"status":    float64(500),
		"duration":  float64(6),
	})
	event.Data["error"] = map[string]any{
		"name":    "Error",
		"message": "token exchange failed",
		"stack":   "Error: token exchange failed\n    at handler (/src/auth.ts:42:11)",
	}

	line := FormatLogEvent(event, true, "serve(@putnami/auth-server)")
	if !strings.Contains(line, "500") {
		t.Errorf("expected 500 in output: %q", line)
	}
	if !strings.Contains(line, "token exchange failed") {
		t.Errorf("expected error message in output: %q", line)
	}
	if !strings.Contains(line, "at handler") {
		t.Errorf("expected stack trace in output: %q", line)
	}

	lineColor := FormatLogEvent(event, false, "serve(@putnami/auth-server)")
	if !strings.Contains(lineColor, "token exchange failed") {
		t.Errorf("expected error message in color output: %q", lineColor)
	}
	if !strings.Contains(lineColor, "at handler") {
		t.Errorf("expected stack trace in color output: %q", lineColor)
	}
}

func TestFormatLogEvent_HTTPErrorNoDetails(t *testing.T) {
	// 500 without error data should not crash or add extra lines
	event := makeLogEvent("error", "GET /fail", map[string]any{
		"method": "GET",
		"status": float64(500),
	})

	line := FormatLogEvent(event, true, "")
	if strings.Count(line, "\n") != 0 {
		t.Errorf("expected single line when no error data: %q", line)
	}
}

func TestFormatLogEvent_SlowDuration(t *testing.T) {
	event := makeLogEvent("info", "GET /slow", map[string]any{
		"method":   "GET",
		"status":   float64(200),
		"duration": float64(2500),
	})

	line := FormatLogEvent(event, true, "")
	if !strings.Contains(line, "2.5s") {
		t.Errorf("expected 2.5s in output: %q", line)
	}
}
