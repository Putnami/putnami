package cli

import (
	"context"
	"testing"
)

// TestParseArgs_JSONFlag confirms --json is parsed into the GlobalFlags shorthand
// that App.Run later resolves into --output=json.
func TestParseArgs_JSONFlag(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"build", "--json"}, nil, nil)
	if !parsed.Global.JSON {
		t.Error("--json should set Global.JSON")
	}
	if parsed.Global.Output != "" {
		t.Errorf("Global.Output = %q, want empty before resolution", parsed.Global.Output)
	}
}

// TestAppRun_JSONConflictIsUsageError checks that --json combined with a
// conflicting --output fails fast with the usage exit code, before any
// workspace or job work.
func TestAppRun_JSONConflictIsUsageError(t *testing.T) {
	t.Chdir(t.TempDir())
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	code := app.Run(context.Background(), []string{"build", "--json", "--output=jsonl"})
	if code != ExitUsage {
		t.Errorf("--json --output=jsonl exit code = %d, want %d (usage)", code, ExitUsage)
	}
}

// TestAppRun_UnknownOutputIsUsageError checks that an unrecognized --output
// value is rejected as a usage error rather than silently auto-detecting.
func TestAppRun_UnknownOutputIsUsageError(t *testing.T) {
	t.Chdir(t.TempDir())
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	code := app.Run(context.Background(), []string{"build", "--output=yaml"})
	if code != ExitUsage {
		t.Errorf("--output=yaml exit code = %d, want %d (usage)", code, ExitUsage)
	}
}
