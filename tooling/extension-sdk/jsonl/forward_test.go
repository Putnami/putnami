package jsonl

import (
	"go.putnami.dev/protocol/features/spectest"

	"testing"
)

func TestMapSeverity(t *testing.T) {
	tests := []struct {
		severity string
		fallback string
		want     string
	}{
		{"ERROR", "info", "error"},
		{"error", "info", "error"},
		{"WARNING", "info", "warn"},
		{"WARN", "info", "warn"},
		{"warn", "info", "warn"},
		{"INFO", "debug", "info"},
		{"info", "debug", "info"},
		{"DEBUG", "info", "debug"},
		{"debug", "info", "debug"},
		{"UNKNOWN", "warn", "warn"},
		{"", "info", "info"},
	}

	for _, tt := range tests {
		got := MapSeverity(tt.severity, tt.fallback)
		if got != tt.want {
			t.Errorf("MapSeverity(%q, %q) = %q, want %q", tt.severity, tt.fallback, got, tt.want)
		}
	}
}

func TestForwardLine_PlainText(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "log-forwarding-fidelity", "a-plain-child-line-is-forwarded-as-text")
	e := New()
	output := captureStdout(t, func() {
		ForwardLine(e, "hello world", "info")
	})

	data := parseJSONL(t, output)
	if data["type"] != "log" {
		t.Errorf("type = %v, want log", data["type"])
	}
	if data["level"] != "info" {
		t.Errorf("level = %v, want info", data["level"])
	}
	if data["message"] != "hello world" {
		t.Errorf("message = %v, want %q", data["message"], "hello world")
	}
}

func TestForwardLine_StructuredJSON(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "log-forwarding-fidelity", "a-structured-child-line-keeps-its-severity-and-fields")
	e := New()
	output := captureStdout(t, func() {
		ForwardLine(e, `{"severity":"ERROR","message":"something failed","extra":"val"}`, "info")
	})

	data := parseJSONL(t, output)
	if data["level"] != "error" {
		t.Errorf("level = %v, want error", data["level"])
	}
	if data["message"] != "something failed" {
		t.Errorf("message = %v, want %q", data["message"], "something failed")
	}
	ctx := data["context"].(map[string]any)
	if ctx["extra"] != "val" {
		t.Errorf("context.extra = %v, want val", ctx["extra"])
	}
}

func TestForwardLine_StructuredJSON_WithError(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		ForwardLine(e, `{"severity":"ERROR","message":"fail","error":{"message":"detail"}}`, "info")
	})

	data := parseJSONL(t, output)
	if data["error"] == nil {
		t.Error("error field should be present")
	}
	errInfo := data["error"].(map[string]any)
	if errInfo["message"] != "detail" {
		t.Errorf("error.message = %v, want detail", errInfo["message"])
	}
}

func TestForwardLine_InvalidJSON(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "log-forwarding-fidelity", "an-unparseable-child-line-is-not-dropped")
	e := New()
	output := captureStdout(t, func() {
		ForwardLine(e, `{invalid json}`, "warn")
	})

	data := parseJSONL(t, output)
	if data["level"] != "warn" {
		t.Errorf("level = %v, want warn (fallback)", data["level"])
	}
}

func TestForwardLine_JSONWithoutSeverity(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		ForwardLine(e, `{"message":"no severity"}`, "info")
	})

	data := parseJSONL(t, output)
	// Should fall back to plain text since no severity+message pair
	if data["message"] != `{"message":"no severity"}` {
		t.Errorf("should treat as plain text, got message = %v", data["message"])
	}
}

func TestForwardLine_ExcludesReservedKeys(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "log-forwarding-fidelity", "reserved-protocol-keys-cannot-overwrite-the-envelope")
	e := New()
	output := captureStdout(t, func() {
		ForwardLine(e, `{"severity":"INFO","message":"msg","timestamp":"2024-01-01","extra":"yes"}`, "info")
	})

	data := parseJSONL(t, output)
	ctx := data["context"].(map[string]any)
	if _, ok := ctx["severity"]; ok {
		t.Error("context should not contain 'severity'")
	}
	if _, ok := ctx["message"]; ok {
		t.Error("context should not contain 'message'")
	}
	if _, ok := ctx["timestamp"]; ok {
		t.Error("context should not contain 'timestamp'")
	}
	if ctx["extra"] != "yes" {
		t.Errorf("context.extra = %v, want yes", ctx["extra"])
	}
}
