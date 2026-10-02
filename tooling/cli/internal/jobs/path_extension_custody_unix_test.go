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
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A hosted run starts a path extension's runtime only after custody ended,
// and preparing it records repository code that names the extension, before
// the first spawn. From then on every handoff of the run credential is
// refused: the job credential of a workspace-fetch, a cache provider's
// authenticate (StartProvider), and a credential provider's
// initialize.runCredential, whose start goes through the same
// runcredential.StartHolder.
func TestAHandoffAfterAPathExtensionRuntimeIsRefused(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "no-credential-after-repository-code")
	// The fixture provider is built before hostedJobTest moves TMPDIR into
	// this test's own directory, which later tests no longer see.
	remote, spawns := custodyProviderCache(t, "run-bearer", nativeProviderExtension(t))
	hostedJobTest(t)
	calls := installCountingJobReadCredential(t, testJobCredential(), nil)
	fetch := extensionproto.WorkspaceFetchCommand
	ws, job := jobCredentialFixture(t, fetch, fetch, nil)

	wsRoot := t.TempDir()
	rel := filepath.Join("tools", "local")
	pathExt := &extension.ExtensionDescription{
		Name: "@acme/local", Version: "1.0.0", Path: filepath.Join(wsRoot, rel), RelPath: rel, LocalSource: true,
		Runtime: &extension.RuntimeDefinition{Executable: "missing-runtime"},
	}
	if err := os.MkdirAll(pathExt.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if !extension.WorkspacePathExtension(wsRoot, pathExt) {
		t.Fatalf("%s is not a path extension of %s", pathExt.Path, wsRoot)
	}
	// The runtime is missing, so the preparation fails before any spawn; the
	// record comes first.
	if err := SynchronizeExtensionRuntimes(t.Context(), &workspace.Workspace{Root: wsRoot}, []*extension.ExtensionDescription{pathExt}, nil); err == nil {
		t.Fatal("preparing a missing runtime succeeded")
	}
	const reason = "extension runtime of @acme/local"

	refused := func(what string, err error, holder string) {
		t.Helper()
		var refusal *runcredential.CustodyError
		if !errors.As(err, &refusal) || refusal.Holder != holder || refusal.Reason != reason {
			t.Errorf("%s = %v, want the refusal of %q after %q", what, err, holder, reason)
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
		t.Error("the credential provider started after the path extension's runtime")
	}
}
