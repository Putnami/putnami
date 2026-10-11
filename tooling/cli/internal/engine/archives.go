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

// unpublishedArchiveProjects returns the selected projects that declare a
// release-archives publish but whose publish run uploads nothing for them —
// a silent failure where `publish --all` packaged the CLI binary every
// release yet no archive publisher was wired, so the binary never reached
// its download channel while the run still reported success.
//
// Detection is provider-agnostic and uses the final dependency graph: any
// archive uploader, whichever extension declares it, reads the
// project's package-archives output and so lists
// "<projectID>:package~archives" in DependsOn. A selected project that declares
// archive-publish intent (the "archives" publish channel or
// options.publish.archives) but whose package-archives key no planned publish
// job consumes has no uploader. The explicit `publish --archives=false`
// boundary is load-bearing: it deliberately removes the archive ecosystem and
// must not make unrelated npm/Go publication fail. An absent flag is not an
// opt-out, because a missing archive provider produces no archive plan nodes at
// all and is exactly the silent failure this guard must keep catching. Returns
// nil unless publish was actually selected — building archives via `putnami
// package` is legitimate.
//
// sessionProjects is the selection the SESSION made, read BEFORE a release-set
// publish narrows it (ReleaseSetRun.ScopePlanning) to the owners of the plan's
// members. Judging the narrowed list reopened the same hole: a project
// that declares an archives publish and owns no release-set member is dropped
// by that narrowing, so the guard never saw it and the run published nothing
// for it while reporting success.
//
// releaseMembers holds the projects that own a member of this session's
// release-set plan. It is nil when the session coordinates no release set and
// non-nil — possibly empty — when it does (ReleaseSetRun.MemberProjectIDs),
// which is how the second return value knows a missing uploader is a missing
// release-set member. The coordinator accounts for member owners: it fails the
// plan when a selected member lacks its package or publish step, and an
// unselected member is already on the channel head. Such a project is skipped
// here. Without that, a release-set run that republishes nothing — for example
// a lockfile-only commit that re-verifies every project — keeps every project
// selected for verification but plans no publish job, and every archive
// project would be reported as unpublished. A project outside the plan is
// still checked, so a missing archive publisher keeps failing the run.
//
// The second return value is the subset whose archives no release-set member
// announces: same failure, different cause, and the report names it so the fix
// is the extension's member declaration rather than installing a publisher.
func unpublishedArchiveProjects(commands []string, commandParams extension.ParamMap, sessionProjects []*workspace.Project, planned []*jobs.ScheduledJob, releaseMembers map[string]struct{}) (unpublished, unannounced []string) {
	publishing := false
	for _, c := range commands {
		if c == "publish" {
			publishing = true
			break
		}
	}
	if !publishing {
		return nil, nil
	}
	// The terminal adapter preserves the two explicit negative spellings in
	// their raw forms: --no-archives becomes false, while --archives=false is
	// the string "false". Other values are not opt-outs; this mirrors planner
	// truthiness and keeps absent or malformed archive providers fail-closed.
	if value, ok := commandParams["archives"]; ok {
		if enabled, ok := value.(bool); ok && !enabled {
			return nil, nil
		}
		if enabled, ok := value.(string); ok && enabled == "false" {
			return nil, nil
		}
	}

	// Projects whose package-archives output a publish job reads (uploads). The
	// archive build's plan name varies — "package~archives" for a Go binary,
	// "package~<ext>~archives" when another extension drives the Go packager —
	// so match the family by prefix/suffix rather than a synthesized key.
	publishedArchives := make(map[string]bool) // project ID
	for _, j := range planned {
		if j == nil || j.Project == nil || j.JobDef == nil {
			continue
		}
		if j.CommandName() != "publish" {
			continue
		}
		for _, dep := range j.DependsOn {
			if projID, ok := archiveDepProject(dep); ok {
				publishedArchives[projID] = true
			}
		}
	}

	for _, p := range sessionProjects {
		if p == nil || !projectDeclaresArchivePublish(p) {
			continue
		}
		if _, coordinated := releaseMembers[p.ID]; coordinated {
			continue
		}
		if publishedArchives[p.ID] {
			continue
		}
		unpublished = append(unpublished, p.Name)
		// A release set publishes exactly its members. Reaching here inside one
		// means no installed extension announced an archive member for this
		// project, so release-set planning excludes it and no upload exists to
		// plan.
		if releaseMembers != nil {
			unannounced = append(unannounced, p.Name)
		}
	}
	sort.Strings(unpublished)
	sort.Strings(unannounced)
	return unpublished, unannounced
}

// archiveDepProject reports whether a dependency key refers to a package
// archives build and, if so, returns the project ID. This is the edge every
// archive uploader has on the archives it reads. Two families qualify, both
// read by the same archive upload job:
//   - release archives: "<projectID>:package~archives" (a Go binary) or
//     "<projectID>:package~<ext>~archives" (another extension driving the Go
//     packager).
//   - content archives: "<projectID>:package~template" (the scaffold packager's
//     step for a `type:"template"` project published on the template-archives
//     channel) or "<projectID>:package~agent-content" (its step for a
//     content-only extension, whose manifest declares agentContent).
//     Scaffold content projects set options.publish.archives to build their
//     archive, and an upload job that consumes this key uploads them.
func archiveDepProject(depKey string) (string, bool) {
	projID, plan, ok := strings.Cut(depKey, ":")
	if !ok {
		return "", false
	}
	if strings.HasPrefix(plan, "package~") &&
		(strings.HasSuffix(plan, "~archives") || strings.HasSuffix(plan, "~template") || strings.HasSuffix(plan, "~agent-content")) {
		return projID, true
	}
	return "", false
}

// projectDeclaresArchivePublish reports whether a project asks to publish
// release archives — via the "archives" publish channel or the
// options.publish.archives job option. options.package.archives alone (build
// without a declared publish) does not count: that project only asked to
// package archives, not upload them.
func projectDeclaresArchivePublish(p *workspace.Project) bool {
	for _, ch := range p.Publish {
		if ch == "archives" {
			return true
		}
	}
	if p.Config != nil && p.Config.Options != nil {
		if pub, ok := p.Config.Options["publish"]; ok {
			if v, ok := pub["archives"].(bool); ok && v {
				return true
			}
		}
	}
	return false
}

// reportUnpublishedArchives reports a publish run that leaves a project's
// declared release archives with no uploader. Planning remains advisory, but an
// actual publish must fail so a release cannot appear successful while its
// binaries never reach the download channel. When discovery skipped an
// extension that could have been the uploader, the skip's root cause is cited
// so the fix (usually `putnami upgrade`) is one line away.
func reportUnpublishedArchives(projects, unannounced []string, skipped []extension.SkippedExtension, g GlobalFlags, fatal bool) {
	if len(projects) == 0 || (g.Quiet && !fatal) {
		return
	}
	level := "warning"
	if fatal {
		level = "error"
	}
	iox.Fprintf(os.Stderr, "putnami: %s: %s declares a release-archives publish but no publish step uploaded them\n", level, strings.Join(projects, ", "))
	iox.Fprintln(os.Stderr, "  the binary will not reach its download channel — install an extension whose publish step uploads release archives, and check that it is active")
	if len(unannounced) > 0 {
		iox.Fprintf(os.Stderr, "  %s%s\n", strings.Join(unannounced, ", "), unannouncedArchiveMembersNote)
	}
	if note := skippedExtensionsNote(skipped); note != "" {
		iox.Fprintf(os.Stderr, "  %s\n", note)
	}
}

// unannouncedArchiveMembersNote explains the cause, appended to the names it
// applies to: the session coordinates a release set, and these projects
// declare a release-archives publish that no extension this run planned
// announces a member for — the announcer may be absent, or skipped by
// discovery (the skipped note that follows says which) — so release-set
// planning drops them and plans no upload at all. A constant rather than a
// formatter because internal/engine is at its function ceiling
// (internal/cli/complexity_ceilings_test.go) and this is one sentence.
const unannouncedArchiveMembersNote = ": no extension this run planned announces an archive release-set member for them, so release-set planning drops them"

// skippedExtensionsNote formats discovery's skip records into the one-line
// root-cause hint appended to the archives guard. Empty when nothing was
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

func archivePublishExitCode(exit int, unpublishedArchives []string, fatal bool) int {
	if len(unpublishedArchives) == 0 || !fatal || exit != ExitSuccess {
		return exit
	}
	return ExitError
}

const unpublishedArchiveFailureKey = "putnami:publish~release-archives"

func unpublishedArchiveFailure(projects, unannounced []string, skipped []extension.SkippedExtension) func(map[string]*jobs.JobResult) {
	return func(results map[string]*jobs.JobResult) {
		if len(projects) == 0 {
			return
		}
		msg := strings.Join(projects, ", ") + " declares a release-archives publish but no publish step uploaded them; install an extension whose publish step uploads release archives, and check that it is active"
		if len(unannounced) > 0 {
			msg += " (" + strings.Join(unannounced, ", ") + unannouncedArchiveMembersNote + ")"
		}
		if note := skippedExtensionsNote(skipped); note != "" {
			msg += " (" + note + ")"
		}
		results[unpublishedArchiveFailureKey] = &jobs.JobResult{
			Status: "failed",
			Error: &jobs.JobError{
				Message: msg,
			},
		}
	}
}
