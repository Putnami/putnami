package lint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	pctx "go.putnami.dev/sdk/extension/context"
)

func TestParseLintOptionsUsesContextParams(t *testing.T) {
	params := pctx.Params{
		"fix":        json.RawMessage("false"),
		"config":     json.RawMessage(`"custom.yml"`),
		"new":        json.RawMessage("true"),
		"timeout":    json.RawMessage(`"45s"`),
		"skip-guard": json.RawMessage("false"),
	}
	got := parseLintOptions(params, []string{"--tool", "golangci-lint"})
	want := lintOptions{
		fix:     false,
		config:  "custom.yml",
		new:     true,
		tool:    "golangci-lint",
		timeout: "45s",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseLintOptions() = %+v, want %+v", got, want)
	}
}

func TestParseLintOptionsExplicitArgsOverrideContext(t *testing.T) {
	params := pctx.Params{
		"fix":       json.RawMessage("false"),
		"config":    json.RawMessage(`"context.yml"`),
		"new":       json.RawMessage("false"),
		"timeout":   json.RawMessage(`"45s"`),
		"skipGuard": json.RawMessage("false"),
	}
	got := parseLintOptions(params, []string{
		"--fix", "true",
		"--config", "argv.yml",
		"--new", "true",
		"--tool", "staticcheck",
		"--timeout", "2m",
		"--skip-guard", "true",
	})
	want := lintOptions{
		fix:       true,
		config:    "argv.yml",
		new:       true,
		tool:      "staticcheck",
		timeout:   "2m",
		skipGuard: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseLintOptions() = %+v, want %+v", got, want)
	}
}

func TestParseLintOptionsRunsTheSkipGuardByDefault(t *testing.T) {
	if got := parseLintOptions(pctx.Params{}, nil); !got.skipGuard {
		t.Fatalf("parseLintOptions() = %+v, want the skip guard on by default", got)
	}
}

func TestGolangciRunArgsUsesAbsolutePathMode(t *testing.T) {
	got := golangciRunArgs(lintOptions{
		fix:     true,
		new:     true,
		timeout: "45",
	}, "custom.yml")
	want := []string{
		"run", "--allow-parallel-runners", "--path-mode", "abs",
		"--fix", "--new", "--config", "custom.yml", "--timeout", "45ms",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("golangciRunArgs() = %q, want %q", got, want)
	}
}

// --- isNumeric ---

func TestIsNumeric_ValidNumbers(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"0", true},
		{"1", true},
		{"42", true},
		{"30000", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := isNumeric(tt.input); got != tt.want {
				t.Errorf("isNumeric(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsNumeric_NonNumeric(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"30s", false},
		{"5m", false},
		{"abc", false},
		{"1.0", false},
		{"-5", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := isNumeric(tt.input); got != tt.want {
				t.Errorf("isNumeric(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsNumeric_Empty(t *testing.T) {
	if isNumeric("") {
		t.Error("isNumeric(\"\") should return false")
	}
}

func TestDerivedGolangciTimeoutMs(t *testing.T) {
	for _, tc := range []struct {
		deadline string
		want     string
	}{
		{"600000", "480000"}, // default scheduler deadline -> bundled 8m
		{"900000", "780000"},
		{"1200000", "1080000"}, // a two-member batch, capped 120s margin
		{"60000", "48000"},
		{"2", "1"},
		{"1", "1"}, // no smaller whole-millisecond timeout exists
	} {
		t.Run(tc.deadline, func(t *testing.T) {
			if got := derivedGolangciTimeoutMs(tc.deadline); got != tc.want {
				t.Fatalf("derivedGolangciTimeoutMs(%q) = %q, want %q", tc.deadline, got, tc.want)
			}
			deadline, _ := strconv.ParseInt(tc.deadline, 10, 64)
			derived, _ := strconv.ParseInt(tc.want, 10, 64)
			if derived <= 0 || derived > deadline || (deadline > 1 && derived >= deadline) {
				t.Fatalf("derived timeout %d is not safely below deadline %d", derived, deadline)
			}
		})
	}

	for _, value := range []string{"", " ", "0", "-1", "8m", "600000ms", "not-a-number"} {
		if got := derivedGolangciTimeoutMs(value); got != "" {
			t.Errorf("derivedGolangciTimeoutMs(%q) = %q, want empty", value, got)
		}
	}
}

func TestParseLintOptionsDerivesDeadlineOnlyWhenUserDidNotSetTimeout(t *testing.T) {
	t.Setenv(extensionproto.TaskDeadlineMsEnv, "1200000") // a two-member 600s batch leader
	derived := parseLintOptions(pctx.Params{}, []string{"--tool", "golangci-lint"})
	if derived.timeout != "1080000" {
		t.Fatalf("derived timeout = %q, want 1080000", derived.timeout)
	}
	if args := golangciRunArgs(derived, ""); !reflect.DeepEqual(args[len(args)-2:], []string{"--timeout", "1080000ms"}) {
		t.Fatalf("golangci args = %v, want derived millisecond timeout", args)
	}

	fromParams := parseLintOptions(pctx.Params{"timeout": json.RawMessage(`"45s"`)}, nil)
	if fromParams.timeout != "45s" {
		t.Fatalf("parameter timeout = %q, want explicit 45s", fromParams.timeout)
	}
	fromArgs := parseLintOptions(pctx.Params{"timeout": json.RawMessage(`"45s"`)}, []string{"--timeout", "2m"})
	if fromArgs.timeout != "2m" {
		t.Fatalf("argument timeout = %q, want explicit 2m", fromArgs.timeout)
	}
}

// --- resolveExtensionRoot ---

func TestResolveExtensionRoot_FromEnv(t *testing.T) {
	t.Setenv("PUTNAMI_EXTENSION_ROOT", "/my/extension/root")
	got := resolveExtensionRoot()
	if got != "/my/extension/root" {
		t.Errorf("resolveExtensionRoot = %q, want %q", got, "/my/extension/root")
	}
}

func TestResolveExtensionRoot_EnvEmpty(t *testing.T) {
	os.Unsetenv("PUTNAMI_EXTENSION_ROOT")
	// Without env var, falls back to executable directory — just ensure it returns a string (not panics)
	got := resolveExtensionRoot()
	// Result depends on where the test binary lives; just check it doesn't panic
	_ = got
}

func TestResolveExtensionRoot_EnvOverridesDefault(t *testing.T) {
	t.Setenv("PUTNAMI_EXTENSION_ROOT", "/custom/path")
	first := resolveExtensionRoot()

	t.Setenv("PUTNAMI_EXTENSION_ROOT", "/other/path")
	second := resolveExtensionRoot()

	if first == second {
		t.Errorf("resolveExtensionRoot should return different values for different env vars, both returned %q", first)
	}
}

// --- parseUnformattedFiles ---

func TestParseUnformattedFiles_Empty(t *testing.T) {
	if got := parseUnformattedFiles(""); len(got) != 0 {
		t.Errorf("parseUnformattedFiles(\"\") = %v, want empty", got)
	}
}

func TestParseUnformattedFiles_Single(t *testing.T) {
	out := `diff bad.go.orig bad.go
--- bad.go.orig
+++ bad.go
@@ -1,3 +1,3 @@
-func Bad()  {
+func Bad() {`
	got := parseUnformattedFiles(out)
	want := []string{"bad.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseUnformattedFiles = %v, want %v", got, want)
	}
}

func TestParseUnformattedFiles_MultipleSortedUnique(t *testing.T) {
	// Unsorted input with a duplicate header must come back sorted and deduped.
	out := `diff sub/z.go.orig sub/z.go
+++ sub/z.go
diff a.go.orig a.go
+++ a.go
diff a.go.orig a.go
+++ a.go`
	got := parseUnformattedFiles(out)
	want := []string{"a.go", "sub/z.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseUnformattedFiles = %v, want %v", got, want)
	}
}

func TestParseUnformattedFiles_IgnoresNonDiffLines(t *testing.T) {
	out := "level=error msg=\"something went wrong\"\nsome other noise"
	if got := parseUnformattedFiles(out); len(got) != 0 {
		t.Errorf("parseUnformattedFiles = %v, want empty (no diff headers)", got)
	}
}

// --- formatDiagPath ---

func TestFormatDiagPath_RelativeJoinedToWorkspace(t *testing.T) {
	got := formatDiagPath("/ws", "/ws/go/extension", "internal/lint/lint.go")
	want := "go/extension/internal/lint/lint.go"
	if got != want {
		t.Errorf("formatDiagPath = %q, want %q", got, want)
	}
}

func TestFormatDiagPath_AbsoluteMadeWorkspaceRelative(t *testing.T) {
	// The root comes from the host: "/ws" has no volume name, so it is not
	// absolute on Windows.
	ws := t.TempDir()
	got := formatDiagPath(ws, filepath.Join(ws, "proj"), filepath.Join(ws, "proj", "main.go"))
	want := "proj/main.go"
	if got != want {
		t.Errorf("formatDiagPath = %q, want %q", got, want)
	}
}

func TestIsToolchainCompatibilityError(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "golangci-lint package panic",
			out:  `[linters_context] typechecking error: package requires newer Go version go1.26 (application built with go1.25)`,
			want: true,
		},
		{
			name: "staticcheck file variant",
			out:  "file requires newer Go version go1.26 (application built with go1.25)",
			want: true,
		},
		{
			name: "real lint finding",
			out:  "main.go:10:2: undefined: nope (typecheck)",
			want: false,
		},
		{
			name: "gofmt formatting finding",
			out:  "main.go:4:1: File is not properly formatted (gofmt)",
			want: false,
		},
		{
			name: "empty",
			out:  "",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isToolchainCompatibilityError(tt.out); got != tt.want {
				t.Errorf("isToolchainCompatibilityError(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

func TestIsStaticcheckToolchainCompatibilityError(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "unsupported version",
			out:  "internal error: unsupported version: 1.26",
			want: true,
		},
		{
			name: "newer go file",
			out:  "error file requires newer Go version go1.26 (application built with go1.25)",
			want: true,
		},
		{
			name: "regular lint failure",
			out:  "./main.go:10:2: undefined: nope",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStaticcheckToolchainCompatibilityError(tt.out); got != tt.want {
				t.Errorf("isStaticcheckToolchainCompatibilityError(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}
