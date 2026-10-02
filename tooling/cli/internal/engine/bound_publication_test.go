package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/extension"
	modeljobs "go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// The executing side of a bound request that carries no
// invocation.publication only gates. These tests drive Engine.Run over the
// release-set session fixture on a tagged commit, the shape that starts a
// release-set provider, and read the fixture provider's call log to see
// whether one started. They set the environment, so none is parallel.

// boundFixtureRequest is the release-set fixture as the executing side of a
// bound request for commands over /app, on a commit tagged v1.0.0. It returns
// the request and the file the fixture provider logs its calls to.
func boundFixtureRequest(t *testing.T, commands []string, publication *runner.PublicationBlock) (Request, string) {
	t.Helper()
	_, req := releaseSetSessionFixture(t)
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv(releaseSetFixtureCallsEnv, calls)
	req.Commands = commands
	req.Global = GlobalFlags{Projects: "/app", Plan: true, NoCache: true}
	req.Portable = &PortableExecution{Request: runner.ExecutionRequest{
		Invocation: runner.InvocationBlock{Commands: commands, Publication: publication},
		Selection:  runner.SelectionBlock{Mode: protocoljob.SelectionModeProjects, Scoped: true, Projects: []string{"/app"}},
		Source: runner.SourceBlock{Versions: []runner.LineVersion{
			{Base: "1.0.0", Full: "1.0.0", Tag: "v1.0.0", Tagged: true},
		}},
	}}
	return req, calls
}

// runBound runs req and returns its result and what it wrote on stderr.
func runBound(t *testing.T, req Request) (SessionResult, string) {
	t.Helper()
	var result SessionResult
	stderr := captureStderr(t, func() {
		result, _ = New().Run(context.Background(), req, discardEvents{})
	})
	return result, stderr
}

// providerCalls returns the calls the fixture provider logged, nil when it
// never started.
func providerCalls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the test's own temp file
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// A bound publish without invocation.publication is refused on its keying
// plan, before the release-set preparation starts the provider. The same
// request with the block starts it, which shows the log would see a start.
func TestBoundPublishRequestWithoutPublicationIsRefusedBeforeAnyProviderStarts(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "gate-only-launches-no-publication", "a-publishing-request-without-the-block-is-refused-before-any-provider")
	req, calls := boundFixtureRequest(t, []string{"build", "publish"}, nil)
	result, stderr := runBound(t, req)
	if result.ExitCode != ExitError || !strings.Contains(stderr, "portable execution refused") || !strings.Contains(stderr, "no invocation.publication") {
		t.Fatalf("bound publish without the block = exit %d, stderr %q; want a refusal naming invocation.publication", result.ExitCode, stderr)
	}
	if got := providerCalls(t, calls); got != nil {
		t.Fatalf("the refused request started the release-set provider: %v", got)
	}

	req, calls = boundFixtureRequest(t, []string{"build", "publish"}, &runner.PublicationBlock{Barrier: []string{"build"}})
	result, stderr = runBound(t, req)
	if strings.Contains(stderr, "no invocation.publication") {
		t.Fatalf("the request with the block was refused: %s", stderr)
	}
	if got := providerCalls(t, calls); !slices.Contains(got, "resolve") {
		t.Fatalf("the request with the block made provider calls %v, want a resolve (exit %d, stderr %q)", got, result.ExitCode, stderr)
	}
}

// A bound gate on the same tagged commit plans no publish job and starts no
// release-set provider.
func TestGateOnlyRequestPlansNoPublishJob(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "gate-only-launches-no-publication", "a-gate-only-request-plans-no-publish-job")
	req, calls := boundFixtureRequest(t, []string{"lint", "test", "build"}, nil)
	req.CommandParams = nil
	result, stderr := runBound(t, req)
	if result.ExitCode != ExitSuccess {
		t.Fatalf("bound gate = exit %d, stderr %q", result.ExitCode, stderr)
	}
	keys := planKeys(t, result.Plan)
	assertPlanned(t, keys, "/app:lint~check", "/app:test~test", "/app:build~compile")
	assertNotPlanned(t, keys, "/app:publish~artifact", "/app:package~artifact")
	if got := providerCalls(t, calls); got != nil {
		t.Fatalf("the gate-only request started the release-set provider: %v", got)
	}
}

// The refusal classifies a task by its command and by its declared effects,
// and a request whose versions start a release-set publication needs the
// block even before any task publishes. Only a bound request is checked.
func TestRefusesUnauthorizedPublicationClassifiesByEffects(t *testing.T) {
	plain := portablePlan()
	app := plain[0].Project
	shipping := append(portablePlan(), portableJob(app, "ship", "/app:build"))
	shipping[len(shipping)-1].JobDef.Traits.SideEffects = extensionproto.SideEffectsCloud
	modeljobs.AttachIdentities(shipping)
	push := portableJob(app, "build")
	declareTaskEffect(push, extension.EffectRegistry)
	pushing := []*jobs.ScheduledJob{push}
	modeljobs.AttachIdentities(pushing)
	publishing := []*jobs.ScheduledJob{portableJob(app, "publish")}
	modeljobs.AttachIdentities(publishing)
	tagged := jobs.ReleaseSetOptions{Commands: []string{"publish"}, Tagged: true}

	request := func(publication *runner.PublicationBlock) *Request {
		return &Request{Portable: &PortableExecution{Request: runner.ExecutionRequest{
			Invocation: runner.InvocationBlock{Publication: publication},
		}}}
	}
	for _, tc := range []struct {
		name    string
		req     *Request
		options jobs.ReleaseSetOptions
		planned []*jobs.ScheduledJob
		names   string
	}{
		{"a gate plan", request(nil), jobs.ReleaseSetOptions{}, plain, ""},
		{"a publish task", request(nil), jobs.ReleaseSetOptions{}, publishing, "plan task /app:publish publishes"},
		{"a cloud task under another command", request(nil), jobs.ReleaseSetOptions{}, shipping, "plan task /app:ship publishes"},
		{"a registry write under build", request(nil), jobs.ReleaseSetOptions{}, pushing, "plan task /app:build publishes"},
		{"a release-set publication", request(nil), tagged, plain, "release-set publication"},
		{"the block", request(&runner.PublicationBlock{Barrier: []string{"build"}}), tagged, shipping, ""},
		{"a local run", &Request{}, tagged, shipping, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var refused bool
			stderr := captureStderr(t, func() { refused = refusesUnauthorizedPublication(tc.req, tc.options, tc.planned) })
			if tc.names == "" {
				if refused || stderr != "" {
					t.Fatalf("refused = %v, stderr %q; want no refusal", refused, stderr)
				}
				return
			}
			if !refused || !strings.Contains(stderr, tc.names) {
				t.Fatalf("refused = %v, stderr %q; want a refusal naming %q", refused, stderr, tc.names)
			}
		})
	}
}
