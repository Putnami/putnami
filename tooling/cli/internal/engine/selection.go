package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"go.putnami.dev/cli/model/extension"
	protocolcli "go.putnami.dev/protocol/cli"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/agentartifacts"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

type autoProjectSelectionNote struct {
	enabled  bool
	mode     workspace.AutoSelectionMode
	baseline string
	branch   string
	commands []string
	reason   workspace.AutoSelectionReason
}

// selectProjects applies default, --impacted, and project filters, resolving the
// outcome onto req.Global (which is why it takes a pointer: --impacted becomes a
// concrete project-ID list that planning and the run markers both read).
//
// Unexported later: selection is an engine STAGE. It mutates
// req.Global, so a caller outside Engine.Run would be resolving selection for a
// run that never happens — the state the run markers and planning read.
func selectProjects(req *Request, ws *workspace.Workspace, extensions ...*extension.ExtensionDescription) ([]*workspace.Project, int) {
	// The executing side of a portable request plans the frozen selection: no
	// smart default, no impact analysis, no marker lookup can widen or narrow it.
	if req.Portable != nil {
		return selectFrozenProjects(req, ws)
	}
	autoNote, code := applyBareProjectSelection(req, ws, extensions)
	if code != ExitSuccess {
		return nil, code
	}

	// Classify what the caller asked for BEFORE --impacted rewrites
	// req.Global.Projects into a concrete id list: after that rewrite an impacted
	// run is indistinguishable from an explicit selector.
	//
	// The rules are the ones shared.ProjectSelection.Requested applies on the
	// interactive surfaces, so one invocation cannot be reported two ways: `--all`
	// is the explicit spelling of the default and narrows nothing, the parser's
	// own sentinels ("*", "[impacted]") are modes rather than selectors, and any
	// filter makes the run scoped.
	selector := strings.TrimSpace(req.Global.Projects)
	selectionMode := protocoljob.SelectionModeAll
	switch {
	case req.Global.Impacted:
		selectionMode = protocoljob.SelectionModeImpacted
	case hasProjectSelectionFilters(req.Global),
		selector != "" && selector != "*" && selector != impactedProjectsSentinel:
		selectionMode = protocoljob.SelectionModeProjects
	}
	var selectionBaseline, selectionBaselineSource string
	req.selectionEvidence = selectionEvidence{requestedMode: selectionMode}

	if req.Global.Impacted {
		// The task index is built from the extensions this run loaded, so the
		// selection reads the same task input declarations the cache keys on
		// (ADR 0044). A run that loaded none keeps the project-level mapping.
		selection, err := workspace.ImpactedSelectionForBaselineWithTasks(
			ws, req.Global.Baseline, workspace.NewTaskIndex(ws, extensions))
		switch {
		case err != nil && impactedFailureEndsRun(req, ws.Root, err):
			return nil, ExitError
		case err != nil:
			// Fallback to all — FilterProjects below uses ws.Projects
			iox.Fprintf(os.Stderr, impactedFallbackExplanation, err)
			req.selectionEvidence.diagnostics = append(req.selectionEvidence.diagnostics, strings.TrimSpace(fmt.Sprintf(impactedFallbackExplanation, err)))
			req.Global.Projects = "*"
			// The wire block reports what RAN, not what was asked for. This
			// branch runs every project, so calling it "impacted" would tell an
			// extension it may validate a subset of a run that in fact covered
			// the whole workspace — a silent under-validation, in exactly the
			// case where the baseline was already untrustworthy. No baseline is
			// reported either: there is none that produced this project set.
			selectionMode = protocoljob.SelectionModeAll
		default:
			req.Global.Baseline = selection.Baseline
			selectionBaseline = selection.Baseline
			// The tier that produced the ref, which the engine used to discard.
			// A consumer distrusts a fallback-tier baseline the same way the run
			// does, and cannot do so from the ref alone.
			selectionBaselineSource = string(selection.BaselineSource)
			req.selectionEvidence.changedPaths = append([]string{}, selection.ChangedFiles...)
			// The whole trace, uncapped, for the session record and the JSONL
			// stream: the block printed below under --verbose is its capped
			// rendering, and a hosted run has no terminal to print it on.
			req.selectionEvidence.openingEvents = []jobs.SessionRecord{{
				Type: workspace.ImpactTraceRecordType, Data: selection.TraceRecord().EventData(),
			}}
			// The per-project scope does not survive the flattening into
			// req.Global.Projects below; it rides the evidence to buildPlan,
			// which keeps a task-scoped project's jobs of the changed
			// extensions and drops the rest.
			req.selectionEvidence.taskScopes = selection.TaskScopes()
			for _, note := range []string{selection.BaselineExplanation(), selection.UnownedRootFilesExplanation()} {
				if note != "" {
					req.selectionEvidence.diagnostics = append(req.selectionEvidence.diagnostics, note)
				}
			}
			// The selection explains itself BEFORE the outcome is announced: a
			// changed path that reached no project is why the count below can be
			// zero over a non-empty diff, or smaller than the commit looks — and a
			// fallback-tier baseline is why the count can be wrong in both
			// directions.
			if shouldPrintAutoProjectSelectionNote(req.Global) {
				if note := selection.BaselineExplanation(); note != "" {
					iox.Fprintln(req.stdout(), note)
				}
				if note := selection.UnownedRootFilesExplanation(); note != "" {
					iox.Fprintln(req.stdout(), note)
				}
				// The other direction — a selection that is too LARGE — is
				// explained on request: which file seeded which project, and
				// which edge pulled each of the others in. --verbose is the
				// catalog's "show more" switch, so no new flag; under --plan
				// the block precedes the table, which is where an operator
				// asking "why 106 projects?" is looking.
				if req.Global.Verbose || req.Global.Debug {
					for _, line := range selection.TraceExplanation() {
						iox.Fprintln(req.stdout(), line)
					}
				}
			}
			if len(selection.Projects) == 0 {
				if autoNote.enabled {
					maybePrintAutoProjectSelectionNote(req.stdout(), autoNote, req.Global, 0)
				} else if !machineOutputRequested(req) {
					// Name the measured-against ref: "nothing changed" is only as
					// trustworthy as the baseline it was computed from.
					iox.Fprintln(req.stdout(), "  No impacted projects found against "+selection.Baseline+". Nothing to do.")
				}
				return nil, ExitSuccess
			}
			// The change's size by category and the areas its code spans:
			// recorded beside the trace, printed after the run.
			shapeEvents, shapeNotes := selection.ChangeShapeEvidence(ws, workspace.ChangeShapeOptions{
				Managed: agentartifacts.ManagedPaths(ws.Root), Quiet: req.Global.Quiet, PlanOnly: req.Global.Plan,
			})
			req.selectionEvidence.openingEvents = append(req.selectionEvidence.openingEvents, shapeEvents...)
			req.selectionEvidence.closingNotes = shapeNotes
			// Pass actual impacted project IDs to FilterProjects
			ids := make([]string, len(selection.Projects))
			for i, p := range selection.Projects {
				ids[i] = p.ID
			}
			req.Global.Projects = strings.Join(ids, ",")
		}
	}

	var defaultExcludeTags []string
	if req.Config != nil && req.Config.Disable != nil {
		defaultExcludeTags = req.Config.Disable.Tags
	}
	// Direct targeting: the user explicitly named projects (not --all, --impacted, or ".").
	// In this case, default exclude tags should not filter out the targeted projects.
	directTarget := !req.Global.All && !req.Global.Impacted &&
		req.Global.Projects != "" && req.Global.Projects != "."
	selectedProjects := workspace.FilterProjects(ws, workspace.FilterOptions{
		Projects:           req.Global.Projects,
		FilterTag:          req.Global.FilterTag,
		ExcludeTag:         req.Global.ExcludeTag,
		Exclude:            req.Global.Exclude,
		DefaultExcludeTags: defaultExcludeTags,
		DirectTarget:       directTarget,
		ScopeIndex:         ws.ScopeIndex,
	})

	if len(selectedProjects) == 0 {
		if autoNote.enabled && req.Global.Impacted {
			maybePrintAutoProjectSelectionNote(req.stdout(), autoNote, req.Global, 0)
			return nil, ExitSuccess
		}
		if !machineOutputRequested(req) {
			iox.Fprintln(req.stdout(), "  No projects matched. Nothing to do.")
		}
		// --impacted's empty set is a legitimate no-op, and so is a lifecycle run
		// over a workspace with no (matching) projects: `putnami install` on an
		// empty or fully tag-excluded workspace has always exited 0
		// (Request.WorkspaceLifecycle note 3).
		if req.Global.Impacted || req.WorkspaceLifecycle {
			return nil, ExitSuccess
		}
		// The notice above is the HUMAN presentation, and it lands on stdout,
		// which machine output suppresses so the document is the only thing
		// there. That left the FAILING exit with nothing on either stream: a
		// caller running `--output=json` and checking the status saw exit 2, an
		// empty stderr and no envelope, and could not tell an empty selection
		// from a crash. A failure names its cause on stderr in every
		// output format — the channel every other engine failure already uses.
		iox.Fprintf(os.Stderr, "putnami: no project matched %s; the workspace resolved %d project(s)\n",
			describeProjectSelection(req.Global), len(ws.Projects))
		return nil, ExitUsage
	}
	if autoNote.enabled {
		maybePrintAutoProjectSelectionNote(req.stdout(), autoNote, req.Global, len(selectedProjects))
	}
	// The run's SCOPE is settled; resolve its cache policy against the same
	// workspace before anything is planned.
	if code := resolveNoCacheProjects(req, ws); code != ExitSuccess {
		return nil, code
	}
	// `emptyImpact` is never set here, and that is not an omission: the engine
	// returns early on an empty impacted set above, so no job is planned and no
	// context is written. The member IS reachable on the interactive extension
	// path, which resolves through shared.ResolveProjectSelection and still runs
	// the subprocess so the extension can report the no-op itself. Forcing the
	// branch here would mean planning work for a run the engine has declined.
	req.selection = jobs.ResolvedRunSelection(
		selectionMode, selectionBaseline, selectionBaselineSource, selectedProjects)
	return selectedProjects, ExitSuccess
}

// resolveReleaseSetSelections resolves this run's release-set mode and the
// selections a channel-backed publish needs, and hands the whole workspace to
// the coordinator.
//
// It exists because one such session answers two different questions. Which
// members are republished is decided by the channel head, so the publish starts
// from every project: none may be truncated by a stale diff, a cwd-derived
// default selection, or a workspace default tag exclusion. What lint, test,
// build and validate cover is decided by the caller's own flags, exactly as it
// would be without the publish — and letting the coordinator narrow that too is
// how a changed library's tests stopped running on every publishing CI run.
//
// The verification half is resolved FIRST, on a COPY of the request, because
// selectProjects mutates req.Global: the copy takes the `--impacted` rewrite,
// the resolved baseline and the reported mode, and the caller's own request
// stays untouched for the coordinator's whole-workspace pass. Nothing else is
// adopted from the copy — the run's marker eligibility, its cache policy and
// its flags are the session's, not this projection's.
//
// The verification half is nil for a session the coordinator does not decide,
// for a publish-only session and for a deploy session: the second verifies
// nothing beside its members, and the third keeps its own selection and WIDENS
// to the members instead (ADR 0017 §2). An empty selection is not nil:
// `--impacted` over a tree where nothing changed verifies nothing and still
// publishes what the head requires.
func resolveReleaseSetSelections(
	req *Request,
	ws *workspace.Workspace,
	extensions ...*extension.ExtensionDescription,
) (jobs.ReleaseSetOptions, *jobs.ReleaseSetVerification, selectionEvidence, int) {
	// The release-set mode is what decides whether there are two selections at
	// all, so it is resolved here rather than beside the caller: a channel a
	// repository protects, or a tag this run cannot attribute to one line, is
	// refused before any selection resolves.
	options, err := buildReleaseSetOptions(req, ws)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return options, nil, selectionEvidence{}, protocolcli.ExitCodeForError(err)
	}
	if req == nil || !jobs.ReleaseSetPlansEveryProject(options) {
		return options, nil, selectionEvidence{}, ExitSuccess
	}
	verification, evidence, code := verificationSelection(req, ws, extensions...)
	if code != ExitSuccess {
		return options, nil, selectionEvidence{}, code
	}
	// "*" is an explicit workspace target and therefore overrides default tag
	// exclusions. Clear --all as well: selection deliberately treats that flag
	// as the ordinary default, which would otherwise keep those exclusions and
	// leave the coordinator unable to key one of its members.
	req.Global.Impacted, req.Global.All, req.Global.Projects = false, false, "*"
	return options, verification, evidence, ExitSuccess
}

// adoptVerificationEvidence binds, onto the run, the evidence for the half of a
// mixed session the coordinator did NOT decide.
//
// The evidence follows the selection it explains. A mixed session's
// verification half was chosen by the caller's own flags, so the baseline
// diagnostics and changed paths a portable request freezes are that
// resolution's, never the coordinator's whole-workspace view — and the recorded
// session's git block names the ref that resolution measured against, which is
// the ref the reported selection tier describes.
func adoptVerificationEvidence(req *Request, preparation *jobs.ReleaseSetPreparation, evidence selectionEvidence) {
	if req == nil || preparation == nil || preparation.Verification == nil {
		return
	}
	req.selectionEvidence = evidence
	if req.selection != nil {
		req.Global.Baseline = req.selection.Baseline
	}
}

// verificationSelection is the projection of the caller's own flags a mixed
// session verifies over. resolveReleaseSetSelections is its only caller and
// states why it exists; this half answers WHICH projects, and refuses the two
// session shapes that have no verification half at all.
func verificationSelection(
	req *Request,
	ws *workspace.Workspace,
	extensions ...*extension.ExtensionDescription,
) (*jobs.ReleaseSetVerification, selectionEvidence, int) {
	if req.Portable != nil {
		return nil, selectionEvidence{}, ExitSuccess
	}
	verifies := false
	for _, command := range req.Commands {
		if command == "deploy" {
			return nil, selectionEvidence{}, ExitSuccess
		}
		if command != "publish" {
			verifies = true
		}
	}
	if !verifies {
		return nil, selectionEvidence{}, ExitSuccess
	}
	gate := *req
	// The copy's human notices are withheld. They narrate a SELECTION STAGE —
	// "No impacted projects found against origin/main. Nothing to do." — and
	// this is a projection of one half of a session that goes on to publish, so
	// printing them would announce a no-op the run is not. The session's own
	// plan output states what it runs, and the one message that names a cause
	// rather than an outcome, the unresolvable-baseline fallback, goes to stderr
	// and is unaffected.
	gate.Stdout = io.Discard
	projects, code := selectProjects(&gate, ws, extensions...)
	if code != ExitSuccess {
		return nil, selectionEvidence{}, code
	}
	// selectProjects returns early, with no selection block, when `--impacted`
	// resolved cleanly over an unchanged tree. That is a real answer — verify
	// nothing, publish what the head requires — and it is reported as the
	// impacted no-op rather than as an absent selection.
	selection := gate.selection
	if selection == nil {
		selection = jobs.ResolvedRunSelection(
			protocoljob.SelectionModeImpacted, gate.Global.Baseline, "", nil)
		selection.EmptyImpact = true
	}
	return &jobs.ReleaseSetVerification{
		Projects: projects, Selection: selection,
	}, gate.selectionEvidence, ExitSuccess
}

// resolveNoCacheProjects turns the raw --no-cache-projects selector into the id
// set the scheduler consults per task.
//
// It runs HERE, in the selection stage, for three reasons: the workspace is
// resolved, the selector grammar is the one --projects already speaks (paths,
// aliases, groups, project names), and a selector that names nothing is a usage
// error this stage already knows how to report.
//
// A selector that matches no project FAILS the run instead of quietly selecting
// nothing. The flag exists to take a project OUT of the cache, so a typo that
// silently left it in would hand back a cached verdict for exactly the task the
// caller asked to re-derive — the fail-open a guard cannot afford.
//
// Tag filters, --exclude and the workspace's default tag exclusions are NOT
// applied: this is a cache-policy target list, not the run's scope, and a
// provider carrying a default-excluded tag must still be nameable here (it is
// the case @putnami/clientgen's workspace check depends on). DirectTarget says
// exactly that to the shared filter.
//
// Nothing it computes reaches a cache key, a run marker, or task params.
func resolveNoCacheProjects(req *Request, ws *workspace.Workspace) int {
	req.noCacheProjects = nil
	selector := strings.TrimSpace(req.Global.NoCacheProjects)
	if selector == "" {
		return ExitSuccess
	}
	// --no-cache already refuses the cache for every task, so the narrower
	// statement of the same intent has nothing left to add.
	if req.Global.NoCache {
		return ExitSuccess
	}
	scoped := workspace.FilterProjects(ws, workspace.FilterOptions{
		Projects:     selector,
		DirectTarget: true,
		ScopeIndex:   ws.ScopeIndex,
	})
	if len(scoped) == 0 {
		iox.Fprintf(os.Stderr,
			"putnami: no project matched --no-cache-projects %q; the workspace resolved %d project(s)\n",
			selector, len(ws.Projects))
		return ExitUsage
	}
	ids := make(map[string]bool, len(scoped))
	for _, project := range scoped {
		ids[project.ID] = true
	}
	req.noCacheProjects = ids
	return ExitSuccess
}

// impactedFailureEndsRun reports whether a failed impact analysis ends the
// run, and prints why. It always does where Git does not manage the workspace
// root: the fallback to every project widens a run whose baseline git could
// not resolve, and there is no repository to resolve one in. Inside a
// repository it does under --impacted-strict only.
func impactedFailureEndsRun(req *Request, wsRoot string, err error) bool {
	if unmanaged := git.Unmanaged(wsRoot); unmanaged != nil {
		iox.Fprintf(os.Stderr, "putnami: --impacted needs git history: %v\n", unmanaged)
		return true
	}
	if req.Global.ImpactedStrict {
		iox.Fprintf(os.Stderr, "putnami: --impacted failed: %v\n", err)
		return true
	}
	return false
}

func applyBareProjectSelection(req *Request, ws *workspace.Workspace, extensions []*extension.ExtensionDescription) (autoProjectSelectionNote, int) {
	if !isBareProjectSelection(req) {
		return autoProjectSelectionNote{}, ExitSuccess
	}

	sessStore := workspace_state.NewSessionStore(ws.Root)
	commandParams := req.runMarkerParams()
	paramsHash := workspace_state.LastBuildParamsHash(commandParams)
	selection, err := workspace.ResolveAutoSelection(ws, req.Commands, func(branch string, commands []string) (string, error) {
		sha, err := sessStore.LastBuildSHA(branch, commands, commandParams)
		if err != nil {
			if req.Global.Debug {
				iox.Fprintf(os.Stderr, "[debug] last build state ignored: %v\n", err)
			}
			return "", nil
		}
		if strings.TrimSpace(sha) != "" || req.Global.NoCache {
			return sha, nil
		}
		// Remote successful-run markers do not carry provenance yet. They may
		// narrow a permissive developer run, but must never let an authoritative
		// CI run skip projects based on an unproven developer marker.
		trust := store.CacheTrust(req.Global.CacheTrust)
		if !trust.AllowsRemoteRunMarkers() {
			return "", nil
		}

		// Speculative run-marker lookup during auto-selection. The degraded-cache
		// notice is suppressed here and surfaced once on the main build path
		// (below) so it is not emitted twice per invocation; skip records are
		// omitted for the same reason (they only enrich that notice).
		//
		// A hosted run reads the marker through the provider it started before
		// the hooks (startHostedRemoteCache), and reads none when it started
		// none: a provider started now would receive the run credential after
		// repository code ran.
		remote := req.hostedRemote
		if !runcredential.Hosted() {
			remote, _ = jobs.LoadRemoteCache(context.Background(), ws.Root, extensions, nil, trust, remoteCacheOptions()...)
		}
		remoteSHA, ok := remote.LookupRunMarker(context.Background(), jobs.RunMarkerWorkspaceID(ws), branch, commands, paramsHash)
		if remote != req.hostedRemote {
			remote.Close()
		}
		if !ok {
			return "", nil
		}
		if !git.CommitReachableFromHead(ws.Root, remoteSHA) {
			if req.Global.Debug {
				iox.Fprintf(os.Stderr, "[debug] remote run marker ignored: sha %q is not reachable from HEAD in this checkout\n", remoteSHA)
			}
			return "", nil
		}
		return remoteSHA, nil
	})
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: resolve default project selection: %v\n", err)
		return autoProjectSelectionNote{}, ExitError
	}
	// A bare run covers every project where Git does not manage the root, but a
	// baseline the caller named is a comparison, and there is nothing to compare.
	if selection.Reason == workspace.AutoSelectionReasonNoRepository && strings.TrimSpace(req.Global.Baseline) != "" {
		if unmanaged := git.Unmanaged(ws.Root); unmanaged != nil {
			iox.Fprintf(os.Stderr, "putnami: --baseline needs git history: %v\n", unmanaged)
			return autoProjectSelectionNote{}, ExitError
		}
	}

	note := autoProjectSelectionNote{
		enabled:  true,
		mode:     selection.Mode,
		baseline: selection.Baseline,
		branch:   selection.Branch,
		commands: append([]string(nil), req.Commands...),
		reason:   selection.Reason,
	}
	switch selection.Mode {
	case workspace.AutoSelectionAll:
		req.Global.All = true
		req.Global.Projects = "*"
		req.Global.AutoSelected = true
	case workspace.AutoSelectionImpacted:
		req.Global.Impacted = true
		req.Global.Projects = "[impacted]"
		req.Global.Baseline = selection.Baseline
		req.Global.AutoSelected = true
	default:
		iox.Fprintf(os.Stderr, "putnami: unknown default project selection mode %q\n", selection.Mode)
		return autoProjectSelectionNote{}, ExitError
	}
	return note, ExitSuccess
}

func isBareProjectSelection(req *Request) bool {
	if req == nil {
		return false
	}
	return !req.Global.All &&
		!req.Global.Impacted &&
		req.Global.Projects == "" &&
		!hasProjectSelectionFilters(req.Global)
}

func hasProjectSelectionFilters(g GlobalFlags) bool {
	return g.FilterTag != "" || g.ExcludeTag != "" || g.Exclude != ""
}

// describeProjectSelection renders what the run ASKED FOR, for a diagnostic
// that has to be actionable from a log alone. It quotes the flags rather than
// the resolved project list because an empty outcome is a question about the
// request: "--projects "/a,/b" matched nothing" says where to look, "0 projects
// matched" does not.
//
// It reads the request, so callers must use it BEFORE --impacted rewrites
// Projects into a concrete id list — or, as the empty-selection guard does,
// only on a branch --impacted never reaches.
func describeProjectSelection(g GlobalFlags) string {
	var described string
	selector := strings.TrimSpace(g.Projects)
	switch {
	case g.Impacted || selector == impactedProjectsSentinel:
		described = "the impacted selection"
	case selector == "" || selector == "*":
		described = "the default selection (every project)"
	default:
		described = "--projects " + strconv.Quote(selector)
	}
	var filters []string
	if g.FilterTag != "" {
		filters = append(filters, "--filter-tag "+strconv.Quote(g.FilterTag))
	}
	if g.ExcludeTag != "" {
		filters = append(filters, "--exclude-tag "+strconv.Quote(g.ExcludeTag))
	}
	if g.Exclude != "" {
		filters = append(filters, "--exclude "+strconv.Quote(g.Exclude))
	}
	if len(filters) > 0 {
		described += " filtered by " + strings.Join(filters, " ")
	}
	return described
}

func maybePrintAutoProjectSelectionNote(w io.Writer, note autoProjectSelectionNote, g GlobalFlags, count int) {
	if !shouldPrintAutoProjectSelectionNote(g) {
		return
	}
	printAutoProjectSelectionNote(w, note, count)
}

// impactedProjectsSentinel is what internal/cli's parser writes into
// Global.Projects for `--impacted`. It is a MODE, never a selector: reading
// Projects raw would classify an impacted run as an explicit selection.
const impactedProjectsSentinel = "[impacted]"

// impactedFallbackExplanation states the ONE branch that turns `--impacted`
// into a whole-workspace run. It is a format string taking the cause.
//
// That branch is not a selection: the change set could not be computed at all
// (an unresolvable baseline, a diff git refused), so the engine runs everything
// because running nothing would report a green gate over an unverified tree. It
// used to be SILENT, which made it indistinguishable from a genuinely wide
// change — the shape that gets diagnosed as "the selector treats a root file as
// everything changed" long after the run, when the baseline evidence is gone.
// The text names the cause, the consequence and the flag that turns the
// consequence into a failure, on stderr, which no adapter treats as a protocol
// channel.
const impactedFallbackExplanation = "putnami: --impacted could not compute the change set (%v) — " +
	"selecting EVERY project. Pass --impacted-strict to fail instead of running the whole workspace.\n"

// shouldPrintAutoProjectSelectionNote reports whether this run has a human
// notice stream to explain itself on: not quiet, not a machine channel, not a
// hosted run.
//
// The machine channels are named, rather than "any --output at all". A
// workspace that declares `"output": "text"` — the human mode, and the one this
// repository declares — used to suppress every selection notice, because the
// config value lands in Global.Output and the test was `!= ""`. Every notice
// the selection stage owes an operator was silent in exactly the workspaces
// that configured human output.
func shouldPrintAutoProjectSelectionNote(g GlobalFlags) bool {
	if g.Quiet {
		return false
	}
	switch protocolcli.OutputMode(g.Output) {
	case protocolcli.OutputAuto, protocolcli.OutputText:
	default:
		return false
	}
	return os.Getenv("K_SERVICE") == ""
}
