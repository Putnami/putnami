package jobs

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/memberprobe"
	"go.putnami.dev/sdk/extension/releaseset"
)

// memberProbeResultKey is the synthetic result row a dry-run publish fails
// under when a registry probe says the real publish cannot succeed as planned.
const memberProbeResultKey = "putnami:publish~member-probe"

// MemberProbeReport aggregates the member-probe events of an executing dry-run
// publish into one report and one verdict.
//
// Each publisher asks its own registry and reports one probe per member; a
// publish job succeeds whatever the answer. This is the single place the
// answers are read, so every conflict shows in one pass and the run fails once,
// naming all of them. It knows no ecosystem: it reads records through the
// protocol's strict reader and compares them with the release-set plan when
// the run has one.
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
// other session: only a dry run emits probes.
func (report MemberProbeReport) Finalizer() func(map[string]*JobResult) {
	if !report.DryRun || !slices.Contains(report.Commands, "publish") {
		return nil
	}
	return func(results map[string]*JobResult) {
		summary := summarizeMemberProbes(report.Run, results)
		summary.print(report.Out, report.Quiet)
		if failure := summary.failure(); failure != "" {
			results[memberProbeResultKey] = &JobResult{Status: "failed", Error: &JobError{Message: failure}}
		}
	}
}

// memberProbeSummary is what the probes of one dry run say, in report order.
type memberProbeSummary struct {
	// blocking are the conflict and unverified probes: each one fails the run.
	blocking []*extproto.MemberProbe
	// reused are the identical probes: the real publish reuses what the
	// registry holds.
	reused []*extproto.MemberProbe
	// absent counts the probes whose registry does not hold the version, and
	// anonymousAbsent those of them answered without a credential.
	absent, anonymousAbsent int
	// rejected are the events that are no usable verdict: one the strict reader
	// refuses, or one about another version than the plan publishes. Each one
	// fails the run.
	rejected []string
	// notProbed are the selected members of the plan no probe speaks about.
	notProbed []string
	// unasked is true when the run has no plan and no job reported a probe.
	unasked bool
}

// summarizeMemberProbes reads every member-probe event of the session. Results
// are walked in key order and probes sorted by member, so the same session
// prints the same report.
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
			probed[key] = true
			if member, expected := selected[key]; expected && probe.Version != member.Version {
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
		default:
			// The strict reader admits four states; the two left make the real
			// publish fail or leave its outcome unknown.
			summary.blocking = append(summary.blocking, probe)
		}
	}
	// With a plan, each selected member no probe speaks about is named with the
	// publisher and the publish step that would have asked. Without one, the
	// run can only say that nothing asked.
	for key, member := range selected {
		if probed[key] {
			continue
		}
		route := run.routes[key]
		summary.notProbed = append(summary.notProbed, fmt.Sprintf("%s %s@%s (publisher %s, step %s)",
			member.Ecosystem, member.Coordinate, member.Version, route.publisher, route.publishStep))
	}
	sort.Strings(summary.notProbed)
	summary.unasked = !planned && len(probed) == 0 && len(summary.rejected) == 0 && len(results) > 0
	return summary
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

// failure is the message the run fails with, naming every member the real
// publish cannot write as planned. It is empty when nothing blocks.
func (summary memberProbeSummary) failure() string {
	blocked := make([]string, 0, len(summary.blocking)+len(summary.rejected))
	for _, probe := range summary.blocking {
		blocked = append(blocked, fmt.Sprintf("%s %s: %s", probe.State, memberProbeLabel(probe), probe.Reason))
	}
	blocked = append(blocked, summary.rejected...)
	if len(blocked) == 0 {
		return ""
	}
	return fmt.Sprintf("publish --dry-run: %d registry check(s) say the publish cannot run as planned: %s",
		len(blocked), strings.Join(blocked, "; "))
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
			lines = append(lines, fmt.Sprintf("  %-10s  %s: the registry holds the same digest %s, so the publish reuses it",
				"reused", memberProbeLabel(probe), probe.RegistryDigest))
		}
		if summary.absent > 0 {
			lines = append(lines, fmt.Sprintf("  %-10s  %d member(s) are not in their registry: the publish uploads them", "absent", summary.absent))
		}
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
