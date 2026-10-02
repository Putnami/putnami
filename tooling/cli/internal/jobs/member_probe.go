package jobs

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/memberprobe"
	"go.putnami.dev/sdk/extension/releaseset"
)

// memberProbeResultKey is the synthetic result row a dry-run publish fails
// under when a registry probe says the real publish cannot succeed as planned.
const memberProbeResultKey = "putnami:publish~member-probe"

// MemberProbeReport aggregates the member-probe events of an executing dry-run
// publish into one report and one verdict.
//
// It reads every member-probe event of the session through the protocol's
// strict reader, compares each one with the release-set plan when the run has
// one, prints every blocking verdict and every moved tag, and fails the run
// once, naming every blocking verdict. It reads no ecosystem-specific field.
type MemberProbeReport struct {
	// Run is the release-set coordination of this session, or nil for a publish
	// without a plan.
	Run *ReleaseSetRun
	// Commands are the commands the session runs.
	Commands []string
	// DryRun is true when the session executes its jobs under --dry-run.
	DryRun bool
	// Quiet suppresses the lines that report nothing to act on.
	Quiet bool
	// Out receives the report.
	Out io.Writer
}

// Finalizer returns the result finalizer of a dry-run publish, or nil for any
// other session. On a blocking verdict it adds a failed result row whose error
// names every one and which carries one error diagnostic per verdict, so the
// machine output lists them.
func (report MemberProbeReport) Finalizer() func(map[string]*JobResult) {
	if !report.DryRun || !slices.Contains(report.Commands, "publish") {
		return nil
	}
	return func(results map[string]*JobResult) {
		summary := summarizeMemberProbes(report.Run, results)
		summary.print(report.Out, report.Quiet)
		if failure := summary.failure(); failure != "" {
			results[memberProbeResultKey] = &JobResult{
				Status: "failed",
				Error:  &JobError{Message: failure},
				Events: summary.diagnosticEvents(),
			}
		}
	}
}

// Diagnostic codes of the failed member-probe result row.
const (
	// MemberProbeConflictCode marks a conflict verdict.
	MemberProbeConflictCode = "member-probe-conflict"
	// MemberProbeUnverifiedCode marks an unverified verdict.
	MemberProbeUnverifiedCode = "member-probe-unverified"
	// MemberProbeRejectedCode marks a member-probe event that is no usable
	// verdict.
	MemberProbeRejectedCode = "member-probe-rejected"
)

// memberProbeSummary is what the probes of one dry run say, in report order.
type memberProbeSummary struct {
	// blocking are the conflict and unverified probes: each one fails the run.
	blocking []*extproto.MemberProbe
	// reused are the identical probes: the real publish reuses what the
	// registry holds.
	reused []*extproto.MemberProbe
	// tagMoves are the tag-move probes: the real publish moves a version tag to
	// other content. Each one is a warning.
	tagMoves []*extproto.MemberProbe
	// absent counts the probes whose registry does not hold the version, and
	// anonymousAbsent those of them answered without a credential.
	absent, anonymousAbsent int
	// rejected are the events that are no usable verdict: one the strict reader
	// refuses, one about a member the plan does not select, or one about
	// another version than the plan publishes. Each one fails the run.
	rejected []string
	// notProbed are the selected members of the plan no probe speaks about.
	notProbed []string
	// unasked is true when the run has no plan and no job reported a probe.
	unasked bool
}

// summarizeMemberProbes reads every member-probe event of the session. Results
// are walked in key order and probes sorted by member, so the same session
// prints the same report. Under a plan, a probe for a member the plan does not
// select is rejected, as reconcilePublishedReleaseSet rejects an unexpected
// publication.
func summarizeMemberProbes(run *ReleaseSetRun, results map[string]*JobResult) memberProbeSummary {
	var summary memberProbeSummary
	planned := run != nil && run.plan != nil
	selected := map[string]releaseset.PlannedMember{}
	if planned {
		for _, member := range run.plan.SelectedMembers() {
			selected[releaseset.MemberKey(member.Ecosystem, member.Coordinate)] = member
		}
	}

	var probes []*extproto.MemberProbe
	probed := map[string]bool{}
	resultKeys := make([]string, 0, len(results))
	for key := range results {
		resultKeys = append(resultKeys, key)
	}
	sort.Strings(resultKeys)
	for _, resultKey := range resultKeys {
		result := results[resultKey]
		if result == nil {
			continue
		}
		for _, event := range result.Events {
			kind, _ := event.Data["kind"].(string)
			if event.Type != EventTypeArtifact || kind != extproto.MemberProbeEventKind {
				continue
			}
			probe, err := memberprobe.Decode(event.Data)
			if err != nil {
				summary.rejected = append(summary.rejected, fmt.Sprintf("job %q emitted an invalid member probe: %v", resultKey, err))
				continue
			}
			key := releaseset.MemberKey(distribution.Ecosystem(probe.Ecosystem), probe.Coordinate)
			member, expected := selected[key]
			if planned && !expected {
				summary.rejected = append(summary.rejected, fmt.Sprintf(
					"job %q probed unexpected release-set member %s", resultKey, printableReleaseKey(key)))
				continue
			}
			probed[key] = true
			if expected && probe.Version != member.Version {
				summary.rejected = append(summary.rejected, fmt.Sprintf(
					"job %q probed %s %s at version %q, and the release-set plan publishes %q",
					resultKey, probe.Ecosystem, probe.Coordinate, probe.Version, member.Version))
				continue
			}
			probes = append(probes, probe)
		}
	}
	sort.SliceStable(probes, func(i, j int) bool { return memberProbeLabel(probes[i]) < memberProbeLabel(probes[j]) })

	for _, probe := range probes {
		switch probe.State {
		case extproto.MemberProbeAbsent:
			summary.absent++
			if probe.Anonymous {
				summary.anonymousAbsent++
			}
		case extproto.MemberProbeIdentical:
			summary.reused = append(summary.reused, probe)
		case extproto.MemberProbeTagMove:
			summary.tagMoves = append(summary.tagMoves, probe)
		default:
			// MemberProbeConflict and MemberProbeUnverified.
			summary.blocking = append(summary.blocking, probe)
		}
	}
	// With a plan, each selected member no probe speaks about is named with
	// what the run knows of the step that would have asked. Without one, the
	// report says only that nothing asked.
	for key, member := range selected {
		if probed[key] {
			continue
		}
		summary.notProbed = append(summary.notProbed, fmt.Sprintf("%s %s@%s (%s)",
			member.Ecosystem, member.Coordinate, member.Version, memberRouteLabel(run.routes[key])))
	}
	sort.Strings(summary.notProbed)
	summary.unasked = !planned && len(probed) == 0 && len(summary.rejected) == 0 && len(results) > 0
	return summary
}

// memberRouteLabel names the publisher and the publish step of a member's
// route, each one only when the run knows it.
func memberRouteLabel(route releaseMemberRoute) string {
	var parts []string
	if route.publisher != "" {
		parts = append(parts, "publisher "+route.publisher)
	}
	if route.publishStep != "" {
		parts = append(parts, "step "+route.publishStep)
	}
	if len(parts) == 0 {
		return "the publish route is unknown"
	}
	return strings.Join(parts, ", ")
}

// memberProbeLabel names what a probe asked: the member, its version, the
// platform when the member has one artifact per platform, and the registry.
func memberProbeLabel(probe *extproto.MemberProbe) string {
	label := fmt.Sprintf("%s %s@%s", probe.Ecosystem, probe.Coordinate, probe.Version)
	if probe.Platform != "" {
		label += " [" + probe.Platform + "]"
	}
	return label + " at " + probe.Registry
}

// blockingVerdict is one verdict that fails the run: its diagnostic code and
// the sentence that names it.
type blockingVerdict struct {
	code, message string
}

// blocked lists every verdict that fails the run, blocking probes first in
// report order, then rejected events.
func (summary memberProbeSummary) blocked() []blockingVerdict {
	verdicts := make([]blockingVerdict, 0, len(summary.blocking)+len(summary.rejected))
	for _, probe := range summary.blocking {
		code := MemberProbeConflictCode
		if probe.State == extproto.MemberProbeUnverified {
			code = MemberProbeUnverifiedCode
		}
		verdicts = append(verdicts, blockingVerdict{code, fmt.Sprintf("%s %s: %s", probe.State, memberProbeLabel(probe), probe.Reason)})
	}
	for _, rejected := range summary.rejected {
		verdicts = append(verdicts, blockingVerdict{MemberProbeRejectedCode, rejected})
	}
	return verdicts
}

// failure is the message the run fails with, naming every member the real
// publish cannot write as planned. It is empty when nothing blocks.
func (summary memberProbeSummary) failure() string {
	verdicts := summary.blocked()
	if len(verdicts) == 0 {
		return ""
	}
	messages := make([]string, 0, len(verdicts))
	for _, verdict := range verdicts {
		messages = append(messages, verdict.message)
	}
	return fmt.Sprintf("publish --dry-run: %d registry check(s) say the publish cannot run as planned: %s",
		len(verdicts), strings.Join(messages, "; "))
}

// diagnosticEvents is one error diagnostic event per blocking verdict, in the
// shape a job's own diagnostic has.
func (summary memberProbeSummary) diagnosticEvents() []RawJobEvent {
	verdicts := summary.blocked()
	events := make([]RawJobEvent, 0, len(verdicts))
	for _, verdict := range verdicts {
		events = append(events, RawJobEvent{
			Version: runtimeproto.MaxKnownProtocolVersion,
			Type:    EventTypeDiagnostic,
			Message: verdict.message,
			Data: map[string]any{
				"severity": string(runtimeproto.SeverityError),
				"message":  verdict.message,
				"code":     verdict.code,
			},
		})
	}
	return events
}

// print writes the report: every blocking probe and every warning always, and
// the lines that need no action unless quiet.
func (summary memberProbeSummary) print(out io.Writer, quiet bool) {
	var lines []string
	for _, probe := range summary.blocking {
		lines = append(lines, fmt.Sprintf("  %-10s  %s: %s", probe.State, memberProbeLabel(probe), probe.Reason))
	}
	for _, rejected := range summary.rejected {
		lines = append(lines, fmt.Sprintf("  %-10s  %s", "rejected", rejected))
	}
	if !quiet {
		for _, probe := range summary.reused {
			// The reason of an identical probe names the staged artifact it
			// compared, which may predate the source.
			note := ""
			if probe.Reason != "" {
				note = " (" + probe.Reason + ")"
			}
			lines = append(lines, fmt.Sprintf("  %-10s  %s: the registry holds the same digest %s, so the publish reuses it%s",
				"reused", memberProbeLabel(probe), probe.RegistryDigest, note))
		}
		if summary.absent > 0 {
			lines = append(lines, fmt.Sprintf("  %-10s  %d member(s) are not in their registry: the publish uploads them", "absent", summary.absent))
		}
	}
	for _, probe := range summary.tagMoves {
		target := probe.ArtifactDigest
		if target == "" {
			target = "the image this publish builds"
		}
		lines = append(lines, fmt.Sprintf("  warning: publish will move tag %s of %s on %s from %s to %s",
			probe.Version, probe.Coordinate, probe.Registry, probe.RegistryDigest, target))
	}
	if summary.anonymousAbsent > 0 {
		lines = append(lines, fmt.Sprintf(
			"  warning: %d of the absent answers came from a request without a credential. A registry answers such a request for a private member as it does for a missing one; sign in and run the dry run again to confirm.",
			summary.anonymousAbsent))
	}
	for _, member := range summary.notProbed {
		lines = append(lines, fmt.Sprintf("  warning: not probed: %s reported no registry check, so this dry run does not say whether the registry already holds it", member))
	}
	if summary.unasked {
		lines = append(lines, "  warning: no publish step reported a registry check, so this dry run does not say whether a version already exists")
	}
	if len(lines) == 0 {
		return
	}
	_, _ = fmt.Fprintln(out, "putnami: publish --dry-run registry checks")
	for _, line := range lines {
		_, _ = fmt.Fprintln(out, line)
	}
}
