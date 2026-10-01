package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

func runCompose(t *testing.T, env *CommandEnv) error {
	t.Helper()
	command, ok := lookupCommand("compose")
	if !ok {
		t.Fatal("compose command is not registered")
	}
	if !command.rawMachineOutput {
		t.Fatal("compose must forward its own documents: its start document streams before the composition runs")
	}
	return command.run(env)
}

func TestCmdCompose_TakesExactlyOneProject(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "runs-with-closure-served-behind-stable-proxies",
		"an-unserveable-member-is-refused-before-anything-starts")
	wsRoot := structuredOutputWorkspace(t)
	cfg := wsproto.Load(wsRoot)
	// compose is a positional leaf (commandmeta.PositionalLeaf): the parser
	// leaves its project in the arguments, never in the subcommand slot.
	for _, env := range []*CommandEnv{
		{Ctx: context.Background(), Cfg: cfg, WsRoot: wsRoot},
		{Ctx: context.Background(), Cfg: cfg, WsRoot: wsRoot, Args: []string{"api", "shared"}},
		{Ctx: context.Background(), Cfg: cfg, WsRoot: wsRoot, Args: []string{"--port", "0"}},
	} {
		err := runCompose(t, env)
		if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage {
			t.Errorf("compose args=%v: %v, want a usage error", env.Args, err)
		}
	}
	err := runCompose(t, &CommandEnv{Ctx: context.Background(), Cfg: cfg, WsRoot: wsRoot, Args: []string{"@acme/missing"}})
	if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage || !strings.Contains(err.Error(), "@acme/missing") {
		t.Errorf("an unknown project: %v, want a usage error naming it", err)
	}
	err = runCompose(t, &CommandEnv{Ctx: context.Background(), Cfg: cfg, WsRoot: wsRoot, Args: []string{"api", "--port", "nope"}})
	if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage || !strings.Contains(err.Error(), "--port") {
		t.Errorf("an invalid --port: %v, want a usage error", err)
	}
	err = runCompose(t, &CommandEnv{Ctx: context.Background(), Cfg: cfg, WsRoot: wsRoot, Args: []string{"api", "--ready-timeout", "-1s"}})
	if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage || !strings.Contains(err.Error(), "--ready-timeout") {
		t.Errorf("an invalid --ready-timeout: %v, want a usage error", err)
	}
	if err := runCompose(t, &CommandEnv{Ctx: context.Background(), Cfg: cfg, Args: []string{"api"}}); err == nil {
		t.Error("compose outside a workspace succeeded")
	}
}

// `compose` is a positional leaf: the parser keeps its project among the
// arguments, out of the subcommand slot. Every catalog lookup keyed by that
// invocation — structured-output support, flag validation, the failure
// envelope's command — must still resolve `compose`.
func TestCompose_TheProjectPositionalIsNotASubcommand(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"compose", "@example/go-items-consumer", "--port", "0", "--no-watch", "--output=json"}, nil, nil)
	if parsed.Err != nil {
		t.Fatalf("ParseArgs: %v", parsed.Err)
	}
	if parsed.Subcommand != "" || len(parsed.RawJobArgs) == 0 || parsed.RawJobArgs[0] != "@example/go-items-consumer" {
		t.Fatalf("subcommand slot = %q, args = %v; want the project as the first argument", parsed.Subcommand, parsed.RawJobArgs)
	}
	if got := commandmeta.CanonicalPath("compose", parsed.Subcommand); got != "compose" {
		t.Errorf("CanonicalPath = %q, want compose", got)
	}
	if code, rejected := rejectStructuredIfUnsupported("json", "compose", parsed.Subcommand); rejected {
		t.Errorf("--output=json was rejected for compose (exit %d)", code)
	}
	if bad := ParseArgs([]string{"compose", "@example/go-items-consumer", "--bogus"}, nil, nil); bad.Err == nil {
		t.Error("an undeclared compose flag was accepted")
	}
}

func TestCmdCompose_FailureEnvelopeCarriesCodeMemberAndPhase(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "typed-readiness-bounds-every-member",
		"a-member-that-never-announces-readiness-fails-naming-member-and-phase")
	wsRoot := structuredOutputWorkspace(t)
	cfg := wsproto.Load(wsRoot)
	// `shared` is a library: compose refuses it while planning, before any
	// engine run, process, proxy or database.
	parsed := ParseArgs([]string{"compose", "shared", "--output=json"}, cfg.Aliases, nil)
	if parsed.Err != nil {
		t.Fatalf("ParseArgs: %v", parsed.Err)
	}
	app := &App{}
	code := ExitSuccess
	out := captureStdout(t, func() {
		code = app.runStructuredCommand(context.Background(), parsed, cfg, wsRoot)
	})
	if code != protocolcli.ExitFailure {
		t.Fatalf("exit = %d, want %d; stdout:\n%s", code, protocolcli.ExitFailure, out)
	}
	var envelope struct {
		Command string `json:"command"`
		Status  string `json:"status"`
		Data    struct {
			Code   string `json:"code"`
			Member string `json:"member"`
			Phase  string `json:"phase"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("failure envelope is not one JSON document: %v\n%s", err, out)
	}
	if envelope.Command != "compose" || envelope.Status != "failure" ||
		envelope.Data.Code != "compose.not_serveable" || envelope.Data.Member != "/libs/shared" || envelope.Data.Phase != "plan" {
		t.Errorf("envelope = %+v", envelope)
	}
}
