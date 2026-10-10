package cli

import (
	"errors"
	"fmt"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

func TestExitCodeForError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/diagnostics-results", "terminal-outcome", "errors-map-to-stable-exit-codes")
	tests := []struct {
		name string
		err  error
		want int
	}{
		// Under the protocol/cli taxonomy usage, no-workspace, not-found,
		// no-match and invalid-config all fold into the Usage code (2); auth and
		// api have their own codes; anything unclassified is the generic
		// Failure code (1).
		{"nil is success", nil, ExitSuccess},
		{"usage sentinel", cmderr.ErrUsage, ExitUsage},
		{"usageErrorf wraps usage", usageErrorf("bad %s", "thing"), ExitUsage},
		{"no workspace", cmderr.ErrNoWorkspace, ExitUsage},
		{"not found", cmderr.ErrNotFound, ExitUsage},
		{"not found wrapped with %w", fmt.Errorf("project x: %w", cmderr.ErrNotFound), ExitUsage},
		{"not found via Classify", cmderr.Classify(errors.New("missing"), cmderr.ErrNotFound), ExitUsage},
		{"no match", cmderr.ErrNoMatch, ExitUsage},
		{"invalid config", cmderr.ErrInvalidConfig, ExitUsage},
		{"invalid config via Classify", cmderr.Classify(errors.New("bad"), cmderr.ErrInvalidConfig), ExitUsage},
		{"leaf usage via Usagef", cmderr.Usagef("project name required"), ExitUsage},
		{"leaf not found via NotFoundf", cmderr.NotFoundf("project not found: %s", "api"), ExitUsage},
		{"leaf invalid via InvalidConfigf", cmderr.InvalidConfigf("invalid semver: %s", "1.x"), ExitUsage},
		{"auth sentinel", cmderr.ErrAuth, ExitAuth},
		{"leaf auth via Authf", cmderr.Authf("token expired"), ExitAuth},
		{"api sentinel", cmderr.ErrAPI, ExitAPI},
		{"leaf api via APIf", cmderr.APIf("registry 500"), ExitAPI},
		{"unclassified error", errors.New("boom"), ExitError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCodeForError(tt.err); got != tt.want {
				t.Errorf("exitCodeForError(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestUsageErrorfMessageHasNoSentinelSuffix(t *testing.T) {
	t.Parallel()
	err := usageErrorf("unknown subcommand: cache %s", "bogus")
	const want = "unknown subcommand: cache bogus"
	if err.Error() != want {
		t.Errorf("usageErrorf message = %q, want %q (sentinel text must not leak)", err.Error(), want)
	}
	if !errors.Is(err, cmderr.ErrUsage) {
		t.Error("usageErrorf result should match ErrUsage via errors.Is")
	}
}

// TestCommandRegistryCoversStructuredCommands locks in that every name that
// was previously hardcoded in isStructuredCommand is registered, so deriving
// isStructuredCommand from the registry preserves the recognized command set.
func TestCommandRegistryCoversStructuredCommands(t *testing.T) {
	t.Parallel()
	want := []string{
		"extensions", "templates", "projects", "workspace", "version", "deps",
		"cache", "config", "context", "migrate", "sessions", "completion",
		"install", "telemetry", "upgrade", "help", "init", "dev", "infra", "scopes",
		"mcp", "pin", "doctor", "change-plan", "impact-plan", "report", "tree",
		"compose", "qualify",
	}
	for _, name := range want {
		if _, ok := lookupCommand(name); !ok {
			t.Errorf("command %q is not registered", name)
		}
		if !isStructuredCommand(name) {
			t.Errorf("isStructuredCommand(%q) = false, want true", name)
		}
	}
	if len(commandRegistry) != len(want) {
		t.Errorf("registry has %d commands, want %d", len(commandRegistry), len(want))
	}
	if isStructuredCommand("build") {
		t.Error("isStructuredCommand(\"build\") = true, want false for a job command")
	}
}

func TestRequireWorkspace(t *testing.T) {
	t.Parallel()
	if err := (&CommandEnv{WsRoot: ""}).requireWorkspace(); !errors.Is(err, cmderr.ErrNoWorkspace) {
		t.Errorf("requireWorkspace with empty root = %v, want ErrNoWorkspace", err)
	}
	if err := (&CommandEnv{WsRoot: "/ws"}).requireWorkspace(); err != nil {
		t.Errorf("requireWorkspace with a root = %v, want nil", err)
	}
}

func TestRegisterCommandRejectsDuplicate(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("registering a duplicate command name should panic")
		}
	}()
	// "extensions" is already registered; the duplicate check fires before any
	// mutation, so the shared registry is left intact.
	registerCommand("extensions", func(*CommandEnv) error { return nil })
}

// TestRegisterCommandRejectsANameTheCatalogDoesNotDeclare is the guard that lets
// commandRegistry stop being a command table (slice A6a).
//
// The registry is now dispatch only: the vocabulary lives in the catalog, and
// isStructuredCommand reads it. That split holds only while the two sets cannot
// diverge — a handler registered under a name the catalog never declares would
// be a command with no help, no completion, no declared workspace requirement
// and no structured-output capability, i.e. a second hand-maintained vocabulary
// growing back one registerCommand call at a time (ADR 0001 §1).
//
// TestCatalog_CoversCommandRegistry asserts the other direction (every catalog
// structured root has a handler); together they are what replaced A0's deleted
// command_tables_baseline_test.go scaffolding.
func TestRegisterCommandRejectsANameTheCatalogDoesNotDeclare(t *testing.T) {
	t.Parallel()
	const uncataloged = "definitely-not-a-catalog-command"
	if commandmeta.IsStructuredRoot(uncataloged) {
		t.Fatalf("%q is in the catalog; pick a name that is not, or this test proves nothing", uncataloged)
	}
	defer func() {
		if recover() == nil {
			t.Errorf("registerCommand(%q) did not panic: a handler can be bound to a name the "+
				"command catalog does not declare, so the registry is a second command vocabulary again",
				uncataloged)
		}
		// The catalog check fires before the map write, so the shared registry is
		// left intact for every other test in this package.
		if _, leaked := lookupCommand(uncataloged); leaked {
			t.Errorf("registerCommand(%q) mutated the shared registry before rejecting", uncataloged)
		}
	}()
	registerCommand(uncataloged, func(*CommandEnv) error { return nil })
}
