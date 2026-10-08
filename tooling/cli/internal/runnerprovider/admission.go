package runnerprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/runnersource"
)

// This file is the ADMISSION LAYER ADR 0032 defers to ADR 0037:
// the snapshot carries tracked and non-ignored untracked bytes, and a planned
// task may still need more — a git-ignored file its cache key selects, or an
// environment variable its key reads. Nothing here plans or schedules; it
// decides, on the plan's own declarations, what the snapshot must carry and
// what cannot travel at all.

// ErrInadmissible marks a remote request the admission refused on policy: a
// planned task keys on ambient environment, or a required ignored input is
// not a capturable file. It is the same class as an unsupported shape — a
// usage refusal before anything is captured or transferred — as opposed to a
// failure to ask Git.
var ErrInadmissible = errors.New("remote request is not admissible")

// recreatedRoots are the directory names, at any depth, whose contents the
// executing engine recreates through its own lifecycle — generation, the
// dependency install, the run's private state — so an ignored input under
// one of them is never bound: `.gen` (generated output, recreated by the
// generate lifecycle), `node_modules` (installed dependencies, recreated by
// the first-use bootstrap) and `.putnami` (sessions, caches and command
// output, owned by the run itself).
var recreatedRoots = map[string]bool{".gen": true, "node_modules": true, ".putnami": true}

// Admission is what the input admission decided for one plan: the paths the
// snapshot must bind, and the ignored paths it deliberately did not bind
// because no task declares them. The second half is reported, never silently
// dropped — a silently omitted required input is the defect this layer exists
// to close.
type Admission struct {
	// Bound are the sorted workspace-relative paths to capture flagged bound.
	Bound []string
	// Unbound names, per task and in plan order, the ignored files that task's
	// key hashes only through the whole-tree fallback.
	Unbound []UnboundInputs
}

// UnboundInputs is one task's ignored fallback inputs.
type UnboundInputs struct {
	// Task is the plan key of the task whose key hashes these paths.
	Task string
	// Paths are the sorted workspace-relative ignored paths that do not travel.
	Paths []string
}

// maxReportedUnbound bounds one task's diagnostic: a whole-tree fallback over
// a dirty worktree can name hundreds of artifacts, and the reader needs the
// shape of the problem, not its inventory.
const maxReportedUnbound = 5

// AdmitInputs decides, for the final plan, which git-ignored inputs the
// snapshot must bind and whether the request can leave this machine at all.
//
// A required ignored input is a file a planned task DECLARES as an input
// (declared key files, closure and workspace patterns, its own filePatterns,
// the command's filePatterns, the project's option layers, generate assets)
// that Git ignores in the worktree and that no lifecycle recreates. It is
// bound explicitly rather than omitted: a snapshot without it would run the
// task against different inputs than the local run and return a different
// verdict for the same request.
//
// A path selected ONLY by the key's whole-tree fallback — what a task with no
// declared file input keys on — is never bound. The fallback is not a
// statement of what the task reads, so binding it would ship whatever the
// worktree happens to hold (a build directory, a virtualenv, a core dump) and
// would refuse the whole request over one stale artifact past the protocol's
// file-size limit. Those paths are returned for reporting instead.
//
// An env-keyed task is refused outright: the values never travel, so the
// request could not describe the same execution.
func AdmitInputs(ctx context.Context, wsRoot string, inputs jobs.PortablePlanInputs) (Admission, error) {
	var admission Admission
	if err := rejectAmbientEnvironment(inputs); err != nil {
		return admission, err
	}
	declared, fallback, names := admissionCandidates(inputs)
	semanticGoEmbed := make(map[string]string)
	for _, task := range inputs.Tasks {
		for _, name := range task.GoEmbedFiles {
			semanticGoEmbed[name] = task.Key
		}
	}
	ignored, err := runnersource.IgnoredPaths(ctx, wsRoot, names)
	if err != nil {
		return admission, err
	}
	for _, name := range names {
		if !ignored[name] {
			continue
		}
		if recreatedByLifecycle(name, inputs.Outputs) {
			if task, required := semanticGoEmbed[name]; required {
				return Admission{}, fmt.Errorf("%w: task %s requires ignored Go embed input %q, which the source snapshot omits as lifecycle-owned output", ErrInadmissible, task, name)
			}
			continue
		}
		if tasks := declared[name]; len(tasks) > 0 {
			if err := admissibleBoundPath(wsRoot, name); err != nil {
				return Admission{}, fmt.Errorf("%w: task %s requires ignored input %q, which cannot be bound: %w", ErrInadmissible, tasks[0], name, err)
			}
			admission.Bound = append(admission.Bound, name)
			continue
		}
		for _, task := range fallback[name] {
			admission.addUnbound(task, name)
		}
	}
	return admission, nil
}

// admissionCandidates groups every candidate path by the tasks that select it,
// separating an explicit declaration from the whole-tree fallback, and returns
// the sorted union to ask Git about in one call.
func admissionCandidates(inputs jobs.PortablePlanInputs) (declared, fallback map[string][]string, names []string) {
	declared, fallback = map[string][]string{}, map[string][]string{}
	seen := map[string]bool{}
	add := func(into map[string][]string, name, task string) {
		into[name] = append(into[name], task)
		if !seen[name] {
			seen[name], names = true, append(names, name)
		}
	}
	for _, task := range inputs.Tasks {
		for _, name := range task.Files {
			add(declared, name, task.Key)
		}
		for _, name := range task.FallbackFiles {
			add(fallback, name, task.Key)
		}
	}
	sort.Strings(names)
	return declared, fallback, names
}

// addUnbound records one unbound path under its task, keeping plan order
// between tasks and sorted paths within one.
func (a *Admission) addUnbound(task, name string) {
	for index := range a.Unbound {
		if a.Unbound[index].Task == task {
			a.Unbound[index].Paths = append(a.Unbound[index].Paths, name)
			return
		}
	}
	a.Unbound = append(a.Unbound, UnboundInputs{Task: task, Paths: []string{name}})
}

// ReportUnbound writes one diagnostic per task whose ignored fallback inputs
// do not travel. It is a warning, not a refusal: the run continues, and the
// task either does not need those files or fails in the snapshot the way it
// would in a fresh clone. The remedy is in the message because the task, not
// this layer, is the only thing that can state what it reads.
func (a Admission) ReportUnbound(w io.Writer) {
	for _, unbound := range a.Unbound {
		shown := unbound.Paths
		suffix := ""
		if len(shown) > maxReportedUnbound {
			suffix = fmt.Sprintf(" and %d more", len(shown)-maxReportedUnbound)
			shown = shown[:maxReportedUnbound]
		}
		iox.Fprintf(w, "putnami: --where remote: task %s declares no file inputs, so %d git-ignored file(s) its cache key hashes locally do not travel with the snapshot: %s%s\n",
			unbound.Task, len(unbound.Paths), strings.Join(shown, ", "), suffix)
		iox.Fprintf(w, "  Declare them as filePatterns — or as a task output when the task generates them — to bind them into the request.\n")
	}
}

// rejectAmbientEnvironment refuses a plan whose cacheable task keys on
// environment values: the request stays credential-free and machine-free by
// construction, so the only honest answer is to not submit it.
func rejectAmbientEnvironment(inputs jobs.PortablePlanInputs) error {
	for _, task := range inputs.Tasks {
		if len(task.Env) > 0 {
			return fmt.Errorf("%w: task %s keys on environment variable(s) %s, which never travel with a remote request; run it locally or declare the input as a file", ErrInadmissible, task.Key, strings.Join(task.Env, ", "))
		}
	}
	return nil
}

// recreatedByLifecycle reports whether a workspace-relative path sits under a
// root the executing engine recreates: a recreatedRoots component at any
// depth, or a declared output of a planned task.
func recreatedByLifecycle(name string, outputs []string) bool {
	for _, component := range strings.Split(name, "/") {
		if recreatedRoots[component] {
			return true
		}
	}
	for _, output := range outputs {
		if name == output || strings.HasPrefix(name, output+"/") {
			return true
		}
	}
	return false
}

// admissibleBoundPath applies the manifest's own path rules and the capture's
// kind rule before anything is captured, so the refusal names the task and
// the path instead of surfacing as a capture failure.
func admissibleBoundPath(wsRoot, name string) error {
	if err := runner.ValidateSourcePath(name); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(wsRoot, filepath.FromSlash(name)))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("expected a regular file or symlink")
	}
	if info.Mode().IsRegular() && info.Size() > runner.MaxSourceFileBytes {
		return fmt.Errorf("file exceeds the protocol's %d byte limit", runner.MaxSourceFileBytes)
	}
	return nil
}

// VerifyAdmission is the executing side of the same decision. The
// materialized snapshot has no Git index, so what it can re-derive is the
// plan's declared inputs on the tree it was given: every admitted bound path
// must be selected by a planned task, must not sit under a recreated root,
// and must exist as a file or symlink; and no planned task may key on
// environment, since the submitter refuses such a plan. A difference means
// the snapshot, the plan or an extension diverged from what was admitted, and
// nothing is scheduled — the same answer validateExpectedPlan gives a
// divergent graph.
func VerifyAdmission(wsRoot string, admitted []string, inputs jobs.PortablePlanInputs) error {
	if err := rejectAmbientEnvironment(inputs); err != nil {
		return fmt.Errorf("the re-planned graph keys on ambient environment: %w", err)
	}
	// Only an EXPLICIT declaration admits a bound path, on this side too: the
	// whole-tree fallback selects whatever the executing tree happens to hold,
	// so accepting it here would admit a path the submitter never bound.
	selected := map[string]bool{}
	for _, task := range inputs.Tasks {
		for _, name := range task.Files {
			selected[name] = true
		}
	}
	var differences []string
	for _, name := range admitted {
		switch {
		case !selected[name]:
			differences = append(differences, fmt.Sprintf("bound input %q is not a declared input of any planned task", name))
		case recreatedByLifecycle(name, inputs.Outputs):
			differences = append(differences, fmt.Sprintf("bound input %q is recreated by the lifecycle and cannot be bound", name))
		default:
			if err := admissibleBoundPath(wsRoot, name); err != nil {
				differences = append(differences, fmt.Sprintf("bound input %q: %v", name, err))
			}
		}
	}
	if len(differences) == 0 {
		return nil
	}
	sort.Strings(differences)
	return fmt.Errorf("the admitted inputs differ from the re-derived ones (%d difference(s)):\n  %s", len(differences), strings.Join(differences, "\n  "))
}

// boundEntries lists the bound paths a captured manifest carries, sorted.
func boundEntries(manifest runner.SourceManifest) []string {
	var bound []string
	for _, entry := range manifest.Entries {
		if entry.Bound {
			bound = append(bound, entry.Path)
		}
	}
	return bound
}
