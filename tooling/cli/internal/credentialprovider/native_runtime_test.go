package credentialprovider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/extension"
	protocolcli "go.putnami.dev/protocol/cli"
	registry "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// fakeRuntimeInfoEnv carries the runtime-info document this test binary
// answers the CLI's runtime handshake with when it runs as an extension
// runtime (see TestMain).
const fakeRuntimeInfoEnv = "PUTNAMI_TEST_FAKE_CREDENTIAL_RUNTIME_INFO"

// providerRuntimeExecutable is the runtime.executable the runtime fixtures
// declare.
const providerRuntimeExecutable = "compiled/putnami-credentials"

// runtimeInfoFor is the handshake answer that verifies ext on this machine.
func runtimeInfoFor(t *testing.T, ext *extension.ExtensionDescription) string {
	t.Helper()
	data, err := json.Marshal(runtimeproto.Info{
		Extension:       ext.Name,
		Version:         ext.Version,
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		CLIContract:     protocolcli.CurrentContract,
		RuntimeProtocol: runtimeproto.MaxKnownProtocolVersion,
		RuntimeABI:      runtimeproto.RuntimeABIVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// storeRuntimeExtension is a credential-provider extension installed in the
// artifact store, as a hosted run requires, whose provider command is
// {extensionRuntime} with args. No runtime is placed; runtimePath is where it
// goes.
func storeRuntimeExtension(t *testing.T, env map[string]string, args ...string) (ext *extension.ExtensionDescription, runtimePath string) {
	t.Helper()
	artifacts := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", artifacts)
	root := filepath.Join(artifacts, "extensions", "fixture-credentials@1.0.0")
	ext = providerExtension("@fixture/credentials", filepath.Join(root, "unused"), env)
	ext.Runtime = &extension.RuntimeDefinition{Executable: providerRuntimeExecutable}
	job := ext.Jobs[registry.CredentialProviderCommand]
	job.Command = "{extensionRuntime}"
	job.Args = args
	t.Setenv(fakeRuntimeInfoEnv, runtimeInfoFor(t, ext))
	return ext, filepath.Join(root, filepath.FromSlash(providerRuntimeExecutable))
}

// nativeRuntimeExtension is storeRuntimeExtension with this test binary as its
// native runtime. It returns the runtime's path.
func nativeRuntimeExtension(t *testing.T, env map[string]string, args ...string) (*extension.ExtensionDescription, string) {
	t.Helper()
	ext, path := storeRuntimeExtension(t, env, args...)
	return ext, fixtureproc.Binary(t, path)
}

// scriptRuntimeExtension is storeRuntimeExtension whose runtime is a shell
// script: it answers the handshake itself and runs this test binary for
// every other command, as a launcher does.
func scriptRuntimeExtension(t *testing.T, env map[string]string) *extension.ExtensionDescription {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a #! runtime cannot run on Windows")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ext, path := storeRuntimeExtension(t, env)
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = __putnami ]; then printf '%s\\n' '" + runtimeInfoFor(t, ext) + "'; exit 0; fi\n" +
		"exec '" + self + "' \"$@\"\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // an executable test fixture
		t.Fatal(err)
	}
	return ext
}

// A credential-provider command {extensionRuntime} starts the extension's
// runtime executable itself, with the command's args, the templates in them
// expanded, on a hosted run and without one. No launcher runs in between.
func TestNewStartsTheRuntimeItsCommandNames(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "hosted"}[hosted], func(t *testing.T) {
			argsFile := filepath.Join(t.TempDir(), "args")
			ext, runtimePath := nativeRuntimeExtension(t, map[string]string{
				fakeProviderEnv: "1", "FAKE_BEARER": "pat", "FAKE_HOSTS": "put.putnami.dev", "FAKE_ARGS_FILE": argsFile,
			}, "credential-provider", "--runtime", "{extensionRuntime}")
			var options []Option
			if hosted {
				t.Cleanup(runcredential.SetForTest(custodyRunCredential))
				options = append(options, WithRunCredential(custodyRunCredential))
			}
			broker := New(t.TempDir(), []*extension.ExtensionDescription{ext}, []string{runner.InvocationProviderInstall},
				"--providers", io.Discard, options...)
			if broker == nil {
				t.Fatal("New built no broker for one declaring extension")
			}
			t.Cleanup(func() { _ = broker.Close() })

			bearer, served, err := broker.Bearer(context.Background(), registry.PurposeRead, mustURL(t, "https://put.putnami.dev/a"))
			if !served || err != nil || bearer != "pat-read" {
				t.Fatalf("Bearer = %q, served %v, %v; want the runtime to serve it", bearer, served, err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{runtimePath, "credential-provider", "--runtime", runtimePath}
			if got := strings.Split(string(args), "\n"); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("the provider ran as %q, want %q", got, want)
			}
		})
	}
}

// On a hosted run a credential-provider starts only as its extension's native
// runtime. A command that is not the runtime, or a runtime that is a script,
// reads more store files after it starts: Start refuses it with an error that
// names the provider and the reason, and no process receives initialize.
// Without --credential-fd the same provider starts.
func TestAHostedProviderStartsOnlyAsItsNativeRuntime(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		ext  func(t *testing.T, env map[string]string) *extension.ExtensionDescription
		want string
	}{
		{name: "a command that is not the runtime", ext: func(t *testing.T, env map[string]string) *extension.ExtensionDescription {
			return providerExtension("@fixture/credentials", executable, env)
		}, want: "is not its extension's runtime executable"},
		{name: "a runtime that is a script", ext: scriptRuntimeExtension, want: "is a script (#!)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := func(t *testing.T, hosted bool) (string, error) {
				initializeFile := filepath.Join(t.TempDir(), "initialize")
				ext := tc.ext(t, map[string]string{
					fakeProviderEnv: "1", "FAKE_BEARER": "pat", "FAKE_HOSTS": "put.putnami.dev", "FAKE_INITIALIZE_FILE": initializeFile,
				})
				var options []Option
				if hosted {
					t.Cleanup(runcredential.SetForTest(custodyRunCredential))
					options = append(options, WithRunCredential(custodyRunCredential))
				}
				broker := New(t.TempDir(), []*extension.ExtensionDescription{ext}, []string{runner.InvocationProviderInstall},
					runcredential.Flag, io.Discard, options...)
				if broker == nil {
					t.Fatal("New built no broker for one declaring extension")
				}
				t.Cleanup(func() { _ = broker.Close() })
				return initializeFile, broker.Start(context.Background())
			}

			t.Run("hosted", func(t *testing.T) {
				initializeFile, err := start(t, true)
				var refusal *runcredential.NativeHolderError
				if !errors.As(err, &refusal) || refusal.Holder != "the credential provider of @fixture/credentials" ||
					!strings.Contains(refusal.Reason, tc.want) {
					t.Fatalf("Start = %v, want a refusal that names the provider and %q", err, tc.want)
				}
				if _, err := os.Stat(initializeFile); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("a refused provider received initialize: %v", err)
				}
			})
			t.Run("local", func(t *testing.T) {
				initializeFile, err := start(t, false)
				if err != nil {
					t.Fatalf("Start = %v, want nil without a run credential", err)
				}
				if _, err := os.Stat(initializeFile); err != nil {
					t.Errorf("the provider received no initialize: %v", err)
				}
			})
		})
	}
}
