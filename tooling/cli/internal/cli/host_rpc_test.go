package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	extproto "go.putnami.dev/protocol/extension"
	hostrpcproto "go.putnami.dev/protocol/extension/hostrpc"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	sdkhostrpc "go.putnami.dev/sdk/extension/hostrpc"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hostrpc"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The host RPC channel of an interactive extension subcommand, end to end: the
// fixture extension is this test binary re-executed as the subcommand, and it
// calls the channel through the SDK client, as an extension author would.

const (
	hostRPCExtensionEnv = "PUTNAMI_CLI_HOSTRPC_EXTENSION"
	// hostRPCNestedRootEnv names the workspace of the nested mode.
	hostRPCNestedRootEnv = "PUTNAMI_CLI_HOSTRPC_NESTED_ROOT"
	hostRPCFeature       = "cli/job-planning-execution"
	hostRPCLifecycle     = "extension-command-host-rpc"
	hostRPCCalls         = "host-rpc-core-calls"
	hostRPCOwners        = "host-rpc-workspace-owners"
)

// TestHostRPCExtensionHelperProcess is the fixture subcommand. It prints the
// channel it received; in probe mode it then calls host.hello and
// collaboration.call and prints the answers, in exit mode it exits 7, and in
// nested mode it runs as a nested putnami (hostRPCNested).
func TestHostRPCExtensionHelperProcess(t *testing.T) {
	t.Parallel()
	mode := os.Getenv(hostRPCExtensionEnv)
	if mode == "" {
		t.Skip("helper process: not invoked as a host RPC extension")
	}
	iox.Fprintf(os.Stdout, "socket=%s\ntoken=%s\n", os.Getenv(hostrpcproto.SocketEnv), os.Getenv(hostrpcproto.TokenEnv))
	switch mode {
	case "exit":
		os.Exit(7)
	case "nested":
		os.Exit(hostRPCNested(t, os.Getenv(hostRPCNestedRootEnv), os.Stdout))
	}
	os.Exit(hostRPCProbe(context.Background(), os.Stdout))
}

// hostRPCNested is a nested putnami that the launched command runs, so it
// inherits the launch's channel. It enters the process as putnami does, then
// prints the channel variables its own environment, a provider the core
// prepares, and the collaboration provider each hold. The fixture
// collaboration provider refuses to serve when it receives a channel, so its
// outcome says whether it did.
func hostRPCNested(t *testing.T, root string, out io.Writer) int {
	if err := enterProcess(nil); err != nil {
		iox.Fprintf(out, "error=%v\n", err)
		return 3
	}
	iox.Fprintf(out, "process=%s\n", hostRPCVariables(os.Environ()))
	iox.Fprintf(out, "provider=%s\n", hostRPCVariables(hostRPCProviderEnv(t, root)))
	envelope := collaborationEnvelope(t.Context(), root, wsproto.Load(root), collabCommandExtensions(t, root),
		collab.ContractTasks, "find", json.RawMessage(`{}`))
	iox.Fprintf(out, "collaboration=%s\n", envelope.Outcome)
	return 0
}

// hostRPCVariables lists the channel variables env holds.
func hostRPCVariables(env []string) string {
	var held []string
	for _, entry := range env {
		if key, _, _ := strings.Cut(entry, "="); key == hostrpcproto.SocketEnv || key == hostrpcproto.TokenEnv {
			held = append(held, entry)
		}
	}
	return strings.Join(held, ",")
}

// hostRPCProviderEnv is the environment of a provider the core prepares in
// root (scheduler.PrepareProviderLaunch), as it prepares the cache provider, the
// runner provider and the session reporter. The provider's own root is root
// too: the nested process exits without running its test cleanup.
func hostRPCProviderEnv(t *testing.T, root string) []string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := &extension.ExtensionDescription{
		Name:    "@test/hostrpc-provider",
		Version: "0.0.1",
		Path:    root,
		Jobs: map[string]*extension.JobDefinition{
			"provider": {ExtensionName: "@test/hostrpc-provider", Name: "provider", Command: executable},
		},
	}
	launch, err := scheduler.PrepareProviderLaunch(t.Context(), root, provider, "provider")
	if err != nil {
		t.Fatal(err)
	}
	return launch.Env
}

// hostRPCProbe makes the probe's calls and prints their answers, one
// key=value line each. A host that never answers fails the probe instead of
// hanging the test.
func hostRPCProbe(ctx context.Context, out io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client, err := sdkhostrpc.Dial()
	if err != nil {
		iox.Fprintf(out, "error=%v\n", err)
		return 3
	}
	defer func() { _ = client.Close() }()
	hello, err := client.Hello(ctx)
	if err != nil {
		iox.Fprintf(out, "error=%v\n", err)
		return 3
	}
	encoded, err := json.Marshal(hello)
	if err != nil {
		iox.Fprintf(out, "error=%v\n", err)
		return 3
	}
	iox.Fprintf(out, "hello=%s\n", encoded)
	envelope, err := client.CallCollaboration(ctx, "tasks", "find", json.RawMessage(`{}`))
	if err != nil {
		iox.Fprintf(out, "error=%v\n", err)
		return 3
	}
	iox.Fprintf(out, "envelope=%s\n", envelope)
	return 0
}

// hostRPCFixtureCommands adds the command group rpcfixture to the
// collaboration fixture provider: probe and exit, two interactive subcommands
// that run the helper process in their mode.
func hostRPCFixtureCommands(t *testing.T) func(*extproto.Manifest) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	task := func(mode string) extproto.TaskDefinition {
		return extproto.TaskDefinition{
			Kind:    "command",
			Command: executable,
			Args:    []string{"-test.run=^TestHostRPCExtensionHelperProcess$", "--"},
			Env:     map[string]string{hostRPCExtensionEnv: mode},
		}
	}
	return func(manifest *extproto.Manifest) {
		manifest.Commands = map[string]extproto.CommandDefinition{
			"rpc-probe": {Description: "Probe the host RPC channel.", Run: []extproto.PipelineStep{{ID: "probe", Task: "rpc-probe"}}},
			"rpc-exit":  {Description: "Exit 7.", Run: []extproto.PipelineStep{{ID: "exit", Task: "rpc-exit"}}},
		}
		manifest.Tasks = map[string]extproto.TaskDefinition{"rpc-probe": task("probe"), "rpc-exit": task("exit")}
		manifest.CommandGroups = map[string]extproto.CommandGroupDefinition{
			"rpcfixture": {
				Description: "Host RPC fixture.",
				Subcommands: map[string]extproto.SubcommandDefinition{
					"probe": {Command: "rpc-probe", Interactive: true, Description: "Probe the channel."},
					"exit":  {Command: "rpc-exit", Interactive: true, Description: "Exit 7."},
				},
			},
		}
	}
}

// hostRPCReport reads the helper's key=value lines.
func hostRPCReport(stdout string) map[string]string {
	report := map[string]string{}
	for _, line := range strings.Split(stdout, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			report[key] = value
		}
	}
	return report
}

// assertChannelClosed fails when the socket or its directory outlived the
// launch.
func assertChannelClosed(t *testing.T, socket string) {
	t.Helper()
	if socket == "" {
		t.Fatal("the launched process received no channel")
	}
	for _, path := range []string{socket, filepath.Dir(socket)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s outlived the launch: %v", path, err)
		}
	}
}

// dialHostRPC opens the channel of a launch bound to binding and connects the
// SDK client to it.
func dialHostRPC(t *testing.T, binding hostRPCBinding) *sdkhostrpc.Client {
	t.Helper()
	var stderr bytes.Buffer
	env, stop := startHostRPC(t.Context(), binding, &stderr)
	t.Cleanup(stop)
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	client, err := sdkhostrpc.Connect(values[hostrpcproto.SocketEnv], values[hostrpcproto.TokenEnv])
	if err != nil {
		t.Fatalf("the channel did not open (%s): %v", stderr.String(), err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestHostRPC_ASubcommandCallsTheCoreAndGetsTheCLIEnvelope(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCLifecycle, "the-launched-process-receives-the-socket-and-token")
	spectest.Proves(t, hostRPCFeature, hostRPCLifecycle, "the-channel-closes-when-the-process-exits-whatever-its-exit")
	spectest.Proves(t, hostRPCFeature, hostRPCCalls, "collaboration-call-returns-the-cli-envelope")
	root := collabCommandWorkspace(t, collabCommandBindings, hostRPCFixtureCommands(t))

	stdout, stderr, code := runCollabCommand(t, root, "", "rpcfixture", "probe")
	report := hostRPCReport(stdout)
	if code != ExitSuccess {
		t.Fatalf("rpcfixture probe exit %d, error %q\nstdout %s\nstderr %s", code, report["error"], stdout, stderr)
	}
	if token, err := hex.DecodeString(report["token"]); err != nil || len(token) != hostrpcproto.TokenBytes {
		t.Errorf("the process received the token %q, want %d hex-encoded bytes", report["token"], hostrpcproto.TokenBytes)
	}
	var hello hostrpcproto.HelloResult
	if err := json.Unmarshal([]byte(report["hello"]), &hello); err != nil {
		t.Fatalf("hello %q: %v", report["hello"], err)
	}
	if hello.Protocol != hostrpcproto.ProtocolVersion ||
		strings.Join(hello.Methods, ",") != strings.Join(hostrpcproto.Methods(), ",") {
		t.Errorf("hello = %+v", hello)
	}

	cli, cliStderr, cliCode := runCollabCommand(t, root, "", "tasks", "find", "--output=json", "--input", "{}")
	if cliCode != ExitSuccess {
		t.Fatalf("tasks find exit %d: %s", cliCode, cliStderr)
	}
	if strings.TrimSuffix(cli, "\n") != report["envelope"] {
		t.Errorf("collaboration.call and the CLI disagree:\nRPC %s\nCLI %s", report["envelope"], cli)
	}
	assertChannelClosed(t, report["socket"])
}

func TestHostRPC_TheChannelClosesWhenTheSubcommandFails(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCLifecycle, "the-channel-closes-when-the-process-exits-whatever-its-exit")
	root := collabCommandWorkspace(t, collabCommandBindings, hostRPCFixtureCommands(t))
	stdout, stderr, code := runCollabCommand(t, root, "", "rpcfixture", "exit")
	if code != 7 {
		t.Fatalf("rpcfixture exit exit %d, want 7\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	assertChannelClosed(t, hostRPCReport(stdout)["socket"])
}

// TestHostRPC_AHostedRunOpensNoChannel holds the launch's half: hostrpc.Start
// refuses a hosted run with ErrHosted (internal/hostrpc proves it), and the
// launch then receives no variable and no warning. Any other refusal warns.
func TestHostRPC_AHostedRunOpensNoChannel(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCLifecycle, "a-hosted-run-opens-no-channel")
	var stderr bytes.Buffer
	env, stop := hostRPCLaunch(nil, hostrpc.ErrHosted, &stderr)
	stop()
	if len(env) != 0 || stderr.Len() != 0 {
		t.Fatalf("a hosted launch received %q and warned %q", env, stderr.String())
	}
	env, stop = hostRPCLaunch(nil, errors.New("the socket path is too long"), &stderr)
	stop()
	if len(env) != 0 || !strings.Contains(stderr.String(), "the socket path is too long") {
		t.Fatalf("a launch whose channel did not open received %q and warned %q", env, stderr.String())
	}
}

// TestHostRPC_AProviderCallReceivesNoChannel starts providers the way the core
// does while a channel is open: from the process that opened it, and from a
// nested putnami that the launched command runs, which inherits the pair.
// Neither a provider the core prepares (scheduler.PrepareProviderLaunch: the cache
// provider, the runner provider, the session reporter) nor the collaboration
// provider that collaboration.call reaches receives the channel, and the
// nested process drops the pair it inherited. The nested putnami is a child
// process, so the test never sets this process's environment.
func TestHostRPC_AProviderCallReceivesNoChannel(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCLifecycle, "a-planned-job-and-a-provider-call-receive-no-channel")
	root := collabCommandWorkspace(t, collabCommandBindings, nil)
	var stderr bytes.Buffer
	channel, stop := startHostRPC(t.Context(), hostRPCBinding{}, &stderr)
	t.Cleanup(stop)
	if len(channel) != 2 {
		t.Fatalf("the channel did not open: %s", stderr.String())
	}

	// The process that opened the channel keeps it out of its own environment.
	if held := hostRPCVariables(hostRPCProviderEnv(t, root)); held != "" {
		t.Errorf("a provider the launching process starts receives %s", held)
	}
	envelope := collaborationEnvelope(t.Context(), root, wsproto.Load(root), collabCommandExtensions(t, root),
		collab.ContractTasks, "find", json.RawMessage(`{}`))
	if envelope.Outcome != collab.OutcomeOK {
		t.Errorf("the collaboration provider the launching process starts answered %s: %+v", envelope.Outcome, envelope.Error)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	nested := exec.CommandContext(t.Context(), executable, "-test.run=^TestHostRPCExtensionHelperProcess$")
	nested.Env = append(os.Environ(), hostRPCExtensionEnv+"=nested", hostRPCNestedRootEnv+"="+root)
	nested.Env = append(nested.Env, channel...)
	stdout, err := nested.Output()
	report := hostRPCReport(string(stdout))
	if err != nil {
		t.Fatalf("the nested putnami failed: %v, error %q\n%s", err, report["error"], stdout)
	}
	if report["socket"] == "" || report["token"] == "" {
		t.Fatalf("the nested putnami inherited no channel:\n%s", stdout)
	}
	if report["process"] != "" {
		t.Errorf("the nested putnami kept %s after entering the process", report["process"])
	}
	if report["provider"] != "" {
		t.Errorf("a provider the nested putnami starts receives %s", report["provider"])
	}
	if report["collaboration"] != string(collab.OutcomeOK) {
		t.Errorf("the collaboration provider the nested putnami starts answered %q", report["collaboration"])
	}
}

func TestHostRPC_VerificationReportAnswersWhatTheEngineAnswers(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCCalls, "verification-report-returns-what-the-cache-corroborates")
	root := collabCommandWorkspace(t, collabCommandBindings, nil)
	ref := hostrpcproto.VerificationReportParams{
		Project: "app",
		Task:    "app:test",
		Path:    "report.xml",
		Digest:  "sha256:" + strings.Repeat("0", 64),
	}
	_, want := engine.VerifyCachedVerificationReport(root, features.VerificationReportRef{
		Project: ref.Project,
		Task:    ref.Task,
		Path:    ref.Path,
		Digest:  ref.Digest,
	})
	if want == nil {
		t.Fatal("the engine corroborated a report no run produced")
	}
	client := dialHostRPC(t, hostRPCBinding{root: root, config: wsproto.Load(root), extensions: collabCommandExtensions(t, root)})
	report, err := client.VerificationReport(t.Context(), ref)
	var refusal *hostrpcproto.Error
	if !errors.As(err, &refusal) || refusal.Code != hostrpcproto.CodeNotVerified || refusal.Message != want.Error() {
		t.Fatalf("verification.report = %q, %v, want the engine's refusal %q", report, err, want)
	}

	outside := dialHostRPC(t, hostRPCBinding{})
	if _, err := outside.VerificationReport(t.Context(), ref); !errors.As(err, &refusal) || refusal.Code != hostrpcproto.CodeUnavailable {
		t.Fatalf("verification.report outside a workspace = %v, want %d", err, hostrpcproto.CodeUnavailable)
	}
}

// hostRPCOwnersWorkspace writes a workspace of four projects: outer holds
// inner, claimant claims the directory of schema as an asset, and no project
// is rooted at the workspace root.
func hostRPCOwnersWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeParityFile(t, root, wsproto.WorkspaceConfigFilename,
		`{"name":"owners","includes":["packages/outer","packages/outer/inner","packages/claimant","packages/schema"]}`)
	writeParityFile(t, root, "packages/outer/putnami.json", `{"name":"outer"}`)
	writeParityFile(t, root, "packages/outer/inner/putnami.json", `{"name":"inner"}`)
	writeParityFile(t, root, "packages/claimant/putnami.json", `{"name":"claimant","build":{"assets":[{"from":"/packages/schema"}]}}`)
	writeParityFile(t, root, "packages/schema/putnami.json", `{"name":"schema"}`)
	for _, file := range []string{"outer/a.go", "outer/inner/b.go", "claimant/c.go", "schema/d.json"} {
		writeParityFile(t, root, "packages/"+file, "x\n")
	}
	writeParityFile(t, root, "README.md", "x\n")
	recordWatchedRootFile(t, root, "go.work", "packages/outer", "packages/claimant")
	return root
}

// recordWatchedRootFile records, in the workspace index, a provider answer in
// which each project at the given paths lists the workspace-root file as
// watched, the way a probing run records it. The workspace loads that answer
// without starting a provider.
func recordWatchedRootFile(t *testing.T, root, file string, projectPaths ...string) {
	t.Helper()
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	result := wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/fixture"}
	for _, path := range projectPaths {
		result.Projects = append(result.Projects, wsproto.ProbeProject{Path: path, WatchedFiles: []string{file}})
	}
	wsproto.NormalizeProbeResult(&result)
	snapshot := workspace.NewSnapshotWithProviders(ws, ws.ProbeDigest(), []workspace.SnapshotProvider{{
		Extension: result.Extension,
		Digest:    wsproto.ProbeResultDigest(result),
		Result:    result,
	}})
	if err := workspace.WriteSnapshot(root, snapshot, workspace.SnapshotWritePolicy{}); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
}

func TestHostRPC_WorkspaceOwnersAnswersWhatTheWorkspaceModelOwns(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCOwners, "workspace-owners-answers-one-entry-per-path-in-request-order")
	spectest.Proves(t, hostRPCFeature, hostRPCOwners, "workspace-owners-are-the-nearest-project-and-every-asset-claimant")
	spectest.Proves(t, hostRPCFeature, hostRPCOwners, "workspace-owners-are-sorted-by-id-and-empty-when-none-owns-the-path")
	root := hostRPCOwnersWorkspace(t)
	client := dialHostRPC(t, hostRPCBinding{root: root, config: wsproto.Load(root)})

	outer := hostrpcproto.OwningProject{ID: "/packages/outer", Path: "packages/outer"}
	inner := hostrpcproto.OwningProject{ID: "/packages/outer/inner", Path: "packages/outer/inner"}
	claimant := hostrpcproto.OwningProject{ID: "/packages/claimant", Path: "packages/claimant"}
	schema := hostrpcproto.OwningProject{ID: "/packages/schema", Path: "packages/schema"}
	for _, tc := range []struct {
		name  string
		paths []string
		want  [][]hostrpcproto.OwningProject
	}{
		{"the deeper project owns its files, not the outer one", []string{"packages/outer/inner/b.go"}, [][]hostrpcproto.OwningProject{{inner}}},
		{"the outer project owns the rest of its directory", []string{"packages/outer/a.go"}, [][]hostrpcproto.OwningProject{{outer}}},
		{"an asset claimant owns the file with its project, sorted by ID", []string{"packages/schema/d.json"}, [][]hostrpcproto.OwningProject{{claimant, schema}}},
		{"a path equal to a project directory belongs to that project", []string{"packages/outer/inner", "packages/outer"}, [][]hostrpcproto.OwningProject{{inner}, {outer}}},
		{"no project owns a file outside every project", []string{"README.md", "packages/other/e.go"}, [][]hostrpcproto.OwningProject{{}, {}}},
		{"the answer keeps the request order and repeats a duplicate",
			[]string{"packages/schema/d.json", "README.md", "packages/outer/inner/b.go", "packages/schema/d.json"},
			[][]hostrpcproto.OwningProject{{claimant, schema}, {}, {inner}, {claimant, schema}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owners, err := client.WorkspaceOwners(t.Context(), tc.paths)
			if err != nil {
				t.Fatal(err)
			}
			if len(owners) != len(tc.paths) {
				t.Fatalf("WorkspaceOwners = %+v, want one entry for each of %v", owners, tc.paths)
			}
			for i, entry := range owners {
				if entry.Path != tc.paths[i] || entry.Projects == nil || !slices.Equal(entry.Projects, tc.want[i]) {
					t.Errorf("entry %d = %+v, want %s owned by %+v", i, entry, tc.paths[i], tc.want[i])
				}
			}
		})
	}
}

func TestHostRPC_WorkspaceOwnersIncludesTheProjectsWhoseProviderWatchesARootFile(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCOwners, "workspace-owners-include-every-project-whose-provider-watches-a-root-file")
	root := hostRPCOwnersWorkspace(t)
	client := dialHostRPC(t, hostRPCBinding{root: root, config: wsproto.Load(root)})
	claimant := hostrpcproto.OwningProject{ID: "/packages/claimant", Path: "packages/claimant"}
	outer := hostrpcproto.OwningProject{ID: "/packages/outer", Path: "packages/outer"}
	owners, err := client.WorkspaceOwners(t.Context(), []string{"go.work", "go.work.sum", "packages/go.work"})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range [][]hostrpcproto.OwningProject{{claimant, outer}, {}, {}} {
		if !slices.Equal(owners[i].Projects, want) {
			t.Errorf("entry %d = %+v, want owners %+v: only a watched file at the workspace root has them", i, owners[i], want)
		}
	}
}

func TestHostRPC_WorkspaceOwnersIsUnavailableOutsideAWorkspace(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, hostRPCFeature, hostRPCOwners, "workspace-owners-is-unavailable-outside-a-workspace")
	_, err := dialHostRPC(t, hostRPCBinding{}).WorkspaceOwners(t.Context(), []string{"a.go"})
	var refusal *hostrpcproto.Error
	if !errors.As(err, &refusal) || refusal.Code != hostrpcproto.CodeUnavailable ||
		refusal.Message != "this command runs outside any workspace, so no project owns a path" {
		t.Fatalf("workspace.owners outside a workspace = %v, want %d", err, hostrpcproto.CodeUnavailable)
	}
}

func TestHostRPC_CollaborationCallRefusesWhatTheCommandPrintsNoEnvelopeFor(t *testing.T) {
	t.Parallel()
	bound := collabCommandWorkspace(t, collabCommandBindings, nil)
	unbound := collabCommandWorkspace(t, "", nil)
	conflicting := collabCommandWorkspace(t, collabCommandBindings, func(manifest *extproto.Manifest) {
		manifest.Commands = map[string]extproto.CommandDefinition{
			"tasks-list": {Description: "list", Run: []extproto.PipelineStep{{ID: "list", Task: "noop"}}},
		}
		manifest.Tasks = map[string]extproto.TaskDefinition{"noop": {Kind: "command", Command: "true"}}
		manifest.CommandGroups = map[string]extproto.CommandGroupDefinition{
			"tasks": {Subcommands: map[string]extproto.SubcommandDefinition{"list": {Command: "tasks-list"}}},
		}
	})
	binding := func(root string) hostRPCBinding {
		return hostRPCBinding{root: root, config: wsproto.Load(root), extensions: collabCommandExtensions(t, root)}
	}
	for _, tc := range []struct {
		name                string
		binding             hostRPCBinding
		contract, operation string
		code                int
	}{
		{"outside any workspace", hostRPCBinding{}, "tasks", "find", hostrpcproto.CodeUnavailable},
		{"no collaboration binding", binding(unbound), "tasks", "find", hostrpcproto.CodeUnavailable},
		{"a word that is not a contract", binding(bound), "foo", "find", hostrpcproto.CodeInvalidParams},
		{"a command group owns the contract word", binding(conflicting), "tasks", "find", hostrpcproto.CodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope, err := dialHostRPC(t, tc.binding).CallCollaboration(t.Context(), tc.contract, tc.operation, nil)
			var refusal *hostrpcproto.Error
			if !errors.As(err, &refusal) || refusal.Code != tc.code {
				t.Fatalf("collaboration.call = %s, %v, want refusal %d", envelope, err, tc.code)
			}
		})
	}
	if isCollaborationCommand(bound, "foo") {
		t.Fatal("the CLI routes foo to the collaboration command, so collaboration.call must too")
	}
}

// TestHostRPC_CollaborationCallRoutesProposalsFinalizeAsAnyOperation pins that
// collaboration.call names no command: proposals finalize reaches the route
// `putnami <contract> <operation>` takes, which answers that the contract
// defines no such operation.
func TestHostRPC_CollaborationCallRoutesProposalsFinalizeAsAnyOperation(t *testing.T) {
	t.Parallel()
	root := collabCommandWorkspace(t, `{"proposals":{"provider":"@test/cli-collab","version":1}}`, func(manifest *extproto.Manifest) {
		manifest.Providers[collab.ContractProposals] = collabCommandProvider(t, collab.ContractProposals)
	})
	binding := hostRPCBinding{root: root, config: wsproto.Load(root), extensions: collabCommandExtensions(t, root)}
	answer, err := dialHostRPC(t, binding).CallCollaboration(t.Context(), collab.ContractProposals, "finalize", nil)
	if err != nil {
		t.Fatalf("collaboration.call proposals finalize = %v, want the route's envelope", err)
	}
	var envelope collab.Envelope
	if err := json.Unmarshal(answer, &envelope); err != nil {
		t.Fatalf("collaboration.call proposals finalize answered %s: %v", answer, err)
	}
	if envelope.Outcome != collab.OutcomeInvalid || envelope.Error == nil || envelope.Error.Reason != collab.ReasonRequestInvalid ||
		!strings.Contains(envelope.Error.Message, `proposals version 1 has no operation "finalize"`) {
		t.Fatalf("collaboration.call proposals finalize = %s, want the route's answer to an operation the contract does not define", answer)
	}
}
