package agentctx

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/launch"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

const (
	userHooksFeature = "cli/agent-workflow-lifecycle"
	userHooksWritten = "host-hooks-user-files"
	userHooksKept    = "host-hooks-user-files-kept"
)

// userHostHookHome opts a test into the user's hook files: its home is a
// directory of its own, and env answers the variables the run reads (CI,
// CLAUDE_CONFIG_DIR, CODEX_HOME). It returns the home.
func userHostHookHome(t *testing.T, env map[string]string) string {
	t.Helper()
	home := t.TempDir()
	getenv, homeDir := hostHookGetenv, hostHookHomeDir
	hostHookGetenv = func(name string) string { return env[name] }
	hostHookHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { hostHookGetenv, hostHookHomeDir = getenv, homeDir })
	return home
}

func readUserFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func userChanges(changes []HostHookChange) []HostHookChange {
	var user []HostHookChange
	for _, change := range changes {
		if change.User {
			user = append(user, change)
		}
	}
	return user
}

// A run that writes a workspace's hooks writes the same hooks, launched by
// putnami, into the user's hook file of each host; a repeated run changes
// nothing.
func TestUserHostHooksFollowTheWorkspaceHooks(t *testing.T) {
	spectest.Proves(t, userHooksFeature, userHooksWritten, "a-repeated-run-leaves-the-user-files")
	home := userHostHookHome(t, nil)
	ws := newHostHooksWorkspace(t, true, true)
	// A source workspace runs its own wrapper; the user file still names
	// putnami, which serves every workspace of the person.
	writeCLILock(t, ws.root, lockfile.LockEntry{Source: lockfile.SourceWorkspace})
	writePutnamiw(t, ws.root, 0o755)

	changes, warn := ws.register(t)
	if warn != "" {
		t.Fatalf("warnings = %q", warn)
	}
	claude := filepath.Join(home, ".claude", "settings.json")
	codex := filepath.Join(home, ".codex", "hooks.json")
	if got, want := readUserFile(t, claude), wantClaudeHooks("putnami"); got != want {
		t.Fatalf("user Claude Code file:\n%s\nwant:\n%s", got, want)
	}
	if got, want := readUserFile(t, codex), wantCodexHooks("putnami"); got != want {
		t.Fatalf("user Codex file:\n%s\nwant:\n%s", got, want)
	}
	if got, want := ws.read(t, claudeHooksFile), wantClaudeHooks(`"$CLAUDE_PROJECT_DIR"/putnamiw`); got != want {
		t.Fatalf("workspace Claude Code file:\n%s\nwant:\n%s", got, want)
	}
	user := userChanges(changes)
	if len(user) != 2 || user[0].File != claude || user[1].File != codex || user[0].Action != HostHookFileCreated {
		t.Fatalf("user changes = %+v", user)
	}
	if !strings.Contains(user[1].Description(), "run /hooks") {
		t.Fatalf("the Codex user change does not name the trust step: %q", user[1].Description())
	}

	if !launch.HostHookRepositoryRecorded(home, ws.root) {
		t.Fatal("the install did not record the repository the relay serves")
	}

	again, warn := ws.register(t)
	if warn != "" || len(again) != 0 {
		t.Fatalf("a repeated run changed %+v (%q)", again, warn)
	}
}

// CLAUDE_CONFIG_DIR and CODEX_HOME move the user files, as they move what the
// hosts read; a relative directory is refused with a warning.
func TestUserHostHooksFollowTheHostsConfigDirectories(t *testing.T) {
	dirs := t.TempDir()
	claudeDir := filepath.Join(dirs, "claude")
	home := userHostHookHome(t, map[string]string{"CLAUDE_CONFIG_DIR": claudeDir, "CODEX_HOME": "relative"})
	ws := newHostHooksWorkspace(t, true, true)

	changes, warn := ws.register(t)
	if !strings.Contains(warn, "CODEX_HOME is not an absolute path") {
		t.Fatalf("warnings = %q", warn)
	}
	if got := readUserFile(t, filepath.Join(claudeDir, "settings.json")); got != wantClaudeHooks("putnami") {
		t.Fatalf("Claude Code file in CLAUDE_CONFIG_DIR:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("the home's .claude was written although CLAUDE_CONFIG_DIR is set (%v)", err)
	}
	if user := userChanges(changes); len(user) != 1 || user[0].Host != extproto.HostClaudeCode {
		t.Fatalf("user changes = %+v", user)
	}
}

// A user file serves every workspace of the person: a run never removes a
// hook from it, keeps an event the workspace asks nothing of, and keeps the
// person's entries.
func TestUserHostHooksAreNeverPruned(t *testing.T) {
	spectest.Proves(t, userHooksFeature, userHooksKept, "the-user-files-are-never-pruned")
	home := userHostHookHome(t, nil)
	claude := filepath.Join(home, ".claude", "settings.json")
	writeUserFile := func(content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(claude), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(claude, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// An event this workspace asks nothing of, holding our handler, and the
	// person's own key.
	sessionEnd := `{
  "theme": "dark",
  "hooks": {
    "SessionEnd": [
      {
        "hooks": [
          {"type": "command", "command": "PUTNAMI_NO_AUTO_INSTALL=1 putnami host-hook agent hook --engine claude-code || true", "timeout": 30}
        ]
      }
    ]
  }
}
`
	writeUserFile(sessionEnd)
	ws := newHostHooksWorkspace(t, true, true)
	if _, warn := ws.register(t); warn != "" {
		t.Fatalf("warnings = %q", warn)
	}
	got := readUserFile(t, claude)
	if !strings.Contains(got, `"SessionEnd": [`) || !strings.Contains(got, `"theme": "dark"`) || !strings.Contains(got, `"SessionStart": [`) {
		t.Fatalf("the user file lost an entry or did not get ours:\n%s", got)
	}

	// A workspace that drops its opt-in leaves the user file as it is.
	before := got
	retired := newHostHooksWorkspace(t, false, true)
	writePathFile(t, retired.root, claudeHooksFile, wantClaudeHooks("putnami"))
	changes, warn := retired.register(t)
	if warn != "" || len(userChanges(changes)) != 0 {
		t.Fatalf("changes = %+v (%q)", changes, warn)
	}
	if retired.exists(claudeHooksFile) {
		t.Fatal("the workspace file kept the retired hooks")
	}
	if after := readUserFile(t, claude); after != before {
		t.Fatalf("the retirement changed the user file:\n%s", after)
	}
}

// A run inside CI writes no user file: a runner hosts no agent session.
func TestUserHostHooksAreNotWrittenInCI(t *testing.T) {
	spectest.Proves(t, userHooksFeature, userHooksKept, "ci-writes-no-user-file")
	for _, value := range []string{"true", "1", "yes"} {
		home := userHostHookHome(t, map[string]string{"CI": value})
		ws := newHostHooksWorkspace(t, true, true)
		changes, warn := ws.register(t)
		if warn != "" || len(userChanges(changes)) != 0 {
			t.Fatalf("CI=%s: changes = %+v (%q)", value, changes, warn)
		}
		if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
			t.Fatalf("CI=%s wrote the user's .claude (%v)", value, err)
		}
		if !ws.exists(claudeHooksFile) {
			t.Fatalf("CI=%s skipped the workspace file too", value)
		}
		if launch.HostHookRepositoryRecorded(home, ws.root) {
			t.Fatalf("CI=%s recorded the repository", value)
		}
	}
}

// A dry run lists the user files it would change and writes none.
func TestUserHostHooksDryRunListsThem(t *testing.T) {
	spectest.Proves(t, userHooksFeature, userHooksKept, "a-dry-run-lists-the-user-files")
	home := userHostHookHome(t, nil)
	ws := newHostHooksWorkspace(t, true, true)
	var warn bytes.Buffer
	changes := PlanHostHooks(ws.root, ws.config(), &warn)
	if warn.Len() != 0 {
		t.Fatalf("warnings = %q", warn.String())
	}
	if user := userChanges(changes); len(user) != 2 {
		t.Fatalf("user changes = %+v", user)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("a dry run wrote the user's .claude (%v)", err)
	}
}

// The client restore hook restores the clients of the checkout the host
// starts in: it never goes into a user file.
func TestUserHostHooksHoldNoClientRestoreHook(t *testing.T) {
	home := userHostHookHome(t, nil)
	ws := newHostHooksWorkspace(t, true, true)
	writePathFile(t, ws.root, wsproto.WorkspaceConfigFilename, `{"name":"hooks-ws","includes":["clients/items"],`+
		`"extensions":{"`+hostHooksExtDir+`":""},"agentArtifacts":["extension:`+hostHooksExtension+`"],`+
		`"options":{"collaboration":{"memory":{"provider":"@acme/memory","version":1}}}}`+"\n")
	writeClientProject(t, ws.root)
	if _, warn := ws.register(t); warn != "" {
		t.Fatalf("warnings = %q", warn)
	}
	if !strings.Contains(ws.read(t, claudeHooksFile), "client-generate") {
		t.Fatal("the workspace file has no restore hook: the fixture does not declare a client")
	}
	if got := readUserFile(t, filepath.Join(home, ".claude", "settings.json")); strings.Contains(got, "client-generate") {
		t.Fatalf("the user file holds the restore hook:\n%s", got)
	}
}

// A workspace that asks for fewer groups at an event than the user file holds
// keeps the extra ones: another workspace of the person asked for them.
func TestUserHostHooksKeepTheGroupsAnotherWorkspaceAskedFor(t *testing.T) {
	spectest.Proves(t, userHooksFeature, userHooksKept, "the-user-files-are-never-pruned")
	home := userHostHookHome(t, nil)
	claude := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claude), 0o755); err != nil {
		t.Fatal(err)
	}
	// Another workspace asked for two Stop groups: the test extension asks
	// for one.
	other := `{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {"type": "command", "command": "PUTNAMI_NO_AUTO_INSTALL=1 putnami host-hook agent hook --engine claude-code || true", "timeout": 30}
        ]
      },
      {
        "hooks": [
          {"type": "command", "command": "PUTNAMI_NO_AUTO_INSTALL=1 putnami host-hook agent hook --engine claude-code --idle-wake --other; s=$?; [ \"$s\" -eq 75 ] && exit 2; exit 0", "timeout": 660, "asyncRewake": true}
        ]
      }
    ]
  }
}
`
	if err := os.WriteFile(claude, []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := newHostHooksWorkspace(t, true, true)
	if _, warn := ws.register(t); warn != "" {
		t.Fatalf("warnings = %q", warn)
	}
	got := readUserFile(t, claude)
	plain := `{"type": "command", "command": "PUTNAMI_NO_AUTO_INSTALL=1 putnami host-hook agent hook --engine claude-code || true", "timeout": 30}`
	if !strings.Contains(got, plain) || !strings.Contains(got, "--idle-wake --other") || !strings.Contains(got, "host-hook agent hook --engine claude-code --idle-wake; s=$?") {
		t.Fatalf("the user file lost a group:\n%s", got)
	}
	again, warn := ws.register(t)
	if warn != "" || len(again) != 0 {
		t.Fatalf("a repeated run changed %+v (%q)", again, warn)
	}
}

// A hook of the user file in the form before the relay is the same hook: the
// run replaces it in place rather than adding a second one.
func TestUserHostHooksReplaceTheSameHookInPlace(t *testing.T) {
	home := userHostHookHome(t, nil)
	claude := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claude), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := strings.ReplaceAll(wantClaudeHooks("putnami"), " host-hook agent hook", " agent hook")
	if err := os.WriteFile(claude, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := newHostHooksWorkspace(t, true, true)
	if _, warn := ws.register(t); warn != "" {
		t.Fatalf("warnings = %q", warn)
	}
	if got := readUserFile(t, claude); got != wantClaudeHooks("putnami") {
		t.Fatalf("user file:\n%s\nwant:\n%s", got, wantClaudeHooks("putnami"))
	}
}
