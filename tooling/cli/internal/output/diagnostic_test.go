package output

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/tooling/cli/internal/jobs"
)

func TestRenderDiagnosticEvent(t *testing.T) {
	t.Run("renders basic diagnostic", func(t *testing.T) {
		var buf bytes.Buffer
		event := jobs.RawJobEvent{
			Type: jobs.EventTypeDiagnostic,
			Data: map[string]any{
				"severity": "error",
				"message":  "unused variable 'x'",
			},
		}

		RenderDiagnosticEvent(&buf, event, "  ", false)
		output := buf.String()

		if !strings.Contains(output, "error") {
			t.Errorf("expected 'error' in output: %s", output)
		}
		if !strings.Contains(output, "unused variable 'x'") {
			t.Errorf("expected message in output: %s", output)
		}
	})

	t.Run("renders with file location", func(t *testing.T) {
		var buf bytes.Buffer
		event := jobs.RawJobEvent{
			Type: jobs.EventTypeDiagnostic,
			Data: map[string]any{
				"severity": "warning",
				"message":  "something wrong",
				"location": map[string]any{
					"file":   "src/main.ts",
					"line":   float64(42),
					"column": float64(5),
				},
			},
		}

		RenderDiagnosticEvent(&buf, event, "  ", false)
		output := buf.String()

		if !strings.Contains(output, "src/main.ts:42:5") {
			t.Errorf("expected file:line:col in output: %s", output)
		}
	})

	t.Run("renders with code", func(t *testing.T) {
		var buf bytes.Buffer
		event := jobs.RawJobEvent{
			Type: jobs.EventTypeDiagnostic,
			Data: map[string]any{
				"severity": "error",
				"code":     "TS2304",
				"message":  "Cannot find name 'Foo'",
			},
		}

		RenderDiagnosticEvent(&buf, event, "  ", false)
		output := buf.String()

		if !strings.Contains(output, "TS2304") {
			t.Errorf("expected error code in output: %s", output)
		}
	})

	t.Run("renders source snippet when file exists", func(t *testing.T) {
		// Create a temp source file
		dir := t.TempDir()
		srcFile := filepath.Join(dir, "test.ts")
		content := "const a = 1;\nconst b = 2;\nconst c = 3;\nconst d = 4;\n"
		os.WriteFile(srcFile, []byte(content), 0o644)

		var buf bytes.Buffer
		event := jobs.RawJobEvent{
			Type: jobs.EventTypeDiagnostic,
			Data: map[string]any{
				"severity": "error",
				"message":  "unused",
				"location": map[string]any{
					"file": srcFile,
					"line": float64(2),
				},
			},
		}

		RenderDiagnosticEvent(&buf, event, "", false)
		output := buf.String()

		if !strings.Contains(output, "const b = 2;") {
			t.Errorf("expected source line in output: %s", output)
		}
		if !strings.Contains(output, ">") {
			t.Errorf("expected > marker on target line: %s", output)
		}
	})
}

func TestReadSourceSnippet(t *testing.T) {
	t.Run("returns empty for nonexistent file", func(t *testing.T) {
		snippet := readSourceSnippet("/nonexistent/file.ts", 1, 0)
		if snippet != "" {
			t.Errorf("expected empty snippet for nonexistent file")
		}
	})

	t.Run("returns empty for invalid line", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "test.txt")
		os.WriteFile(f, []byte("line1\nline2\n"), 0o644)

		snippet := readSourceSnippet(f, 0, 0)
		if snippet != "" {
			t.Errorf("expected empty snippet for line 0")
		}

		snippet = readSourceSnippet(f, 100, 0)
		if snippet != "" {
			t.Errorf("expected empty snippet for line beyond file")
		}
	})

	t.Run("shows context lines", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "test.txt")
		os.WriteFile(f, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644)

		snippet := readSourceSnippet(f, 3, 0)
		if !strings.Contains(snippet, "line2") { // line before
			t.Errorf("expected line before in snippet: %s", snippet)
		}
		if !strings.Contains(snippet, "line3") { // target line
			t.Errorf("expected target line in snippet: %s", snippet)
		}
		if !strings.Contains(snippet, "line4") { // line after
			t.Errorf("expected line after in snippet: %s", snippet)
		}
	})

	t.Run("shows column indicator", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "test.txt")
		os.WriteFile(f, []byte("const x = 1;\n"), 0o644)

		snippet := readSourceSnippet(f, 1, 7)
		if !strings.Contains(snippet, "^") {
			t.Errorf("expected column indicator ^ in snippet: %s", snippet)
		}
	})
}

func TestExtractLocation(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "located-diagnostics", "severity-code-and-location-extract")
	t.Run("direct fields", func(t *testing.T) {
		data := map[string]any{
			"file":   "main.go",
			"line":   float64(10),
			"column": float64(5),
		}
		loc := extractLocation(data)
		if loc.file != "main.go" || loc.line != 10 || loc.column != 5 {
			t.Errorf("unexpected location: %+v", loc)
		}
	})

	t.Run("nested location object", func(t *testing.T) {
		data := map[string]any{
			"location": map[string]any{
				"file":   "main.go",
				"line":   float64(20),
				"column": float64(3),
			},
		}
		loc := extractLocation(data)
		if loc.file != "main.go" || loc.line != 20 || loc.column != 3 {
			t.Errorf("unexpected location: %+v", loc)
		}
	})

	t.Run("nested overrides direct", func(t *testing.T) {
		data := map[string]any{
			"file": "direct.go",
			"location": map[string]any{
				"file": "nested.go",
				"line": float64(5),
			},
		}
		loc := extractLocation(data)
		if loc.file != "nested.go" {
			t.Errorf("expected nested to override direct, got %s", loc.file)
		}
	})
}
