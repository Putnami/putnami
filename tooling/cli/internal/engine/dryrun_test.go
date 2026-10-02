package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestProcessCapabilityAuthorization_PlanOnlyNeverRequiresAGateOrGrant(t *testing.T) {
	for _, test := range []struct {
		name string
		req  func() *Request
	}{
		{name: "plan", req: func() *Request {
			req := &Request{}
			req.Global.Plan = true
			return req
		}},
		{name: "dry-run preview", req: func() *Request {
			req := &Request{}
			req.Global.DryRun = true
			return req
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(extensionproto.CloudTokenEnv, "preview-only-runtime-capability")
			t.Setenv(extensionproto.CloudCapabilityAfterEnv, "missing-gate")
			ctx := jobs.CaptureProcessCapabilities(context.Background())
			protected := dryRunFixtureJob("publish", extension.SideEffectsRegistry, false)

			req := test.req()
			authorization, err := jobs.AuthorizeProcessCapabilities(
				ctx, []*jobs.ScheduledJob{protected}, !req.Global.Plan && !req.previewsOnly(),
			)
			if err != nil {
				t.Fatalf("preview capability authorization = %v, want success without validating execution gates", err)
			}
			if authorization != nil {
				t.Fatal("preview produced a runtime capability grant")
			}
		})
	}
}

func dryRunFixtureJob(name, sideEffects string, declaresDryRun bool, deps ...string) *jobs.ScheduledJob {
	def := &extension.JobDefinition{Name: name, ExtensionName: "@putnami/test"}
	def.Traits.SideEffects = sideEffects
	if declaresDryRun {
		def.Flags = map[string]extension.FlagDefinition{"dry-run": {}}
	}
	return &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: "/p", Name: "p", Path: "p"},
		Extension: &extension.ExtensionDescription{Name: "@putnami/test"},
		JobDef:    def,
		DependsOn: deps,
	}
}

func executingDryRunRequest() *Request {
	req := &Request{ExecutesUnderDryRun: true, CommandParams: map[string]any{"dry-run": true}}
	req.Global.Quiet = true
	return req
}

// TestDropUndryableSideEffectJobs_ExcludesUndeclaredSideEffects pins the
// safety invariant: under an executing dry-run, a side-effecting task without
// a declared dry-run flag never runs — and its dependents fall with it — while
// declared publishers and side-effect-free work keep running.
func TestDropUndryableSideEffectJobs_ExcludesUndeclaredSideEffects(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "preview-no-effects", "only-declared-dry-run-simulations-survive")
	declared := dryRunFixtureJob("publish", extension.SideEffectsRegistry, true)
	undeclared := dryRunFixtureJob("publish-legacy", extension.SideEffectsRegistry, false)
	local := dryRunFixtureJob("package", "", false)
	dependent := dryRunFixtureJob("verify", "", false, undeclared.Key())

	kept := dropUndryableSideEffectJobs(executingDryRunRequest(),
		[]*jobs.ScheduledJob{declared, undeclared, local, dependent})

	names := make(map[string]bool, len(kept))
	for _, job := range kept {
		names[job.JobDef.Name] = true
	}
	if !names["publish"] || !names["package"] {
		t.Fatalf("kept = %v, want the declared publisher and the local dependency kept", names)
	}
	if names["publish-legacy"] {
		t.Fatal("an undeclared side-effecting job survived an executing dry-run")
	}
	if names["verify"] {
		t.Fatal("a dependent of an excluded job survived — it would wait on a dependency that never runs")
	}
}

func TestDropUndryableSideEffectJobs_KeepsPipelineStepsWithCommandDryRunFlag(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "preview-no-effects", "only-declared-dry-run-simulations-survive")
	publisher := dryRunFixtureJob("publish~release", extension.SideEffectsRegistry, false)
	publisher.JobDef.CommandName = "publish"
	publisher.Extension.Jobs = map[string]*extension.JobDefinition{
		"publish": {
			Name:  "publish",
			Flags: map[string]extension.FlagDefinition{"dry-run": {}},
		},
	}

	kept := dropUndryableSideEffectJobs(executingDryRunRequest(), []*jobs.ScheduledJob{publisher})
	if len(kept) != 1 || kept[0] != publisher {
		t.Fatalf("kept = %#v, want pipeline publisher kept when its command declares --dry-run", kept)
	}
}

func TestDropUndryableSideEffectJobs_InertOutsideExecutingDryRun(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "preview-no-effects", "only-declared-dry-run-simulations-survive")
	undeclared := dryRunFixtureJob("deploy", extension.SideEffectsCloud, false)

	// Not executing under dry-run at all (ordinary run).
	plain := &Request{}
	plain.Global.Quiet = true
	if kept := dropUndryableSideEffectJobs(plain, []*jobs.ScheduledJob{undeclared}); len(kept) != 1 {
		t.Fatal("an ordinary run must not be filtered")
	}

	// The alias shape without --dry-run: ExecutesUnderDryRun is always true
	// there, but no dry-run param was synthesized — the job must run.
	alias := &Request{ExecutesUnderDryRun: true}
	alias.Global.Quiet = true
	if kept := dropUndryableSideEffectJobs(alias, []*jobs.ScheduledJob{undeclared}); len(kept) != 1 {
		t.Fatal("an alias run without --dry-run must not be filtered")
	}

	// The alias shape WITH --dry-run: the param is gated on the command's own
	// group⊕subcommand flag surface, which the planned JobDef does not carry —
	// the filter must not misread a correctly gated alias job as undeclared
	// (PlanExtension marks the alias path).
	aliasDryRun := executingDryRunRequest()
	aliasDryRun.PlanExtension = &PlanExtensionSelection{Name: "@putnami/test", Command: "deploy"}
	if kept := dropUndryableSideEffectJobs(aliasDryRun, []*jobs.ScheduledJob{undeclared}); len(kept) != 1 {
		t.Fatal("an alias dry-run must not be filtered: the alias gates the param on its own flag surface")
	}
}

// TestPublishFinalizers_ProbeReportOnlyUnderAnExecutingDryRunPublish pins when
// a publish session reads its registry probes: only when its jobs receive the
// dry-run parameter. A preview executes no job, and a real publish emits no
// probe. Without a release-set plan the release-set commit is absent, so the
// probe report is the session's only publish finalizer.
func TestPublishFinalizers_ProbeReportOnlyUnderAnExecutingDryRunPublish(t *testing.T) {
	t.Parallel()
	dryRun := executingDryRunRequest()
	dryRun.Commands = []string{"publish"}
	preview := &Request{Commands: []string{"publish"}}
	preview.Global.DryRun = true
	for name, test := range map[string]struct {
		req    *Request
		probes bool
	}{
		"executing dry-run publish": {req: dryRun, probes: true},
		"dry-run preview":           {req: preview},
		"real publish":              {req: &Request{Commands: []string{"publish"}}},
	} {
		finalizers := publishFinalizers(context.Background(), test.req, nil)
		if len(finalizers) != 1 || (finalizers[0] != nil) != test.probes {
			t.Errorf("%s: publishFinalizers = %d finalizer(s), probe report present = %v, want %v",
				name, len(finalizers), len(finalizers) == 1 && finalizers[0] != nil, test.probes)
		}
	}
}

// TestExecute_DryRunPublishFailsOnAConflictProbe drives one conflict probe from
// a publish job's output through the run: the job succeeds, the probe report
// fails the run with a non-zero exit, and the session's machine output carries
// the verdict as an error diagnostic of the failed member-probe row.
func TestExecute_DryRunPublishFailsOnAConflictProbe(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "one-pass-verdict", "conflict-fails-the-run")
	var stream bytes.Buffer
	emitter := runtimeproto.NewEmitterForVersion(&stream, runtimeproto.MaxKnownProtocolVersion)
	probe := map[string]any{
		"ecosystem": "go", "coordinate": "go.putnami.dev/mod", "version": "v1.2.3",
		"registry": "https://go.putnami.dev", "state": extensionproto.MemberProbeConflict,
		"reason": "the registry already holds this version with another zip digest",
	}
	if err := emitter.ArtifactData("go", "go.putnami.dev/mod", extensionproto.MemberProbeEventKind, "", probe); err != nil {
		t.Fatal(err)
	}
	f := newExecuteFixture(t)
	f.planned[0].JobDef.Name = "publish"
	f.planned[0].JobDef.Command = fixtureproc.Write(t, filepath.Join(t.TempDir(), "publish"), fixtureproc.Program{Stdout: stream.String()})
	f.req = executingDryRunRequest()
	f.req.WorkspaceRoot, f.req.Config, f.req.Commands = f.wsRoot, f.cfg, []string{"publish"}
	f.req.Global.Output = "jsonl"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var code int
	out := captureStdout(t, func() {
		code = f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project},
			&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil,
			publishFinalizers(ctx, f.req, nil)...).ExitCode
	})

	if code != ExitError {
		t.Fatalf("exit code = %d, want %d: a conflict probe fails the dry run\noutput:\n%s", code, ExitError, out)
	}
	var failure *protocolcli.TaskRecord
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Record == protocolcli.RecordTaskEnd && record.Task != nil && record.Task.Status == "failed" {
			failure = record.Task
		}
	}
	if failure == nil || failure.Error == nil || !strings.Contains(failure.Error.Message, "conflict go go.putnami.dev/mod@v1.2.3 at https://go.putnami.dev") {
		t.Fatalf("failed task = %+v, want the member-probe row naming the conflict\noutput:\n%s", failure, out)
	}
	if len(failure.Diagnostics) != 1 || failure.Diagnostics[0].Code != jobs.MemberProbeConflictCode ||
		!strings.Contains(failure.Diagnostics[0].Message, "another zip digest") {
		t.Fatalf("diagnostics = %+v, want one conflict diagnostic with the reason", failure.Diagnostics)
	}
}
