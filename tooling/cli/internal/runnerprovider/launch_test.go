package runnerprovider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/extension"
	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

func TestMain(m *testing.M) {
	code := m.Run()
	fixtureproc.Remove()
	os.Exit(code)
}

// runtimeRunnerExtension is an installed runner-provider extension whose
// provider command runs the runtime it declares at compiled/putnami-runner.
func runtimeRunnerExtension(root string) *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: "@fixture/runner", Version: "1.0.0", Path: filepath.Join(root, "ext"),
		Runtime:  &extension.RuntimeDefinition{Executable: "compiled/putnami-runner"},
		Commands: map[string]string{runner.ProviderCommandName: "provider"},
		Jobs: map[string]*extension.JobDefinition{runner.ProviderCommandName: {
			Name: runner.ProviderCommandName, Command: "{extensionRuntime}", Args: []string{"runner-provider"},
			Env: map[string]string{"FIXTURE_RUNTIME": "{extensionRuntime}"},
		}},
	}
}

// A runner provider declared as {extensionRuntime} launches the runtime the
// CLI verified with the runtime-info handshake: compiled/<name>, and
// compiled/<name>.exe on Windows. A runtime it cannot prepare is an error, so
// a remote run stops before it submits anything.
func TestLaunchSpecForRunsThePreparedRuntime(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "providers-run-the-prepared-runtime", "a-runner-provider-launches-the-prepared-runtime")
	root := t.TempDir()
	ext := runtimeRunnerExtension(root)
	info, err := json.Marshal(runtimeproto.Info{
		Extension: ext.Name, Version: ext.Version, Platform: runtime.GOOS + "/" + runtime.GOARCH,
		CLIContract: protocolcli.CurrentContract, RuntimeProtocol: runtimeproto.MaxKnownProtocolVersion,
		RuntimeABI: runtimeproto.RuntimeABIVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := fixtureproc.Write(t, filepath.Join(ext.Path, "compiled", "putnami-runner"), fixtureproc.Program{Stdout: string(info)})
	provider := &extension.ResolvedProvider{ExtensionName: ext.Name, Command: runner.ProviderCommandName}

	spec, err := LaunchSpecFor(context.Background(), root, []*extension.ExtensionDescription{ext}, provider)
	if err != nil {
		t.Fatal(err)
	}
	// The messages never print spec.Env: it carries this process's environment.
	if spec.Command != want || strings.Join(spec.Args, " ") != "runner-provider" || spec.Dir != root {
		t.Fatalf("launch command %q, args %q, dir %q; want the prepared runtime %q in %q", spec.Command, spec.Args, spec.Dir, want, root)
	}
	if !slices.Contains(spec.Env, "FIXTURE_RUNTIME="+want) {
		t.Fatalf("provider env does not carry FIXTURE_RUNTIME=%s", want)
	}

	missing := runtimeRunnerExtension(filepath.Join(root, "missing"))
	_, err = LaunchSpecFor(context.Background(), root, []*extension.ExtensionDescription{missing}, provider)
	if err == nil || !strings.Contains(err.Error(), extensionproto.FailureRuntimeExecutableMissing) || !strings.Contains(err.Error(), ext.Name) {
		t.Fatalf("a runner provider without its runtime = %v, want %s naming %s", err, extensionproto.FailureRuntimeExecutableMissing, ext.Name)
	}
}
