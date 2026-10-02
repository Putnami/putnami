//go:build unix

package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A hosted run starts a path extension's code only after custody ended, and
// the first start records repository code that names the extension, before
// the first spawn: the probe of a runtime toolchain its manifest declares,
// which runs the candidate and arguments the manifest chooses, and the
// preparation of its runtime. From then on every handoff of the run credential
// is refused: the job credential of a workspace-fetch, a cache provider's
// authenticate (StartProvider), and a credential provider's
// initialize.runCredential, whose start goes through the same
// runcredential.StartHolder.
func TestAHandoffAfterAPathExtensionRuntimeIsRefused(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "no-credential-after-repository-code")
	fixtureproc.Prepare(t)

	// pathExtension is the path extension @acme/local of a new workspace,
	// whose runtime executable is missing.
	pathExtension := func(t *testing.T) (string, *extension.ExtensionDescription) {
		t.Helper()
		wsRoot := t.TempDir()
		rel := filepath.Join("tools", "local")
		ext := &extension.ExtensionDescription{
			Name: "@acme/local", Version: "1.0.0", Path: filepath.Join(wsRoot, rel), RelPath: rel, LocalSource: true,
			Runtime: &extension.RuntimeDefinition{Executable: "missing-runtime"},
		}
		if err := os.MkdirAll(ext.Path, 0o755); err != nil {
			t.Fatal(err)
		}
		if !extension.WorkspacePathExtension(wsRoot, ext) {
			t.Fatalf("%s is not a path extension of %s", ext.Path, wsRoot)
		}
		return wsRoot, ext
	}

	for _, c := range []struct {
		name, reason string
		// before runs before the run is hosted, and returns what start runs
		// once it is.
		before func(t *testing.T) (start func(t *testing.T))
	}{
		{
			name:   "toolchain probe",
			reason: "runtime toolchain probe of @acme/local",
			before: func(t *testing.T) func(t *testing.T) {
				bin, record := t.TempDir(), filepath.Join(t.TempDir(), "runs.jsonl")
				writeProbedProgram(t, filepath.Join(bin, "compiler"), fixtureproc.Program{Record: record, Stdout: "1.2.3\n"})
				return func(t *testing.T) {
					wsRoot, ext := pathExtension(t)
					ext.Runtime.Toolchains = map[string]extensionproto.RuntimeToolchain{"compiler": runtimeToolchainFixture("compiler")}
					writeRuntimeToolchainLock(t, wsRoot, "compiler", "1.2.3", "integrity-a")
					if err := resolveRuntimeToolchains(wsRoot, ext, []string{"compiler"}, []string{"PATH=" + bin}); err != nil {
						t.Fatal(err)
					}
					if runs := fixtureproc.Runs(t, record); len(runs) != 1 {
						t.Fatalf("the toolchain probe ran %d times, want once", len(runs))
					}
				}
			},
		},
		{
			name:   "runtime preparation",
			reason: "extension runtime of @acme/local",
			before: func(t *testing.T) func(t *testing.T) {
				return func(t *testing.T) {
					wsRoot, ext := pathExtension(t)
					// The runtime is missing, so the preparation fails before any
					// spawn; the record comes first.
					if err := SynchronizeExtensionRuntimes(t.Context(), &workspace.Workspace{Root: wsRoot}, []*extension.ExtensionDescription{ext}, nil); err == nil {
						t.Fatal("preparing a missing runtime succeeded")
					}
				}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The fixture programs are written before hostedJobTest moves
			// TMPDIR into this test's own directory, which later tests no
			// longer see.
			remote, spawns := custodyProviderCache(t, "run-bearer", nativeProviderExtension(t))
			start := c.before(t)
			hostedJobTest(t)
			calls := installCountingJobReadCredential(t, testJobCredential(), nil)
			fetch := extensionproto.WorkspaceFetchCommand
			ws, job := jobCredentialFixture(t, fetch, fetch, nil)
			start(t)

			refused := func(what string, err error, holder string) {
				t.Helper()
				var refusal *runcredential.CustodyError
				if !errors.As(err, &refusal) || refusal.Holder != holder || refusal.Reason != c.reason {
					t.Errorf("%s = %v, want the refusal of %q after %q", what, err, holder, c.reason)
				}
			}

			result, err := RunJob(WithDependencyFetch(t.Context()), ws, job, nil, nil, nil, nil)
			refused("the job credential", err, "job "+job.Key())
			if result != nil || calls.Load() != 0 {
				t.Errorf("the fetch ran (%+v) or the provider was asked %d times, want neither", result, calls.Load())
			}

			refused("the cache provider's authenticate", remote.StartProvider(t.Context(), nil, nil), "the cache provider of "+fakeProviderExtensionName)
			if n := spawns.Load(); n != 0 {
				t.Errorf("the cache provider was started %d times, want never", n)
			}

			started := false
			err = runcredential.StartHolder("the credential provider of @acme/cloud", func() error { started = true; return nil })
			refused("the credential provider's initialize", err, "the credential provider of @acme/cloud")
			if started {
				t.Error("the credential provider started after the path extension's code")
			}
		})
	}
}
