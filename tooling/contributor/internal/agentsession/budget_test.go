package agentsession

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
	ext "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// manifestPath is the manifest that declares the hooks, from this package.
var manifestPath = filepath.Join("..", "..", "putnami.extension.json")

func loadManifest(t *testing.T) *ext.Manifest {
	t.Helper()
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := ext.StrictLoadManifest(manifestPath, data)
	if diag.HasErrors(diags) {
		t.Fatalf("the manifest is invalid: %v", diags)
	}
	return manifest
}

// TestTheBudgetsFitTheDeclaredTimeouts pins every declared hook against the
// code that runs it: the hook stops at its budget, launcherMargin before the
// host's timeout, and the longest path of each event fits its budget.
func TestTheBudgetsFitTheDeclaredTimeouts(t *testing.T) {
	spectest.Proves(t, sessionFeature, budgetRequirement, "the-budgets-fit-the-declared-timeouts")
	manifest := loadManifest(t)
	hooks := manifest.HostHooks
	if hooks == nil || !slices.Equal(hooks.Command, []string{"agent", "hook"}) || hooks.Requires.Contract != collab.ContractMissions {
		t.Fatalf("hostHooks %+v", hooks)
	}
	wakes := 0
	for engine, entries := range hooks.Hosts {
		for _, entry := range entries {
			gotEngine, idleWake, err := parseHookArgs(entry.Args)
			if err != nil || gotEngine != engine {
				t.Errorf("%s %s: args %v answer engine %q, %v", engine, entry.Event, entry.Args, gotEngine, err)
				continue
			}
			if idleWake != entry.Wake {
				t.Errorf("%s %s: --idle-wake %v but wake %v", engine, entry.Event, idleWake, entry.Wake)
			}
			if idleWake {
				wakes++
			}
			declared := time.Duration(entry.TimeoutSeconds) * time.Second
			if budget := eventBudget(engine, entry.Event, idleWake); budget+launcherMargin != declared {
				t.Errorf("%s %s: budget %v and margin %v, declared %v", engine, entry.Event, budget, launcherMargin, declared)
			}
			for _, tool := range entry.Tools {
				if !isPlanTool(tool) {
					t.Errorf("%s: the hook ignores the declared tool %s", engine, tool)
				}
			}
			for _, tool := range entry.MCPTools {
				if sessionToolKind(tool) == "" {
					t.Errorf("%s: the hook ignores the declared tool %s", engine, tool)
				}
			}
		}
	}
	if wakes != 1 {
		t.Errorf("%d wake entries, want the Claude Code idle wake only", wakes)
	}
	// The longest path of each event: the calls it makes, each bounded by
	// callTimeout, and the waits.
	for _, path := range []struct {
		name   string
		budget time.Duration
		cost   time.Duration
	}{
		// the branch, the origin, sync and memory.mission.
		{"session start", eventBudget(claude, hookEventSessionStart, false), 2*gitTimeout + 2*callTimeout},
		// the identity of a session no start opened, sync and the
		// republish of a message the sync answered.
		{"stop", eventBudget(claude, hookEventStop, false), 2*gitTimeout + 2*callTimeout},
		{"stop failure", eventBudget(claude, hookEventStopFailure, false), 2*gitTimeout + callTimeout},
		// the identity, sync, the wait, the sync in flight at its end and
		// the sync that publishes the answer.
		{"codex stop", eventBudget(codex, hookEventStop, false), 2*gitTimeout + callTimeout + sessionDecisionWait + 2*callTimeout},
		// the wait for the Stop, the window, the sync in flight at its end
		// and the sync that publishes the message.
		{"idle wake", eventBudget(claude, hookEventStop, true), idleWakeStopWait + sessionIdleWakeWindow + 2*callTimeout},
	} {
		if path.cost > path.budget {
			t.Errorf("%s: the longest path takes %v, more than the budget %v", path.name, path.cost, path.budget)
		}
	}
}

// hangingHost never answers: each call waits until its context ends.
type hangingHost struct{ calls, closes int }

func (h *hangingHost) Call(ctx context.Context, _, _ string, _ json.RawMessage) (collab.Envelope, error) {
	h.calls++
	<-ctx.Done()
	return collab.Envelope{}, ctx.Err()
}

func (h *hangingHost) Close() error {
	h.closes++
	return nil
}

// TestAHookEndsWithItsBudget pins that every call carries the event's
// deadline: a provider that never answers cannot hold a hook past it.
func TestAHookEndsWithItsBudget(t *testing.T) {
	spectest.Proves(t, sessionFeature, budgetRequirement, "the-budgets-fit-the-declared-timeouts")
	h := newHarness(t)
	host := &hangingHost{}
	h.env.Host = host
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	var stdout, stderr bytes.Buffer
	code := Hook(ctx, h.env, []string{"--engine", claude}, bytes.NewReader(h.payload(claude, "session-start", claudeSession, nil)), &stdout, &stderr)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("the hook ran %v past its deadline", elapsed)
	}
	if code != 0 || host.calls != 2 || host.closes != 2 {
		t.Errorf("exit %d after %d calls and %d closes", code, host.calls, host.closes)
	}
	if !logSays(h.hookLog(), "context deadline exceeded") {
		t.Errorf("hook.log %v", h.hookLog())
	}
}
