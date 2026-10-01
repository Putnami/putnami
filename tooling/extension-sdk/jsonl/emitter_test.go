package jsonl

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout captures stdout during a function call and returns the output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	origStdout := os.Stdout
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = origStdout

	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()

	return buf.String()
}

func parseJSONL(t *testing.T, output string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("no JSONL output")
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &data); err != nil {
		t.Fatalf("parse JSONL: %v (line: %q)", err, lines[0])
	}
	return data
}

func TestNew(t *testing.T) {
	e := New()
	if e == nil {
		t.Fatal("New() returned nil")
	}
}

func TestEmitter_Meta(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.Meta("@putnami/go", "build")
	})

	data := parseJSONL(t, output)
	if data["type"] != "meta" {
		t.Errorf("type = %v, want meta", data["type"])
	}
	if data["v"] != float64(1) {
		t.Errorf("v = %v, want 1", data["v"])
	}
	if data["message"] != "Starting build" {
		t.Errorf("message = %v, want %q", data["message"], "Starting build")
	}
	d := data["data"].(map[string]any)
	if d["extension"] != "@putnami/go" {
		t.Errorf("extension = %v, want %q", d["extension"], "@putnami/go")
	}
	if d["job"] != "build" {
		t.Errorf("job = %v, want %q", d["job"], "build")
	}
}

func TestEmitter_Log(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.Log("info", "hello world")
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

func TestEmitter_LogEvent(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.LogEvent("error", "something failed",
			map[string]any{"key": "val"},
			map[string]any{"code": "E001"},
		)
	})

	data := parseJSONL(t, output)
	if data["type"] != "log" {
		t.Errorf("type = %v, want log", data["type"])
	}
	if data["level"] != "error" {
		t.Errorf("level = %v, want error", data["level"])
	}
	if data["context"] == nil {
		t.Error("context should be present")
	}
	if data["error"] == nil {
		t.Error("error should be present")
	}
}

func TestEmitter_LogEvent_NilMaps(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.LogEvent("info", "no extra", nil, nil)
	})

	data := parseJSONL(t, output)
	if _, ok := data["context"]; ok {
		t.Error("context should not be present when nil")
	}
	if _, ok := data["error"]; ok {
		t.Error("error should not be present when nil")
	}
}

func TestEmitter_Convenience(t *testing.T) {
	e := New()
	tests := []struct {
		name  string
		fn    func()
		level string
	}{
		{"Info", func() { e.Info("msg") }, "info"},
		{"Warn", func() { e.Warn("msg") }, "warn"},
		{"Error", func() { e.Error("msg") }, "error"},
		{"Debug", func() { e.Debug("msg") }, "debug"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := captureStdout(t, tt.fn)
			data := parseJSONL(t, output)
			if data["level"] != tt.level {
				t.Errorf("level = %v, want %v", data["level"], tt.level)
			}
		})
	}
}

func TestEmitter_PhaseStart(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.PhaseStart("compile")
	})

	data := parseJSONL(t, output)
	if data["type"] != "phase" {
		t.Errorf("type = %v, want phase", data["type"])
	}
	if data["name"] != "compile" {
		t.Errorf("name = %v, want compile", data["name"])
	}
	if data["action"] != "start" {
		t.Errorf("action = %v, want start", data["action"])
	}
}

func TestEmitter_PhaseEnd(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.PhaseEnd("compile", "success")
	})

	data := parseJSONL(t, output)
	if data["type"] != "phase" {
		t.Errorf("type = %v, want phase", data["type"])
	}
	if data["action"] != "end" {
		t.Errorf("action = %v, want end", data["action"])
	}
	if data["status"] != "success" {
		t.Errorf("status = %v, want success", data["status"])
	}
}

func TestEmitter_Progress(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.Progress(2, 5, "Compiling...")
	})

	data := parseJSONL(t, output)
	if data["type"] != "progress" {
		t.Errorf("type = %v, want progress", data["type"])
	}
	if data["current"] != float64(2) {
		t.Errorf("current = %v, want 2", data["current"])
	}
	if data["total"] != float64(5) {
		t.Errorf("total = %v, want 5", data["total"])
	}
	if data["message"] != "Compiling..." {
		t.Errorf("message = %v, want %q", data["message"], "Compiling...")
	}
}

func TestEmitter_Diagnostic_WithLocation(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.Diagnostic("error", "undefined var", "main.go", 42)
	})

	data := parseJSONL(t, output)
	if data["type"] != "diagnostic" {
		t.Errorf("type = %v, want diagnostic", data["type"])
	}
	if data["severity"] != "error" {
		t.Errorf("severity = %v, want error", data["severity"])
	}
	if data["message"] != "undefined var" {
		t.Errorf("message = %v, want %q", data["message"], "undefined var")
	}
	loc := data["location"].(map[string]any)
	if loc["file"] != "main.go" {
		t.Errorf("file = %v, want main.go", loc["file"])
	}
	if loc["line"] != float64(42) {
		t.Errorf("line = %v, want 42", loc["line"])
	}
}

func TestEmitter_Diagnostic_NoLocation(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.Diagnostic("warning", "something", "", 0)
	})

	data := parseJSONL(t, output)
	if _, ok := data["location"]; ok {
		t.Error("location should not be present when file is empty")
	}
}

func TestEmitter_DiagnosticWithCode(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.DiagnosticWithCode("error", "unused import", "main.go", 5, 3, "TS6133")
	})

	data := parseJSONL(t, output)
	if data["type"] != "diagnostic" {
		t.Errorf("type = %v, want diagnostic", data["type"])
	}
	if data["code"] != "TS6133" {
		t.Errorf("code = %v, want TS6133", data["code"])
	}
	loc := data["location"].(map[string]any)
	if loc["file"] != "main.go" {
		t.Errorf("file = %v, want main.go", loc["file"])
	}
	if loc["line"] != float64(5) {
		t.Errorf("line = %v, want 5", loc["line"])
	}
	if loc["column"] != float64(3) {
		t.Errorf("column = %v, want 3", loc["column"])
	}
}

func TestEmitter_DiagnosticWithCode_NoLocation(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.DiagnosticWithCode("warning", "msg", "", 0, 0, "W001")
	})

	data := parseJSONL(t, output)
	if _, ok := data["location"]; ok {
		t.Error("location should not be present when file is empty")
	}
	if data["code"] != "W001" {
		t.Errorf("code = %v, want W001", data["code"])
	}
}

func TestEmitter_Metric(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.Metric("binary-size", 4096, "bytes")
	})

	data := parseJSONL(t, output)
	if data["type"] != "metric" {
		t.Errorf("type = %v, want metric", data["type"])
	}
	if data["name"] != "binary-size" {
		t.Errorf("name = %v, want binary-size", data["name"])
	}
	if data["value"] != float64(4096) {
		t.Errorf("value = %v, want 4096", data["value"])
	}
	if data["unit"] != "bytes" {
		t.Errorf("unit = %v, want bytes", data["unit"])
	}
}

func TestEmitter_Artifact(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.Artifact("bin-1", "app", "file", "/out/app")
	})

	data := parseJSONL(t, output)
	if data["type"] != "artifact" {
		t.Errorf("type = %v, want artifact", data["type"])
	}
	if data["id"] != "bin-1" {
		t.Errorf("id = %v, want bin-1", data["id"])
	}
	if data["name"] != "app" {
		t.Errorf("name = %v, want app", data["name"])
	}
	if data["kind"] != "file" {
		t.Errorf("kind = %v, want file", data["kind"])
	}
	if data["path"] != "/out/app" {
		t.Errorf("path = %v, want /out/app", data["path"])
	}
}

func TestEmitter_ArtifactWithData(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.ArtifactWithData("img-1", "app", "docker", "gcr.io/app:v1", map[string]any{
			"size": 1024,
		})
	})

	data := parseJSONL(t, output)
	if data["type"] != "artifact" {
		t.Errorf("type = %v, want artifact", data["type"])
	}
	if data["size"] != float64(1024) {
		t.Errorf("size = %v, want 1024", data["size"])
	}
}

func TestEmitter_Summary(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.Summary("Build successful")
	})

	data := parseJSONL(t, output)
	if data["type"] != "summary" {
		t.Errorf("type = %v, want summary", data["type"])
	}
	if data["message"] != "Build successful" {
		t.Errorf("message = %v, want %q", data["message"], "Build successful")
	}
}

func TestEmitter_SummaryWithData(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.SummaryWithData("payload", map[string]any{"visibility": "always"})
	})

	data := parseJSONL(t, output)
	if data["type"] != "summary" {
		t.Errorf("type = %v, want summary", data["type"])
	}
	if data["message"] != "payload" {
		t.Errorf("message = %v, want payload", data["message"])
	}
	if data["visibility"] != "always" {
		t.Errorf("visibility = %v, want always", data["visibility"])
	}
}

func TestEmitter_Result_OK(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.Result("OK", map[string]any{"binaryPath": "/out/app"})
	})

	data := parseJSONL(t, output)
	if data["type"] != "result" {
		t.Errorf("type = %v, want result", data["type"])
	}
	if data["message"] != "Job OK" {
		t.Errorf("message = %v, want %q", data["message"], "Job OK")
	}
	d := data["data"].(map[string]any)
	if d["status"] != "OK" {
		t.Errorf("status = %v, want OK", d["status"])
	}
	if d["data"] == nil {
		t.Error("data.data should be present")
	}
}

func TestEmitter_Result_NilData(t *testing.T) {
	e := New()
	output := captureStdout(t, func() {
		e.Result("SKIP", nil)
	})

	data := parseJSONL(t, output)
	d := data["data"].(map[string]any)
	if d["status"] != "SKIP" {
		t.Errorf("status = %v, want SKIP", d["status"])
	}
	if _, ok := d["data"]; ok {
		t.Error("data.data should not be present when nil")
	}
}

func TestEmitter_AllEventsHaveTimestamp(t *testing.T) {
	e := New()

	events := []func(){
		func() { e.Meta("ext", "job") },
		func() { e.Log("info", "msg") },
		func() { e.PhaseStart("p") },
		func() { e.PhaseEnd("p", "success") },
		func() { e.Progress(1, 2, "x") },
		func() { e.Diagnostic("error", "m", "", 0) },
		func() { e.Metric("n", 1, "u") },
		func() { e.Artifact("i", "n", "k", "p") },
		func() { e.Summary("s") },
		func() { e.Result("OK", nil) },
	}

	for i, fn := range events {
		output := captureStdout(t, fn)
		data := parseJSONL(t, output)
		if _, ok := data["time"]; !ok {
			t.Errorf("event %d missing 'time' field", i)
		}
	}
}

func TestEmitter_AllEventsHaveVersion(t *testing.T) {
	e := New()

	events := []func(){
		func() { e.Meta("ext", "job") },
		func() { e.Log("info", "msg") },
		func() { e.Result("OK", nil) },
	}

	for i, fn := range events {
		output := captureStdout(t, fn)
		data := parseJSONL(t, output)
		if data["v"] != float64(1) {
			t.Errorf("event %d: v = %v, want 1", i, data["v"])
		}
	}
}
