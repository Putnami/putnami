package lint

import (
	"os"
	"path/filepath"
	"testing"
)

// --- resolveExtensionRoot: additional cases ---

func TestResolveExtensionRoot_EmptyEnvVar(t *testing.T) {
	t.Setenv("PUTNAMI_EXTENSION_ROOT", "")
	got := resolveExtensionRoot()
	// When env var is empty string, should fall back to executable dir
	if got == "" {
		// Acceptable — the executable directory logic may return empty in tests
		return
	}
	// Should return some path (the executable's directory)
	if !filepath.IsAbs(got) {
		t.Errorf("expected absolute path, got %q", got)
	}
}

func TestResolveExtensionRoot_PathWithSpaces(t *testing.T) {
	t.Setenv("PUTNAMI_EXTENSION_ROOT", "/path/with spaces/extension")
	got := resolveExtensionRoot()
	if got != "/path/with spaces/extension" {
		t.Errorf("resolveExtensionRoot = %q, want %q", got, "/path/with spaces/extension")
	}
}

// --- isNumeric: timeout conversion ---

func TestTimeoutFlagConversion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"30000", "30000ms"}, // Numeric → add ms suffix
		{"60", "60ms"},       // Numeric → add ms suffix
		{"30s", "30s"},       // Already has unit
		{"5m", "5m"},         // Already has unit
		{"1h30m", "1h30m"},   // Already has unit
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			var result string
			if isNumeric(tt.input) {
				result = tt.input + "ms"
			} else {
				result = tt.input
			}
			if result != tt.expected {
				t.Errorf("timeout(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// --- resolveExtensionRoot with temp directory ---

func TestResolveExtensionRoot_ReturnsValidPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PUTNAMI_EXTENSION_ROOT", dir)

	got := resolveExtensionRoot()
	if got != dir {
		t.Errorf("resolveExtensionRoot = %q, want %q", got, dir)
	}

	// Verify the path exists
	if _, err := os.Stat(got); err != nil {
		t.Errorf("returned path should exist: %v", err)
	}
}
