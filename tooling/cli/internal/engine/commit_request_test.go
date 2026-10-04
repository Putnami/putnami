package engine

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	modeljobs "go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// commitExecution is the engine binding of a version 2 request over
// selection, with publication as its invocation.publication.
func commitExecution(selection runner.RequestedSelection, base string, publication *runner.PublicationBlock) *PortableExecution {
	return &PortableExecution{Commit: &runner.CommitRequest{
		Source:     runner.CommitSource{Commit: strings.Repeat("a", 40), Base: base},
		Invocation: runner.InvocationBlock{Commands: []string{"build", "test"}, Publication: publication},
		Selection:  selection,
	}}
}

// A version 2 request is the run's only selection authority: it replaces
// every selection flag with the mode it names, which the ordinary selection
// stage resolves. A version 1 request, or none, leaves the flags alone.
func TestCommitRequestBindsItsRequestedSelection(t *testing.T) {
	t.Parallel()
	stale := GlobalFlags{
		Projects: "/other", All: true, Impacted: true, Baseline: "main", AutoSelected: true,
		FilterTag: "tag", ExcludeTag: "skip", Exclude: "/lib", NoCacheProjects: "/app", Where: "remote", NoCache: true,
	}
	base := strings.Repeat("d", 40)
	for name, tc := range map[string]struct {
		portable *PortableExecution
		want     GlobalFlags
	}{
		"impacted": {commitExecution(runner.RequestedSelection{Mode: runner.SelectionModeImpacted}, base, nil),
			GlobalFlags{Impacted: true, Baseline: base, Where: "remote", NoCache: true}},
		"projects": {commitExecution(runner.RequestedSelection{Mode: runner.SelectionModeProjects, Projects: []string{"app", "libs/core"}}, "", nil),
			GlobalFlags{Projects: "app,libs/core", Where: "remote", NoCache: true}},
		"all": {commitExecution(runner.RequestedSelection{Mode: runner.SelectionModeAll}, "", nil),
			GlobalFlags{All: true, Where: "remote", NoCache: true}},
		"version 1": {&PortableExecution{}, stale},
		"none":      {nil, stale},
	} {
		global := stale
		tc.portable.bindSelection(&global)
		if !reflect.DeepEqual(global, tc.want) {
			t.Errorf("%s: bound flags = %+v, want %+v", name, global, tc.want)
		}
	}
}

// Every stage that reads a frozen request's snapshot asks frozen(), and the
// stages that read the invocation read whichever request is bound.
func TestPortableExecutionNamesItsVersion(t *testing.T) {
	t.Parallel()
	var none *PortableExecution
	if none.frozen() || none.invocation() != nil {
		t.Fatal("no bound request reads as one")
	}
	snapshot := &PortableExecution{Request: runner.ExecutionRequest{Invocation: runner.InvocationBlock{Commands: []string{"lint"}}}}
	if !snapshot.frozen() || snapshot.invocation() != &snapshot.Request.Invocation {
		t.Fatal("a version 1 request is not frozen on its own invocation")
	}
	commit := commitExecution(runner.RequestedSelection{Mode: runner.SelectionModeAll}, "", nil)
	if commit.frozen() || commit.invocation() != &commit.Commit.Invocation {
		t.Fatal("a version 2 request reads as frozen, or not on its own invocation")
	}
}

// The executing side of a version 2 request has no expected plan to compare
// and no bound inputs to verify: its seam checks the plan this engine made
// for a task outside the checkout and for publication, and refuses no
// declared effect that invocation.publication authorizes. A version 1 request
// over the same plan, with an expected plan that names nothing, is refused
// there.
func TestCommitRequestSeamChecksItsOwnPlan(t *testing.T) {
	t.Parallel()
	plain := portablePlan()
	shipping := append(portablePlan(), portableJob(plain[0].Project, "ship", "/app:build"))
	shipping[len(shipping)-1].JobDef.Traits.SideEffects = extensionproto.SideEffectsCloud
	modeljobs.AttachIdentities(shipping)
	root := t.TempDir()
	inside := portablePlan()
	inside[0].JobDef.Cwd = filepath.Join(root, "app")
	outside := portablePlan()
	outside[0].JobDef.Cwd = hostAbs("/elsewhere")
	all := runner.RequestedSelection{Mode: runner.SelectionModeAll}
	for _, tc := range []struct {
		name     string
		portable *PortableExecution
		planned  []*jobs.ScheduledJob
		refusal  string
	}{
		{"a frozen request with another plan", &PortableExecution{}, plain, "differs from the expected plan"},
		{"a commit request", commitExecution(all, "", nil), plain, ""},
		{"a commit request with a task inside the checkout", commitExecution(all, "", nil), inside, ""},
		{"a commit request with a task outside the checkout", commitExecution(all, "", nil), outside, "runs outside the workspace"},
		{"a commit request that publishes without the block", commitExecution(all, "", nil), shipping, "no invocation.publication"},
		{"a commit request that publishes behind its barrier", commitExecution(all, "", &runner.PublicationBlock{Barrier: []string{"build"}}), shipping, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &Request{WorkspaceRoot: root, Portable: tc.portable}
			var result SessionResult
			var handled bool
			stderr := captureStderr(t, func() {
				result, handled = New().portableSeam(context.Background(), req, nil, nil, tc.planned, nil)
			})
			if tc.refusal == "" {
				if handled || stderr != "" {
					t.Fatalf("handled = %v, stderr %q; want the run to execute", handled, stderr)
				}
				return
			}
			if !handled || result.ExitCode != ExitError || !strings.Contains(stderr, tc.refusal) {
				t.Fatalf("handled = %v, exit %d, stderr %q; want a refusal naming %q", handled, result.ExitCode, stderr, tc.refusal)
			}
		})
	}
}

// A version 2 caller authorizes publication before any plan exists, so a
// block over a plan that publishes nothing is a run that publishes nothing.
// A plan that publishes, by command name or by declared effects, keeps every
// version 1 rule.
func TestCommitPublicationHoldsAPublishingPlanToTheBarrier(t *testing.T) {
	t.Parallel()
	plain := portablePlan()
	app := plain[0].Project
	publishing := append(portablePlan(), portableJob(app, "publish", "/app:build"))
	modeljobs.AttachIdentities(publishing)
	skipping := append(portablePlan(), portableJob(app, "ship", "/app:build"))
	skipping[len(skipping)-1].JobDef.Traits.SideEffects = extensionproto.SideEffectsCloud
	modeljobs.AttachIdentities(skipping)
	request := func(publication *runner.PublicationBlock) runner.CommitRequest {
		return *commitExecution(runner.RequestedSelection{Mode: runner.SelectionModeAll}, "", publication).Commit
	}
	behindTest := &runner.PublicationBlock{Barrier: []string{"test"}}
	for _, tc := range []struct {
		name    string
		request runner.CommitRequest
		planned []*jobs.ScheduledJob
		refusal string
	}{
		{"a gate plan without the block", request(nil), plain, ""},
		{"a gate plan with the block", request(behindTest), plain, ""},
		{"an empty plan with the block", request(behindTest), nil, ""},
		{"a publish task without the block", request(nil), publishing, "no invocation.publication"},
		{"a cloud task without the block", request(nil), skipping, "no invocation.publication"},
		{"a cloud task that skips a barrier task", request(behindTest), skipping, "does not wait for barrier task /app:test~unit"},
	} {
		err := validateCommitPublication(tc.request, tc.planned, tc.planned)
		if tc.refusal == "" && err != nil {
			t.Errorf("%s: refused: %v", tc.name, err)
		}
		if tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal)) {
			t.Errorf("%s: %v; want a refusal naming %q", tc.name, err, tc.refusal)
		}
	}
}

// A version 2 request plans its checkout, which Git manages: the stages that
// a frozen request exempts from Git read it like a local run's.
func TestCommitRequestReadsItsCheckoutLikeALocalRun(t *testing.T) {
	t.Parallel()
	commit := &Request{Commands: []string{"publish"}, WorkspaceRoot: t.TempDir(), Portable: commitExecution(runner.RequestedSelection{Mode: runner.SelectionModeAll}, "", nil)}
	if err := requireRepository(commit); err == nil {
		t.Fatal("a version 2 publish outside a repository was not refused")
	}
	frozen := &Request{Commands: []string{"publish"}, WorkspaceRoot: t.TempDir(), Portable: &PortableExecution{}}
	if err := requireRepository(frozen); err != nil {
		t.Fatalf("a frozen publish in its snapshot was refused: %v", err)
	}
	if !readsAncestry(&Request{Commands: []string{"build"}, Portable: commitExecution(runner.RequestedSelection{Mode: runner.SelectionModeAll}, "", &runner.PublicationBlock{Barrier: []string{"build"}})}) {
		t.Fatal("a version 2 request carrying invocation.publication reads no ancestry")
	}
}
