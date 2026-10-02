package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	model "go.putnami.dev/cli/model/extension"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/envkeys"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const unavailableRuntimeToolchainIdentity = "unavailable"

// toolchainProvisioningCommand is the well-known verb whose tasks install the
// workspace's language ecosystem. Its run writes the files the lock's toolchain
// pins derive from (go.work, package.json#packageManager), and `putnami install`
// pins them only after it succeeds. Its toolchain references therefore resolve
// as if optional: a missing pin, a missing host integrity or no matching
// candidate yields the unavailable identity instead of failing the run, so a
// workspace whose lock does not pin a toolchain yet can always be installed. A
// reference that a planned job of another command also requires resolves
// strictly, and a job of any other command never runs on a provisioning
// resolution while a lock exists (RequirePlannedRuntimeToolchains,
// ensureJobRuntimeToolchains). The prepare toolchains of a local-source
// extension resolve the same way only in a run whose every active command is
// toolchainProvisioningCommand (provisioningOnlyRun).
//
// extensionproto.WorkspaceFetchCommand resolves the same way: a hosted run
// fetches the dependencies before the installers run, so the fetch starts
// before the pinned toolchain may be installed (isToolchainProvisioningCommand).
const toolchainProvisioningCommand = "workspace-install"

// isToolchainProvisioningCommand reports whether command, or the command a
// pipeline step job belongs to, is toolchainProvisioningCommand or
// extensionproto.WorkspaceFetchCommand, which runs before it.
func isToolchainProvisioningCommand(command string) bool {
	switch extensionproto.BaseCommandName(command) {
	case toolchainProvisioningCommand, extensionproto.WorkspaceFetchCommand:
		return true
	}
	return false
}

// provisioningOnlyRun reports whether the run's active commands, dependency
// closure included, are all toolchainProvisioningCommand. Such a run starts no
// job or hook that another command's pin is meant to stop, so its extension
// runtimes may be prepared without a usable pin. A prepared runtime's digest
// carries the unavailable identity, so it is never served to a pinned run.
func provisioningOnlyRun(activeCommands map[string]bool) bool {
	provisioning := false
	for command, active := range activeCommands {
		if !active {
			continue
		}
		if !isToolchainProvisioningCommand(command) {
			return false
		}
		provisioning = true
	}
	return provisioning
}

// runtimeToolchainMu guards every read and write of
// ExtensionDescription.RuntimeToolchains. Resolution normally completes in the
// single-threaded synchronization stage, but a hand-built or internal job can
// still reach ensureJobRuntimeToolchains from a scheduler worker, and the same
// extension description is shared by every job that uses it. The critical
// sections are a map lookup or one probe, so serializing them also stops two
// workers from probing the same executable twice.
var runtimeToolchainMu sync.Mutex

// resolveRuntimeToolchainsForCommands resolves the requirements of the tasks
// the selected commands can run, before project selection, planning and every
// cache key. Resolving here rather than per job is what makes the resolved
// identity an INPUT to the key: a job that resolved its toolchain after the key
// was computed would be stored under an identity it did not run with.
//
// The scan is command-scoped, not manifest-scoped: a run that plans no task of
// an extension pays no probe for its toolchains.
func resolveRuntimeToolchainsForCommands(
	workspaceRoot string,
	exts []*model.ExtensionDescription,
	activeCommands map[string]bool,
	base []string,
) error {
	return resolveRuntimeToolchainsForCommandsContext(context.Background(), workspaceRoot, exts, activeCommands, base)
}

func resolveRuntimeToolchainsForCommandsContext(ctx context.Context, workspaceRoot string, exts []*model.ExtensionDescription, activeCommands map[string]bool, base []string) error {
	return resolveRuntimeToolchainsForCommandsMode(ctx, workspaceRoot, exts, activeCommands, base, resolveStrict)
}

// resolveRuntimeToolchainsForCommandsMode resolves the aliases only
// toolchainProvisioningCommand reaches as if optional, and every other one
// with mode.
func resolveRuntimeToolchainsForCommandsMode(ctx context.Context, workspaceRoot string, exts []*model.ExtensionDescription, activeCommands map[string]bool, base []string, mode toolchainResolutionMode) error {
	for _, ext := range exts {
		required, provisioning := commandRuntimeToolchainRefSets(ext, activeCommands)
		if err := resolveRuntimeToolchainRefs(ctx, workspaceRoot, ext, required, base, mode); err != nil {
			return err
		}
		if err := resolveRuntimeToolchainRefs(ctx, workspaceRoot, ext, provisioning, base, resolveProvisioning); err != nil {
			return err
		}
	}
	return nil
}

// commandRuntimeToolchainRefs returns the sorted toolchain aliases the active
// commands of one extension can require, however each one resolves.
func commandRuntimeToolchainRefs(ext *model.ExtensionDescription, activeCommands map[string]bool) []string {
	required, provisioning := commandRuntimeToolchainRefSets(ext, activeCommands)
	refs := map[string]bool{}
	for _, ref := range append(required, provisioning...) {
		refs[ref] = true
	}
	return sortedRuntimeToolchainRefs(refs)
}

// commandRuntimeToolchainRefSets returns the sorted toolchain aliases the
// active commands of one extension can require, split by how they resolve.
// provisioning holds the aliases only toolchainProvisioningCommand reaches;
// required holds every other one, including an alias the provisioning command
// shares with another active command. The two sets never overlap.
//
// A command's own resolved definition carries its aliases only when its
// pipeline has a single step, so the pipeline steps are read as well and every
// reachable task contributes.
func commandRuntimeToolchainRefSets(ext *model.ExtensionDescription, activeCommands map[string]bool) (required, provisioning []string) {
	if ext == nil || ext.Runtime == nil || len(ext.Runtime.Toolchains) == 0 {
		return nil, nil
	}
	strict := map[string]bool{}
	lenient := map[string]bool{}
	for commandName, def := range ext.Jobs {
		if def == nil || !activeCommands[commandName] {
			continue
		}
		refs := strict
		if isToolchainProvisioningCommand(commandName) {
			refs = lenient
		}
		for _, ref := range def.Toolchains {
			refs[ref] = true
		}
		for _, step := range def.PipelineSteps {
			task, declared := ext.Tasks[step.Task]
			if !declared {
				continue
			}
			for _, ref := range task.Toolchains {
				refs[ref] = true
			}
			for _, ref := range ext.Runtime.RunToolchains {
				refs[ref] = true
			}
		}
	}
	for ref := range strict {
		delete(lenient, ref)
	}
	return sortedRuntimeToolchainRefs(strict), sortedRuntimeToolchainRefs(lenient)
}

// resolveRuntimeToolchainsForPreparation resolves the prepare toolchains of
// every local-source extension, strictly unless provisioning (see
// provisioningOnlyRun).
func resolveRuntimeToolchainsForPreparation(ctx context.Context, workspaceRoot string, exts []*model.ExtensionDescription, base []string, provisioning bool) error {
	for _, ext := range exts {
		if ext == nil || ext.Runtime == nil || ext.Runtime.Prepare == nil || !ext.LocalSource {
			continue
		}
		if err := resolveRuntimeToolchainRefs(ctx, workspaceRoot, ext, ext.Runtime.Prepare.Toolchains, base, provisioningMode(provisioning)); err != nil {
			return err
		}
	}
	return nil
}

// ensureJobRuntimeToolchains resolves what a job requires and the run has not
// resolved yet. RequirePlannedRuntimeToolchains calls it for each planned job
// before any key exists, which resolves the toolchains synchronization left
// unresolved (resolveDeferred). At execution it is the backstop for a job
// neither stage covered — an internal job, or a plan a test builds by hand. Such
// a job has already been keyed without its toolchain identity, so the backstop
// keeps it correct; it does not replace resolveRuntimeToolchainsForCommands.
//
// A job of toolchainProvisioningCommand resolves what is missing as if
// optional. A job of any other command resolves it strictly, and it also
// refuses a required toolchain the run already resolved unavailable once a lock
// exists: only a provisioning resolution produces that state, and it must not
// reach a command the missing pin is meant to stop.
func ensureJobRuntimeToolchains(ws *workspace.Workspace, job *ScheduledJob) error {
	if job == nil || job.Extension == nil || job.JobDef == nil || len(job.JobDef.Toolchains) == 0 {
		return nil
	}
	provisioning := isToolchainProvisioningCommand(job.CommandName())
	runtimeToolchainMu.Lock()
	missing := make([]string, 0, len(job.JobDef.Toolchains))
	var unavailable []string
	for _, ref := range job.JobDef.Toolchains {
		resolution, ok := job.Extension.RuntimeToolchains[ref]
		switch {
		case !ok:
			missing = append(missing, ref)
		case !provisioning && !resolution.Available && requiredRuntimeToolchain(job.Extension, ref):
			unavailable = append(unavailable, ref)
		}
	}
	runtimeToolchainMu.Unlock()
	if len(missing) == 0 && len(unavailable) == 0 {
		return nil
	}
	if ws == nil {
		return errors.New("resolve job runtime toolchains: workspace is required")
	}
	if len(unavailable) > 0 {
		if err := refuseProvisioningRuntimeToolchains(ws.Root, job.Extension, unavailable); err != nil {
			return err
		}
	}
	return resolveRuntimeToolchainRefs(context.Background(), ws.Root, job.Extension, missing, os.Environ(), provisioningMode(provisioning))
}

// RequirePlannedRuntimeToolchains resolves, before any planned job runs or is
// served from the cache, each toolchain a planned job requires that runtime
// synchronization left unresolved (resolveDeferred). A job of another command
// is checked before a job of toolchainProvisioningCommand, so an alias both
// need resolves strictly. The error is the one strict resolution returns.
func RequirePlannedRuntimeToolchains(ws *workspace.Workspace, planned []*ScheduledJob) error {
	for _, provisioning := range []bool{false, true} {
		for _, job := range planned {
			if job == nil || isToolchainProvisioningCommand(job.CommandName()) != provisioning {
				continue
			}
			if err := ensureJobRuntimeToolchains(ws, job); err != nil {
				return runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
					"resolve extension runtime toolchains: %v", err)
			}
		}
	}
	return nil
}

// requiredRuntimeToolchain reports whether ext declares ref as a required
// toolchain. The caller holds runtimeToolchainMu.
func requiredRuntimeToolchain(ext *model.ExtensionDescription, ref string) bool {
	if ext.Runtime == nil {
		return false
	}
	requirement, declared := ext.Runtime.Toolchains[ref]
	return declared && !requirement.Optional
}

// refuseProvisioningRuntimeToolchains fails when a workspace lock exists: the
// required refs then resolved unavailable only because the provisioning command
// tolerated a missing or unusable pin. Without a lock, the unavailable identity
// is the bootstrap floor every command shares, and the job may run.
func refuseProvisioningRuntimeToolchains(workspaceRoot string, ext *model.ExtensionDescription, refs []string) error {
	locked, err := lockfile.ReadLockFile(workspaceRoot)
	if err != nil {
		return fmt.Errorf("read workspace lock for runtime toolchains: %w", err)
	}
	if locked == nil {
		return nil
	}
	ref := slices.Min(refs)
	return fmt.Errorf("resolve extension %q runtime toolchain %q: workspace lock has no usable %q pin; only %s and %s run without one",
		ext.Name, ref, ext.Runtime.Toolchains[ref].Lock, extensionproto.WorkspaceFetchCommand, toolchainProvisioningCommand)
}

func resolveRuntimeToolchains(workspaceRoot string, ext *model.ExtensionDescription, refs []string, base []string) error {
	return resolveRuntimeToolchainsContext(context.Background(), workspaceRoot, ext, refs, base)
}

func resolveRuntimeToolchainsContext(ctx context.Context, workspaceRoot string, ext *model.ExtensionDescription, refs []string, base []string) error {
	return resolveRuntimeToolchainRefs(ctx, workspaceRoot, ext, refs, base, resolveStrict)
}

// toolchainResolutionMode is what a required toolchain the workspace lock does
// not satisfy becomes: no pin, no host integrity, or no matching candidate.
type toolchainResolutionMode int

const (
	// resolveStrict fails the resolution.
	resolveStrict toolchainResolutionMode = iota
	// resolveProvisioning records the unavailable identity, as for an optional
	// toolchain (see toolchainProvisioningCommand).
	resolveProvisioning
	// resolveDeferred records nothing. A command's toolchains resolve before
	// planning, so a run may reach an alias that no job it plans requires: an
	// extension with no project of its language plans no job.
	// RequirePlannedRuntimeToolchains fails the run only when a planned job
	// requires the alias.
	resolveDeferred
)

// provisioningMode is resolveProvisioning when provisioning holds, and
// resolveStrict otherwise.
func provisioningMode(provisioning bool) toolchainResolutionMode {
	if provisioning {
		return resolveProvisioning
	}
	return resolveStrict
}

// markRuntimeToolchainProbes records that the run starts repository code
// before it resolves a runtime toolchain of ext, unless ext is installed from
// the artifact store: a toolchain probe runs the candidate and arguments that
// the manifest of ext chooses, and the manifest of any other extension is
// repository content.
func markRuntimeToolchainProbes(workspaceRoot string, ext *model.ExtensionDescription) {
	if !extension.InStoreRoot(store.ResolveArtifactStoreRoot(workspaceRoot), ext) {
		runcredential.MarkRepositoryCodeStarted("runtime toolchain probe of " + ext.Name)
	}
}

// resolveRuntimeToolchainRefs resolves refs against the workspace lock and
// records each resolution on ext. mode decides what a required ref the lock
// does not satisfy becomes.
func resolveRuntimeToolchainRefs(ctx context.Context, workspaceRoot string, ext *model.ExtensionDescription, refs []string, base []string, mode toolchainResolutionMode) error {
	if len(refs) == 0 {
		return nil
	}
	if ext == nil || ext.Runtime == nil {
		return errors.New("resolve runtime toolchains: extension runtime is not declared")
	}
	markRuntimeToolchainProbes(workspaceRoot, ext)
	if err := lockRuntimeToolchains(ctx); err != nil {
		return err
	}
	defer runtimeToolchainMu.Unlock()
	locked, err := lockfile.ReadLockFile(workspaceRoot)
	if err != nil {
		return fmt.Errorf("read workspace lock for runtime toolchains: %w", err)
	}
	if ext.RuntimeToolchains == nil {
		ext.RuntimeToolchains = map[string]model.RuntimeToolchainResolution{}
	}
	unique := map[string]bool{}
	for _, ref := range refs {
		unique[ref] = true
	}
	if locked == nil {
		// A workspace with NO lock document has not been installed yet, and
		// `putnami install` / `deps install` are the commands that write the very
		// file a pin would come from. Refusing here would make the workspace
		// unbootstrappable. There is also no pin to violate: every reference
		// resolves to the unavailable identity, so the run is keyed — and its
		// prepared runtime digested — apart from any later pinned run, and the
		// bootstrap artifact can never be served to one.
		//
		// This is not the missing-pin case. A lock that EXISTS and does not pin a
		// required alias stays a hard failure below.
		for _, ref := range sortedRuntimeToolchainRefs(unique) {
			if _, exists := ext.RuntimeToolchains[ref]; exists {
				continue
			}
			requirement, declared := ext.Runtime.Toolchains[ref]
			if !declared {
				return fmt.Errorf("resolve extension %q runtime toolchain %q: declaration is missing", ext.Name, ref)
			}
			ext.RuntimeToolchains[ref] = unavailableRuntimeToolchainResolution(requirement, lockfile.LockEntry{})
		}
		return nil
	}
	for _, ref := range sortedRuntimeToolchainRefs(unique) {
		if _, exists := ext.RuntimeToolchains[ref]; exists {
			continue
		}
		requirement, declared := ext.Runtime.Toolchains[ref]
		if !declared {
			return fmt.Errorf("resolve extension %q runtime toolchain %q: declaration is missing", ext.Name, ref)
		}
		optional := requirement.Optional || mode == resolveProvisioning
		deferred := !optional && mode == resolveDeferred
		entry, pinned := locked.GetToolchain(requirement.Lock)
		if !pinned || strings.TrimSpace(entry.Version) == "" {
			if optional {
				ext.RuntimeToolchains[ref] = unavailableRuntimeToolchainResolution(requirement, entry)
				continue
			}
			if deferred {
				continue
			}
			return fmt.Errorf("resolve extension %q runtime toolchain %q: workspace lock has no exact %q pin: "+
				"add a project that declares it, or run `putnami install` to record the declared one", ext.Name, ref, requirement.Lock)
		}
		integrity := entry.IntegrityFor(runtime.GOOS, runtime.GOARCH)
		if strings.TrimSpace(integrity) == "" {
			if optional {
				ext.RuntimeToolchains[ref] = unavailableRuntimeToolchainResolution(requirement, entry)
				continue
			}
			if deferred {
				continue
			}
			return fmt.Errorf("resolve extension %q runtime toolchain %q: workspace lock has no integrity for %s", ext.Name, ref, HostPlatform())
		}
		resolution, rejections, found := resolveRuntimeToolchain(ctx, workspaceRoot, requirement, entry.Version, integrity, base)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !found {
			detail := runtimeToolchainRejectionText(rejections)
			if optional {
				// An unavailable optional toolchain does not fail the run, so the
				// rejection reasons reach the user only through this warning. They
				// never enter the resolution or its identity.
				if requirement.Optional {
					//nolint:gosec // G706 false positive: dynamic values are structured slog attributes, not a format string.
					slog.Warn("runtime toolchain: optional toolchain is unavailable; no candidate matched the locked version",
						"extension", ext.Name, "toolchain", ref, "version", entry.Version, "rejections", detail)
				} else {
					// Installing the pinned toolchain can be what the provisioning
					// run is about to do, so nothing is wrong yet. Once the run
					// ended, ReportProvisionedRuntimeToolchains probes again and
					// warns only about a toolchain that is still missing.
					//nolint:gosec // G706 false positive: dynamic values are structured slog attributes, not a format string.
					slog.Debug("runtime toolchain: "+toolchainProvisioningCommand+" starts before the pinned toolchain is installed; no candidate matched the locked version",
						"extension", ext.Name, "toolchain", ref, "version", entry.Version, "rejections", detail)
				}
				ext.RuntimeToolchains[ref] = unavailableRuntimeToolchainResolution(requirement, entry)
				continue
			}
			if deferred {
				continue
			}
			if detail != "" {
				detail = ": " + detail
			}
			return fmt.Errorf("resolve extension %q runtime toolchain %q: no candidate matched locked version %q%s",
				ext.Name, ref, entry.Version, detail)
		}
		ext.RuntimeToolchains[ref] = resolution
	}
	return validateResolvedRuntimeToolchainEnvironment(ext, refs)
}

// ReportProvisionedRuntimeToolchains reports, once a run of
// toolchainProvisioningCommand ended, on each required toolchain the run
// resolved unavailable while the lock pins it for this host: no candidate
// matched the pin when the run started, and installing it can be what the run
// did. Each is probed again. One that now resolves is reported at info level;
// one that still does not keeps the warning, with the rejected candidates. A
// run whose command dependency closure holds no provisioning command reports
// nothing: only provisioning resolves a pinned required toolchain unavailable.
// The resolutions the run used are not changed.
//
// The extensions are exts and those of the planned jobs, each once: a run that
// plans a copy of an extension resolves the copy its jobs carry.
func ReportProvisionedRuntimeToolchains(ctx context.Context, workspaceRoot string, exts []*model.ExtensionDescription, planned []*ScheduledJob, commands []string) {
	exts = slices.Clone(exts)
	for _, job := range planned {
		if job != nil && job.Extension != nil && !slices.Contains(exts, job.Extension) {
			exts = append(exts, job.Extension)
		}
	}
	if ctx.Err() != nil || !provisioningCommandActive(commandDependencyClosure(exts, commands)) {
		return
	}
	locked, err := lockfile.ReadLockFile(workspaceRoot)
	if err != nil || locked == nil {
		return
	}
	type pending struct {
		extension, ref string
		requirement    extensionproto.RuntimeToolchain
		entry          lockfile.LockEntry
		integrity      string
	}
	var probes []pending
	if err := lockRuntimeToolchains(ctx); err != nil {
		return
	}
	for _, ext := range exts {
		if ext == nil || ext.Runtime == nil {
			continue
		}
		for _, ref := range sortedRuntimeToolchainRefs(runtimeToolchainRefSet(ext.RuntimeToolchains)) {
			requirement, declared := ext.Runtime.Toolchains[ref]
			if !declared || requirement.Optional || ext.RuntimeToolchains[ref].Available {
				continue
			}
			entry, pinned := locked.GetToolchain(requirement.Lock)
			integrity := entry.IntegrityFor(runtime.GOOS, runtime.GOARCH)
			if !pinned || strings.TrimSpace(entry.Version) == "" || strings.TrimSpace(integrity) == "" {
				continue
			}
			markRuntimeToolchainProbes(workspaceRoot, ext)
			probes = append(probes, pending{extension: ext.Name, ref: ref, requirement: requirement, entry: entry, integrity: integrity})
		}
	}
	runtimeToolchainMu.Unlock()

	base := os.Environ()
	for _, probe := range probes {
		_, rejections, found := resolveRuntimeToolchain(ctx, workspaceRoot, probe.requirement, probe.entry.Version, probe.integrity, base)
		if ctx.Err() != nil {
			return
		}
		if found {
			slog.Info("runtime toolchain: "+toolchainProvisioningCommand+" installed the pinned toolchain",
				"extension", probe.extension, "toolchain", probe.ref, "version", probe.entry.Version)
			continue
		}
		//nolint:gosec // G706 false positive: dynamic values are structured slog attributes, not a format string.
		slog.Warn("runtime toolchain: the pinned toolchain is still unavailable after "+toolchainProvisioningCommand+"; no candidate matched the locked version",
			"extension", probe.extension, "toolchain", probe.ref, "version", probe.entry.Version,
			"rejections", runtimeToolchainRejectionText(rejections))
	}
}

// provisioningCommandActive reports whether toolchainProvisioningCommand is
// among the active commands. A fetch alone installs no toolchain: the
// installers that run after it do, and their run reports.
func provisioningCommandActive(activeCommands map[string]bool) bool {
	for command, active := range activeCommands {
		if active && extensionproto.BaseCommandName(command) == toolchainProvisioningCommand {
			return true
		}
	}
	return false
}

// runtimeToolchainRefSet returns the aliases resolutions records.
func runtimeToolchainRefSet(resolutions map[string]model.RuntimeToolchainResolution) map[string]bool {
	refs := make(map[string]bool, len(resolutions))
	for ref := range resolutions {
		refs[ref] = true
	}
	return refs
}

// runtimeToolchainRejection names one candidate the resolver considered and the
// reason it did not qualify.
type runtimeToolchainRejection struct {
	candidate string
	reason    string
}

// resolveRuntimeToolchain returns the first candidate that qualifies. When none
// does, it returns one rejection per considered candidate, in declaration order.
func resolveRuntimeToolchain(ctx context.Context, workspaceRoot string, requirement extensionproto.RuntimeToolchain, version, integrity string, base []string) (model.RuntimeToolchainResolution, []runtimeToolchainRejection, bool) {
	rejections := make([]runtimeToolchainRejection, 0, len(requirement.Candidates))
	candidateEnv := runtimeToolchainCandidateEnv(workspaceRoot, base)
	insideWorkspace := hostedWorkspaceProgram(workspaceRoot)
	for index, candidate := range requirement.Candidates {
		if ctx.Err() != nil {
			return model.RuntimeToolchainResolution{}, rejections, false
		}
		executable := runtimeToolchainCandidatePath(workspaceRoot, candidate, version, candidateEnv)
		label := describeRuntimeToolchainCandidate(index, candidate, executable)
		reject := func(reason string) {
			rejections = append(rejections, runtimeToolchainRejection{candidate: label, reason: reason})
		}
		if executable == "" {
			reject(unexpandedCandidateReason(candidate))
			continue
		}
		realExecutable, err := filepath.EvalSymlinks(executable)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				reject("path missing")
			} else {
				reject("path unresolvable: " + err.Error())
			}
			continue
		}
		if insideWorkspace(realExecutable) {
			reject("inside the workspace; a hosted run starts no program the repository controls")
			continue
		}
		if !runtimeExecutable(realExecutable) {
			reject("not executable")
			continue
		}
		probeEnv := append([]string(nil), base...)
		for _, name := range requirement.Probe.Unset {
			probeEnv = removeEnvKey(probeEnv, name)
		}
		for name, value := range requirement.Probe.Environment {
			probeEnv = setEnv(probeEnv, name, strings.ReplaceAll(value, extensionproto.RuntimeToolchainVersionToken, version))
		}
		probeEnv = prependEnvPath(probeEnv, filepath.Dir(realExecutable))
		got, err := probeRuntimeToolchain(ctx, realExecutable, requirement.Probe.Args, probeEnv, toolVersionProbeTimeout)
		if err != nil {
			reject(err.Error())
			continue
		}
		want := strings.ReplaceAll(requirement.Probe.Expect, extensionproto.RuntimeToolchainVersionToken, version)
		if got != want {
			reject(fmt.Sprintf("version %q does not match locked %q", got, want))
			continue
		}
		environment, ok := deriveRuntimeToolchainEnvironment(realExecutable, version, requirement.Environment)
		if !ok {
			reject("environment bindings cannot be derived from the executable")
			continue
		}
		return model.RuntimeToolchainResolution{
			Available:   true,
			Executable:  realExecutable,
			Environment: environment,
			Identity:    runtimeToolchainIdentity(requirement, version, integrity, true),
		}, nil, true
	}
	return model.RuntimeToolchainResolution{}, rejections, false
}

// describeRuntimeToolchainCandidate names a candidate by its 1-based position,
// its declared source and path, and the host path it expanded to, if any.
func describeRuntimeToolchainCandidate(index int, candidate extensionproto.RuntimeToolchainCandidate, executable string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "candidate %d (%s", index+1, candidate.From)
	if candidate.Environment != "" {
		b.WriteString(" " + candidate.Environment)
	}
	fmt.Fprintf(&b, " %q)", candidate.Path)
	if executable != "" {
		b.WriteString(" at " + executable)
	}
	return b.String()
}

// runtimeToolchainRejectionText renders one "<candidate>: <reason>" entry per
// rejection, separated by "; ", in the order given. It is empty when there are
// no rejections.
func runtimeToolchainRejectionText(rejections []runtimeToolchainRejection) string {
	parts := make([]string, 0, len(rejections))
	for _, rejection := range rejections {
		parts = append(parts, rejection.candidate+": "+rejection.reason)
	}
	return strings.Join(parts, "; ")
}

// runtimeToolchainWorkspaceRootEnv names the workspace root in the environment
// every job receives (BuildEnvVars).
const runtimeToolchainWorkspaceRootEnv = "PUTNAMI_WORKSPACE_ROOT"

// hostedWorkspaceProgram returns the test a hosted run applies to a resolved
// candidate before it probes it: true when the program lies inside the
// workspace, whose files the repository controls. The probe runs before the
// fetch jobs and the resolution serves every later job, so a hosted run takes
// its toolchains from outside the workspace only. A relative path resolves
// against the engine's working directory, as the probe starts it, and a path
// the test cannot place counts as inside. Without the run credential, the test
// accepts every candidate.
func hostedWorkspaceProgram(workspaceRoot string) func(string) bool {
	if !runcredential.Hosted() || workspaceRoot == "" {
		return func(string) bool { return false }
	}
	root, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		root = filepath.Clean(workspaceRoot)
	}
	return func(path string) bool {
		abs, err := filepath.Abs(path)
		if err != nil {
			return true
		}
		rel, err := filepath.Rel(root, abs)
		return err != nil || rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
	}
}

// runtimeToolchainCandidateEnv is the environment candidates expand against:
// base with runtimeToolchainWorkspaceRootEnv set to this workspace's root, as
// its jobs receive it, or unset without one. An `environment` candidate below
// that variable finds a toolchain an extension installed inside the workspace.
// A value inherited from a parent job never applies.
func runtimeToolchainCandidateEnv(workspaceRoot string, base []string) []string {
	env := append([]string(nil), base...)
	if workspaceRoot == "" {
		return removeEnvKey(env, runtimeToolchainWorkspaceRootEnv)
	}
	return setEnv(env, runtimeToolchainWorkspaceRootEnv, workspaceRoot)
}

func runtimeToolchainCandidatePath(workspaceRoot string, candidate extensionproto.RuntimeToolchainCandidate, version string, base []string) string {
	rel := filepath.FromSlash(strings.ReplaceAll(candidate.Path, extensionproto.RuntimeToolchainVersionToken, version))
	var path string
	switch candidate.From {
	case extensionproto.RuntimeToolchainCandidatePath:
		path = lookupRuntimePath(base, rel)
	case extensionproto.RuntimeToolchainCandidateEnvironment:
		if root := strings.TrimSpace(envLastValue(base, candidate.Environment)); root != "" {
			path = filepath.Join(root, rel)
		}
	case extensionproto.RuntimeToolchainCandidateHome:
		if root := runtimeHomeDir(base); root != "" {
			path = filepath.Join(root, rel)
		}
	case extensionproto.RuntimeToolchainCandidatePutnamiHome:
		path = filepath.Join(PutnamiHome(workspaceRoot, base), rel)
	}
	return withExecutableSuffix(runtime.GOOS, path)
}

// withExecutableSuffix adds ".exe" to a Windows path that has no extension. An
// empty path stays empty: the candidate expanded to nothing, and
// unexpandedCandidateReason says why.
func withExecutableSuffix(goos, path string) string {
	if goos == "windows" && path != "" && filepath.Ext(path) == "" {
		return path + ".exe"
	}
	return path
}

// unexpandedCandidateReason says why candidate expanded to no path on this
// host: the variable it is relative to has no value, PATH holds no such
// program, or no home directory resolves.
func unexpandedCandidateReason(candidate extensionproto.RuntimeToolchainCandidate) string {
	switch candidate.From {
	case extensionproto.RuntimeToolchainCandidateEnvironment:
		return "environment variable " + candidate.Environment + " is not set"
	case extensionproto.RuntimeToolchainCandidatePath:
		return "no executable on PATH"
	case extensionproto.RuntimeToolchainCandidateHome:
		return "no home directory is set"
	}
	return "path missing"
}

// PutnamiHome returns the Putnami home of a command that runs in workspaceRoot
// with env: PUTNAMI_HOME, else .putnami under the user's home directory, else
// .putnami under workspaceRoot. It is the directory a putnami-home candidate
// of a runtime toolchain is relative to, so everything the CLI reads or writes
// beside a toolchain resolves the home through it.
func PutnamiHome(workspaceRoot string, env []string) string {
	if root := strings.TrimSpace(envLastValue(env, "PUTNAMI_HOME")); root != "" {
		return filepath.Clean(root)
	}
	if home := runtimeHomeDir(env); home != "" {
		return filepath.Join(home, ".putnami")
	}
	return filepath.Join(workspaceRoot, ".putnami")
}

// runtimeHomeDir resolves the home directory a `home` candidate is relative to.
// The child environment wins when it names one, so a curated environment stays
// authoritative and a test can pin the answer. os.UserHomeDir is the fallback:
// $HOME alone is empty on Windows, where the value lives in %USERPROFILE%, and
// it is also empty in a stripped environment, which would silently skip the
// candidate instead of finding the tool.
func runtimeHomeDir(env []string) string {
	if home := strings.TrimSpace(envLastValue(env, runtimeHomeEnvName())); home != "" {
		return filepath.Clean(home)
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Clean(home)
}

// runtimeHomeEnvName is the variable os.UserHomeDir reads on this platform, so
// an environment-supplied home and the fallback never disagree about which
// variable carries it.
func runtimeHomeEnvName() string {
	if runtime.GOOS == "windows" {
		return "USERPROFILE"
	}
	return "HOME"
}

func lookupRuntimePath(env []string, name string) string {
	for _, dir := range filepath.SplitList(envLastValue(env, "PATH")) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if runtime.GOOS == "windows" && filepath.Ext(candidate) == "" {
			candidate += ".exe"
		}
		if runtimeExecutable(candidate) {
			return candidate
		}
	}
	return ""
}

func runtimeExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0)
}

// probeRuntimeToolchain runs the declared probe under timeout and returns its
// trimmed one-line output. The error states why the output cannot identify a
// version: a timeout, a failed start, a failed exit, output that stays open
// after the probe exits, or output that is empty, too long, or spans several
// lines.
func probeRuntimeToolchain(ctx context.Context, path string, args, env []string, timeout time.Duration) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &boundedBuffer{limit: toolVersionProbeOutputLimit}
	//nolint:gosec // G702: path is an executable candidate from the validated extension manifest; probing it is the resolver's purpose.
	cmd := exec.CommandContext(probeCtx, path, args...)
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = toolVersionProbeWaitDelay
	if err := cmd.Run(); err != nil {
		if ctx.Err() == nil && errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("probe timed out after %s", timeout)
		}
		if errors.Is(err, exec.ErrWaitDelay) {
			return "", fmt.Errorf("probe output stayed open after exit (wait delay %s)", toolVersionProbeWaitDelay)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("probe failed: %w", exitErr)
		}
		if cmd.Process == nil {
			return "", fmt.Errorf("probe failed to start: %w", err)
		}
		return "", fmt.Errorf("probe failed: %w", err)
	}
	if out.truncated {
		return "", fmt.Errorf("probe output too long (over %d bytes)", toolVersionProbeOutputLimit)
	}
	value := strings.TrimSpace(string(out.buf))
	switch {
	case value == "":
		return "", errors.New("probe output empty")
	case len(value) > toolVersionMaxLen:
		return "", fmt.Errorf("probe output too long (%d bytes, limit %d)", len(value), toolVersionMaxLen)
	case strings.ContainsAny(value, "\r\n"):
		return "", errors.New("probe output malformed (more than one line)")
	}
	return value, nil
}

func deriveRuntimeToolchainEnvironment(executable, version string, bindings map[string]extensionproto.RuntimeToolchainEnvironment) (map[string]string, bool) {
	environment := make(map[string]string, len(bindings))
	for name, binding := range bindings {
		var value string
		switch binding.From {
		case extensionproto.RuntimeToolchainEnvironmentExecutable:
			value = executable
		case extensionproto.RuntimeToolchainEnvironmentAncestor:
			value = executable
			for range binding.Levels {
				parent := filepath.Dir(value)
				if parent == value {
					return nil, false
				}
				value = parent
			}
		case extensionproto.RuntimeToolchainEnvironmentLiteral:
			value = strings.ReplaceAll(binding.Value, extensionproto.RuntimeToolchainVersionToken, version)
		default:
			return nil, false
		}
		environment[name] = value
	}
	return environment, true
}

func validateResolvedRuntimeToolchainEnvironment(ext *model.ExtensionDescription, refs []string) error {
	values := map[string]string{}
	for _, ref := range refs {
		resolution, ok := ext.RuntimeToolchains[ref]
		if !ok || !resolution.Available {
			continue
		}
		for name, value := range resolution.Environment {
			if previous, exists := values[name]; exists && previous != value {
				return fmt.Errorf("resolve extension %q runtime toolchains: requirements assign conflicting values to environment %q", ext.Name, name)
			}
			values[name] = value
		}
	}
	return nil
}

// applyRuntimeToolchains builds the child environment of a subprocess bound to
// the given resolved toolchains.
//
// The child's PATH is minimal in the only sense PATH can express safely: the
// resolved directories come first, in declaration order, and each inherited
// directory appears once after them. The bare name of a pinned tool therefore
// always resolves to the executable the lock pinned, and the declaration's own
// environment bindings (a compiler root, a toolchain mode, an install root) pin
// it a second time for the tools that read those instead of PATH.
//
// Removing the inherited directories that still offer that name was measured
// and rejected: a shared `bin` directory carries hundreds of unrelated tools
// beside the pinned one — on this workspace's own gate, dropping the directory
// holding `go` also dropped `gpg`, and every signed `git commit` in a test
// fixture failed. PATH cannot hide one file from a directory, so the choice is
// between the pinned tool winning and every co-located tool staying reachable;
// this takes both by ordering, and pays for it by not defending against a child
// that rebuilds PATH from scratch — which no longer inherits ours anyway.
func applyRuntimeToolchains(base []string, ext *model.ExtensionDescription, refs []string) []string {
	env, _ := applyRuntimeToolchainsContext(context.Background(), base, ext, refs)
	return env
}

func applyRuntimeToolchainsContext(ctx context.Context, base []string, ext *model.ExtensionDescription, refs []string) ([]string, error) {
	if ext == nil || ext.Runtime == nil || len(refs) == 0 {
		return append([]string(nil), base...), nil
	}
	env := append([]string(nil), base...)
	ordered := append([]string(nil), refs...)
	sort.Strings(ordered)
	var resolvedDirs []string
	if err := lockRuntimeToolchains(ctx); err != nil {
		return nil, err
	}
	for _, ref := range ordered {
		resolution, ok := ext.RuntimeToolchains[ref]
		if !ok {
			continue
		}
		declaration := ext.Runtime.Toolchains[ref]
		if !resolution.Available {
			// An optional toolchain that no candidate satisfied publishes its
			// executable bindings EMPTY rather than leaving the ambient value in
			// place. The task then chooses another operation or fails naming the
			// tool; it never runs against a binary the lock did not pin.
			for name, binding := range declaration.Environment {
				if binding.From == extensionproto.RuntimeToolchainEnvironmentExecutable {
					env = setEnv(env, name, "")
				}
			}
			continue
		}
		if declaration.PrependPath {
			resolvedDirs = append(resolvedDirs, filepath.Dir(resolution.Executable))
		}
		for name, value := range resolution.Environment {
			env = setEnv(env, name, value)
		}
	}
	runtimeToolchainMu.Unlock()
	if len(resolvedDirs) == 0 {
		return env, nil
	}
	return setEnv(env, "PATH", minimalRuntimePath(envLastValue(env, "PATH"), resolvedDirs)), nil
}

// ProviderRuntimeEnvironment applies the same already-resolved toolchain
// environment as ordinary tasks to an engine-owned provider subprocess.
// Call SynchronizeExtensionRuntimesForCommands before constructing the launch.
func ProviderRuntimeEnvironment(ctx context.Context, base []string, ext *model.ExtensionDescription, refs []string) ([]string, error) {
	return applyRuntimeToolchainsContext(ctx, base, ext, refs)
}

// RunToolchainEnvironment resolves the toolchains every task of ext runs with
// (runtime.runToolchains) and returns base with their environment applied, for
// a command that starts one of those tools outside the task graph. Resolution
// is the one a job of an ordinary command gets: strict against the workspace
// lock, and the unavailable bootstrap identity when no lock exists yet, which
// leaves the ambient PATH in charge. Resolution records on ext and reuses an
// earlier record, so ext must be a description no run shares, and a caller that
// wants a fresh answer discovers ext again.
func RunToolchainEnvironment(ctx context.Context, workspaceRoot string, ext *model.ExtensionDescription, base []string) ([]string, error) {
	if ext == nil || ext.Runtime == nil || len(ext.Runtime.RunToolchains) == 0 {
		return append([]string(nil), base...), nil
	}
	refs := append([]string(nil), ext.Runtime.RunToolchains...)
	if err := resolveRuntimeToolchainsContext(ctx, workspaceRoot, ext, refs, base); err != nil {
		return nil, err
	}
	return applyRuntimeToolchainsContext(ctx, base, ext, refs)
}

func lockRuntimeToolchains(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if runtimeToolchainMu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// ProviderExecutable resolves a bare command against its prepared child PATH.
// os/exec otherwise resolves it against the parent's PATH before assigning Env.
//
// On Unix the lookup is the native runtime toolchain resolver's. On Windows it
// is the rule os/exec applies there, run against the child's PATH and PATHEXT:
// a bare provider name such as npx finds npx.cmd, as it did when os/exec
// resolved it.
func ProviderExecutable(command string, env []string) (string, error) {
	return providerExecutable(command, env, runtime.GOOS)
}

func providerExecutable(command string, env []string, goos string) (string, error) {
	if strings.ContainsAny(command, "/\\") {
		return command, nil
	}
	path := ""
	if goos == "windows" {
		path = lookupWindowsProviderPath(env, command)
	} else {
		path = lookupRuntimePath(env, command)
	}
	if path == "" {
		return "", fmt.Errorf("provider executable %q is unavailable in its prepared PATH", command)
	}
	return path, nil
}

// defaultWindowsPathExt is the extension list os/exec uses on Windows when
// PATHEXT is unset or empty.
const defaultWindowsPathExt = ".com;.exe;.bat;.cmd"

// lookupWindowsProviderPath is exec.LookPath's Windows rule applied to env: each
// PATH directory in order, and in each one the name as written when it already
// has an extension, then the name with each PATHEXT extension in order.
// Variable names compare without regard to case, as Windows compares them.
func lookupWindowsProviderPath(env []string, name string) string {
	keys := envkeys.Keys{Fold: true}
	exts := windowsPathExts(keys.Last(env, "PATHEXT"))
	for _, dir := range filepath.SplitList(keys.Last(env, "PATH")) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if filepath.Ext(name) != "" && windowsExecutableFile(candidate) {
			return candidate
		}
		for _, ext := range exts {
			if windowsExecutableFile(candidate + ext) {
				return candidate + ext
			}
		}
	}
	return ""
}

// windowsPathExts parses PATHEXT the way os/exec does on Windows: lower case,
// empty entries skipped, and a missing leading dot added.
func windowsPathExts(value string) []string {
	if value == "" {
		value = defaultWindowsPathExt
	}
	var exts []string
	for _, ext := range strings.Split(strings.ToLower(value), ";") {
		if ext == "" {
			continue
		}
		if ext[0] != '.' {
			ext = "." + ext
		}
		exts = append(exts, ext)
	}
	return exts
}

// windowsExecutableFile is the Windows executable test: a file, not a
// directory. Windows has no executable bit; PATHEXT decides what runs.
func windowsExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// minimalRuntimePath returns path with the resolved directories first and each
// remaining directory kept once, in its inherited order. A later duplicate can
// never be reached, so carrying it only makes the child's environment longer
// and its PATH harder to read in a failure report.
func minimalRuntimePath(path string, resolvedDirs []string) string {
	kept := make([]string, 0, len(resolvedDirs)+8)
	seen := make(map[string]bool, len(resolvedDirs)+8)
	for _, dir := range append(append([]string(nil), resolvedDirs...), filepath.SplitList(path)...) {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		kept = append(kept, dir)
	}
	return strings.Join(kept, string(filepath.ListSeparator))
}

func runtimeToolchainIdentityForRefs(ext *model.ExtensionDescription, refs []string) string {
	if ext == nil || len(refs) == 0 {
		return ""
	}
	ordered := append([]string(nil), refs...)
	sort.Strings(ordered)
	parts := make([]string, 0, len(ordered))
	runtimeToolchainMu.Lock()
	defer runtimeToolchainMu.Unlock()
	for _, ref := range ordered {
		resolution, ok := ext.RuntimeToolchains[ref]
		if !ok {
			parts = append(parts, ref+"=unresolved")
			continue
		}
		parts = append(parts, ref+"="+resolution.Identity)
	}
	return strings.Join(parts, ";")
}

func runtimeToolchainIdentity(requirement extensionproto.RuntimeToolchain, version, integrity string, available bool) string {
	h := sha256.New()
	declaration, _ := json.Marshal(requirement)
	hashField(h, "declaration", declaration)
	hashField(h, "version", []byte(version))
	hashField(h, "platform", []byte(HostPlatform()))
	hashField(h, "integrity", []byte(integrity))
	availability := unavailableRuntimeToolchainIdentity
	if available {
		availability = "available"
	}
	hashField(h, "availability", []byte(availability))
	return hex.EncodeToString(h.Sum(nil))
}

func unavailableRuntimeToolchainResolution(requirement extensionproto.RuntimeToolchain, entry lockfile.LockEntry) model.RuntimeToolchainResolution {
	return model.RuntimeToolchainResolution{Identity: runtimeToolchainIdentity(requirement, entry.Version,
		entry.IntegrityFor(runtime.GOOS, runtime.GOARCH), false)}
}

func sortedRuntimeToolchainRefs(refs map[string]bool) []string {
	ordered := make([]string, 0, len(refs))
	for ref := range refs {
		ordered = append(ordered, ref)
	}
	sort.Strings(ordered)
	return ordered
}
