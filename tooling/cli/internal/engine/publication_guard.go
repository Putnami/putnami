package engine

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// unservedProject is a selected project and the declared publish channels no
// planned job serves.
type unservedProject struct {
	name     string
	channels []string
}

// unpublishedProjects returns the selected projects that declare publish
// channels (workspace.Project.Publish) and the declared channels no planned job
// serves — a silent failure where `publish --all` ran, yet no installed
// extension planned a publish step for a declared channel, so the project never
// reached its registry while the run still reported success. A channel name is
// an opaque string from the project's own declaration; the guard knows none.
//
// Detection is provider-agnostic and uses the final plan. A project's declared
// channel is served when the plan holds a job whose task declares that channel
// in publishChannels (jobs.TaskDeclarationOf), and that job is the project's
// own, or lists a job of that project in DependsOn (a dependency key is
// "<projectID>:<plan>", so only the project ID prefix counts, never a step
// suffix). The command's traits decide nothing: a job serves only the channels
// its task declares, and a job whose task declares none serves nothing. An
// explicit `--<channel>=false` or `--no-<channel>` is the opt-out of that
// channel: it deliberately removes it and must not make unrelated publication
// fail, and a project whose declared channels are all opted out is skipped. An
// absent flag is not an opt-out, because a missing provider produces no plan
// nodes at all and is exactly the silent failure this guard must keep
// catching. Returns nil unless the run publishes (ReleaseSetOptions.Publishes)
// — building with `putnami package` is legitimate.
//
// sessionProjects is the selection the SESSION made, read BEFORE a release-set
// publish narrows it (ReleaseSetRun.ScopePlanning) to the owners of the plan's
// members. Judging the narrowed list reopened the same hole: a project
// that declares a publish channel and owns no release-set member is dropped
// by that narrowing, so the guard never saw it and the run published nothing
// for it while reporting success.
//
// releaseMembers holds the projects that own a member of this session's
// release-set plan. It is nil when the session coordinates no release set and
// non-nil — possibly empty — when it does (ReleaseSetRun.MemberProjectIDs),
// which is how the second return value knows a missing publish step is a
// missing release-set member. The coordinator accounts for member owners: it
// fails the plan when a selected member lacks its package or publish step, and
// an unselected member is already on the channel head. Such a project is
// skipped here. Without that, a release-set run that republishes nothing — for
// example a lockfile-only commit that re-verifies every project — keeps every
// project selected for verification but plans no publish job, and every
// declaring project would be reported as unpublished. A project outside the
// plan is still checked, so a missing publisher keeps failing the run.
//
// The second return value is the subset whose channels no release-set member
// announces: same failure, different cause, and the report names it so the fix
// is the extension's member declaration rather than installing a publisher.
func unpublishedProjects(publishes bool, commandParams extension.ParamMap, sessionProjects []*workspace.Project, planned []*jobs.ScheduledJob, releaseMembers map[string]struct{}) (unserved []unservedProject, unannounced []string) {
	if !publishes {
		return nil, nil
	}

	// The {project ID, channel} pairs the plan serves: a planned job whose task
	// declares a channel serves it for its own project and for every project
	// one of whose jobs it depends on.
	served := make(map[[2]string]bool)
	for _, j := range planned {
		declaration := jobs.TaskDeclarationOf(j)
		if declaration == nil || j.Project == nil {
			continue
		}
		projects := []string{j.Project.ID}
		for _, dep := range j.DependsOn {
			if projID, _, ok := strings.Cut(dep, ":"); ok {
				projects = append(projects, projID)
			}
		}
		for _, projID := range projects {
			for _, ch := range declaration.PublishChannels {
				served[[2]string{projID, ch}] = true
			}
		}
	}

	for _, p := range sessionProjects {
		if p == nil || len(p.Publish) == 0 {
			continue
		}
		if _, coordinated := releaseMembers[p.ID]; coordinated {
			continue
		}
		// The terminal adapter preserves the two explicit negative spellings in
		// their raw forms: --no-<channel> becomes false, while --<channel>=false
		// is the string "false". Other values are not opt-outs; this mirrors
		// planner truthiness and keeps absent or malformed providers fail-closed.
		var channels []string
		for _, ch := range p.Publish {
			if served[[2]string{p.ID, ch}] {
				continue
			}
			if off, ok := commandParams[ch].(bool); ok && !off {
				continue
			}
			if off, ok := commandParams[ch].(string); ok && off == "false" {
				continue
			}
			channels = append(channels, ch)
		}
		if len(channels) == 0 {
			continue
		}
		unserved = append(unserved, unservedProject{name: p.Name, channels: channels})
		// A release set publishes exactly its members. Reaching here inside one
		// means no installed extension announced a member for this project, so
		// release-set planning excludes it and no publish step exists to plan.
		if releaseMembers != nil {
			unannounced = append(unannounced, p.Name)
		}
	}
	sort.Slice(unserved, func(i, j int) bool { return unserved[i].name < unserved[j].name })
	sort.Strings(unannounced)
	return unserved, unannounced
}

// unservedSummary names each project with its unserved channels, in the
// wording both the report and the failure use.
func unservedSummary(unserved []unservedProject) string {
	parts := make([]string, 0, len(unserved))
	for _, u := range unserved {
		noun := "channel"
		if len(u.channels) > 1 {
			noun = "channels"
		}
		parts = append(parts, fmt.Sprintf("%s: no planned publish step serves its declared publish %s %s", u.name, noun, strings.Join(u.channels, ", ")))
	}
	return strings.Join(parts, "; ")
}

// reportUnpublishedProjects reports a publish run that leaves a project's
// declared publish channels with no publish step. Planning remains advisory,
// but an actual publish must fail so a release cannot appear successful while
// nothing reaches the registry. When discovery skipped an extension that could
// have been the publisher, the skip's root cause is cited so the fix (usually
// `putnami upgrade`) is one line away.
func reportUnpublishedProjects(unserved []unservedProject, unannounced []string, skipped []extension.SkippedExtension, g GlobalFlags, fatal bool) {
	if len(unserved) == 0 || (g.Quiet && !fatal) {
		return
	}
	level := "warning"
	if fatal {
		level = "error"
	}
	iox.Fprintf(os.Stderr, "putnami: %s: %s\n", level, unservedSummary(unserved))
	iox.Fprintln(os.Stderr, "  nothing is published for these channels — install the extension that publishes each channel, and check that it is active")
	if len(unannounced) > 0 {
		iox.Fprintf(os.Stderr, "  %s%s\n", strings.Join(unannounced, ", "), unannouncedMembersNote)
	}
	if note := skippedExtensionsNote(skipped); note != "" {
		iox.Fprintf(os.Stderr, "  %s\n", note)
	}
}

// unannouncedMembersNote explains the cause, appended to the names it
// applies to: the session coordinates a release set, and these projects
// declare publish channels that no extension this run planned announces a
// member for — the announcer may be absent, or skipped by discovery (the
// skipped note that follows says which) — so release-set planning drops them
// and plans no publish step at all. A constant rather than a formatter because
// internal/engine is at its function ceiling
// (internal/cli/complexity_ceilings_test.go) and this is one sentence.
const unannouncedMembersNote = ": no extension this run planned announces a release-set member for them, so release-set planning drops them"

// skippedExtensionsNote formats discovery's skip records into the one-line
// root-cause hint appended to the publication guard. Empty when nothing was
// skipped.
func skippedExtensionsNote(skipped []extension.SkippedExtension) string {
	if len(skipped) == 0 {
		return ""
	}
	parts := make([]string, 0, len(skipped))
	for _, s := range skipped {
		parts = append(parts, fmt.Sprintf("%s was skipped: %v", s.Name, s.Reason))
	}
	return "note: " + strings.Join(parts, "; ") +
		" — a newer published version may already fix this; run `putnami extensions update` or `putnami upgrade`"
}

func publicationGuardExitCode(exit int, unserved []unservedProject, fatal bool) int {
	if len(unserved) == 0 || !fatal || exit != ExitSuccess {
		return exit
	}
	return ExitError
}

const unpublishedFailureKey = "putnami:publish~declared-channels"

func unpublishedFailure(unserved []unservedProject, unannounced []string, skipped []extension.SkippedExtension) func(map[string]*jobs.JobResult) {
	return func(results map[string]*jobs.JobResult) {
		if len(unserved) == 0 {
			return
		}
		msg := unservedSummary(unserved) + "; install the extension that publishes each channel, and check that it is active"
		if len(unannounced) > 0 {
			msg += " (" + strings.Join(unannounced, ", ") + unannouncedMembersNote + ")"
		}
		if note := skippedExtensionsNote(skipped); note != "" {
			msg += " (" + note + ")"
		}
		results[unpublishedFailureKey] = &jobs.JobResult{
			Status: "failed",
			Error: &jobs.JobError{
				Message: msg,
			},
		}
	}
}
