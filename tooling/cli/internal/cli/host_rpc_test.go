package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	extproto "go.putnami.dev/protocol/extension"
	hostrpcproto "go.putnami.dev/protocol/extension/hostrpc"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	sdkhostrpc "go.putnami.dev/sdk/extension/hostrpc"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// The host RPC channel of an interactive extension subcommand, end to end: the
// fixture extension is this test binary re-executed as the subcommand, and it
// calls the channel through the SDK client, as an extension author would.

const (
	hostRPCExtensionEnv = "PUTNAMI_CLI_HOSTRPC_EXTENSION"
	hostRPCFeature      = "cli/job-planning-execution"
	hostRPCLifecycle    = "extension-command-host-rpc"
	hostRPCCalls        = "host-rpc-core-calls"
)

// TestHostRPCExtensionHelperProcess is the fixture subcommand. It prints the
// channel it received; in probe mode it then calls host.hello and
// collaboration.call and prints the answers, and in exit mode it exits 7.
func TestHostRPCExtensionHelperProcess(t *testing.T) {
	t.Parallel()
	mode := os.Getenv(hostRPCExtensionEnv)
	if mode == "" {
		t.Skip("helper process: not invoked as a host RPC extension")
	}
	iox.Fprintf(os.Stdout, "socket=%s\ntoken=%s\n", os.Getenv(hostrpcproto.SocketEnv), os.Getenv(hostrpcproto.TokenEnv))
	if mode == "exit" {
		os.Exit(7)
	}
	os.Exit(hostRPCProbe(context.Background(), os.Stdout))
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

func TestHostRPC_AHostedRunOpensNoChannel(t *testing.T) {
	spectest.Proves(t, hostRPCFeature, hostRPCLifecycle, "a-hosted-run-opens-no-channel")
	t.Cleanup(runcredential.SetForTest("hosted-bearer"))
	var stderr bytes.Buffer
	env, stop := startHostRPC(t.Context(), hostRPCBinding{}, &stderr)
	stop()
	if len(env) != 0 || stderr.Len() != 0 {
		t.Fatalf("a hosted launch received %q and warned %q", env, stderr.String())
	}
}

func TestHostRPC_AProcessDropsTheChannelItInherited(t *testing.T) {
	spectest.Proves(t, hostRPCFeature, hostRPCLifecycle, "a-planned-job-and-a-provider-tool-receive-no-channel")
	t.Setenv(hostrpcproto.SocketEnv, "/outer/host.sock")
	t.Setenv(hostrpcproto.TokenEnv, "outer-token")
	if err := enterProcess(nil); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{hostrpcproto.SocketEnv, hostrpcproto.TokenEnv} {
		if value, set := os.LookupEnv(key); set {
			t.Errorf("%s=%s survived the process entry", key, value)
		}
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
		{"proposals finalize is a command", binding(bound), "proposals", "finalize", hostrpcproto.CodeInvalidParams},
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
}
