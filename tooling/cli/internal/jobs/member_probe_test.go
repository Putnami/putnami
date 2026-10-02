package jobs

import (
	"bytes"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/memberprobe"
	"go.putnami.dev/sdk/extension/releaseset"
)

// memberProbeEvent is one member-probe event as a publish job emits it and the
// scheduler reads it back: written by the runtime emitter, parsed by the
// reader every job event goes through.
func memberProbeEvent(t *testing.T, probe extensionproto.MemberProbe) RawJobEvent {
	t.Helper()
	wire, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(wire, &data); err != nil {
		t.Fatal(err)
	}
	return rawArtifactEvent(t, probe.Ecosystem, probe.Coordinate, extensionproto.MemberProbeEventKind, data)
}

func rawArtifactEvent(t *testing.T, id, name, kind string, data map[string]any) RawJobEvent {
	t.Helper()
	var stream bytes.Buffer
	emitter := runtimeproto.NewEmitterForVersion(&stream, runtimeproto.MaxKnownProtocolVersion)
	if err := emitter.ArtifactData(id, name, kind, "", data); err != nil {
		t.Fatal(err)
	}
	event, ok := parseRawEvent(strings.TrimSpace(stream.String()))
	if !ok {
		t.Fatalf("runtime artifact %q failed strict parsing", stream.String())
	}
	return event
}

func probeOf(ecosystem, coordinate, version, registry, state string) extensionproto.MemberProbe {
	probe := extensionproto.MemberProbe{Ecosystem: ecosystem, Coordinate: coordinate, Version: version, Registry: registry, State: state}
	switch state {
	case extensionproto.MemberProbeConflict:
		probe.ArtifactDigest, probe.RegistryDigest = digestFor('a'), digestFor('b')
		probe.Reason = "the registry already holds this version with another digest"
	case extensionproto.MemberProbeIdentical:
		probe.ArtifactDigest, probe.RegistryDigest = digestFor('a'), digestFor('a')
	case extensionproto.MemberProbeUnverified:
		probe.Reason = "the registry could not be reached: connection refused"
	case extensionproto.MemberProbeTagMove:
		probe.ArtifactDigest, probe.RegistryDigest = digestFor('a'), digestFor('b')
	}
	return probe
}

// dryRunResults is one successful publish job per probe, keyed in order.
func dryRunResults(t *testing.T, probes ...extensionproto.MemberProbe) map[string]*JobResult {
	t.Helper()
	results := make(map[string]*JobResult, len(probes))
	for index, probe := range probes {
		key := string(rune('a'+index)) + ":publish~" + probe.Ecosystem
		results[key] = &JobResult{Status: "success", Events: []RawJobEvent{memberProbeEvent(t, probe)}}
	}
	return results
}

// finalizeDryRun runs the dry-run finalizer over results and returns the report
// it printed and the failure it recorded, empty when the run still passes.
func finalizeDryRun(t *testing.T, run *ReleaseSetRun, results map[string]*JobResult) (report, failure string) {
	t.Helper()
	var out bytes.Buffer
	finalize := MemberProbeReport{Run: run, Commands: []string{"publish"}, DryRun: true, Out: &out}.Finalizer()
	if finalize == nil {
		t.Fatal("a dry-run publish has no member-probe finalizer")
	}
	finalize(results)
	if result := results[memberProbeResultKey]; result != nil {
		if result.Status != "failed" || result.Error == nil {
			t.Fatalf("member-probe result = %+v, want a failed row with a message", result)
		}
		failure = result.Error.Message
	}
	return out.String(), failure
}

const (
	testNPMRegistry = "https://npm.putnami.dev"
	testGoRegistry  = "https://go.putnami.dev"
	testOCIRegistry = "oci.putnami.dev"
)

// One member the registry already holds at another digest fails the dry run,
// and the failure names the member, the registry and the version. The members
// the registry does not hold are counted, not listed.
func TestDryRunFailsOnOneConflictAmongSeveralMembers(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "one-pass-verdict", "one-conflict-fails-and-is-named")
	results := dryRunResults(t,
		probeOf("npm", "@putnami/runtime", "1.4.0", testNPMRegistry, extensionproto.MemberProbeAbsent),
		probeOf("go", "go.putnami.dev/sdk/extension", "v1.4.0", testGoRegistry, extensionproto.MemberProbeConflict),
		probeOf("oci", "putnami/cli", "1.4.0", testOCIRegistry, extensionproto.MemberProbeAbsent),
	)

	report, failure := finalizeDryRun(t, nil, results)

	const member = "go go.putnami.dev/sdk/extension@v1.4.0 at https://go.putnami.dev"
	if !strings.Contains(failure, "conflict "+member) || !strings.Contains(failure, "another digest") {
		t.Fatalf("failure = %q, want it to name the member, the registry and the version", failure)
	}
	if !strings.Contains(failure, "1 registry check(s)") {
		t.Fatalf("failure = %q, want exactly one blocking check", failure)
	}
	if !strings.Contains(report, "conflict    "+member) || !strings.Contains(report, "2 member(s) are not in their registry") {
		t.Fatalf("report does not name the conflict and count the absent members:\n%s", report)
	}
	if strings.Contains(report, "@putnami/runtime") {
		t.Fatalf("report lists an absent member one by one:\n%s", report)
	}
}

// Every conflict of the run is reported in one pass, in a stable order.
func TestDryRunNamesEveryConflictInOnePass(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "one-pass-verdict", "every-conflict-is-named")
	platform := probeOf("put", "putnami/cli", "1.4.0", "https://put.putnami.dev", extensionproto.MemberProbeConflict)
	platform.Platform = "linux/amd64"
	results := dryRunResults(t,
		probeOf("oci", "putnami/cli", "1.4.0", testOCIRegistry, extensionproto.MemberProbeConflict),
		platform,
		probeOf("npm", "@putnami/runtime", "1.4.0", testNPMRegistry, extensionproto.MemberProbeConflict),
		probeOf("go", "go.putnami.dev/sdk/extension", "v1.4.0", testGoRegistry, extensionproto.MemberProbeConflict),
	)

	report, failure := finalizeDryRun(t, nil, results)

	members := []string{
		"conflict go go.putnami.dev/sdk/extension@v1.4.0 at https://go.putnami.dev",
		"conflict npm @putnami/runtime@1.4.0 at https://npm.putnami.dev",
		"conflict oci putnami/cli@1.4.0 at oci.putnami.dev",
		"conflict put putnami/cli@1.4.0 [linux/amd64] at https://put.putnami.dev",
	}
	last := -1
	for _, member := range members {
		at := strings.Index(failure, member)
		if at <= last {
			t.Fatalf("failure = %q, want every conflict named once, sorted by member; missing or misplaced %q", failure, member)
		}
		last = at
	}
	if !strings.Contains(failure, "4 registry check(s)") || strings.Count(report, "\n  conflict ") != 4 {
		t.Fatalf("want four conflicts in the failure and the report:\n%s\n%s", failure, report)
	}
}

// A registry that could not answer fails the dry run: it never passes in
// silence, and the failure carries the reason.
func TestDryRunFailsOnAnUnverifiedProbe(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "one-pass-verdict", "unverified-fails")
	results := dryRunResults(t,
		probeOf("npm", "@putnami/runtime", "1.4.0", testNPMRegistry, extensionproto.MemberProbeAbsent),
		probeOf("go", "go.putnami.dev/sdk/extension", "v1.4.0", testGoRegistry, extensionproto.MemberProbeUnverified),
	)

	report, failure := finalizeDryRun(t, nil, results)

	const member = "unverified go go.putnami.dev/sdk/extension@v1.4.0 at https://go.putnami.dev: the registry could not be reached"
	if !strings.Contains(failure, member) {
		t.Fatalf("failure = %q, want it to name the member and the reason", failure)
	}
	if !strings.Contains(report, "unverified  go go.putnami.dev/sdk/extension@v1.4.0") {
		t.Fatalf("report does not name the unverified member:\n%s", report)
	}
}

// A run whose registries hold none of the versions passes, and says how many
// members it would upload.
func TestDryRunPassesWhenEveryMemberIsAbsent(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "one-pass-verdict", "all-absent-passes")
	results := dryRunResults(t,
		probeOf("npm", "@putnami/runtime", "1.4.0", testNPMRegistry, extensionproto.MemberProbeAbsent),
		probeOf("go", "go.putnami.dev/sdk/extension", "v1.4.0", testGoRegistry, extensionproto.MemberProbeAbsent),
	)

	report, failure := finalizeDryRun(t, nil, results)

	if failure != "" {
		t.Fatalf("an all-absent dry run failed: %s", failure)
	}
	if !strings.Contains(report, "2 member(s) are not in their registry") || strings.Contains(report, "warning") {
		t.Fatalf("report = %q, want the absent count and no warning", report)
	}
}

// A member the registry holds with the same digest is not a conflict: the run
// passes and the report says the publish reuses it. When the probe compared an
// artifact an earlier package staged, the line says so and names it.
func TestDryRunReportsAnIdenticalMemberAsReused(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "one-pass-verdict", "identical-is-reported-as-reused")
	staged := probeOf("go", "go.putnami.dev/sdk/extension", "v1.4.0", testGoRegistry, extensionproto.MemberProbeIdentical)
	staged.Reason = memberprobe.StagedReason("module zip")
	results := dryRunResults(t,
		probeOf("npm", "@putnami/runtime", "1.4.0", testNPMRegistry, extensionproto.MemberProbeIdentical),
		staged,
		probeOf("oci", "putnami/api", "1.4.0", testOCIRegistry, extensionproto.MemberProbeAbsent),
	)

	report, failure := finalizeDryRun(t, nil, results)

	if failure != "" {
		t.Fatalf("an identical member failed the dry run: %s", failure)
	}
	built := "reused      npm @putnami/runtime@1.4.0 at https://npm.putnami.dev: the registry holds the same digest " +
		digestFor('a') + ", so the publish reuses it\n"
	if !strings.Contains(report, built) {
		t.Fatalf("report does not say the member is reused, with its digest and no note:\n%s", report)
	}
	noted := "reused      go go.putnami.dev/sdk/extension@v1.4.0 at https://go.putnami.dev: the registry holds the same digest " +
		digestFor('a') + ", so the publish reuses it " +
		"(compared with the module zip the last `package` staged; re-run `package` if the source changed since)\n"
	if !strings.Contains(report, noted) {
		t.Fatalf("report does not name the staged module zip of the reused member:\n%s", report)
	}
}

// A version tag the registry holds at other content is a warning, not a
// failure: the real publish moves the tag. The warning names the tag, the
// member, the registry and both digests, or the image the publish builds when
// the dry run has no local digest, and quiet keeps it.
func TestDryRunWarnsAboutAMovedTag(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "one-pass-verdict", "tag-move-is-a-warning")
	moved := probeOf("oci", "putnami/api", "1.4.0", testOCIRegistry, extensionproto.MemberProbeTagMove)
	unbuilt := probeOf("oci", "putnami/worker", "1.4.0", testOCIRegistry, extensionproto.MemberProbeTagMove)
	unbuilt.ArtifactDigest = ""
	results := dryRunResults(t, moved, unbuilt)
	var out bytes.Buffer
	MemberProbeReport{Commands: []string{"publish"}, DryRun: true, Quiet: true, Out: &out}.Finalizer()(results)

	if result := results[memberProbeResultKey]; result != nil {
		t.Fatalf("a moved tag failed the dry run: %+v", result)
	}
	report := out.String()
	for _, want := range []string{
		"warning: publish will move tag 1.4.0 of putnami/api on oci.putnami.dev from " + digestFor('b') + " to " + digestFor('a'),
		"warning: publish will move tag 1.4.0 of putnami/worker on oci.putnami.dev from " + digestFor('b') + " to the image this publish builds",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report does not contain %q:\n%s", want, report)
		}
	}
}

// A moved tag does not hide a conflict elsewhere: the run fails on the
// conflict alone, and the moved tag stays a warning.
func TestDryRunFailsOnAConflictBesideAMovedTag(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "one-pass-verdict", "tag-move-does-not-hide-a-conflict")
	results := dryRunResults(t,
		probeOf("oci", "putnami/api", "1.4.0", testOCIRegistry, extensionproto.MemberProbeTagMove),
		probeOf("go", "go.putnami.dev/sdk/extension", "v1.4.0", testGoRegistry, extensionproto.MemberProbeConflict),
	)

	report, failure := finalizeDryRun(t, nil, results)

	if !strings.Contains(failure, "1 registry check(s)") || !strings.Contains(failure, "conflict go go.putnami.dev/sdk/extension@v1.4.0") {
		t.Fatalf("failure = %q, want the one conflict", failure)
	}
	if strings.Contains(failure, "putnami/api") {
		t.Fatalf("failure = %q names the moved tag", failure)
	}
	if events := results[memberProbeResultKey].Events; len(events) != 1 {
		t.Fatalf("failed row carries %d diagnostics, want the conflict's alone", len(events))
	}
	if !strings.Contains(report, "warning: publish will move tag 1.4.0 of putnami/api") {
		t.Fatalf("report does not warn about the moved tag:\n%s", report)
	}
}

func probePlanRun(members ...releaseset.PlannedMember) *ReleaseSetRun {
	run := &ReleaseSetRun{plan: &releaseset.Plan{Members: members}, dryRun: true, routes: map[string]releaseMemberRoute{}}
	for _, member := range members {
		publisher, step := "@putnami/go", string(member.Ecosystem)
		if member.Ecosystem == "put" {
			publisher, step = "@putnami/cloud", "archives"
		}
		run.routes[releaseset.MemberKey(member.Ecosystem, member.Coordinate)] = releaseMemberRoute{
			projectID: member.ProjectID, publisher: publisher, publishCommand: "publish", publishStep: step,
		}
	}
	return run
}

// With a plan, a selected member no publisher probed is named with its
// publisher and its publish step. It is a warning: the run still passes, and it
// never passes in silence. A member the plan does not select is not expected.
func TestDryRunWarnsAboutAPlannedMemberNoPublisherProbed(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "plan-coverage", "unprobed-member-is-a-warning")
	run := probePlanRun(
		releaseset.PlannedMember{Ecosystem: "go", Coordinate: "go.putnami.dev/sdk/extension", Version: "v1.4.0", Selected: true, ProjectID: "/sdk"},
		releaseset.PlannedMember{Ecosystem: "put", Coordinate: "putnami/cli", Version: "1.4.0", Selected: true, ProjectID: "/cli"},
		releaseset.PlannedMember{Ecosystem: "npm", Coordinate: "@putnami/runtime", Version: "1.3.0", ProjectID: "/runtime"},
	)
	results := dryRunResults(t,
		probeOf("go", "go.putnami.dev/sdk/extension", "v1.4.0", testGoRegistry, extensionproto.MemberProbeAbsent),
	)

	report, failure := finalizeDryRun(t, run, results)

	if failure != "" {
		t.Fatalf("an unprobed member failed the dry run: %s", failure)
	}
	if !strings.Contains(report, "warning: not probed: put putnami/cli@1.4.0 (publisher @putnami/cloud, step archives)") {
		t.Fatalf("report does not name the unprobed member, its publisher and its step:\n%s", report)
	}
	if strings.Contains(report, "@putnami/runtime") {
		t.Fatalf("report expects a probe for a member the plan does not select:\n%s", report)
	}
}

// Under a plan, a probe for a member the plan does not select, or does not
// know, is rejected and fails the run, as an unexpected publication does. A
// selected member whose route the run does not know is named without an empty
// publisher or step.
func TestDryRunRejectsAProbeForAMemberThePlanDoesNotSelect(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "plan-coverage", "unexpected-probe-fails")
	run := probePlanRun(
		releaseset.PlannedMember{Ecosystem: "go", Coordinate: "go.putnami.dev/sdk/extension", Version: "v1.4.0", Selected: true, ProjectID: "/sdk"},
		releaseset.PlannedMember{Ecosystem: "npm", Coordinate: "@putnami/runtime", Version: "1.3.0", ProjectID: "/runtime"},
		releaseset.PlannedMember{Ecosystem: "oci", Coordinate: "putnami/cli", Version: "1.4.0", Selected: true, ProjectID: "/cli"},
	)
	delete(run.routes, releaseset.MemberKey("oci", "putnami/cli"))
	results := dryRunResults(t,
		probeOf("go", "go.putnami.dev/sdk/extension", "v1.4.0", testGoRegistry, extensionproto.MemberProbeAbsent),
		probeOf("npm", "@putnami/runtime", "1.3.0", testNPMRegistry, extensionproto.MemberProbeAbsent),
		probeOf("npm", "@putnami/unknown", "1.0.0", testNPMRegistry, extensionproto.MemberProbeIdentical),
	)

	report, failure := finalizeDryRun(t, run, results)

	for _, want := range []string{
		`job "b:publish~npm" probed unexpected release-set member npm/@putnami/runtime`,
		`job "c:publish~npm" probed unexpected release-set member npm/@putnami/unknown`,
		"2 registry check(s)",
	} {
		if !strings.Contains(failure, want) {
			t.Fatalf("failure = %q, want %q", failure, want)
		}
	}
	if strings.Contains(report, "reused") || !strings.Contains(report, "1 member(s) are not in their registry") {
		t.Fatalf("report counts an unexpected probe:\n%s", report)
	}
	if !strings.Contains(report, "not probed: oci putnami/cli@1.4.0 (the publish route is unknown)") {
		t.Fatalf("report does not say the route is unknown:\n%s", report)
	}
}

// The route of an unprobed member names what the run knows of it.
func TestMemberRouteLabelNamesWhatIsKnown(t *testing.T) {
	for _, tc := range []struct {
		route releaseMemberRoute
		want  string
	}{
		{releaseMemberRoute{publisher: "@putnami/go", publishStep: "go"}, "publisher @putnami/go, step go"},
		{releaseMemberRoute{publisher: "@putnami/go"}, "publisher @putnami/go"},
		{releaseMemberRoute{publishStep: "go"}, "step go"},
		{releaseMemberRoute{}, "the publish route is unknown"},
	} {
		if got := memberRouteLabel(tc.route); got != tc.want {
			t.Errorf("memberRouteLabel(%+v) = %q, want %q", tc.route, got, tc.want)
		}
	}
}

// The failed result row carries one error diagnostic per blocking verdict, so
// the machine output lists each one with its code.
func TestDryRunFailureCarriesOneDiagnosticPerVerdict(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "one-pass-verdict", "verdicts-reach-the-machine-output")
	results := dryRunResults(t,
		probeOf("go", "go.putnami.dev/sdk/extension", "v1.4.0", testGoRegistry, extensionproto.MemberProbeConflict),
		probeOf("npm", "@putnami/runtime", "1.4.0", testNPMRegistry, extensionproto.MemberProbeUnverified),
		probeOf("oci", "putnami/cli", "1.4.0", testOCIRegistry, extensionproto.MemberProbeAbsent),
	)
	results["d:publish~put"] = &JobResult{Status: "success", Events: []RawJobEvent{
		rawArtifactEvent(t, "put", "putnami/cli", extensionproto.MemberProbeEventKind, map[string]any{"state": "superseded"}),
	}}

	_, failure := finalizeDryRun(t, nil, results)

	task := TaskResultOf(nil, results[memberProbeResultKey])
	want := []struct{ code, message string }{
		{MemberProbeConflictCode, "conflict go go.putnami.dev/sdk/extension@v1.4.0 at https://go.putnami.dev: "},
		{MemberProbeUnverifiedCode, "unverified npm @putnami/runtime@1.4.0 at https://npm.putnami.dev: "},
		{MemberProbeRejectedCode, `job "d:publish~put" emitted an invalid member probe`},
	}
	if len(task.Diagnostics) != len(want) {
		t.Fatalf("diagnostics = %+v, want one per blocking verdict", task.Diagnostics)
	}
	for index, diagnostic := range task.Diagnostics {
		if diagnostic.Severity != "error" || diagnostic.Code != want[index].code || !strings.HasPrefix(diagnostic.Message, want[index].message) {
			t.Fatalf("diagnostic %d = %+v, want error %s %q", index, diagnostic, want[index].code, want[index].message)
		}
		if !strings.Contains(failure, diagnostic.Message) {
			t.Fatalf("failure %q does not carry diagnostic %q", failure, diagnostic.Message)
		}
	}
}

// A probe about another version than the plan publishes is no verdict on the
// planned member: it is rejected and fails the run.
func TestDryRunRejectsAProbeForAnotherVersionThanPlanned(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "plan-coverage", "probe-version-must-match-the-plan")
	run := probePlanRun(
		releaseset.PlannedMember{Ecosystem: "go", Coordinate: "go.putnami.dev/sdk/extension", Version: "v1.4.0", Selected: true, ProjectID: "/sdk"},
	)
	results := dryRunResults(t,
		probeOf("go", "go.putnami.dev/sdk/extension", "v1.3.9", testGoRegistry, extensionproto.MemberProbeAbsent),
	)

	report, failure := finalizeDryRun(t, run, results)

	if !strings.Contains(failure, `probed go go.putnami.dev/sdk/extension at version "v1.3.9", and the release-set plan publishes "v1.4.0"`) {
		t.Fatalf("failure = %q, want the version mismatch", failure)
	}
	if !strings.Contains(report, "rejected") || strings.Contains(report, "not probed") || strings.Contains(report, "are not in their registry") {
		t.Fatalf("report = %q, want the rejection only: the probe is neither counted nor missing", report)
	}
}

// Without a plan the probes are aggregated all the same.
func TestDryRunAggregatesWithoutAPlan(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "one-pass-verdict", "no-plan-still-aggregates")
	results := dryRunResults(t,
		probeOf("npm", "@acme/widget", "2.0.0", "https://registry.npmjs.org", extensionproto.MemberProbeConflict),
		probeOf("npm", "@acme/gadget", "2.0.0", "https://registry.npmjs.org", extensionproto.MemberProbeAbsent),
	)
	// A job of the same session that emitted no probe changes nothing.
	results["z:build"] = &JobResult{Status: "success"}

	report, failure := finalizeDryRun(t, nil, results)

	if !strings.Contains(failure, "conflict npm @acme/widget@2.0.0 at https://registry.npmjs.org") {
		t.Fatalf("failure = %q, want the conflict named without a plan", failure)
	}
	if strings.Contains(report, "not probed") || strings.Contains(report, "no publish step") {
		t.Fatalf("report warns about coverage it cannot know without a plan:\n%s", report)
	}
}

// An absent answer obtained without a credential is weaker than an
// authenticated one: the report says so once, however many there are.
func TestDryRunWarnsOnceAboutAnonymousAbsentAnswers(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "plan-coverage", "anonymous-absent-is-a-warning")
	first := probeOf("npm", "@putnami/runtime", "1.4.0", testNPMRegistry, extensionproto.MemberProbeAbsent)
	second := probeOf("go", "go.putnami.dev/sdk/extension", "v1.4.0", testGoRegistry, extensionproto.MemberProbeAbsent)
	first.Anonymous, second.Anonymous = true, true
	results := dryRunResults(t, first, second,
		probeOf("oci", "putnami/cli", "1.4.0", testOCIRegistry, extensionproto.MemberProbeAbsent))

	report, failure := finalizeDryRun(t, nil, results)

	if failure != "" {
		t.Fatalf("an anonymous absent answer failed the dry run: %s", failure)
	}
	if strings.Count(report, "warning:") != 1 || !strings.Contains(report, "2 of the absent answers came from a request without a credential") {
		t.Fatalf("report = %q, want one warning counting the two anonymous answers", report)
	}
}

// A probe this build cannot read is a verdict it cannot drop: an unknown state,
// an unknown field or a missing reason fails the run and names the job.
func TestDryRunFailsOnAProbeTheStrictReaderRefuses(t *testing.T) {
	spectest.Proves(t, "cli/publish-dry-run-probe", "one-pass-verdict", "unreadable-probe-fails")
	valid := map[string]any{
		"ecosystem": "npm", "coordinate": "@putnami/runtime", "version": "1.4.0",
		"registry": testNPMRegistry, "state": extensionproto.MemberProbeAbsent,
	}
	type override struct {
		key   string
		value any
	}
	for name, change := range map[string]override{
		"unknown state":            {"state", "superseded"},
		"unknown field":            {"overwritable", true},
		"conflict without reason":  {"state", extensionproto.MemberProbeConflict},
		"credential in registry":   {"registry", "https://user:secret@npm.putnami.dev"},
		"identical without digest": {"state", extensionproto.MemberProbeIdentical},
	} {
		t.Run(name, func(t *testing.T) {
			data := maps.Clone(valid)
			data[change.key] = change.value
			results := map[string]*JobResult{"a:publish~npm": {
				Status: "success",
				Events: []RawJobEvent{rawArtifactEvent(t, "npm", "@putnami/runtime", extensionproto.MemberProbeEventKind, data)},
			}}

			report, failure := finalizeDryRun(t, nil, results)

			if !strings.Contains(failure, `job "a:publish~npm" emitted an invalid member probe`) {
				t.Fatalf("failure = %q, want the unreadable probe to fail the run and name the job", failure)
			}
			if strings.Contains(failure, "secret") || strings.Contains(report, "are not in their registry") {
				t.Fatalf("an unreadable probe leaked or was counted:\n%s\n%s", failure, report)
			}
		})
	}
}

// A dry-run publish whose jobs reported no probe at all does not pass in
// silence either: without a plan the report says that nothing asked.
func TestDryRunSaysWhenNoPublisherAskedARegistry(t *testing.T) {
	results := map[string]*JobResult{"a:publish~archives": {Status: "success"}}

	report, failure := finalizeDryRun(t, nil, results)

	if failure != "" {
		t.Fatalf("a dry run without probes failed: %s", failure)
	}
	if !strings.Contains(report, "warning: no publish step reported a registry check") {
		t.Fatalf("report = %q, want the warning that nothing asked", report)
	}
}

// Quiet keeps what needs action and drops what does not.
func TestQuietDryRunKeepsFailuresAndWarnings(t *testing.T) {
	anonymous := probeOf("npm", "@putnami/runtime", "1.4.0", testNPMRegistry, extensionproto.MemberProbeAbsent)
	anonymous.Anonymous = true
	results := dryRunResults(t, anonymous,
		probeOf("go", "go.putnami.dev/sdk/extension", "v1.4.0", testGoRegistry, extensionproto.MemberProbeIdentical),
		probeOf("oci", "putnami/cli", "1.4.0", testOCIRegistry, extensionproto.MemberProbeConflict),
	)
	var out bytes.Buffer

	MemberProbeReport{Commands: []string{"publish"}, DryRun: true, Quiet: true, Out: &out}.Finalizer()(results)

	report := out.String()
	if !strings.Contains(report, "conflict    oci putnami/cli@1.4.0") || !strings.Contains(report, "warning:") {
		t.Fatalf("quiet report dropped a failure or a warning:\n%s", report)
	}
	if strings.Contains(report, "reused") || strings.Contains(report, "are not in their registry") {
		t.Fatalf("quiet report kept a line that needs no action:\n%s", report)
	}
	if results[memberProbeResultKey] == nil {
		t.Fatal("a quiet dry run did not fail on a conflict")
	}
}

// Only an executing dry-run publish reads probes: a real publish, a preview and
// any other command carry no finalizer.
func TestMemberProbeFinalizerIsForADryRunPublishOnly(t *testing.T) {
	for name, report := range map[string]MemberProbeReport{
		"real publish":   {Commands: []string{"publish"}},
		"dry-run build":  {Commands: []string{"build"}, DryRun: true},
		"dry-run deploy": {Commands: []string{"deploy"}, DryRun: true},
	} {
		if report.Finalizer() != nil {
			t.Errorf("%s carries a member-probe finalizer", name)
		}
	}
}
