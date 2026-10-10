package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/cli"
)

// TestRuntimeHandshakeAnswersBeforeDispatch pins the ordering main depends on.
//
// Core prepares this binary and immediately asks it who it is, with
// `__putnami runtime-info` and NO --putnamiContext. cli.RunSubcommand would
// reject that as a usage error and exit the process before a descriptor was
// ever written, so the handshake has to be answered first — and the dispatcher
// must not run at all when it is.
func TestRuntimeHandshakeAnswersBeforeDispatch(t *testing.T) {
	var stdout bytes.Buffer
	dispatched := false

	code, err := runEntrypoint([]string{"__putnami", "runtime-info"}, nil, &stdout, io.Discard, func(map[string]cli.JobFunc) {
		dispatched = true
	})
	if err != nil {
		t.Fatalf("runtime handshake: %v", err)
	}
	if code != 0 {
		t.Errorf("runtime handshake exit code = %d, want 0", code)
	}
	if dispatched {
		t.Fatal("the subcommand dispatcher ran during the handshake; it would have demanded a --putnamiContext core does not send")
	}

	var info runtimeproto.Info
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		t.Fatalf("decode runtime descriptor %q: %v", stdout.String(), err)
	}
	if info.Extension != extensionName {
		t.Errorf("descriptor extension = %q, want %q; core matches it against the manifest that spawned this binary", info.Extension, extensionName)
	}
	// The runtime reports the base contract, which core checks exactly. The
	// manifest's stamp is that base or an additive rung above it: the sdd
	// manifest declares `git:` inputs, so it carries rung 7.
	if info.CLIContract != protocolcli.CurrentContract {
		t.Errorf("descriptor cliContract = %d, want the base contract %d core checks", info.CLIContract, protocolcli.CurrentContract)
	}
	if stamp := contractOfCommittedManifest(t); stamp < info.CLIContract || stamp > protocolcli.LatestContract {
		t.Errorf("manifest cliContract = %d, want from the runtime's %d up to the latest %d",
			stamp, info.CLIContract, protocolcli.LatestContract)
	}
}

// TestNonHandshakeArgsReachTheDispatcher is the other half of the ordering: an
// ordinary subcommand must NOT be swallowed by the handshake check, and it must
// see the real dispatch table.
func TestNonHandshakeArgsReachTheDispatcher(t *testing.T) {
	for _, args := range [][]string{
		{"selfcheck", "--putnamiContext", "ctx.json"},
		{"__putnami"},
		{"__putnami", "runtime-info", "extra"},
		nil,
	} {
		var stdout bytes.Buffer
		var got map[string]cli.JobFunc
		_, err := runEntrypoint(args, nil, &stdout, io.Discard, func(commands map[string]cli.JobFunc) { got = commands })
		if err != nil {
			t.Fatalf("args %v: %v", args, err)
		}
		if got == nil {
			t.Errorf("args %v never reached the dispatcher", args)
		}
		if stdout.Len() != 0 {
			t.Errorf("args %v wrote %q to stdout before dispatch; a job owns its own stream", args, stdout.String())
		}
	}
}

// TestDispatchTableMatchesTheManifest is the seam check jobs, command groups,
// and MCP tools extend.
//
// Every task in putnami.extension.json whose command is the prepared runtime
// selects its work with its own leading arguments, so a manifest that names
// work this binary does not handle is a task that fails at run time in a
// consumer's workspace with "Unknown command". Asserting both directions keeps
// the tables and the manifest from drifting apart in either one: a handler with
// no task is dead code, a task with no handler is a broken command.
//
// There are TWO tables since command groups arrived, and the manifest says which one a task
// belongs to by how many arguments it passes: one argument names a job
// (`specs-validate`), two name an interactive command path (`specs validate`).
// The distinction is not cosmetic — the first writes JSONL the CLI parses, the
// second writes the command's own document to the terminal.
func TestDispatchTableMatchesTheManifest(t *testing.T) {
	m := committedManifest(t)

	jobs, interactive := map[string]string{}, map[string]string{}
	for name, task := range m.Tasks {
		if task.Command != "{extensionRuntime}" {
			continue
		}
		switch len(task.Args) {
		case 0:
			t.Errorf("task %q runs the extension runtime with no arguments; it selects no work", name)
		case 1:
			jobs[task.Args[0]] = name
		case 2:
			interactive[task.Args[0]+" "+task.Args[1]] = name
		default:
			t.Errorf("task %q passes %d arguments; a job takes one and a command path takes two", name, len(task.Args))
		}
	}

	handlers := commandHandlers()
	for subcommand, task := range jobs {
		if _, ok := handlers[subcommand]; !ok {
			t.Errorf("task %q dispatches to job %q, which this binary does not handle", task, subcommand)
		}
	}
	for subcommand := range handlers {
		if _, ok := jobs[subcommand]; !ok {
			t.Errorf("job %q has a handler but no manifest task runs it", subcommand)
		}
	}

	subcommands := interactiveSubcommands()
	for path, task := range interactive {
		if _, ok := subcommands[path]; !ok {
			t.Errorf("task %q dispatches to command %q, which this binary does not handle", task, path)
		}
	}
	for path := range subcommands {
		if _, ok := interactive[path]; !ok {
			t.Errorf("command %q has a handler but no manifest task runs it", path)
		}
	}

	if len(handlers) == 0 || len(subcommands) == 0 {
		t.Fatal("a dispatch table is empty; this assertion would pass vacuously")
	}
}

// TestInteractiveDispatchCoversTheManifestGroups closes the last gap between
// the two: a manifest that declares `putnami features list` must reach a
// handler THROUGH its flat command and task, not merely have a same-named entry
// in the table above.
func TestInteractiveDispatchCoversTheManifestGroups(t *testing.T) {
	m := committedManifest(t)
	subcommands := interactiveSubcommands()

	declared := 0
	for group, definition := range m.CommandGroups {
		for name, sub := range definition.Subcommands {
			declared++
			path := group + " " + name
			entry, handled := subcommands[path]
			if !handled {
				t.Errorf("manifest declares %q with no handler in this binary", path)
				continue
			}
			if entry.group != group || entry.name != name {
				t.Errorf("handler for %q reports itself as %q %q", path, entry.group, entry.name)
			}
			flat, found := m.Commands[sub.Command]
			if !found || len(flat.Run) != 1 {
				t.Errorf("%q routes to %q, which is not a one-step command", path, sub.Command)
				continue
			}
			task := m.Tasks[flat.Run[0].Task]
			if len(task.Args) != 2 || task.Args[0] != group || task.Args[1] != name {
				t.Errorf("%q runs task %q with args %v, want [%s %s]", path, flat.Run[0].Task, task.Args, group, name)
			}
		}
	}
	if declared != len(subcommands) {
		t.Errorf("the manifest declares %d subcommands and this binary handles %d", declared, len(subcommands))
	}
}

// TestHandlerNamesAreStable states, as a table a reviewer reads, what this
// binary can do today. New work adds rows here; nothing should ever remove one
// silently.
func TestHandlerNamesAreStable(t *testing.T) {
	got := make([]string, 0, len(commandHandlers()))
	for name := range commandHandlers() {
		got = append(got, name)
	}
	sort.Strings(got)

	want := []string{"architecture-validate", "codeowners-sync", "decisions-validate", "docs-links-validate", "features-validate", "recipes-validate", "selfcheck", "specs-ratchet-validate", "specs-validate"}
	if len(got) != len(want) {
		t.Fatalf("handlers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("handler %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// committedManifest parses the extension manifest that ships beside this
// binary, two directories up.
func committedManifest(t *testing.T) *proto.Manifest {
	t.Helper()
	path := filepath.Join("..", "..", "putnami.extension.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	m, diags := proto.ParseManifest(data)
	if m == nil || diag.HasErrors(diags) {
		t.Fatalf("parse extension manifest: %v", diags)
	}
	return m
}

func contractOfCommittedManifest(t *testing.T) int {
	t.Helper()
	return committedManifest(t).CLIContract
}
