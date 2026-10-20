package completion

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
)

// writeExtensionWorkspace writes a workspace whose @acme/flow extension
// declares the given commands, each running one cache-free command task.
func writeExtensionWorkspace(t *testing.T, commands ...string) string {
	t.Helper()
	return writeFlowWorkspace(t, commands, "")
}

// writeGroupWorkspace writes a workspace whose @acme/flow extension declares
// only the command group name, which carries the one command name-run: it
// declares no job command a user types.
func writeGroupWorkspace(t *testing.T, name string) string {
	t.Helper()
	groups := `, "commandGroups": {"` + name + `": {"description": "Group.", "subcommands": {"run": {"description": "Run.", "command": "` + name + `-run"}}}}`
	return writeFlowWorkspace(t, []string{name + "-run"}, groups)
}

func writeFlowWorkspace(t *testing.T, commands []string, groups string) string {
	t.Helper()
	dir := t.TempDir()
	declared := make([]string, 0, len(commands))
	for _, name := range commands {
		declared = append(declared, `"`+name+`": {"description": "Run.", "run": [{"id": "r", "task": "t"}]}`)
	}
	files := map[string]string{
		"putnami.workspace.json":      `{"name": "job-flags-ws", "includes": ["flow"]}`,
		"flow/putnami.json":           `{"name": "@acme/flow"}`,
		"flow/putnami.extension.json": `{"name": "@acme/flow", "cliContract": 4, "commands": {` + strings.Join(declared, ", ") + `}, "tasks": {"t": {"kind": "command", "command": "echo", "cache": false}}` + groups + `}`,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// jobOnlyFlagNames returns the flags that only extension commands read.
func jobOnlyFlagNames() []string {
	var names []string
	for _, flag := range commandmeta.GlobalFlags() {
		if flag.JobOnly {
			names = append(names, flag.Long)
		}
	}
	return names
}

func TestCompletionOffersTheJobFlagsOnlyWithAJobCommand(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "job-flags-follow-job-commands", "help-and-completion-hide-the-job-flags-where-no-job-command-is-loaded")
	jobFlags := jobOnlyFlagNames()
	if len(jobFlags) == 0 {
		t.Fatal("the catalog marks no flag that only extension commands read")
	}

	withJob := writeExtensionWorkspace(t, "lint-docs")
	cases := []struct {
		name    string
		wsRoot  string
		offered bool
	}{
		{"outside a workspace", "", false},
		{"a workspace with no extension", writeCompletionWorkspace(t), false},
		{"an extension that declares only a provider command", writeExtensionWorkspace(t, cache.ProviderCommandName), false},
		{"an extension that declares a command group that a core root shadows", writeGroupWorkspace(t, "projects"), false},
		{"an extension that declares a job command", withJob, true},
		{"an extension that declares only a command group", writeGroupWorkspace(t, "ship"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := gatherCompletionContext(c.wsRoot, &wsproto.Config{}, nil)
			names := ctx.globalFlagNames()
			for _, flag := range jobFlags {
				if got := sharedtest.Contains(names, flag); got != c.offered {
					t.Errorf("global flags offer %s = %v, want %v", flag, got, c.offered)
				}
			}
			// What the core reads stays, with the flag hosted launchers read from help.
			for _, flag := range []string{"--credential-fd", "--no-cache", "--projects", "--dry-run", "--output", "--help", "--version"} {
				if !sharedtest.Contains(names, flag) {
					t.Errorf("global flags miss %s, which no job command is needed to read", flag)
				}
			}

			for shell, generate := range map[string]func(*bytes.Buffer){
				"bash": func(b *bytes.Buffer) { CompletionBash(b, c.wsRoot, &wsproto.Config{}, nil) },
				"zsh":  func(b *bytes.Buffer) { CompletionZsh(b, c.wsRoot, &wsproto.Config{}, nil) },
				"fish": func(b *bytes.Buffer) { CompletionFish(b, c.wsRoot, &wsproto.Config{}, nil) },
			} {
				var script bytes.Buffer
				generate(&script)
				if got := strings.Contains(script.String(), "max-parallel"); got != c.offered {
					t.Errorf("%s script offers --max-parallel = %v, want %v", shell, got, c.offered)
				}
			}
		})
	}
}
