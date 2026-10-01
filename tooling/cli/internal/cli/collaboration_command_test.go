package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	collab "go.putnami.dev/protocol/collaboration"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The CLI entry path of the collaboration contracts, measured against the MCP
// entry path on the same workspace. The provider is this test binary
// re-executed; the fixture declares it by absolute path, so discovery finds it
// exactly as it finds any path-declared extension.

const collabCommandProviderEnv = "PUTNAMI_CLI_COLLAB_PROVIDER"

// TestCollaborationCommandProviderHelperProcess is the fixture provider.
func TestCollaborationCommandProviderHelperProcess(t *testing.T) {
	t.Parallel()
	if os.Getenv(collabCommandProviderEnv) == "" {
		t.Skip("helper process: not invoked as a collaboration provider")
	}
	key := func(contract, operation string) collab.OperationKey {
		return collab.OperationKey{Contract: contract, Version: 1, Operation: operation}
	}
	task := func(ref collab.Ref, title string) collab.Task {
		return collab.Task{Ref: ref, Revision: "r1", Title: title, State: collab.TaskStateOpen}
	}
	now := "2026-09-24T08:00:00Z"
	handlers := map[collab.OperationKey]collab.Handler{
		key("tasks", "find"): func(context.Context, collab.Call) (any, *collab.Failure) {
			return &collab.TaskListResult{Items: []collab.Task{task(collab.Ref{Source: "local:cli", ID: "T-1"}, "first")}}, nil
		},
		key("tasks", "get"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			return nil, collab.Fail(collab.OutcomeNotFound, "task.missing", "no task %s", call.Input.(*collab.TaskRefInput).Ref.ID)
		},
		key("tasks", "create"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			input := call.Input.(*collab.TaskCreateInput)
			return &collab.TaskCreateResult{Task: task(collab.Ref{Source: "local:cli", ID: "T-2"}, input.Title), Created: true}, nil
		},
		key("tasks", "update"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			return &collab.TaskResult{Task: task(call.Input.(*collab.TaskUpdateInput).Ref, "updated")}, nil
		},
		key("tasks", "transition"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			input := call.Input.(*collab.TaskTransitionInput)
			result := task(input.Ref, "moved")
			result.State = input.State
			return &collab.TaskResult{Task: result}, nil
		},
		key("memory", "context"): func(_ context.Context, call collab.Call) (any, *collab.Failure) {
			selection, _ := json.Marshal(call.Request.Selection)
			return &collab.MemoryListResult{Items: []collab.MemoryRecord{{
				Ref: collab.Ref{Source: "local:cli", ID: "N-1"}, Revision: "r1", Kind: collab.MemoryKindNote,
				Content:    string(selection),
				Provenance: collab.Provenance{RecordedAt: now},
				Freshness:  collab.Freshness{UpdatedAt: now, RetrievedAt: now},
			}}}, nil
		},
		key("memory", "mission"): func(context.Context, collab.Call) (any, *collab.Failure) {
			return nil, collab.Fail(collab.OutcomeNotFound, "mission.missing", "no mission")
		},
		key("memory", "checkpoint"): func(context.Context, collab.Call) (any, *collab.Failure) {
			return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonRevisionConflict, "moved")
		},
	}
	if err := collab.Serve(context.Background(), os.Stdin, os.Stdout, handlers); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

// collabCommandTool decodes one fixture provider tool from the manifest JSON an
// extension author writes, with annotations that agree with the catalog.
func collabCommandTool(t *testing.T, contract, operation string) extproto.ToolDefinition {
	t.Helper()
	spec, _ := collab.Lookup(contract, 1)
	op, _ := spec.Operation(operation)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	readOnly := op.Access == collab.AccessRead
	declaration := fmt.Sprintf(`{"contract":%q,"version":1,"operation":%q`, contract, operation)
	switch op.Preconditions {
	case collab.PreconditionRuleDeclared:
		declaration += `,"preconditions":"checked"`
	case collab.PreconditionRuleAtomic:
		declaration += `,"preconditions":"atomic"`
	}
	document := fmt.Sprintf(`{"description":%q,"inputSchema":{"type":"object"},`+
		`"annotations":{"readOnlyHint":%t,"destructiveHint":%t,"idempotentHint":%t,"openWorldHint":false},`+
		`"_meta":{"putnami.dev/contract":{"access":%q,"readOnly":%t,"supportsDryRun":false},"putnami.dev/provider":%s}},`+
		`"command":%q,"args":["-test.run=^TestCollaborationCommandProviderHelperProcess$"],`+
		`"env":{%q:"1"},"timeoutMs":20000,"workspaceSelection":%t}`,
		"Fixture "+contract+"."+operation+".", readOnly, op.Destructive, readOnly, op.Access, readOnly, declaration,
		executable, collabCommandProviderEnv, op.WorkspaceSelection)
	var def extproto.ToolDefinition
	if err := json.Unmarshal([]byte(document), &def); err != nil {
		t.Fatalf("decode %s: %v", document, err)
	}
	return def
}

// collabCommandWorkspace writes a two-project workspace binding tasks and
// memory to the fixture provider. extra adds tools, commands or groups to the
// provider manifest.
func collabCommandWorkspace(t *testing.T, bindings string, extra func(*extproto.Manifest)) string {
	t.Helper()
	root := t.TempDir()
	extensionRoot := filepath.Join(t.TempDir(), "provider")
	writeParityFile(t, extensionRoot, extproto.ManifestFilename, collabCommandManifest(t, "0.0.1", extra))
	document := `{"name":"cli-collab","includes":["packages/app","packages/lib"],"extensions":[` + mustJSONString(t, extensionRoot) + `]`
	if bindings != "" {
		document += `,"options":{"collaboration":` + bindings + `}`
	}
	collabCommandProjects(t, root, document+"}")
	return root
}

// collabCommandManifest is the fixture provider's manifest at one release:
// every required operation of tasks and memory. extra adds tools, commands or
// groups.
func collabCommandManifest(t *testing.T, version string, extra func(*extproto.Manifest)) string {
	t.Helper()
	tools := map[string]extproto.ToolDefinition{}
	for _, contract := range []string{collab.ContractTasks, collab.ContractMemory} {
		spec, _ := collab.Lookup(contract, 1)
		for _, name := range spec.RequiredOperations() {
			tools["fixture."+contract+"."+name] = collabCommandTool(t, contract, name)
		}
	}
	manifest := extproto.Manifest{Name: "@test/cli-collab", Version: version, CLIContract: protocolcli.CurrentContract, Tools: tools}
	if extra != nil {
		extra(&manifest)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// collabCommandProjects writes the workspace document and its two projects,
// then records the workspace index.
func collabCommandProjects(t *testing.T, root, document string) {
	t.Helper()
	writeParityFile(t, root, wsproto.WorkspaceConfigFilename, document)
	writeParityFile(t, filepath.Join(root, "packages", "app"), "putnami.json", `{"name":"app","type":"application","dependencies":["lib"]}`)
	writeParityFile(t, filepath.Join(root, "packages", "lib"), "putnami.json", `{"name":"lib","type":"library"}`)
	workspace.InvalidateLoadCache(root)
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatalf("load fixture workspace: %v", err)
	}
	if _, err := workspace.RefreshSnapshot(ws, workspace.SnapshotWritePolicy{}); err != nil {
		t.Fatalf("record fixture workspace index: %v", err)
	}
	workspace.InvalidateLoadCache(root)
}

const collabCommandBindings = `{"tasks":{"provider":"@test/cli-collab","version":1},"memory":{"provider":"@test/cli-collab","version":1}}`

func collabCommandExtensions(t *testing.T, root string) []*extension.ExtensionDescription {
	t.Helper()
	exts, err := extension.DiscoverExtensions(root, wsproto.Load(root), projectPathsForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	return exts
}

// runCollabCommand parses args the way App.Run does for this workspace and
// dispatches them through the extension command dispatch.
func runCollabCommand(t *testing.T, root string, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	exts := collabCommandExtensions(t, root)
	groups := addCollaborationGroups(extension.CommandGroupNames(exts), root)
	parsed := ParseArgs(args, nil, groups)
	if parsed.Err != nil {
		t.Fatalf("the parser rejected %v: %v", args, parsed.Err)
	}
	if !groups[parsed.Commands[0]] {
		t.Fatalf("%s is not a command group of this workspace", parsed.Commands[0])
	}
	var out, errOut bytes.Buffer
	code = (&App{}).runExtensionStructuredCommand(context.Background(), parsed, wsproto.Load(root), root, exts,
		strings.NewReader(stdin), &out, &errOut)
	return out.String(), errOut.String(), code
}

func decodeCollabEnvelope(t *testing.T, text string) collab.Envelope {
	t.Helper()
	envelope, diags := collab.ParseEnvelope([]byte(strings.TrimSpace(text)))
	if diags != nil {
		t.Fatalf("the command printed an envelope the contract refuses: %v\n%s", diags, text)
	}
	return *envelope
}

func TestCollaborationCommand_TheCLIAndMCPReturnTheSameEnvelope(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "one-route-two-entry-paths", "the-cli-and-mcp-return-the-same-envelope")
	root := collabCommandWorkspace(t, collabCommandBindings, nil)
	srv := newMCPServer(root, wsproto.Load(root), "collaboration-test")
	cases := []struct {
		contract, operation, input string
		outcome                    collab.Outcome
	}{
		{"tasks", "create", `{"title":"write the guide","idempotencyKey":"k1"}`, collab.OutcomeOK},
		{"tasks", "find", `{}`, collab.OutcomeOK},
		{"tasks", "get", `{"ref":{"source":"local:cli","id":"T-9"}}`, collab.OutcomeNotFound},
		{"memory", "context", `{"projects":["app"]}`, collab.OutcomeOK},
		{"memory", "checkpoint", `{"mission":"m","precondition":{"expectedRevision":"r1"},"idempotencyKey":"k","content":"x"}`, collab.OutcomeConflict},
		{"tasks", "capabilities", ``, collab.OutcomeOK},
	}
	for _, tc := range cases {
		t.Run(tc.contract+"."+tc.operation, func(t *testing.T) {
			args := []string{tc.contract, tc.operation, "--output=json"}
			if tc.input != "" {
				args = append(args, "--input", tc.input)
			}
			stdout, stderr, code := runCollabCommand(t, root, "", args...)
			cli := decodeCollabEnvelope(t, stdout)
			if cli.Outcome != tc.outcome {
				t.Fatalf("CLI outcome = %s, want %s: %+v (stderr %q)", cli.Outcome, tc.outcome, cli.Error, stderr)
			}
			if want := map[bool]int{true: ExitSuccess, false: ExitError}[tc.outcome == collab.OutcomeOK]; code != want {
				t.Errorf("exit code = %d, want %d", code, want)
			}
			if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
				t.Errorf("--output=json prints one compact line, got %q", stdout)
			}

			arguments := tc.input
			if arguments == "" {
				arguments = "{}"
			}
			blocks, isError := callParityTool(t, srv, tc.contract+"."+tc.operation, arguments)
			if len(blocks) != 1 {
				t.Fatalf("MCP answered %d blocks", len(blocks))
			}
			mcp := decodeCollabEnvelope(t, blocks[0])
			if isError != (mcp.Outcome != collab.OutcomeOK) {
				t.Errorf("MCP isError %v disagrees with outcome %s", isError, mcp.Outcome)
			}
			cliJSON, _ := json.Marshal(cli)
			mcpJSON, _ := json.Marshal(mcp)
			if !bytes.Equal(cliJSON, mcpJSON) {
				t.Errorf("the entry paths disagree:\nCLI %s\nMCP %s", cliJSON, mcpJSON)
			}
		})
	}
}

// collabUnpreparedProviderWorkspace binds tasks and memory to a registry
// provider whose release 0.0.1 is still installed behind the stable link while
// the workspace lock pins no release of it, so no exact release can be
// prepared. Preparation fails without reaching a registry, which keeps the
// case free of process-wide environment.
func collabUnpreparedProviderWorkspace(t *testing.T) string {
	t.Helper()
	const name = "@test/cli-collab"
	root := t.TempDir()
	writeParityFile(t, layout.ArtifactDir(root, layout.Extensions, name, "0.0.1"), extproto.ManifestFilename, collabCommandManifest(t, "0.0.1", nil))
	if err := layout.LinkArtifact(root, layout.Extensions, name, "0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := lockfile.WriteLockFile(root, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}
	collabCommandProjects(t, root, `{"name":"cli-collab","includes":["packages/app","packages/lib"],`+
		`"extensions":{"@test/cli-collab":"stable"},"options":{"collaboration":`+collabCommandBindings+`}}`)
	return root
}

// A provider whose exact locked release was not prepared is unavailable on
// both entry paths. The release left behind the stable link never serves, and
// the two paths return the same envelope.
func TestCollaborationCommand_AnUnpreparedLockedProviderIsUnavailableOnBothEntryPaths(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "explicit-binding", "a-missing-or-disabled-provider-is-unavailable")
	spectest.Proves(t, "cli/collaboration-providers", "one-route-two-entry-paths", "the-cli-and-mcp-return-the-same-envelope")
	root := collabUnpreparedProviderWorkspace(t)
	if exts := collabCommandExtensions(t, root); len(exts) != 1 || exts[0].Version != "0.0.1" {
		t.Fatalf("discovery = %+v, want the stale 0.0.1 behind the stable link", exts)
	}
	srv := newMCPServer(root, wsproto.Load(root), "collaboration-test")
	for _, tc := range []struct{ contract, operation, input string }{
		{"tasks", "find", `{}`},
		{"tasks", "create", `{"title":"t","idempotencyKey":"k1"}`},
		{"memory", "context", `{}`},
	} {
		t.Run(tc.contract+"."+tc.operation, func(t *testing.T) {
			stdout, stderr, code := runCollabCommand(t, root, "", tc.contract, tc.operation, "--output=json", "--input", tc.input)
			cli := decodeCollabEnvelope(t, stdout)
			if cli.Outcome != collab.OutcomeUnavailable || cli.Error == nil || cli.Error.Reason != collab.ReasonBindingProviderMissing {
				t.Fatalf("CLI envelope = %+v (stderr %q), want unavailable/%s", cli, stderr, collab.ReasonBindingProviderMissing)
			}
			if code != ExitError {
				t.Errorf("exit code = %d, want %d", code, ExitError)
			}
			blocks, isError := callParityTool(t, srv, tc.contract+"."+tc.operation, tc.input)
			if len(blocks) != 1 || !isError {
				t.Fatalf("MCP answered %d blocks, isError %v", len(blocks), isError)
			}
			cliJSON, _ := json.Marshal(cli)
			mcpJSON, _ := json.Marshal(decodeCollabEnvelope(t, blocks[0]))
			if !bytes.Equal(cliJSON, mcpJSON) {
				t.Errorf("the entry paths disagree:\nCLI %s\nMCP %s", cliJSON, mcpJSON)
			}
		})
	}

	var help bytes.Buffer
	exts := collabCommandExtensions(t, root)
	printCommandGroupHelp(context.Background(), &help, root, wsproto.Load(root), exts, "tasks", "", "")
	if !strings.Contains(help.String(), "was not prepared from the workspace lock") {
		t.Errorf("help does not report the unprepared provider:\n%s", help.String())
	}
}

func TestCollaborationCommand_ReadsTheRequestFromAFileOrStdin(t *testing.T) {
	t.Parallel()
	root := collabCommandWorkspace(t, collabCommandBindings, nil)
	request := `{"ref":{"source":"local:cli","id":"T-1"},"state":"done"}`
	stdout, _, code := runCollabCommand(t, root, request, "tasks", "transition", "--input-file", "-")
	if code != ExitSuccess || decodeCollabEnvelope(t, stdout).Outcome != collab.OutcomeOK {
		t.Fatalf("stdin request: code %d, %s", code, stdout)
	}
	file := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(file, []byte(request), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, code = runCollabCommand(t, root, "", "tasks", "transition", "--input-file="+file)
	envelope := decodeCollabEnvelope(t, stdout)
	var moved collab.TaskResult
	if err := json.Unmarshal(envelope.Result, &moved); err != nil || code != ExitSuccess || moved.Task.State != collab.TaskStateDone {
		t.Fatalf("file request: code %d, %s", code, stdout)
	}
	// Human output is the same document, indented.
	if !strings.Contains(stdout, "\n  \"contract\": \"tasks\"") {
		t.Errorf("human output is not the indented envelope: %q", stdout)
	}
}

func TestCollaborationCommand_UsageErrors(t *testing.T) {
	t.Parallel()
	root := collabCommandWorkspace(t, collabCommandBindings, nil)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"an unknown flag", []string{"tasks", "find", "--query", "x"}, "unexpected argument"},
		{"a selection flag", []string{"tasks", "find", "--projects", "app"}, "takes no --projects flag"},
		{"two inputs", []string{"tasks", "find", "--input", "{}", "--input-file", "-"}, "exclusive"},
		{"a flag without value", []string{"tasks", "find", "--input"}, "needs a value"},
		{"a missing file", []string{"tasks", "find", "--input-file", filepath.Join(t.TempDir(), "absent.json")}, "read --input-file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runCollabCommand(t, root, "", tc.args...)
			if code != ExitUsage || !strings.Contains(stderr, tc.want) || stdout != "" {
				t.Fatalf("code %d, stdout %q, stderr %q; want exit %d naming %q", code, stdout, stderr, ExitUsage, tc.want)
			}
		})
	}
	// A request the contract refuses is an envelope, not a usage error: the
	// caller reads the same outcome on both entry paths.
	stdout, _, code := runCollabCommand(t, root, "", "tasks", "create", "--input", `{"title":"x"}`)
	if code != ExitError || decodeCollabEnvelope(t, stdout).Outcome != collab.OutcomeInvalid {
		t.Fatalf("code %d, %s", code, stdout)
	}
	// An optional operation the provider lacks has no MCP tool, and on the
	// command line it answers unsupported rather than pretending to run.
	stdout, _, code = runCollabCommand(t, root, "", "tasks", "claim", "--input", `{"ref":{"source":"local:cli","id":"T-1"},"holder":"me"}`)
	if envelope := decodeCollabEnvelope(t, stdout); code != ExitError || envelope.Outcome != collab.OutcomeUnsupported ||
		envelope.Error.Reason != collab.ReasonOperationUnsupported {
		t.Fatalf("code %d, %s", code, stdout)
	}
}

func TestCollaborationCommand_HelpDescribesTheBinding(t *testing.T) {
	t.Parallel()
	root := collabCommandWorkspace(t, collabCommandBindings, nil)
	stdout, _, code := runCollabCommand(t, root, "", "tasks")
	if code != ExitSuccess {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{"Usage: putnami tasks <operation>", "Binding: @test/cli-collab, version 1 (bound)", "claim", "optional, unsupported", "required, supported"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help lacks %q:\n%s", want, stdout)
		}
	}
	stdout, _, _ = runCollabCommand(t, root, "", "proposals")
	if !strings.Contains(stdout, "Binding: none (unbound)") {
		t.Errorf("an unbound contract's help:\n%s", stdout)
	}
}

func TestCollaborationCommand_AContractWordIsACommandOnlyWhenBound(t *testing.T) {
	t.Parallel()
	root := collabCommandWorkspace(t, "", nil)
	groups := addCollaborationGroups(map[string]bool{}, root)
	if len(groups) != 0 {
		t.Fatalf("a workspace without options.collaboration reserved %v", groups)
	}
	if isCollaborationCommand(root, "tasks") {
		t.Error("tasks is routed in a workspace that binds nothing")
	}
	if isCollaborationCommand(collabCommandWorkspace(t, collabCommandBindings, nil), "deploy") {
		t.Error("a word that names no contract is routed")
	}
}

func TestCollaborationCommand_AnExtensionCommandOfTheSameNameIsRefused(t *testing.T) {
	t.Parallel()
	root := collabCommandWorkspace(t, collabCommandBindings, func(manifest *extproto.Manifest) {
		manifest.Commands = map[string]extproto.CommandDefinition{
			"tasks-list": {Description: "list", Run: []extproto.PipelineStep{{ID: "list", Task: "noop"}}},
		}
		manifest.Tasks = map[string]extproto.TaskDefinition{"noop": {Command: "true"}}
		manifest.CommandGroups = map[string]extproto.CommandGroupDefinition{
			"tasks": {Subcommands: map[string]extproto.SubcommandDefinition{"list": {Command: "tasks-list"}}},
		}
	})
	stdout, stderr, code := runCollabCommand(t, root, "", "tasks", "find")
	if code != ExitUsage || stdout != "" || !strings.Contains(stderr, "a collaboration contract of this workspace") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}
