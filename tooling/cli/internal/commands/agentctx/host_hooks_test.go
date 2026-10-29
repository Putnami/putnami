package agentctx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

const (
	hostHooksExtension = "@acme/sessions"
	hostHooksExtDir    = "/ext/sessions"
	claudeHooksFile    = ".claude/settings.json"
	codexHooksFile     = ".codex/hooks.json"
)

// hostHooksDeclaration is the hostHooks member of the test extension. It
// requires the memory contract: any contract this CLI knows serves.
const hostHooksDeclaration = `{
  "requires": {"contract": "memory"},
  "command": ["agent", "hook"],
  "hosts": {
    "claude-code": [
      {"event": "SessionStart", "args": ["--engine", "claude-code"], "timeoutSeconds": 30},
      {"event": "PostToolUse", "tools": ["TodoWrite", "TaskCreate"], "args": ["--engine", "claude-code"], "timeoutSeconds": 30},
      {"event": "Stop", "args": ["--engine", "claude-code", "--idle-wake"], "wake": true, "timeoutSeconds": 660}
    ],
    "codex": [
      {"event": "PostToolUse", "tools": ["update_plan"], "args": ["--engine", "codex"], "timeoutSeconds": 30},
      {"event": "Stop", "args": ["--engine", "codex"], "timeoutSeconds": 240}
    ]
  }
}`

// hostHooksExtensionManifest is a local extension at contract 17 whose
// interactive `agent hook` subcommand the hooks run.
func hostHooksExtensionManifest(hooks string) string {
	return fmt.Sprintf(`{
  "name": %q,
  "cliContract": %d,
  "agentContent": {"path": "content", "source": "src"},
  "commandGroups": {"agent": {"subcommands": {"hook": {"command": "agent-hook", "interactive": true}}}},
  "commands": {"agent-hook": {"run": [{"id": "hook", "task": "hook"}]}},
  "tasks": {"hook": {"kind": "command", "command": "echo"}},
  "hostHooks": %s
}`, hostHooksExtension, protocolcli.HostHooksContract, hooks)
}

// hostHooksWorkspace is a workspace that declares the test extension by path.
type hostHooksWorkspace struct {
	root    string
	optedIn bool
	bound   bool
}

func newHostHooksWorkspace(t *testing.T, optedIn, bound bool) *hostHooksWorkspace {
	t.Helper()
	ws := &hostHooksWorkspace{root: t.TempDir(), optedIn: optedIn, bound: bound}
	writePathFile(t, ws.root, strings.TrimPrefix(hostHooksExtDir, "/")+"/putnami.extension.json", hostHooksExtensionManifest(hostHooksDeclaration))
	ws.writeConfig(t)
	return ws
}

func (ws *hostHooksWorkspace) writeConfig(t *testing.T) {
	t.Helper()
	config := `{"name":"hooks-ws","extensions":{"` + hostHooksExtDir + `":""}`
	if ws.optedIn {
		config += `,"agentArtifacts":["extension:` + hostHooksExtension + `"]`
	}
	if ws.bound {
		config += `,"options":{"collaboration":{"memory":{"provider":"@acme/memory","version":1}}}`
	}
	writePathFile(t, ws.root, wsproto.WorkspaceConfigFilename, config+"}\n")
}

func (ws *hostHooksWorkspace) config() *wsproto.Config {
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{hostHooksExtDir: ""}}}
	if ws.optedIn {
		cfg.AgentArtifacts = []string{extensionAgentContentPrefix + hostHooksExtension}
	}
	return cfg
}

func (ws *hostHooksWorkspace) register(t *testing.T) ([]HostHookChange, string) {
	t.Helper()
	var warn bytes.Buffer
	changes := RegisterHostHooks(ws.root, ws.config(), &warn)
	return changes, warn.String()
}

func (ws *hostHooksWorkspace) read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ws.root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (ws *hostHooksWorkspace) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(ws.root, filepath.FromSlash(rel)))
	return err == nil
}

// hookEnv starts every hook command: a hook never runs the first-use install.
const hookEnv = "PUTNAMI_NO_AUTO_INSTALL=1 "

// wantClaudeHooks is the Claude Code file the test extension's hooks produce
// in a workspace that has none, started with launcher.
func wantClaudeHooks(launcher string) string {
	return `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "` + hookEnv + jsonEscape(launcher) + ` agent hook --engine claude-code || true",
            "timeout": 30
          }
        ]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "TodoWrite|TaskCreate",
        "hooks": [
          {
            "type": "command",
            "command": "` + hookEnv + jsonEscape(launcher) + ` agent hook --engine claude-code || true",
            "timeout": 30
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "` + hookEnv + jsonEscape(launcher) + ` agent hook --engine claude-code --idle-wake; s=$?; [ \"$s\" -eq 75 ] && exit 2; exit 0",
            "timeout": 660,
            "asyncRewake": true
          }
        ]
      }
    ]
  }
}
`
}

// wantCodexHooks is the Codex file the test extension's hooks produce in a
// workspace that has none, started with launcher.
func wantCodexHooks(launcher string) string {
	return `{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "^(update_plan)$",
        "hooks": [
          {
            "type": "command",
            "command": "` + hookEnv + jsonEscape(launcher) + ` agent hook --engine codex || true",
            "timeout": 30
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "` + hookEnv + jsonEscape(launcher) + ` agent hook --engine codex || true",
            "timeout": 240
          }
        ]
      }
    ]
  }
}
`
}

func jsonEscape(value string) string {
	return strings.ReplaceAll(value, `"`, `\"`)
}

// A workspace without hook files gets both, in the format the hosts write,
// and Codex's trust step is reported. A second run changes nothing.
func TestHostHooksAreWrittenWhenAbsentAndRepeatChangesNothing(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hooks-written-on-explicit-lifecycle", "an-opted-in-bound-installed-extension-gets-its-hooks")
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hooks-written-on-explicit-lifecycle", "a-repeated-run-changes-nothing")
	ws := newHostHooksWorkspace(t, true, true)
	changes, warn := ws.register(t)
	if warn != "" {
		t.Fatalf("warnings = %q", warn)
	}
	if len(changes) != 2 || changes[0].Action != HostHookFileCreated || changes[1].Action != HostHookFileCreated {
		t.Fatalf("changes = %+v, want both files created", changes)
	}
	if got := changes[1].Description(); !strings.Contains(got, "run /hooks") || strings.Contains(changes[0].Description(), "/hooks") {
		t.Fatalf("descriptions = %q, %q; only Codex needs the trust step", changes[0].Description(), got)
	}
	if got, want := ws.read(t, claudeHooksFile), wantClaudeHooks("putnami"); got != want {
		t.Fatalf("Claude Code hooks:\n%s\nwant:\n%s", got, want)
	}
	if got, want := ws.read(t, codexHooksFile), wantCodexHooks("putnami"); got != want {
		t.Fatalf("Codex hooks:\n%s\nwant:\n%s", got, want)
	}

	before := [2]string{ws.read(t, claudeHooksFile), ws.read(t, codexHooksFile)}
	if changes, warn := ws.register(t); len(changes) != 0 || warn != "" {
		t.Fatalf("a repeated run changed %+v (%q)", changes, warn)
	}
	if after := [2]string{ws.read(t, claudeHooksFile), ws.read(t, codexHooksFile)}; after != before {
		t.Fatal("a repeated run rewrote a hook file")
	}
}

// The merge table: what a run does to a Claude Code file in each state it can
// find one in. Each case states the file before and after, byte for byte.
func TestHostHooksMergeTable(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hooks-ownership-by-command", "putnami-handlers-are-replaced-in-place")
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hooks-ownership-by-command", "the-person-s-entries-keep-their-bytes")

	ours := func(launcher, args string, timeout int) string {
		return fmt.Sprintf(`{
        "hooks": [
          {
            "type": "command",
            "command": "%s%s agent hook %s || true",
            "timeout": %d
          }
        ]
      }`, hookEnv, jsonEscape(launcher), args, timeout)
	}
	personStop := `{
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "say done",
            "timeout": 5
          }
        ]
      }`
	wake := `{
        "hooks": [
          {
            "type": "command",
            "command": "PUTNAMI_NO_AUTO_INSTALL=1 putnami agent hook --engine claude-code --idle-wake; s=$?; [ \"$s\" -eq 75 ] && exit 2; exit 0",
            "timeout": 660,
            "asyncRewake": true
          }
        ]
      }`
	postToolUse := `{
        "matcher": "TodoWrite|TaskCreate",
        "hooks": [
          {
            "type": "command",
            "command": "PUTNAMI_NO_AUTO_INSTALL=1 putnami agent hook --engine claude-code || true",
            "timeout": 30
          }
        ]
      }`
	settings := func(sessionStart, stop string) string {
		return `{
  "permissions": {
    "allow": ["Bash(git:*)"]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [{"type": "command", "command": "guard.sh"}]
      }
    ],
    "SessionStart": [
      ` + sessionStart + `
    ],
    "PostToolUse": [
      ` + postToolUse + `
    ],
    "Stop": [
      ` + stop + `
    ]
  },
  "env": {"A": "1"}
}
`
	}
	current := settings(ours("putnami", "--engine claude-code", 30), personStop+",\n      "+wake)

	for _, tc := range []struct {
		name   string
		before string
		want   string
	}{
		{
			name:   "ours current",
			before: current,
			want:   current,
		},
		{
			name:   "ours stale after a CLI change",
			before: settings(ours("putnami", "--engine claude", 10), personStop+",\n      "+strings.Replace(wake, "--idle-wake", "--wake", 1)),
			want:   current,
		},
		{
			name: "ours stale after a launcher change",
			before: settings(ours("./putnamiw", "--engine claude-code", 30), personStop+",\n      "+
				strings.Replace(wake, "putnami agent", `\"$CLAUDE_PROJECT_DIR\"/putnamiw agent`, 1)),
			want: current,
		},
		{
			name:   "ours from a CLI that let a hook install",
			before: strings.ReplaceAll(current, hookEnv, ""),
			want:   current,
		},
		{
			name: "ours written twice",
			before: settings(ours("putnami", "--engine claude-code", 30)+",\n      "+ours("putnami", "--engine claude-code", 30),
				personStop+",\n      "+wake),
			want: current,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := newHostHooksWorkspace(t, true, true)
			writePathFile(t, ws.root, claudeHooksFile, tc.before)
			changes, warn := ws.register(t)
			if warn != "" {
				t.Fatalf("warnings = %q", warn)
			}
			if got := ws.read(t, claudeHooksFile); got != tc.want {
				t.Fatalf("Claude Code file:\n%s\nwant:\n%s", got, tc.want)
			}
			changed := tc.before != tc.want
			for _, change := range changes {
				if change.File == claudeHooksFile && !changed {
					t.Fatalf("a current file was reported as %s", change.Action)
				}
			}
		})
	}
}

// The person's groups, events and keys keep their text and place, in a file
// indented with four spaces too; ours are added after them.
func TestHostHooksKeepThePersonsEntriesByteForByte(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hooks-ownership-by-command", "the-person-s-entries-keep-their-bytes")
	ws := newHostHooksWorkspace(t, true, true)
	person := `{
    "theme": "dark",
    "hooks": {
        "Stop": [
            {
                "hooks": [{"type": "command", "command": "other-tool session hook || true", "timeout": 30}]
            }
        ],
        "Notification": [{"hooks": [{"type": "command", "command": "notify"}]}]
    }
}
`
	writePathFile(t, ws.root, claudeHooksFile, person)
	if _, warn := ws.register(t); warn != "" {
		t.Fatalf("warnings = %q", warn)
	}
	want := `{
    "theme": "dark",
    "hooks": {
        "Stop": [
            {
                "hooks": [{"type": "command", "command": "other-tool session hook || true", "timeout": 30}]
            },
            {
                "hooks": [
                    {
                        "type": "command",
                        "command": "PUTNAMI_NO_AUTO_INSTALL=1 putnami agent hook --engine claude-code --idle-wake; s=$?; [ \"$s\" -eq 75 ] && exit 2; exit 0",
                        "timeout": 660,
                        "asyncRewake": true
                    }
                ]
            }
        ],
        "Notification": [{"hooks": [{"type": "command", "command": "notify"}]}],
        "SessionStart": [
            {
                "hooks": [
                    {
                        "type": "command",
                        "command": "PUTNAMI_NO_AUTO_INSTALL=1 putnami agent hook --engine claude-code || true",
                        "timeout": 30
                    }
                ]
            }
        ],
        "PostToolUse": [
            {
                "matcher": "TodoWrite|TaskCreate",
                "hooks": [
                    {
                        "type": "command",
                        "command": "PUTNAMI_NO_AUTO_INSTALL=1 putnami agent hook --engine claude-code || true",
                        "timeout": 30
                    }
                ]
            }
        ]
    }
}
`
	if got := ws.read(t, claudeHooksFile); got != want {
		t.Fatalf("Claude Code file:\n%s\nwant:\n%s", got, want)
	}
}

// A group that holds a handler of the person beside ours keeps the person's,
// with its matcher; ours moves to its own group right after it.
func TestHostHooksSplitAMixedGroup(t *testing.T) {
	ws := newHostHooksWorkspace(t, true, true)
	writePathFile(t, ws.root, claudeHooksFile, `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {"type": "command", "command": "putnami agent hook --engine claude-code || true", "timeout": 30},
          {"type": "command", "command": "mine"}
        ]
      }
    ]
  }
}
`)
	if _, warn := ws.register(t); warn != "" {
		t.Fatalf("warnings = %q", warn)
	}
	got := ws.read(t, claudeHooksFile)
	want := `"SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {"type": "command", "command": "mine"}
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "PUTNAMI_NO_AUTO_INSTALL=1 putnami agent hook --engine claude-code || true",
            "timeout": 30
          }
        ]
      }
    ]`
	if !strings.Contains(got, want) {
		t.Fatalf("Claude Code file:\n%s\nwant it to hold:\n%s", got, want)
	}
}

// A file the run cannot read safely is left as it is, with a warning that
// names it; the other host's file is still written.
func TestHostHooksLeaveAnUnusableFileUntouched(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hooks-ownership-by-command", "an-unusable-file-is-left-untouched")
	for _, tc := range []struct {
		name, content, reason string
	}{
		{"unparseable", `{"hooks": {`, "not valid JSON"},
		{"not an object", `[]`, "not a JSON object"},
		{"hooks twice", `{"hooks": {}, "hooks": {}}`, "declares hooks more than once"},
		{"hooks not an object", `{"hooks": []}`, "hooks member is not an object"},
		{"event not an array", `{"hooks": {"Stop": {}}}`, "hooks.Stop member is not an array"},
		{"event twice", `{"hooks": {"Stop": [], "Stop": []}}`, "hooks.Stop more than once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := newHostHooksWorkspace(t, true, true)
			writePathFile(t, ws.root, claudeHooksFile, tc.content)
			changes, warn := ws.register(t)
			if got := ws.read(t, claudeHooksFile); got != tc.content {
				t.Fatalf("the file was rewritten: %q", got)
			}
			if !strings.Contains(warn, claudeHooksFile) || !strings.Contains(warn, tc.reason) {
				t.Fatalf("warning = %q, want it to name %s and say %q", warn, claudeHooksFile, tc.reason)
			}
			if len(changes) != 1 || changes[0].File != codexHooksFile {
				t.Fatalf("changes = %+v, want only the Codex file", changes)
			}
		})
	}

	t.Run("a link", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need privileges on Windows")
		}
		ws := newHostHooksWorkspace(t, true, true)
		target := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(target, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(ws.root, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(ws.root, ".claude", "settings.json")); err != nil {
			t.Fatal(err)
		}
		_, warn := ws.register(t)
		if !strings.Contains(warn, "not a regular file") {
			t.Fatalf("warning = %q", warn)
		}
		if data, _ := os.ReadFile(target); string(data) != "{}\n" {
			t.Fatalf("the link target was written: %q", data)
		}
	})
}

// Without the opt-in or the binding, nothing is written; and the hooks a
// workspace had are removed, with the person's entries kept. A file that held
// only ours is deleted.
func TestHostHooksRetireWithTheOptInOrTheBinding(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hooks-written-on-explicit-lifecycle", "no-opt-in-or-no-binding-writes-nothing")
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hooks-ownership-by-command", "retirement-removes-putnami-handlers")
	for _, tc := range []struct {
		name           string
		optedIn, bound bool
	}{
		{"not opted in", false, true},
		{"unbound", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh := newHostHooksWorkspace(t, tc.optedIn, tc.bound)
			if changes, warn := fresh.register(t); len(changes) != 0 || warn != "" {
				t.Fatalf("changes = %+v (%q), want none", changes, warn)
			}
			if fresh.exists(claudeHooksFile) || fresh.exists(codexHooksFile) {
				t.Fatal("a hook file was created")
			}

			ws := newHostHooksWorkspace(t, true, true)
			writePathFile(t, ws.root, claudeHooksFile, `{
  "env": {"A": "1"},
  "hooks": {
    "Stop": [
      {
        "hooks": [{"type": "command", "command": "say done"}]
      }
    ]
  }
}
`)
			if _, warn := ws.register(t); warn != "" {
				t.Fatalf("warnings = %q", warn)
			}
			ws.optedIn, ws.bound = tc.optedIn, tc.bound
			ws.writeConfig(t)
			changes, warn := ws.register(t)
			if warn != "" {
				t.Fatalf("warnings = %q", warn)
			}
			want := `{
  "env": {"A": "1"},
  "hooks": {
    "Stop": [
      {
        "hooks": [{"type": "command", "command": "say done"}]
      }
    ]
  }
}
`
			if got := ws.read(t, claudeHooksFile); got != want {
				t.Fatalf("Claude Code file after retirement:\n%s\nwant:\n%s", got, want)
			}
			if ws.exists(codexHooksFile) {
				t.Fatal("the Codex file that held only Putnami's hooks was kept")
			}
			if len(changes) != 2 || changes[0].Action != HostHookFileUpdated || changes[1].Action != HostHookFileRemoved {
				t.Fatalf("changes = %+v", changes)
			}
		})
	}
}

// An opted-in extension whose hooks cannot be read keeps the entries it has,
// and the run says why. A collaboration block written twice binds nothing.
func TestHostHooksKeepEntriesTheyCannotReadAgain(t *testing.T) {
	ws := newHostHooksWorkspace(t, true, true)
	if _, warn := ws.register(t); warn != "" {
		t.Fatal(warn)
	}
	before := ws.read(t, claudeHooksFile)
	manifest := filepath.Join(ws.root, "ext", "sessions", "putnami.extension.json")
	broken := strings.Replace(hostHooksExtensionManifest(hostHooksDeclaration), `"interactive": true`, `"interactive": false`, 1)
	if err := os.WriteFile(manifest, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	changes, warn := ws.register(t)
	if len(changes) != 0 || !strings.Contains(warn, "the host hooks of "+hostHooksExtension+" are not written") ||
		!strings.Contains(warn, "must be interactive") {
		t.Fatalf("changes = %+v, warning = %q", changes, warn)
	}
	if got := ws.read(t, claudeHooksFile); got != before {
		t.Fatal("hooks were changed for an extension whose declaration cannot be used")
	}

	twice := newHostHooksWorkspace(t, true, true)
	writePathFile(t, twice.root, wsproto.WorkspaceConfigFilename,
		`{"options":{"collaboration":{"memory":{"provider":"@acme/memory","version":1}},"collaboration":{}}}`)
	if changes, _ := twice.register(t); len(changes) != 0 {
		t.Fatalf("an ambiguous binding wrote %+v", changes)
	}
}

// A workspace in which no extension declares hooks never reads the files, so
// an unparseable one draws no warning.
func TestHostHooksNeedADeclaringExtension(t *testing.T) {
	root := t.TempDir()
	writePathFile(t, root, claudeHooksFile, "{")
	var warn bytes.Buffer
	if changes := RegisterHostHooks(root, &wsproto.Config{}, &warn); len(changes) != 0 || warn.Len() != 0 {
		t.Fatalf("changes = %+v, warning = %q", changes, warn.String())
	}
	if changes := RegisterHostHooks(root, nil, &warn); len(changes) != 0 || warn.Len() != 0 {
		t.Fatalf("a nil config wrote %+v (%q)", changes, warn.String())
	}
}

// A dry run reports the change and writes nothing.
func TestPlanHostHooksWritesNothing(t *testing.T) {
	ws := newHostHooksWorkspace(t, true, true)
	changes := PlanHostHooks(ws.root, ws.config(), nil)
	if len(changes) != 2 || ws.exists(claudeHooksFile) || ws.exists(codexHooksFile) {
		t.Fatalf("plan = %+v; files written: %v %v", changes, ws.exists(claudeHooksFile), ws.exists(codexHooksFile))
	}
}

// A source workspace runs its own putnamiw from the root each host provides,
// and a hook written by either launcher is recognized as Putnami's.
func TestHostHooksLauncherFollowsTheWorkspaceSource(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hooks-exit-guard", "a-source-workspace-runs-its-putnamiw")
	ws := newHostHooksWorkspace(t, true, true)
	if _, warn := ws.register(t); warn != "" {
		t.Fatal(warn)
	}
	writeCLILock(t, ws.root, lockfile.LockEntry{Source: lockfile.SourceWorkspace})
	writePutnamiw(t, ws.root, 0o755)
	if _, warn := ws.register(t); warn != "" {
		t.Fatal(warn)
	}
	if got, want := ws.read(t, claudeHooksFile), wantClaudeHooks(`"$CLAUDE_PROJECT_DIR"/putnamiw`); got != want {
		t.Fatalf("Claude Code hooks:\n%s\nwant:\n%s", got, want)
	}
	if got, want := ws.read(t, codexHooksFile), wantCodexHooks(`"$(git rev-parse --show-toplevel)"/putnamiw`); got != want {
		t.Fatalf("Codex hooks:\n%s\nwant:\n%s", got, want)
	}

	// A lock that cannot be read cannot rule a source workspace out.
	writePathFile(t, ws.root, lockfile.LockFilename, "{")
	changes, warn := ws.register(t)
	if len(changes) != 0 || !strings.Contains(warn, lockfile.LockFilename) || strings.Contains(warn, mcpConfigPath) {
		t.Fatalf("changes = %+v, warning = %q", changes, warn)
	}
}

// Claude Code matchers are alternations; Codex matchers are anchored regular
// expressions.
func TestHostHookMatcherRendering(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hooks-exit-guard", "matchers-follow-each-host")
	entry := extproto.HostHookEntry{Event: "PostToolUse", Tools: []string{"TodoWrite", "TaskCreate"}}
	if got, want := hostHookMatcher(extproto.HostClaudeCode, entry), "TodoWrite|TaskCreate"; got != want {
		t.Errorf("Claude Code matcher = %q, want %q", got, want)
	}
	if got, want := hostHookMatcher(extproto.HostCodex, entry), "^(TodoWrite|TaskCreate)$"; got != want {
		t.Errorf("Codex matcher = %q, want %q", got, want)
	}
	if got := hostHookMatcher(extproto.HostCodex, extproto.HostHookEntry{Event: "Stop"}); got != "" {
		t.Errorf("a hook without tools has matcher %q", got)
	}
}

// A handler is Putnami's when it runs a known launcher and a declared path,
// word for word, as a command: after NAME=value words, a shell operator or a
// reserved word, never as the argument of another command.
func TestHostHookOwnershipByCommand(t *testing.T) {
	paths := [][]string{{"agent", "hook"}}
	for command, want := range map[string]bool{
		"putnami agent hook":                                      true,
		"putnami agent hook --engine codex || true":               true,
		"./putnamiw agent hook --engine codex":                    true,
		`"$CLAUDE_PROJECT_DIR"/putnamiw agent hook; s=$?; exit 0`: true,
		`"$(git rev-parse --show-toplevel)"/putnamiw agent hook`:  true,
		"cd x && putnami agent hook":                              true,
		"PUTNAMI_NO_AUTO_INSTALL=1 putnami agent hook || true":    true,
		"A=1  B= ./putnamiw agent hook":                           true,
		"if true; then putnami agent hook; fi":                    true,
		"(putnami agent hook)":                                    true,
		"echo putnami agent hook":                                 false,
		"echo 'putnami agent hook'":                               false,
		"say done; echo run putnami agent hook":                   false,
		"A=1 echo putnami agent hook":                             false,
		"A=$(x) putnami agent hook":                               false,
		"xputnami agent hook":                                     false,
		"ifputnami agent hook":                                    false,
		"putnami agent hooks":                                     false,
		"putnami agent hook-x":                                    false,
		"putnami agents hook":                                     false,
		"/usr/local/bin/putnami agent hook":                       false,
		"putnamiw agent hook":                                     false,
		"other-tool session hook || true":                         false,
		"":                                                        false,
	} {
		if got := ownsHostHookCommand(command, paths); got != want {
			t.Errorf("ownsHostHookCommand(%q) = %v, want %v", command, got, want)
		}
	}
}

// The guards, run by a POSIX shell against a stub launcher: an ordinary hook
// always exits 0; a wake hook exits 2 only when the command exits 75.
func TestHostHookGuardsMapExitCodes(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hooks-exit-guard", "only-exit-75-wakes")
	if runtime.GOOS == "windows" {
		t.Skip("the hosts run hooks through a POSIX shell")
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX shell")
	}
	stubDir := t.TempDir()
	// The stub records whether the command turned the first-use install off.
	stub := "#!/bin/sh\nprintf '%s' \"${PUTNAMI_NO_AUTO_INSTALL-unset}\" > \"$STUB_SEEN\"\nexit \"$STUB_EXIT\"\n"
	seen := filepath.Join(t.TempDir(), "seen")
	if err := os.WriteFile(filepath.Join(stubDir, "putnami"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	path := []string{"agent", "hook"}
	wake := renderHostHookGroup(extproto.HostClaudeCode, "putnami", path,
		extproto.HostHookEntry{Event: "Stop", Args: []string{"--idle-wake"}, Wake: true, TimeoutSeconds: 660}).Hooks[0]
	if want := `PUTNAMI_NO_AUTO_INSTALL=1 putnami agent hook --idle-wake; s=$?; [ "$s" -eq 75 ] && exit 2; exit 0`; wake.Command != want || !wake.AsyncRewake {
		t.Fatalf("wake handler = %+v, want command %q with asyncRewake", wake, want)
	}
	ordinary := renderHostHookGroup(extproto.HostCodex, "putnami", path,
		extproto.HostHookEntry{Event: "Stop", TimeoutSeconds: 240}).Hooks[0]
	if want := "PUTNAMI_NO_AUTO_INSTALL=1 putnami agent hook || true"; ordinary.Command != want || ordinary.AsyncRewake {
		t.Fatalf("ordinary handler = %+v, want command %q", ordinary, want)
	}
	for _, tc := range []struct {
		command   string
		exit      int
		wantCode  int
		guardName string
	}{
		{wake.Command, 75, 2, "wake"},
		{wake.Command, 2, 0, "wake"},
		{wake.Command, 1, 0, "wake"},
		{wake.Command, 0, 0, "wake"},
		{ordinary.Command, 75, 0, "ordinary"},
		{ordinary.Command, 2, 0, "ordinary"},
		{ordinary.Command, 1, 0, "ordinary"},
		{ordinary.Command, 0, 0, "ordinary"},
	} {
		_ = os.Remove(seen)
		cmd := exec.Command(shell, "-c", tc.command)
		// The test's own variables come last, so they win over inherited ones.
		env := make([]string, 0, len(os.Environ())+3)
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, shared.NoAutoInstallEnv+"=") {
				env = append(env, kv)
			}
		}
		env = append(env, "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			fmt.Sprintf("STUB_EXIT=%d", tc.exit), "STUB_SEEN="+seen)
		cmd.Env = env
		err := cmd.Run()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != tc.wantCode {
			t.Errorf("%s hook, command exits %d: hook exits %d, want %d", tc.guardName, tc.exit, code, tc.wantCode)
		}
		if got, err := os.ReadFile(seen); err != nil || string(got) != "1" {
			t.Errorf("%s hook: the launcher saw %s %q (%v), want 1", tc.guardName, shared.NoAutoInstallEnv, got, err)
		}
	}
}
