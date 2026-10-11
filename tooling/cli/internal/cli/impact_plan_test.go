package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const impactPlanTestBase = "0123456789012345678901234567890123456789"

func runImpactPlan(t *testing.T, env *CommandEnv) error {
	t.Helper()
	command, ok := lookupCommand("impact-plan")
	if !ok {
		t.Fatal("impact-plan command is not registered")
	}
	return command.run(env)
}

// TestCmdImpactPlanUsageErrors holds the command line to one command list and
// to --base as the only selection: every other shape is a usage error, raised
// before anything is planned.
func TestCmdImpactPlanUsageErrors(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "named-commands", "the-command-list-is-required")
	t.Parallel()
	wsRoot := structuredOutputWorkspace(t)
	cfg := wsproto.Load(wsRoot)
	env := func(args []string, global GlobalFlags) *CommandEnv {
		return &CommandEnv{Ctx: context.Background(), Cfg: cfg, WsRoot: wsRoot, Args: args, Global: global}
	}
	for _, tc := range []struct {
		name string
		env  *CommandEnv
		want string
	}{
		{"no command list", env([]string{"--base", impactPlanTestBase}, GlobalFlags{}), "impact-plan takes exactly one command list"},
		{"two command lists", env([]string{"lint", "test", "--base", impactPlanTestBase}, GlobalFlags{}), "impact-plan takes exactly one command list"},
		{"a subcommand slot", &CommandEnv{Ctx: context.Background(), Cfg: cfg, WsRoot: wsRoot, Sub: "lint", Args: []string{"test"}}, "impact-plan takes exactly one command list"},
		{"--baseline", env([]string{"lint"}, GlobalFlags{Baseline: "main"}), "impact-plan owns impact selection"},
		{"--impacted", env([]string{"lint"}, GlobalFlags{Impacted: true}), "impact-plan owns impact selection"},
		{"--projects", env([]string{"lint"}, GlobalFlags{Projects: "/apps/api"}), "impact-plan owns impact selection"},
		{"--all", env([]string{"lint"}, GlobalFlags{All: true}), "impact-plan owns impact selection"},
		{"a command named twice", env([]string{"lint,test,lint", "--base", impactPlanTestBase}, GlobalFlags{}), "impact-plan names lint twice"},
		{"an empty list", env([]string{",", "--base", impactPlanTestBase}, GlobalFlags{}), "no command in"},
		{"an unknown flag", env([]string{"lint", "--bse", impactPlanTestBase}, GlobalFlags{}), "--bse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runImpactPlan(t, tc.env)
			if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want a usage error containing %q", err, tc.want)
			}
		})
	}
	if err := runImpactPlan(t, &CommandEnv{Ctx: context.Background(), Args: []string{"lint", "--base", impactPlanTestBase}}); err == nil {
		t.Error("impact-plan outside a workspace succeeded")
	}
}

// TestImpactPlanCommandsResolveEachAliasToOneCommand holds the parsing of the
// command list to the rules of `putnami <command[,command...]>`, and refuses
// the lists a plan cannot name one command at a time.
func TestImpactPlanCommandsResolveEachAliasToOneCommand(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "named-commands", "aliases-resolve-to-one-command-each")
	t.Parallel()
	cfg := &wsproto.Config{Aliases: map[string]string{"check": "lint", "gate": "lint,test", "verify": "check"}}

	got, err := impactPlanCommands("t, l ,b", cfg)
	if err != nil || !reflect.DeepEqual(got, []string{"test", "lint", "build"}) {
		t.Fatalf("impactPlanCommands = %v, %v; want [test lint build]", got, err)
	}
	got, err = impactPlanCommands("verify,test,b", cfg)
	if err != nil || !reflect.DeepEqual(got, []string{"lint", "test", "build"}) {
		t.Fatalf("impactPlanCommands = %v, %v; want [lint test build] in the order named", got, err)
	}
	got, err = impactPlanCommands("validate,lint", nil)
	if err != nil || !reflect.DeepEqual(got, []string{"validate", "lint"}) {
		t.Fatalf("impactPlanCommands without a workspace config = %v, %v", got, err)
	}
	_, err = impactPlanCommands("l,check", cfg)
	if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage || !strings.Contains(err.Error(), `impact-plan names lint twice in "l,check"`) {
		t.Fatalf("two names of one command: %v, want a usage error naming lint", err)
	}
	_, err = impactPlanCommands("gate", cfg)
	if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage || !strings.Contains(err.Error(), `"lint,test" in "gate" is not one command`) {
		t.Fatalf("a multi-command alias: %v, want a usage error naming it", err)
	}
	_, err = impactPlanCommands("loop", &wsproto.Config{Aliases: map[string]string{"loop": "loop2", "loop2": "loop"}})
	if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("a cyclic alias: %v, want a usage error", err)
	}
}

// TestImpactPlanPlannerRoutesThroughEngine holds the planner to the commands
// it was built with: the engine plans exactly that list, in order, as an
// immutable impacted plan, and the result and its errors name that list.
func TestImpactPlanPlannerRoutesThroughEngine(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "named-commands", "the-planner-plans-the-named-commands")
	t.Parallel()
	root := structuredOutputWorkspace(t)
	workspace.InvalidateLoadCache(root)
	commands := []string{"test", "build"}
	var requested []string
	planner := newImpactPlanPlanner("impact-plan", root, &wsproto.Config{}, commands,
		func(_ context.Context, request engine.Request, _ engine.EventSink) (engine.SessionResult, error) {
			requested = request.Commands
			if request.WorkspaceRoot != root || !request.Global.Impacted || request.Global.Baseline != impactPlanTestBase ||
				request.Global.NoCache || !request.Global.Plan || !request.Global.Quiet {
				t.Fatalf("request = %#v, want an immutable impacted plan of the workspace", request)
			}
			return engine.SessionResult{ExitCode: engine.ExitError}, nil
		})
	commands[0] = "lint"

	result, err := planner(context.Background(), impactPlanTestBase, false)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !reflect.DeepEqual(requested, []string{"test", "build"}) || !reflect.DeepEqual(result.Commands, []string{"test", "build"}) {
		t.Fatalf("engine commands = %v, result commands = %v; want the list the planner was built with", requested, result.Commands)
	}
	if result.Complete {
		t.Fatal("a failed engine plan is reported complete")
	}

	engineErr := errors.New("engine refused the plan")
	failing := newImpactPlanPlanner("impact-plan", root, &wsproto.Config{}, []string{"lint"},
		func(context.Context, engine.Request, engine.EventSink) (engine.SessionResult, error) {
			return engine.SessionResult{}, engineErr
		})
	if _, err := failing(context.Background(), impactPlanTestBase, true); err == nil || err.Error() != "plan lint: engine refused the plan" || !errors.Is(err, engineErr) {
		t.Fatalf("error = %v, want the engine error naming lint", err)
	}

	complete := newImpactPlanPlanner("impact-plan", root, &wsproto.Config{}, []string{"test"},
		func(context.Context, engine.Request, engine.EventSink) (engine.SessionResult, error) {
			return engine.SessionResult{ExitCode: engine.ExitSuccess}, nil
		})
	result, err = complete(context.Background(), impactPlanTestBase, true)
	if err != nil || !result.Complete || !reflect.DeepEqual(result.Commands, []string{"test"}) {
		t.Fatalf("result = %+v, %v; want a complete plan of test", result, err)
	}
}

// TestImpactPlanRefusesACommandNoExtensionDeclares holds the run impact-plan
// plans with to the job map of the discovered extensions: a command none of
// them declares is a usage error naming it, before selection, and a declared
// command reaches the engine unchanged.
func TestImpactPlanRefusesACommandNoExtensionDeclares(t *testing.T) {
	spectest.Proves(t, "cli/impact-plan", "named-commands", "an-undeclared-command-is-refused")
	t.Parallel()
	discovered := &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{{
		Name: "@example/quality",
		Jobs: map[string]*extension.JobDefinition{
			"lint": {Name: "lint", ExtensionName: "@example/quality"},
			"test": {Name: "test", ExtensionName: "@example/quality"},
		},
	}}}
	validate := func(commands []string, discovered *extension.DiscoveryResult) error {
		t.Helper()
		var validator func(*extension.DiscoveryResult, GlobalFlags) error
		run := refuseUndeclaredCommands(commands, func(_ context.Context, request engine.Request, _ engine.EventSink) (engine.SessionResult, error) {
			if !reflect.DeepEqual(request.Commands, []string{"planned"}) || !request.Global.Plan {
				t.Fatalf("request = %#v, want the request of the planner unchanged", request)
			}
			validator = request.ValidateCommandFlags
			return engine.SessionResult{ExitCode: engine.ExitSuccess}, nil
		})
		if _, err := run(context.Background(), engine.Request{Commands: []string{"planned"}, Global: GlobalFlags{Plan: true}}, nil); err != nil {
			t.Fatalf("run: %v", err)
		}
		if validator == nil {
			t.Fatal("the impact-plan run sets no command validator")
		}
		return validator(discovered, GlobalFlags{})
	}

	for _, commands := range [][]string{{"lint"}, {"test", "lint"}} {
		if err := validate(commands, discovered); err != nil {
			t.Errorf("%v: %v, want the declared commands accepted", commands, err)
		}
	}
	for _, tc := range []struct {
		commands   []string
		discovered *extension.DiscoveryResult
		want       string
	}{
		{[]string{"typo"}, discovered, "no extension of this workspace declares typo,"},
		{[]string{"lint", "typo", "test", "build"}, discovered, "no extension of this workspace declares typo, build,"},
		{[]string{"lint"}, &extension.DiscoveryResult{}, "no extension of this workspace declares lint,"},
		{[]string{"lint"}, nil, "no extension of this workspace declares lint,"},
	} {
		err := validate(tc.commands, tc.discovered)
		if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage || !strings.Contains(err.Error(), tc.want) ||
			strings.Contains(err.Error(), "extensions list") ||
			!strings.Contains(err.Error(), commandmeta.CommandsThatLeftTheCoreURL) {
			t.Errorf("%v: error = %v, want a usage error containing %q and the reference of the moved commands, without a false pointer to extensions list", tc.commands, err, tc.want)
		}
	}

	// The commands are copied when the run is built: a later change to the
	// caller's slice does not change what is checked.
	commands := []string{"lint"}
	var validator func(*extension.DiscoveryResult, GlobalFlags) error
	run := refuseUndeclaredCommands(commands, func(_ context.Context, request engine.Request, _ engine.EventSink) (engine.SessionResult, error) {
		validator = request.ValidateCommandFlags
		return engine.SessionResult{}, nil
	})
	commands[0] = "typo"
	if _, err := run(context.Background(), engine.Request{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := validator(discovered, GlobalFlags{}); err != nil {
		t.Fatalf("the validator read the caller's slice after the run was built: %v", err)
	}
}
