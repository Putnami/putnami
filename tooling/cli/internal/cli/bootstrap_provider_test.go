package cli

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/credentialprovider"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// The run credential always starts a bootstrap provider. Without it, only the
// install provider does, from the flag or the environment as
// invocationProviders reads them, and a bound execution request starts none.
func TestBootstrapSourceFollowsTheRunCredentialAndInstall(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		hosted  bool
		args    []string
		env     string
		bound   bool
		enabled bool
		source  string
	}{
		{name: "flag off", args: []string{"build"}},
		{name: "install from the flag", args: []string{"build", "--providers", "install"}, enabled: true, source: providersFromFlag},
		{name: "install from the environment", args: []string{"build"}, env: "install", enabled: true, source: credentialprovider.ProvidersEnv},
		{name: "the flag wins over the environment", args: []string{"build", "--providers", "publish"}, env: "install"},
		{name: "publish only", args: []string{"build"}, env: "publish"},
		{name: "unknown in the environment", args: []string{"build"}, env: "deploy"},
		{name: "bound request", env: "install", bound: true},
		{name: "hosted", hosted: true, args: []string{"build"}, enabled: true, source: runcredential.Flag},
		{name: "hosted with install", hosted: true, args: []string{"build", "--providers", "install"}, enabled: true, source: providersFromFlag},
		{name: "hosted with an unknown provider", hosted: true, args: []string{"build"}, env: "deploy", enabled: true, source: runcredential.Flag},
		{name: "hosted bound request", hosted: true, bound: true, enabled: true, source: runcredential.Flag},
	} {
		source, enabled := bootstrapSource(c.args, c.env, c.hosted, c.bound)
		if enabled != c.enabled || (enabled && source != c.source) {
			t.Errorf("%s: bootstrapSource = %q, %v; want %q, %v", c.name, source, enabled, c.source, c.enabled)
		}
	}
}

// The bootstrap provider opens before the relaunch that downloads the pinned
// CLI, serves the lock-pinned extension downloads, and ends before discovery
// and before the workspace's provider is installed. The bound-request adapter
// ends it before it installs the request's provider.
func TestBootstrapProviderEndsBeforeTheWorkspaceProvider(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "credential-on-a-descriptor", "bootstrap-provider-serves-only-the-locked-downloads")
	for file, c := range map[string]struct {
		function string
		order    []string
	}{
		"app.go": {"Run", []string{
			"openBootstrapProvider", "Relaunch", "CaptureProcessCapabilities",
			"ensureArtifactsAndClose", "discoverDispatchExtensions", "resolveExecutionPolicy",
		}},
		"runner_execute.go": {"runBoundRequest", []string{
			"ensureArtifactsAndClose", "ensureArtifactsForProcessMode", "installCredentialProviders",
		}},
	} {
		calls := callOffsets(t, file, c.function)
		for i, name := range c.order {
			offset, ok := calls[name]
			if !ok {
				t.Fatalf("%s: %s never calls %s", file, c.function, name)
			}
			if i > 0 && offset < calls[c.order[i-1]] {
				t.Errorf("%s: %s calls %s before %s", file, c.function, name, c.order[i-1])
			}
		}
	}
}

// The bootstrap provider materializes the lock-pinned artifacts for every
// invocation in a workspace, a structured command such as `install` included,
// since those materialize them only after the workspace's provider starts.
// Outside a workspace, and in a release-set provider child, it does not.
func TestBootstrapEnsuresArtifactsForEveryWorkspaceInvocation(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		wsRoot       string
		providerMode bool
		want         bool
	}{
		{wsRoot: "/ws", want: true},
		{wsRoot: "/ws", providerMode: true},
		{wsRoot: ""},
	} {
		if got := bootstrapEnsuresArtifacts(c.wsRoot, c.providerMode); got != c.want {
			t.Errorf("bootstrapEnsuresArtifacts(%q, %v) = %v, want %v", c.wsRoot, c.providerMode, got, c.want)
		}
	}
}
