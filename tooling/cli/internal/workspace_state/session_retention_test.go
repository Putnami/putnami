package workspace_state

import (
	"go.putnami.dev/protocol/features/spectest"

	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// TestRetentionFromWorkspace pins the resolution the CLI uses: the workspace
// config's `sessions.keep` when it is authored and positive, the default
// otherwise. There is deliberately no environment layer — the session store is
// per-worktree, unlike the machine-global build store.
func TestRetentionFromWorkspace(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-record-durability", "retention-is-configurable-and-its-default-is-unchanged")
	keep := func(n int) *int { return &n }

	cases := []struct {
		name string
		cfg  *wsproto.Config
		want int
	}{
		{"nil config", nil, DefaultMaxSessions},
		{"no sessions block", &wsproto.Config{Name: "ws"}, DefaultMaxSessions},
		{"empty sessions block", &wsproto.Config{Sessions: &wsproto.SessionsConfig{}}, DefaultMaxSessions},
		{"authored keep", &wsproto.Config{Sessions: &wsproto.SessionsConfig{Keep: keep(500)}}, 500},
		{"keep of one", &wsproto.Config{Sessions: &wsproto.SessionsConfig{Keep: keep(1)}}, 1},
		// The schema's minimum of 1 keeps these unauthorable; a config that
		// bypassed the schema falls back to the default rather than being read
		// as "unlimited" or as "delete everything".
		{"zero keep falls back", &wsproto.Config{Sessions: &wsproto.SessionsConfig{Keep: keep(0)}}, DefaultMaxSessions},
		{"negative keep falls back", &wsproto.Config{Sessions: &wsproto.SessionsConfig{Keep: keep(-5)}}, DefaultMaxSessions},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RetentionFromWorkspace(tc.cfg); got != tc.want {
				t.Errorf("RetentionFromWorkspace() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestNewSessionStoreWithRetention_DefaultIsUnchanged pins that the default
// constructor and an unconfigured workspace produce the same store, so
// retention behavior is byte-identical when nothing is authored.
func TestNewSessionStoreWithRetention_DefaultIsUnchanged(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-record-durability", "retention-is-configurable-and-its-default-is-unchanged")
	wsRoot := t.TempDir()
	if got := NewSessionStore(wsRoot).maxSessions; got != DefaultMaxSessions {
		t.Errorf("NewSessionStore keeps %d, want %d", got, DefaultMaxSessions)
	}
	configured := NewSessionStoreWithRetention(wsRoot, RetentionFromWorkspace(nil))
	if configured.maxSessions != DefaultMaxSessions {
		t.Errorf("unconfigured workspace keeps %d, want %d", configured.maxSessions, DefaultMaxSessions)
	}
	if configured.Root() != NewSessionStore(wsRoot).Root() {
		t.Errorf("roots differ: %q vs %q", configured.Root(), NewSessionStore(wsRoot).Root())
	}
	if got := NewSessionStoreWithRetention(wsRoot, 0).maxSessions; got != DefaultMaxSessions {
		t.Errorf("a non-positive keep keeps %d, want the default %d", got, DefaultMaxSessions)
	}
}

// TestSessionStore_PruneHonorsConfiguredRetention runs the real Prune over a
// store built the way the engine builds it, and shows the store settling at the
// configured count instead of the default.
func TestSessionStore_PruneHonorsConfiguredRetention(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-record-durability", "retention-is-configurable-and-its-default-is-unchanged")
	keep := 3
	wsRoot := t.TempDir()
	store := NewSessionStoreWithRetention(wsRoot,
		RetentionFromWorkspace(&wsproto.Config{Sessions: &wsproto.SessionsConfig{Keep: &keep}}))

	var ids []string
	for i := 0; i < 25; i++ {
		id := sessionIDForTest(i)
		ids = append(ids, id)
		if err := os.MkdirAll(filepath.Join(store.Root(), id), 0o755); err != nil {
			t.Fatalf("create session %s: %v", id, err)
		}
	}

	if err := store.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	remaining, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != keep {
		t.Fatalf("store settled at %d sessions, want the configured %d: %v", len(remaining), keep, remaining)
	}
	// The newest records are the ones retained.
	want := []string{ids[24], ids[23], ids[22]}
	for i, id := range want {
		if remaining[i] != id {
			t.Errorf("retained[%d] = %q, want %q (newest first)", i, remaining[i], id)
		}
	}

	// The same store, unconfigured, keeps 20 of the same 25.
	defaultStore := NewSessionStore(t.TempDir())
	for _, id := range ids {
		if err := os.MkdirAll(filepath.Join(defaultStore.Root(), id), 0o755); err != nil {
			t.Fatalf("create session %s: %v", id, err)
		}
	}
	if err := defaultStore.Prune(); err != nil {
		t.Fatalf("Prune (default): %v", err)
	}
	defaultRemaining, err := defaultStore.List()
	if err != nil {
		t.Fatalf("List (default): %v", err)
	}
	if len(defaultRemaining) != DefaultMaxSessions {
		t.Fatalf("unconfigured store settled at %d sessions, want %d", len(defaultRemaining), DefaultMaxSessions)
	}
}

// sessionIDForTest builds a chronologically sortable session directory name, the
// same <date>-<time>-<suffix> shape a real session uses.
func sessionIDForTest(i int) string {
	return "20260101-" + twoDigits(i/60) + twoDigits(i%60) + "00-" + twoDigits(i) + "beef"
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
