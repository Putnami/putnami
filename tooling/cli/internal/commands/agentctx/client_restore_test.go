package agentctx

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// clientProjectFile is the putnami.json of a project that generates its Go
// client from a provider's contract.
const clientProjectFile = `{"name":"items-client","generatedFrom":{"service":"catalog.items","language":"go"}}` + "\n"

// writeClientProject writes the client project into root and drops the cached
// load, so the next registration reads it.
func writeClientProject(t *testing.T, root string) {
	t.Helper()
	writePathFile(t, root, "clients/items/putnami.json", clientProjectFile)
	workspace.InvalidateLoadCache(root)
}

// dropClientProject turns the client project into an ordinary one.
func dropClientProject(t *testing.T, root string) {
	t.Helper()
	writePathFile(t, root, "clients/items/putnami.json", `{"name":"items-client"}`+"\n")
	workspace.InvalidateLoadCache(root)
}

// clientWorkspace is a workspace that declares no extension and holds one
// client project.
func clientWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writePathFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"clients-ws","includes":["clients/items"]}`+"\n")
	writeClientProject(t, root)
	return root
}

func registerClientHooks(t *testing.T, root string, cfg *wsproto.Config) []HostHookChange {
	t.Helper()
	var warn bytes.Buffer
	changes := RegisterHostHooks(root, cfg, &warn)
	if warn.Len() != 0 {
		t.Fatalf("warnings = %q", warn.String())
	}
	return changes
}

func readRootFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func rootFileExists(root, rel string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

// restoreCommand is the restore hook's command for a launcher.
func restoreCommand(launcher string) string {
	return jsonEscape(launcher) + ` client-generate --all >&2 || true`
}

// wantRestoreHooks is a hook file that holds the restore hook alone.
func wantRestoreHooks(launcher string) string {
	return `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "` + restoreCommand(launcher) + `",
            "timeout": 600
          }
        ]
      }
    ]
  }
}
`
}

// A workspace that declares a generated client gets the restore hook in both
// hosts, with no extension and no collaboration binding. The hook keeps the
// first-use install on. A second run changes nothing.
func TestClientRestoreHookIsWrittenWhenAClientExists(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "a-fresh-worktree-restores-generated-clients", "install-writes-the-restore-hook-when-a-client-exists")
	root := clientWorkspace(t)
	changes := registerClientHooks(t, root, &wsproto.Config{})
	if len(changes) != 2 || changes[0].Action != HostHookFileCreated || changes[1].Action != HostHookFileCreated {
		t.Fatalf("changes = %+v, want both files created", changes)
	}
	for _, rel := range []string{claudeHooksFile, codexHooksFile} {
		got := readRootFile(t, root, rel)
		if want := wantRestoreHooks("putnami"); got != want {
			t.Fatalf("%s:\n%s\nwant:\n%s", rel, got, want)
		}
		if strings.Contains(got, hookEnv) {
			t.Fatalf("%s turns the first-use install off:\n%s", rel, got)
		}
	}
	if changes := registerClientHooks(t, root, &wsproto.Config{}); len(changes) != 0 {
		t.Fatalf("a repeated run changed %+v", changes)
	}

	// A source workspace runs its own putnamiw from the root each host
	// provides.
	writeCLILock(t, root, lockfile.LockEntry{Source: lockfile.SourceWorkspace})
	writePutnamiw(t, root, 0o755)
	registerClientHooks(t, root, &wsproto.Config{})
	if got, want := readRootFile(t, root, claudeHooksFile), wantRestoreHooks(`"$CLAUDE_PROJECT_DIR"/putnamiw`); got != want {
		t.Fatalf("Claude Code hooks:\n%s\nwant:\n%s", got, want)
	}
	if got, want := readRootFile(t, root, codexHooksFile), wantRestoreHooks(`"$(git rev-parse --show-toplevel)"/putnamiw`); got != want {
		t.Fatalf("Codex hooks:\n%s\nwant:\n%s", got, want)
	}
}

// A provider that commits clientgen.json declares a client too.
func TestClientRestoreHookCountsAClientGeneratorConfig(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "a-fresh-worktree-restores-generated-clients", "install-writes-the-restore-hook-when-a-client-exists")
	root := t.TempDir()
	writePathFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"clients-ws","includes":["services/items"]}`+"\n")
	writePathFile(t, root, "services/items/putnami.json", `{"name":"items"}`+"\n")
	writePathFile(t, root, "services/items/clientgen.json", "{}\n")
	workspace.InvalidateLoadCache(root)
	registerClientHooks(t, root, &wsproto.Config{})
	if got, want := readRootFile(t, root, claudeHooksFile), wantRestoreHooks("putnami"); got != want {
		t.Fatalf("Claude Code hooks:\n%s\nwant:\n%s", got, want)
	}
}

// The restore hook follows the hooks an active extension declares at
// SessionStart, so a workspace that gains a client keeps the extension's
// entry where it was.
func TestClientRestoreHookFollowsTheExtensionHooks(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "a-fresh-worktree-restores-generated-clients", "install-writes-the-restore-hook-when-a-client-exists")
	ws := newHostHooksWorkspace(t, true, true)
	if _, warn := ws.register(t); warn != "" {
		t.Fatal(warn)
	}
	writePathFile(t, ws.root, wsproto.WorkspaceConfigFilename, `{"name":"hooks-ws","includes":["clients/items"],`+
		`"extensions":{"`+hostHooksExtDir+`":""},"agentArtifacts":["extension:`+hostHooksExtension+`"],`+
		`"options":{"collaboration":{"memory":{"provider":"@acme/memory","version":1}}}}`+"\n")
	writeClientProject(t, ws.root)
	changes, warn := ws.register(t)
	if warn != "" {
		t.Fatal(warn)
	}
	if len(changes) != 2 || changes[0].Action != HostHookFileUpdated || changes[1].Action != HostHookFileUpdated {
		t.Fatalf("changes = %+v, want both files updated", changes)
	}
	claude := ws.read(t, claudeHooksFile)
	extension := strings.Index(claude, `agent hook --engine claude-code || true`)
	restore := strings.Index(claude, restoreCommand("putnami"))
	if extension < 0 || restore < extension || strings.Index(claude, `"PostToolUse"`) < restore {
		t.Fatalf("SessionStart does not hold the extension's hook, then the restore hook:\n%s", claude)
	}
	// Codex has no extension hook at SessionStart: the restore hook adds the
	// event after the ones the file holds.
	codex := ws.read(t, codexHooksFile)
	if strings.Index(codex, `"SessionStart"`) < strings.Index(codex, `"Stop"`) || !strings.Contains(codex, restoreCommand("putnami")) {
		t.Fatalf("Codex hooks:\n%s", codex)
	}
}

// When the last client goes, the next run removes the restore hook: a file
// that held nothing else is removed, and the person's entries stay.
func TestClientRestoreHookIsRemovedWhenNoClientRemains(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "a-fresh-worktree-restores-generated-clients", "install-removes-the-restore-hook-when-no-client-remains")
	root := clientWorkspace(t)
	person := `{
  "theme": "dark",
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "echo putnami client-generate",
            "timeout": 5
          }
        ]
      }
    ]
  }
}
`
	writePathFile(t, root, claudeHooksFile, person)
	registerClientHooks(t, root, &wsproto.Config{})
	if got := readRootFile(t, root, claudeHooksFile); !strings.Contains(got, restoreCommand("putnami")) || !strings.Contains(got, "echo putnami client-generate") {
		t.Fatalf("Claude Code hooks:\n%s", got)
	}

	dropClientProject(t, root)
	changes := registerClientHooks(t, root, &wsproto.Config{})
	if len(changes) != 2 || changes[0].Action != HostHookFileUpdated || changes[1].Action != HostHookFileRemoved {
		t.Fatalf("changes = %+v, want the Claude Code file updated and the Codex file removed", changes)
	}
	if got := readRootFile(t, root, claudeHooksFile); got != person {
		t.Fatalf("Claude Code hooks:\n%s\nwant the person's file:\n%s", got, person)
	}
	if rootFileExists(root, codexHooksFile) {
		t.Fatal("the Codex file that held only the restore hook was kept")
	}
	if changes := registerClientHooks(t, root, &wsproto.Config{}); len(changes) != 0 {
		t.Fatalf("a repeated run changed %+v", changes)
	}
}

// A workspace that does not load can neither confirm nor rule out a client,
// so its restore hook stays, with a warning.
func TestClientRestoreHookStaysWhenTheWorkspaceDoesNotLoad(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "a-fresh-worktree-restores-generated-clients", "install-removes-the-restore-hook-when-no-client-remains")
	root := clientWorkspace(t)
	registerClientHooks(t, root, &wsproto.Config{})
	before := readRootFile(t, root, claudeHooksFile)

	writePathFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"clients-ws","includes":["clients/items","a","b"]}`+"\n")
	writePathFile(t, root, "a/putnami.json", `{"name":"a","dependencies":["b"]}`+"\n")
	writePathFile(t, root, "b/putnami.json", `{"name":"b","dependencies":["a"]}`+"\n")
	dropClientProject(t, root)
	var warn bytes.Buffer
	if changes := RegisterHostHooks(root, &wsproto.Config{}, &warn); len(changes) != 0 {
		t.Fatalf("changes = %+v", changes)
	}
	if !strings.Contains(warn.String(), "the client restore hook is not written") {
		t.Fatalf("warning = %q", warn.String())
	}
	if got := readRootFile(t, root, claudeHooksFile); got != before {
		t.Fatalf("Claude Code hooks:\n%s\nwant:\n%s", got, before)
	}
}

// The restore hook is Putnami's by its command, as an extension's hook is.
func TestClientRestoreHookOwnershipByCommand(t *testing.T) {
	paths := [][]string{clientRestorePath}
	owned := []string{
		`putnami client-generate --all >&2 || true`,
		`"$CLAUDE_PROJECT_DIR"/putnamiw client-generate --all >&2 || true`,
		`"$(git rev-parse --show-toplevel)"/putnamiw client-generate >&2 || true`,
		`./putnamiw client-generate`,
	}
	for _, command := range owned {
		if !ownsHostHookCommand(command, paths) {
			t.Errorf("ownsHostHookCommand(%q) = false, want true", command)
		}
	}
	for _, command := range []string{`echo putnami client-generate`, `putnami client-generate-all`} {
		if ownsHostHookCommand(command, paths) {
			t.Errorf("ownsHostHookCommand(%q) = true, want false", command)
		}
	}
	for _, host := range []string{extproto.HostClaudeCode, extproto.HostCodex} {
		group := renderClientRestoreGroup(host, "putnami")
		if len(group.Hooks) != 1 || !ownsHostHookCommand(group.Hooks[0].Command, paths) {
			t.Errorf("%s: the rendered restore hook is not recognized: %+v", host, group)
		}
	}
}
