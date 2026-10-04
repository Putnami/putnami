package jobs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/sdk/extension/envkeys"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const extensionRuntimeDigestVersion = "putnami-extension-runtime-v1"

type extensionRuntimeError struct {
	code string
	err  error
}

func (e *extensionRuntimeError) Error() string {
	return fmt.Sprintf("%s: %v", e.code, e.err)
}

func (e *extensionRuntimeError) Unwrap() error { return e.err }

func runtimeFailure(code, format string, args ...any) error {
	return &extensionRuntimeError{code: code, err: fmt.Errorf(format, args...)}
}

func jobReferencesExtensionRuntime(job *ScheduledJob) bool {
	if job == nil || job.JobDef == nil {
		return false
	}
	return definitionReferencesExtensionRuntime(job.JobDef)
}

func definitionReferencesExtensionRuntime(def *extension.JobDefinition) bool {
	if def == nil {
		return false
	}
	return fieldsReferenceExtensionRuntime(def.Command, def.Cwd, def.Args, def.Env)
}

func jobRuntimeUse(
	ext *extension.ExtensionDescription,
	def *extension.JobDefinition,
) *extensionRuntimeUse {
	if def == nil {
		return nil
	}
	if definitionReferencesExtensionRuntime(def) {
		return &extensionRuntimeUse{kind: "task", name: def.Name}
	}
	for _, step := range def.PipelineSteps {
		task, ok := ext.Tasks[step.Task]
		if ok && fieldsReferenceExtensionRuntime(task.Command, task.Cwd, task.Args, task.Env) {
			return &extensionRuntimeUse{kind: "task", name: step.Task}
		}
	}
	return nil
}

func hookReferencesExtensionRuntime(def *extension.HookDefinition) bool {
	if def == nil {
		return false
	}
	return fieldsReferenceExtensionRuntime(def.Command, def.Cwd, def.Args, def.Env)
}

func fieldsReferenceExtensionRuntime(command, cwd string, args []string, env map[string]string) bool {
	token := "{" + extensionproto.TemplateVarExtensionRuntime + "}"
	if strings.Contains(command, token) || strings.Contains(cwd, token) {
		return true
	}
	for _, arg := range args {
		if strings.Contains(arg, token) {
			return true
		}
	}
	for _, value := range env {
		if strings.Contains(value, token) {
			return true
		}
	}
	return false
}

type extensionRuntimeUse struct {
	kind string
	name string
}

// SynchronizeExtensionRuntimes resolves every declared runtime in the extension
// set. Explicit hook callers and focused callers use this complete operation;
// the engine uses the command-scoped wrapper below to choose the demanded
// extension set first. Local sources are prepared once per content digest by
// artifactstore.Admit; installed sources are validated in place.
//
// THE STAGE IS A DAG, AND THE ONLY EDGES IT HAS ARE WITHIN ONE EXTENSION.
// Each extension's chain is resolve digest → (store hit ?
// verify : stage → generate → verify → publish) → verify handshake, and no
// chain reads anything another chain writes:
//
//   - the digest is computed from the extension's own source tree and the local
//     module replacements it declares, all read-only;
//   - preparation happens in a staging directory the store creates privately per
//     admit, and is published by atomic rename;
//   - the only shared writable state is the machine-global artifact store, whose
//     per-digest ownership lock already serializes the mutating step ACROSS
//     PROCESSES (flock(2) keys on the open file description, so sibling
//     goroutines in this process are held apart by the same lock);
//   - the only per-extension write is that extension's own RuntimeExecutable /
//     RuntimeDigest, and the plan below visits each description exactly once.
//
// So the chains run concurrently and the mutations stay exclusive without a new
// lock. report may be nil, which records no attribution.
//
// Errors keep the serial function's identity: an undeclared runtime is reported
// for the FIRST extension in slice order that has one, before any preparation
// starts, and a preparation failure is reported for the lowest-indexed
// extension that failed. Siblings are NOT canceled when one fails — a canceled
// sibling's error would race the real one for the report, and a stage that
// blames the wrong extension costs more than the seconds it saves.
func SynchronizeExtensionRuntimes(
	ctx context.Context,
	ws *workspace.Workspace,
	extensions []*extension.ExtensionDescription,
	report *PreparationReport,
) error {
	return synchronizeExtensionRuntimesResolving(ctx, workspaceRootOf(ws), extensions, report, false)
}

// workspaceRootOf is the root the runtime stage resolves the artifact store
// and toolchain lock from, or "" without a workspace.
func workspaceRootOf(ws *workspace.Workspace) string {
	if ws == nil {
		return ""
	}
	return ws.Root
}

// synchronizeExtensionRuntimesResolving is SynchronizeExtensionRuntimes whose
// prepare toolchains resolve as the provisioning command's do when
// provisioning is set (see provisioningOnlyRun).
func synchronizeExtensionRuntimesResolving(
	ctx context.Context,
	workspaceRoot string,
	extensions []*extension.ExtensionDescription,
	report *PreparationReport,
	provisioning bool,
) error {
	artifacts := artifactstore.New(store.ResolveArtifactStoreRoot(workspaceRoot))
	if err := resolveRuntimeToolchainsForPreparation(ctx, workspaceRoot, extensions, os.Environ(), provisioning); err != nil {
		return runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
			"resolve extension runtime toolchains: %v", err)
	}
	return synchronizeExtensionRuntimesWithEnv(ctx, extensions, report, artifacts, os.Environ())
}

// synchronizeExtensionRuntimes carries the artifact store as an explicit stage
// dependency. Production resolves the machine-global store above; focused
// callers can supply an isolated store without mutating PUTNAMI_ARTIFACT_DIR,
// which also makes independent runtime chains safe to exercise concurrently.
func synchronizeExtensionRuntimes(
	ctx context.Context,
	extensions []*extension.ExtensionDescription,
	report *PreparationReport,
	artifacts *artifactstore.Store,
) error {
	return synchronizeExtensionRuntimesWithEnv(ctx, extensions, report, artifacts, os.Environ())
}

func synchronizeExtensionRuntimesWithEnv(
	ctx context.Context,
	extensions []*extension.ExtensionDescription,
	report *PreparationReport,
	artifacts *artifactstore.Store,
	prepareEnv []string,
) error {
	nodes, err := planExtensionRuntimeSync(extensions)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return nil
	}

	started := time.Now()
	workers := preparationWorkers(len(nodes), runtime.NumCPU(), totalMemoryBytes())
	spans := make([]*preparationSpans, len(nodes))
	errs := make([]error, len(nodes))
	for i := range spans {
		spans[i] = &preparationSpans{}
	}

	if workers <= 1 || len(nodes) == 1 {
		// One worker is the pre-parallel stage exactly: nothing is in flight when
		// a node fails, so stopping is the same verdict as joining would give and
		// no work is started that the run will not use.
		for i, ext := range nodes {
			if errs[i] = synchronizeDeclaredExtensionRuntimeWithEnv(ctx, ext, spans[i], artifacts, prepareEnv); errs[i] != nil {
				break
			}
		}
	} else {
		gate := make(chan struct{}, workers)
		var wg sync.WaitGroup
		for i, ext := range nodes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				gate <- struct{}{}
				defer func() { <-gate }()
				errs[i] = synchronizeDeclaredExtensionRuntimeWithEnv(ctx, ext, spans[i], artifacts, prepareEnv)
			}()
		}
		wg.Wait()
	}

	for _, worker := range spans {
		report.merge(worker)
	}
	report.observeStage(time.Since(started), workers)
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// planExtensionRuntimeSync turns the requested extension set into the stage's
// independent nodes, rejecting a manifest that cannot run before any work
// starts.
//
// Descriptions are de-duplicated BY POINTER, not by name: two entries for the
// same description would otherwise become two workers writing the same
// RuntimeExecutable field. Two DISTINCT descriptions that resolve to the same
// content digest stay two nodes and converge in the store, which is the
// content-addressed reuse working rather than a case to special-case here.
func planExtensionRuntimeSync(
	extensions []*extension.ExtensionDescription,
) ([]*extension.ExtensionDescription, error) {
	nodes := make([]*extension.ExtensionDescription, 0, len(extensions))
	seen := make(map[*extension.ExtensionDescription]bool, len(extensions))
	for _, ext := range extensions {
		if ext == nil || seen[ext] {
			continue
		}
		seen[ext] = true
		if ext.Runtime == nil {
			if use := firstExtensionRuntimeUse(ext, true); use != nil {
				return nil, runtimeNotDeclared(ext, use)
			}
			continue
		}
		nodes = append(nodes, ext)
	}
	return nodes, nil
}

// preparationWorkers bounds the stage's fan-out.
//
// Every node is a COMPILE that already fans out inside its own toolchain, so
// the CPU bound is the visible core count rather than the scheduler's 2x/3x
// oversubscription for mixed plans — and the same memory-pressure cap the
// scheduler applies to an all-heavy plan applies here, because that is exactly
// what this plan is. Hardware is a parameter so the policy is testable without
// one.
func preparationWorkers(nodes, logicalCPU int, totalMemoryBytes uint64) int {
	if nodes <= 1 {
		return 1
	}
	workers := min(nodes, max(1, logicalCPU))
	if memoryCap := memoryWorkerCap(parallelModeAuto, totalMemoryBytes, 1000); memoryCap > 0 {
		workers = min(workers, memoryCap)
	}
	return max(1, workers)
}

// SynchronizeExtensionRuntimesForCommands resolves only runtimes used by the
// selected commands (including their command dependency closure) and by the
// preBuild hook that will bracket those jobs. It still runs before project
// selection and planning; only the demanded extension surface is narrower.
//
// A task toolchain the workspace lock does not satisfy stays unresolved rather
// than failing here: the run may plan no job that requires it, as in a
// workspace with an extension and no project of its language. The caller runs
// RequirePlannedRuntimeToolchains on the plan before any job runs.
func SynchronizeExtensionRuntimesForCommands(
	ctx context.Context,
	ws *workspace.Workspace,
	extensions []*extension.ExtensionDescription,
	commands []string,
	report *PreparationReport,
) error {
	return synchronizeExtensionRuntimesForCommands(ctx, workspaceRootOf(ws), extensions, commands, report, resolveDeferred)
}

// synchronizeExtensionRuntimesForCommands is SynchronizeExtensionRuntimesForCommands
// for a caller that holds the workspace root rather than the loaded workspace.
// mode applies to the task toolchains of the active commands other than
// toolchainProvisioningCommand.
func synchronizeExtensionRuntimesForCommands(
	ctx context.Context,
	workspaceRoot string,
	extensions []*extension.ExtensionDescription,
	commands []string,
	report *PreparationReport,
	mode toolchainResolutionMode,
) error {
	activeCommands := commandDependencyClosure(extensions, commands)
	demanded := make([]*extension.ExtensionDescription, 0, len(extensions))
	for _, ext := range extensions {
		if ext == nil {
			continue
		}
		use := commandRuntimeUse(ext, activeCommands)
		if use == nil {
			continue
		}
		if ext.Runtime == nil {
			return runtimeNotDeclared(ext, use)
		}
		demanded = append(demanded, ext)
	}
	if err := synchronizeExtensionRuntimesResolving(ctx, workspaceRoot, demanded, report, provisioningOnlyRun(activeCommands)); err != nil {
		return err
	}
	// Task toolchains are resolved for EVERY extension whose active commands
	// declare one, not only the demanded ones: an extension can pin the tool a
	// task runs without its own runtime executable being referenced by this
	// command. Resolution has to happen here — before selection, planning and
	// every cache key — so the identity a job runs with is the identity it is
	// keyed under. A task toolchain mode leaves unresolved resolves once the
	// plan is final (RequirePlannedRuntimeToolchains), still before any key.
	return resolveCommandRuntimeToolchains(ctx, workspaceRoot, extensions, activeCommands, mode)
}

// resolveCommandRuntimeToolchains resolves the toolchains the active commands
// of extensions declare, the last step of synchronizeExtensionRuntimesForCommands.
func resolveCommandRuntimeToolchains(
	ctx context.Context,
	workspaceRoot string,
	extensions []*extension.ExtensionDescription,
	activeCommands map[string]bool,
	mode toolchainResolutionMode,
) error {
	if err := resolveRuntimeToolchainsForCommandsMode(ctx, workspaceRoot, extensions, activeCommands, os.Environ(), mode); err != nil {
		return runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
			"resolve extension runtime toolchains: %v", err)
	}
	return nil
}

func commandRuntimeUse(
	ext *extension.ExtensionDescription,
	activeCommands map[string]bool,
) *extensionRuntimeUse {
	contributes := false
	for commandName, def := range ext.Jobs {
		if !activeCommands[commandName] {
			continue
		}
		contributes = true
		if use := jobRuntimeUse(ext, def); use != nil {
			return use
		}
	}
	if contributes {
		return firstHookRuntimeUse(ext, false)
	}
	return nil
}

func commandDependencyClosure(
	extensions []*extension.ExtensionDescription,
	commands []string,
) map[string]bool {
	active := make(map[string]bool, len(commands))
	for _, command := range commands {
		active[command] = true
	}
	changed := true
	for changed {
		changed = false
		addDependency := func(rawDependency string) {
			dependency := strings.TrimPrefix(strings.TrimSpace(rawDependency), "!")
			if dependency != "" && !active[dependency] {
				active[dependency] = true
				changed = true
			}
		}
		for _, ext := range extensions {
			if ext == nil {
				continue
			}
			for commandName, def := range ext.Jobs {
				if def == nil {
					continue
				}
				if !active[commandName] {
					continue
				}
				for _, rawDependency := range def.CommandDependsOn {
					addDependency(rawDependency)
				}
				// Session prerequisites are expanded by the planner after this
				// synchronization stage. Include both their target command and their
				// gates here so every extension runtime the final DAG can reference is
				// ready before planning begins.
				for _, prerequisite := range def.SessionPrerequisites {
					addDependency(prerequisite.Command)
					for _, gate := range prerequisite.DependsOn {
						addDependency(gate)
					}
				}
			}
		}
	}
	return active
}

func firstHookRuntimeUse(ext *extension.ExtensionDescription, all bool) *extensionRuntimeUse {
	if ext == nil || ext.Hooks == nil {
		return nil
	}
	type hookRuntimeDefinition struct {
		name string
		def  *extension.HookDefinition
	}
	hooks := []hookRuntimeDefinition{
		{name: "preBuild", def: ext.Hooks.PreBuild},
	}
	if all {
		// The cache lifecycle is no longer a hook: it is
		// the reserved cache-clean/cache-gc COMMANDS, whose {extensionRuntime}
		// use is already found by the ordinary job scan in
		// firstExtensionRuntimeUse, so it needs no entry here.
		hooks = append(hooks,
			hookRuntimeDefinition{name: "onInstall", def: ext.Hooks.OnInstall},
		)
	}
	for _, hook := range hooks {
		if hookReferencesExtensionRuntime(hook.def) {
			return &extensionRuntimeUse{kind: "hook", name: hook.name}
		}
	}
	return nil
}

func firstExtensionRuntimeUse(
	ext *extension.ExtensionDescription,
	allHooks bool,
) *extensionRuntimeUse {
	for _, def := range ext.Jobs {
		if use := jobRuntimeUse(ext, def); use != nil {
			return use
		}
	}
	return firstHookRuntimeUse(ext, allHooks)
}

func runtimeNotDeclared(
	ext *extension.ExtensionDescription,
	use *extensionRuntimeUse,
) error {
	return runtimeFailure(extensionproto.FailureRuntimeNotDeclared,
		"%s %q references {%s}, but extension %q declares no runtime",
		use.kind, use.name, extensionproto.TemplateVarExtensionRuntime, ext.Name)
}

func synchronizeDeclaredExtensionRuntimeWithEnv(
	ctx context.Context,
	ext *extension.ExtensionDescription,
	spans *preparationSpans,
	artifacts *artifactstore.Store,
	prepareEnv []string,
) error {
	digest := ""
	if ext.LocalSource && ext.Runtime.Prepare != nil {
		var err error
		resolving := time.Now()
		digest, err = extensionRuntimeDigest(ext)
		spans.since(PreparationResolution, resolving)
		if err != nil {
			return runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
				"digest extension %q runtime inputs: %v", ext.Name, err)
		}
	}
	executable, err := prepareOrLoadExtensionRuntimeWithDigestAndEnv(ctx, artifacts, ext, digest, spans, prepareEnv)
	if err != nil {
		return err
	}
	ext.RuntimeExecutable = executable
	ext.RuntimeDigest = digest
	return nil
}

// ExtensionImplementationIdentity names the implementation a provider binding
// runs, for the workspace snapshot to key a recorded probe answer on: the
// runtime input digest of a workspace-local extension (the value its prepared
// runtime carries), the version of a registry-installed one. Empty when the
// extension declares no runtime or its inputs cannot be read.
func ExtensionImplementationIdentity(ext *extension.ExtensionDescription) string {
	if ext == nil {
		return ""
	}
	if ext.RuntimeDigest != "" {
		return ext.RuntimeDigest
	}
	if ext.Runtime == nil || ext.Runtime.Prepare == nil {
		return ""
	}
	if !ext.LocalSource {
		return ext.Version
	}
	digest, err := extensionRuntimeDigest(ext)
	if err != nil {
		return ""
	}
	return digest
}

func isLocalDevExtensionSource(ws *workspace.Workspace, job *ScheduledJob) bool {
	if ws == nil || ws.Root == "" || job == nil || job.Extension == nil || !job.Extension.LocalSource {
		return false
	}
	if job.Extension.RelPath == "" {
		return true
	}
	root := jobExtensionRoot(ws, job)
	rel, err := filepath.Rel(ws.Root, root)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolvedExtensionRuntime(job *ScheduledJob) (string, error) {
	if !jobReferencesExtensionRuntime(job) {
		return "", nil
	}
	if job == nil || job.Extension == nil || job.Extension.Runtime == nil {
		return "", runtimeFailure(extensionproto.FailureRuntimeNotDeclared,
			"task %q references {%s}, but extension %q declares no runtime",
			job.JobDef.Name, extensionproto.TemplateVarExtensionRuntime, extensionName(job))
	}
	if job.Extension.RuntimeExecutable == "" {
		return "", runtimeFailure(extensionproto.FailureRuntimeExecutableMissing,
			"extension %q runtime was not resolved during workspace synchronization",
			job.Extension.Name)
	}
	return job.Extension.RuntimeExecutable, nil
}

func extensionName(job *ScheduledJob) string {
	if job != nil && job.Extension != nil {
		return job.Extension.Name
	}
	return ""
}

func prepareOrLoadExtensionRuntime(
	ctx context.Context,
	artifacts *artifactstore.Store,
	ext *extension.ExtensionDescription,
) (string, error) {
	return prepareOrLoadExtensionRuntimeWithDigestAndEnv(ctx, artifacts, ext, "", nil, os.Environ())
}

func prepareOrLoadExtensionRuntimeWithDigestAndEnv(
	ctx context.Context,
	artifacts *artifactstore.Store,
	ext *extension.ExtensionDescription,
	digest string,
	spans *preparationSpans,
	prepareEnv []string,
) (string, error) {
	def := ext.Runtime
	root := filepath.Clean(ext.Path)
	if root == "." || root == "" {
		root = filepath.Clean(jobExtensionRoot(nil, &ScheduledJob{Extension: ext}))
	}
	// Both branches below start the extension's code: its prepare command and
	// its runtime-info handshake. An extension installed from the artifact
	// store is registry code; any other extension, a local one first of all,
	// is repository code, recorded before the first spawn.
	if artifacts == nil || !extension.InStoreRoot(artifacts.Root(), ext) {
		runcredential.MarkRepositoryCodeStarted("extension runtime of " + ext.Name)
	}

	if !ext.LocalSource || def.Prepare == nil {
		executable := runtimeExecutablePath(root, def.Executable, runtime.GOOS)
		verifying := time.Now()
		if err := validateRuntimeExecutable(executable); err != nil {
			spans.since(PreparationVerification, verifying)
			return "", runtimeFailure(extensionproto.FailureRuntimeExecutableMissing,
				"extension %q runtime %q is unavailable: %v%s", ext.Name, def.Executable, err,
				unsuffixedRuntimeHint(root, def.Executable, runtime.GOOS))
		}
		spans.since(PreparationVerification, verifying)
		if err := validateRuntimeHandshake(ctx, executable, ext, spans); err != nil {
			return "", err
		}
		return executable, nil
	}

	if digest == "" {
		var err error
		resolving := time.Now()
		digest, err = extensionRuntimeDigest(ext)
		spans.since(PreparationResolution, resolving)
		if err != nil {
			return "", runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
				"digest extension %q runtime inputs: %v", ext.Name, err)
		}
	}
	// The store admit IS the mutation step: it takes the per-digest ownership
	// lock, hands out a private staging tree, and publishes by atomic rename.
	// The classified spans filed by the stage callback below are disjoint
	// sub-intervals of it, so the residual — the admit's wall minus what the
	// callback accounted for — is exactly the lock wait plus the publish, and
	// cannot go negative. Attributing it as mutation is what makes the
	// serialization cost of two processes contending on one workspace visible
	// instead of vanishing into an unattributed remainder.
	admitting := time.Now()
	accountedBefore := spans.elapsed()
	dir, err := artifacts.AdmitContext(ctx, digest, func(stageDir string) error {
		sourceView := filepath.Join(stageDir, ".source")
		outputRoot := filepath.Join(stageDir, "runtime")
		staging := time.Now()
		if err := os.MkdirAll(sourceView, 0o755); err != nil {
			spans.since(PreparationMutation, staging)
			return runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
				"create isolated source view: %v", err)
		}
		if err := os.MkdirAll(outputRoot, 0o755); err != nil {
			spans.since(PreparationMutation, staging)
			return runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
				"create runtime output: %v", err)
		}
		spans.since(PreparationMutation, staging)

		resolving := time.Now()
		inputs, collectErr := collectRuntimeInputs(root, def.Prepare.Inputs)
		spans.since(PreparationResolution, resolving)
		if collectErr != nil {
			return runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
				"collect extension %q runtime inputs: %v", ext.Name, collectErr)
		}
		staging = time.Now()
		sourceRoot, copyErr := stageRuntimeSourceView(root, sourceView, inputs)
		spans.since(PreparationMutation, staging)
		if copyErr != nil {
			return runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
				"stage extension %q runtime inputs: %v", ext.Name, copyErr)
		}

		vars := map[string]string{
			"extensionRoot":                         sourceRoot,
			extensionproto.TemplateVarRuntimeOutput: outputRoot,
		}
		command := extension.ExpandTemplateVars(def.Prepare.Command, vars)
		args := make([]string, len(def.Prepare.Args))
		for i, arg := range def.Prepare.Args {
			args[i] = extension.ExpandTemplateVars(arg, vars)
		}
		env := applyRuntimeToolchains(prepareEnv, ext, def.Prepare.Toolchains)
		env = setEnv(env, "GOWORK", "off")
		env = setEnv(env, "PUTNAMI_EXTENSION_ROOT", sourceRoot)
		env = setEnv(env, "PWD", sourceRoot)
		generating := time.Now()
		out, processState, runErr := runRuntimePrepareCommand(ctx, command, args, sourceRoot, env)
		spans.add(PreparationGeneration, time.Since(generating), spawnCPU(processState))
		if runErr != nil {
			return runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
				"prepare extension %q runtime with %q: %v: %s",
				ext.Name, def.Prepare.Command, runErr, strings.TrimSpace(string(out)))
		}
		executable := runtimeExecutablePath(outputRoot, def.Executable, runtime.GOOS)
		verifying := time.Now()
		if err := validateRuntimeExecutable(executable); err != nil {
			spans.since(PreparationVerification, verifying)
			return runtimeFailure(extensionproto.FailureRuntimePrepareOutputMissing,
				"extension %q prepare did not produce executable %q: %v%s",
				ext.Name, def.Executable, err, unsuffixedRuntimeHint(outputRoot, def.Executable, runtime.GOOS))
		}
		// Re-hashing the inputs is VERIFICATION, not resolution: the identity is
		// already decided, and this run only proves the source did not move under
		// the preparation that claims to implement it.
		after, digestErr := extensionRuntimeDigest(ext)
		spans.since(PreparationVerification, verifying)
		if digestErr != nil {
			return runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
				"re-hash extension %q runtime inputs: %v", ext.Name, digestErr)
		}
		if after != digest {
			return runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
				"extension %q runtime inputs changed during preparation", ext.Name)
		}
		if handshakeErr := validateRuntimeHandshake(ctx, executable, ext, spans); handshakeErr != nil {
			return handshakeErr
		}
		reclaiming := time.Now()
		removeErr := os.RemoveAll(sourceView)
		spans.since(PreparationMutation, reclaiming)
		if removeErr != nil {
			return runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
				"remove isolated source view: %v", removeErr)
		}
		return nil
	})
	spans.add(PreparationMutation, time.Since(admitting)-(spans.elapsed()-accountedBefore), 0)
	if err != nil {
		if ctx.Err() != nil {
			return "", runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
				"admit extension %q runtime: %w", ext.Name, err)
		}
		return "", err
	}
	executable := runtimeExecutablePath(filepath.Join(dir, "runtime"), def.Executable, runtime.GOOS)
	verifying := time.Now()
	if err := validateRuntimeExecutable(executable); err != nil {
		spans.since(PreparationVerification, verifying)
		return "", runtimeFailure(extensionproto.FailureRuntimeExecutableMissing,
			"prepared extension %q runtime disappeared: %v", ext.Name, err)
	}
	spans.since(PreparationVerification, verifying)
	if err := validateRuntimeHandshake(ctx, executable, ext, spans); err != nil {
		return "", err
	}
	return executable, nil
}

func runRuntimePrepareCommand(
	ctx context.Context,
	command string,
	args []string,
	dir string,
	env []string,
) ([]byte, *os.ProcessState, error) {
	const attempts = 6
	delay := 5 * time.Millisecond
	for attempt := range attempts {
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err == nil || !isTextFileBusy(err) || attempt == attempts-1 {
			return out, cmd.ProcessState, err
		}
		select {
		case <-ctx.Done():
			return out, cmd.ProcessState, ctx.Err()
		case <-time.After(delay):
			delay *= 2
		}
	}
	panic("unreachable runtime prepare retry loop")
}

func isTextFileBusy(err error) bool {
	return errors.Is(err, syscall.ETXTBSY)
}

func extensionRuntimeDigest(ext *extension.ExtensionDescription) (string, error) {
	if ext == nil || ext.Runtime == nil || ext.Runtime.Prepare == nil {
		return "", errors.New("runtime preparation is not declared")
	}
	// The root is walked, so it must be the directory itself: a directory
	// link at ext.Path, a junction on Windows included, is followed first.
	root, err := dirlink.Resolve(ext.Path)
	if err != nil {
		return "", fmt.Errorf("resolve extension root: %w", err)
	}
	inputs, err := collectRuntimeInputs(root, ext.Runtime.Prepare.Inputs)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	hashField(h, "schema", []byte(extensionRuntimeDigestVersion))
	hashField(h, "extension", []byte(ext.Name))
	hashField(h, "version", []byte(ext.Version))
	hashField(h, "platform", []byte(runtime.GOOS+"/"+runtime.GOARCH))
	hashField(h, "runtime-abi", []byte(fmt.Sprintf("%d", runtimeproto.RuntimeABIVersion)))
	hashField(h, "toolchains", []byte(runtimeToolchainIdentityForRefs(ext, ext.Runtime.Prepare.Toolchains)))
	declaration, err := json.Marshal(ext.Runtime)
	if err != nil {
		return "", fmt.Errorf("marshal runtime declaration: %w", err)
	}
	hashField(h, "declaration", declaration)
	for _, input := range ext.Runtime.Prepare.Inputs {
		hashField(h, "input-pattern", []byte(input))
	}
	for _, rel := range inputs {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return "", fmt.Errorf("stat runtime input %s: %w", rel, err)
		}
		hashField(h, "input/"+rel+"/mode", []byte(fmt.Sprintf("%o", info.Mode()&(os.ModeType|0o111))))
		file, err := os.Open(full)
		if err != nil {
			return "", fmt.Errorf("open runtime input %s: %w", rel, err)
		}
		if err := hashStream(h, "input/"+rel, info.Size(), file); err != nil {
			_ = file.Close()
			return "", fmt.Errorf("hash runtime input %s: %w", rel, err)
		}
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("close runtime input %s: %w", rel, err)
		}
	}
	replacements, err := runtimeReplacementClosure(root)
	if err != nil {
		return "", err
	}
	for _, replacement := range replacements {
		if err := hashRuntimeReplacementTree(h, "module-replacement/"+replacement.label, replacement.root); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// collectRuntimeInputs returns the slash-form paths, relative to root, of the
// regular files under root that patterns select, sorted. A directory link at
// root, a junction on Windows included, is followed first, so the walk reads
// the directory the digest hashes and staging copies, never the link itself.
func collectRuntimeInputs(root string, patterns []string) ([]string, error) {
	resolved, err := dirlink.Resolve(root)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime input root: %w", err)
	}
	root = filepath.Clean(resolved)
	matches := make(map[string]bool)
	err = filepath.WalkDir(root, func(full string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		matched := false
		for _, pattern := range patterns {
			if matchRuntimeInput(pattern, rel) {
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("runtime input %q is a symlink", rel)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("runtime input %q is not a regular file", rel)
		}
		matches[rel] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for rel := range matches {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

func matchRuntimeInput(pattern, rel string) bool {
	patternParts := strings.Split(path.Clean(pattern), "/")
	relParts := strings.Split(path.Clean(rel), "/")
	var match func(int, int) bool
	match = func(pi, ri int) bool {
		if pi == len(patternParts) {
			return ri == len(relParts)
		}
		if patternParts[pi] == "**" {
			if match(pi+1, ri) {
				return true
			}
			return ri < len(relParts) && match(pi, ri+1)
		}
		if ri == len(relParts) {
			return false
		}
		ok, err := path.Match(patternParts[pi], relParts[ri])
		return err == nil && ok && match(pi+1, ri+1)
	}
	return match(0, 0)
}

func copyRuntimeInputs(root, dest string, inputs []string) error {
	for _, rel := range inputs {
		source := filepath.Join(root, filepath.FromSlash(rel))
		target := filepath.Join(dest, filepath.FromSlash(rel))
		targetDir := filepath.Dir(target)
		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			return err
		}
		info, err := os.Lstat(source)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("runtime input %q is not a regular non-symlink file", rel)
		}
		mode := os.FileMode(0o444)
		if info.Mode()&0o111 != 0 {
			mode = 0o555
		}
		if err := copyRuntimeInputFile(source, target, mode); err != nil {
			return err
		}
	}
	return nil
}

// copyRuntimeTree stages a replaced module's enumerated tree. Regular files are
// copied; a contained symlink is recreated verbatim, so the staged view keeps
// the shape hashRuntimeReplacementTree fingerprinted. collectRuntimeTree already
// refused every link that leaves the replacement root, so a recreated link can
// only resolve inside the staged copy.
func copyRuntimeTree(root, dest string, entries []runtimeTreeEntry) error {
	for _, entry := range entries {
		source := filepath.Join(root, filepath.FromSlash(entry.rel))
		target := filepath.Join(dest, filepath.FromSlash(entry.rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if entry.symlink {
			if err := stageRuntimeSymlink(source, entry.target, target); err != nil {
				return err
			}
			continue
		}
		info, err := os.Lstat(source)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			// The entry changed type after the enumeration classified it. Refuse
			// rather than copy through a link the digest never validated.
			return fmt.Errorf("module replacement input %q is not a regular non-symlink file", entry.rel)
		}
		mode := os.FileMode(0o444)
		if info.Mode()&0o111 != 0 {
			mode = 0o555
		}
		if err := copyRuntimeInputFile(source, target, mode); err != nil {
			return err
		}
	}
	return nil
}

// stageRuntimeSymlink recreates the link at source. A link to a directory goes
// through dirlink, which makes it a junction on Windows, where a symbolic link
// needs Developer Mode. A link to a file stays a symbolic link, and on Windows
// without Developer Mode its error names the way out.
func stageRuntimeSymlink(source, linkTarget, linkPath string) error {
	create := symlinkRuntimeFile(source)
	if info, err := os.Stat(source); err == nil && info.IsDir() {
		create = dirlink.Create
	}
	err := create(linkTarget, linkPath)
	if err == nil || !errors.Is(err, os.ErrExist) {
		return err
	}
	// Two staged roots can nest — a replacement living inside another
	// replacement's directory stages the same path twice. Both stagings come
	// from the same enumeration of the same bytes, so replace instead of
	// failing, the way the file copy's rename already does.
	if err := os.Remove(linkPath); err != nil {
		return err
	}
	return create(linkTarget, linkPath)
}

// symlinkRuntimeFile returns os.Symlink for the replaced module's file link at
// source. When the platform refuses the link for want of the symbolic-link
// privilege, the error names Developer Mode and source.
func symlinkRuntimeFile(source string) func(target, link string) error {
	return func(target, link string) error {
		err := os.Symlink(target, link)
		if err != nil && symlinkPrivilegeMissing(err) {
			return fmt.Errorf(
				"module replacement input %s is a file symbolic link, which Windows creates only with Developer Mode: "+
					"turn on Developer Mode (Settings > System > For developers), or replace the link with a regular file: %w",
				source, err)
		}
		return err
	}
}

func copyRuntimeInputFile(source, target string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	targetDir := filepath.Dir(target)
	// Never expose an executable at its final path while its inode is still
	// open for writing. Linux rejects a concurrent exec in that window with
	// ETXTBSY, which is observable when several runtimes are staged together.
	// A closed temporary inode followed by rename makes the final path ready
	// for exec from the instant it becomes visible.
	out, err := os.CreateTemp(targetDir, ".putnami-runtime-input-")
	if err != nil {
		_ = in.Close()
		return err
	}
	temporary := out.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if err := out.Chmod(mode); err != nil {
		_ = out.Close()
		_ = in.Close()
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeOutErr := out.Close()
	closeInErr := in.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeOutErr != nil {
		return closeOutErr
	}
	if closeInErr != nil {
		return closeInErr
	}
	if err := os.Rename(temporary, target); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

// runtimeReplacement is one module a local replace directive reaches. root is
// the physical directory its tree is read from. stage is where the declaring
// module's go.mod expects it: a relative directive joins the declaring
// module's stage path lexically, as the go command does, so a replacement
// behind a directory link is staged at the path the go.mod names rather than
// where the link points. An absolute directive keeps the physical root, since
// the build reads that path outside the staged view.
type runtimeReplacement struct {
	label string
	root  string
	stage string
}

func runtimeReplacementClosure(extensionRoot string) ([]runtimeReplacement, error) {
	type pendingModule struct {
		label string
		root  string
		stage string
	}
	queue := []pendingModule{{label: "extension", root: extensionRoot, stage: filepath.Clean(extensionRoot)}}
	seen := map[string]bool{filepath.Clean(extensionRoot): true}
	var replacements []runtimeReplacement
	for len(queue) > 0 {
		module := queue[0]
		queue = queue[1:]
		goMod := filepath.Join(module.root, "go.mod")
		if _, err := os.Stat(goMod); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		paths, err := localDirectivePaths(goMod, "replace")
		if err != nil {
			return nil, err
		}
		for _, declared := range paths {
			replacementRoot := declared
			if !filepath.IsAbs(replacementRoot) {
				replacementRoot = filepath.Join(module.root, replacementRoot)
			}
			replacementRoot, err = dirlink.Resolve(replacementRoot)
			if err != nil {
				return nil, fmt.Errorf("resolve local replacement %q from %s: %w", declared, goMod, err)
			}
			replacementRoot = filepath.Clean(replacementRoot)
			if seen[replacementRoot] {
				continue
			}
			seen[replacementRoot] = true
			label := module.label + "/" + filepath.ToSlash(declared)
			stage := replacementRoot
			if !filepath.IsAbs(declared) {
				stage = filepath.Join(module.stage, declared)
			}
			replacements = append(replacements, runtimeReplacement{label: label, root: replacementRoot, stage: stage})
			queue = append(queue, pendingModule{label: label, root: replacementRoot, stage: stage})
		}
	}
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].label < replacements[j].label })
	return replacements, nil
}

func stageRuntimeSourceView(extensionRoot, viewRoot string, inputs []string) (string, error) {
	physicalExtensionRoot, err := dirlink.Resolve(extensionRoot)
	if err != nil {
		return "", fmt.Errorf("resolve extension source root: %w", err)
	}
	extensionRoot = physicalExtensionRoot
	replacements, err := runtimeReplacementClosure(extensionRoot)
	if err != nil {
		return "", err
	}
	common := filepath.Clean(extensionRoot)
	// Two directories staged at one path would merge into one tree.
	staged := map[string]string{common: common}
	for _, replacement := range replacements {
		if other, ok := staged[replacement.stage]; ok {
			return "", fmt.Errorf("runtime sources %q and %q both stage at %q", other, replacement.root, replacement.stage)
		}
		staged[replacement.stage] = replacement.root
		common = commonPathAncestor(common, replacement.stage)
	}
	stagePath := func(root string) (string, error) {
		rel, err := filepath.Rel(common, root)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("runtime source %q is outside common module root %q", root, common)
		}
		return filepath.Join(viewRoot, rel), nil
	}
	stagedExtension, err := stagePath(extensionRoot)
	if err != nil {
		return "", err
	}
	if err := copyRuntimeInputs(extensionRoot, stagedExtension, inputs); err != nil {
		return "", err
	}
	for _, replacement := range replacements {
		stagedReplacement, err := stagePath(replacement.stage)
		if err != nil {
			return "", err
		}
		entries, err := collectRuntimeTree(replacement.root)
		if err != nil {
			return "", err
		}
		if err := copyRuntimeTree(replacement.root, stagedReplacement, entries); err != nil {
			return "", err
		}
	}
	return stagedExtension, nil
}

func commonPathAncestor(a, b string) string {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	for {
		rel, err := filepath.Rel(a, b)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return a
		}
		parent := filepath.Dir(a)
		if parent == a {
			return a
		}
		a = parent
	}
}

// runtimeTreeEntry is one member of a replaced module's tree. Staging and
// hashing consume the same enumeration, so a prepared runtime's digest always
// describes the tree its build actually reads.
type runtimeTreeEntry struct {
	rel string
	// symlink marks an entry staged as a link rather than copied bytes.
	symlink bool
	// target is the raw link target of a symlink entry, empty otherwise.
	target string
}

func collectRuntimeTree(root string) ([]runtimeTreeEntry, error) {
	root = filepath.Clean(root)
	var out []runtimeTreeEntry
	err := filepath.WalkDir(root, func(full string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if runtimeReplacementIgnoredDir(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			if !runtimeReplacementLinkContained(rel, target) {
				return fmt.Errorf(
					"module replacement input %q is a symlink to %q outside the replacement root",
					rel, target)
			}
			out = append(out, runtimeTreeEntry{rel: rel, symlink: true, target: target})
			return nil
		}
		if runtime.GOOS == "windows" && info.Mode()&os.ModeIrregular != 0 {
			return runtimeTreeReparsePointError(full, rel, info)
		}
		if info.Mode().IsRegular() {
			out = append(out, runtimeTreeEntry{rel: rel})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, err
}

// runtimeTreeReparsePointError refuses a replaced module's entry that Go
// reports as irregular on Windows. A directory junction stores an absolute
// target, which a staged copy at another path cannot reproduce; any other
// reparse point has no content the walk can hash or stage.
func runtimeTreeReparsePointError(full, rel string, info os.FileInfo) error {
	if dirlink.IsLink(full, info) {
		target, _ := os.Readlink(full)
		return fmt.Errorf(
			"module replacement input %q is a directory junction to %q: a staged copy cannot reproduce its absolute target, "+
				"so replace it with the directory itself or with a relative symbolic link inside the module",
			rel, target)
	}
	return fmt.Errorf(
		"module replacement input %q is a reparse point that cannot be staged (mode %v): replace it with a regular file",
		rel, info.Mode())
}

// runtimeReplacementLinkContained reports whether a symlink of the replaced
// module stays inside that module. Containment is decided lexically, without
// resolving intermediate links: the staged view is a copy at another path, so
// only a relative target that stays under the replacement root still points at
// staged bytes once it is recreated there. An absolute target, or one that
// climbs out, would let the content-addressed build read bytes from outside the
// tree this digest covers, which is exactly what the previous blanket refusal
// protected against.
func runtimeReplacementLinkContained(rel, target string) bool {
	slashed := filepath.ToSlash(target)
	// filepath.IsAbs alone misses a rooted "/x" on Windows, path.IsAbs alone
	// misses a volume-qualified "C:\x"; either match means the target is not relative.
	if target == "" || filepath.IsAbs(target) || path.IsAbs(slashed) {
		return false
	}
	// path.Join cleans its result, so a target that climbs above the module
	// root keeps a leading "..".
	resolved := path.Join(path.Dir(rel), slashed)
	return resolved != ".." && !strings.HasPrefix(resolved, "../")
}

// runtimeReplacementStagedProjectExclusions are the project-walk exclusions a
// replaced module keeps: vendor/ is build input whenever a module vendors its
// dependencies, and dist/ is ordinary source here — the planner skips those
// names while LOOKING for projects, not while building one.
var runtimeReplacementStagedProjectExclusions = map[string]bool{"vendor": true, "dist": true}

// A local replacement module is source, regardless of directory names such as
// coverage, dist, generated, or tools. Only orchestrator/VCS state, the
// scheduler-owned .gen output tree, and the install output the planner already
// refuses to walk are excluded unconditionally. Build and generate refresh files
// throughout .gen, and any install rewrites its own output tree; hashing or
// staging either makes the prepared-runtime digest follow the previous task
// invocation, or the last install, instead of the replacement module's source.
// That output is also where an install plants the ordinary bin links a staged
// copy cannot resolve. The install-directory names come from the planner's
// shared exclusion table rather than being spelled here, because core does not
// name package managers. The match is by base name and applies to any entry:
// an install may plant those names as symlinks rather than directories.
func runtimeReplacementIgnoredDir(rel string) bool {
	base := path.Base(rel)
	if base == ".git" || base == ".putnami" || base == ".gen" {
		return true
	}
	return projectWalkExcludedDirs[base] && !runtimeReplacementStagedProjectExclusions[base]
}

// runtimeExecutablePath is the file under root that holds the runtime a
// manifest declares at executable, on a machine running goos. Every manifest
// declares the path without a suffix (compiled/putnami-go); on windows the
// file is compiled/putnami-go.exe, the name pkgmeta.ExecutableName gives it,
// which is the one the packagers write into a Windows archive. It is the only
// name the CLI runs: a file at the declared name without the suffix is not a
// Windows build of the runtime, so its absence is runtime.executable_missing.
func runtimeExecutablePath(root, executable, goos string) string {
	return filepath.Join(root, filepath.FromSlash(pkgmeta.ExecutableName(goos, executable)))
}

// unsuffixedRuntimeHint completes a missing-runtime error on goos when root
// holds the declared executable without the ".exe" suffix runtimeExecutablePath
// requires. It is empty when the two names agree, which is every platform but
// Windows, and when no such file exists.
func unsuffixedRuntimeHint(root, executable, goos string) string {
	declared := filepath.Join(root, filepath.FromSlash(executable))
	expected := runtimeExecutablePath(root, executable, goos)
	if declared == expected {
		return ""
	}
	if info, err := os.Lstat(declared); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return fmt.Sprintf("; %s exists without the .exe suffix, which is not a Windows build of the runtime: "+
		"the extension must provide a windows/amd64 executable at %s", executable, pkgmeta.ExecutableName(goos, executable))
}

func validateRuntimeExecutable(executable string) error {
	info, err := os.Lstat(executable)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("executable is a symlink")
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode()&0o111 == 0) {
		return errors.New("path is not an executable regular file")
	}
	return nil
}

// describeRuntimeExecutable renders the on-disk identity of a runtime
// executable for handshake-failure diagnostics. A handshake subprocess that
// dies on a signal (SIGSEGV/SIGBUS) with empty stderr, or returns truncated
// output, is the signature of a file whose bytes moved under an in-flight exec
// — so the message must answer two questions without a re-run: WHICH tree the
// executable came from (an in-stage <store>/tmp/staging-* path vs. the
// published <store>/sha256/... entry) and whether the file was still intact.
// size/mode/mtime come from the portable FileInfo; the platform stat carries
// the inode and device, which identify the file even after a rename. Purely
// diagnostic: it never changes the failure verdict, and a stat that itself
// fails (the tree really did disappear) is reported rather than swallowed.
func describeRuntimeExecutable(executable string) string {
	info, err := os.Lstat(executable)
	if err != nil {
		return fmt.Sprintf("executable %s: stat failed: %v", executable, err)
	}
	identity := ""
	if sys := info.Sys(); sys != nil {
		identity = fmt.Sprintf(" stat=%+v", sys)
	}
	return fmt.Sprintf("executable %s: size=%d mode=%v mtime=%s%s",
		executable, info.Size(), info.Mode(),
		info.ModTime().UTC().Format(time.RFC3339Nano), identity)
}

// runtimeHandshakeDeadline is how long a runtime the operating system has
// already started may take to answer `__putnami runtime-info`.
//
// The value is settled. Raising it would only move the load at which a
// healthy runtime is refused — the same rule applied to the capability probes.
// What keeps load out of the verdict is WHEN this clock starts: see
// validateRuntimeHandshake.
const runtimeHandshakeDeadline = 10 * time.Second

// runtimeHandshakeClock arms the deadline of one handshake. It is called exactly
// once, after exec.Cmd.Start has returned, so process is the started runtime. It
// returns the channel that delivers when the runtime's time to answer is over,
// and a function that releases whatever backs that channel.
//
// Production arms a wall-clock timer (wallClockHandshakeDeadline). A test
// substitutes a channel it controls, so a timeout classification is asserted
// without a budget that a loaded host could miss.
type runtimeHandshakeClock func(process *os.Process) (expired <-chan time.Time, release func())

func wallClockHandshakeDeadline(*os.Process) (<-chan time.Time, func()) {
	timer := time.NewTimer(runtimeHandshakeDeadline)
	return timer.C, func() { timer.Stop() }
}

// validateRuntimeHandshake proves the resolved executable really is the runtime
// the manifest describes, before anything else is allowed to invoke it.
//
// spans, when non-nil, charges the whole handshake to the VERIFICATION phase of
// the preparation attribution, with the subprocess's measured child CPU read
// from the same ProcessState the physical execution ledger reads. A process
// that never started reports no CPU rather than a measured zero.
//
// The deadline counts from the moment the runtime process EXISTS, not from the
// spawn request. exec.Cmd.Start returns only once the operating system
// has admitted the executable. On darwin, admission includes the code-signature
// and policy assessment of a binary written moments earlier. That assessment
// queues behind every other fresh binary on a loaded machine and consumes none
// of the runtime's own time. Start cannot be interrupted, so a deadline that
// already counted admission could not shorten it: it could only kill the runtime
// the instant it started, refusing it for the machine's work instead of its
// own. A deadline that does fire is reported as runtime.handshake_timeout, with
// the measured start, run, and CPU times, so no reader mistakes load for a
// malformed build.
func validateRuntimeHandshake(
	ctx context.Context,
	executable string,
	ext *extension.ExtensionDescription,
	spans *preparationSpans,
) error {
	return runRuntimeHandshake(ctx, executable, ext, spans, wallClockHandshakeDeadline)
}

// startRuntimeHandshake starts `executable __putnami runtime-info`, retrying
// ETXTBSY with the same bounded backoff as runRuntimePrepareCommand. The
// executable was written moments earlier, and a process forked by another
// goroutine in that window keeps the write descriptor until it execs: Linux
// refuses the exec for that instant.
func startRuntimeHandshake(ctx context.Context, executable string, stdout, stderr *bytes.Buffer) (*exec.Cmd, error) {
	const attempts = 6
	delay := 5 * time.Millisecond
	for attempt := range attempts {
		// executable is one of exactly three paths this package builds — the
		// installed tree's declared executable, the in-stage prepare output, or
		// the published store entry — each joined from a manifest-declared
		// relative path and each passed through validateRuntimeExecutable
		// (regular file, not a symlink, executable bit) IMMEDIATELY before the
		// handshake. Executing it is the handshake's entire purpose: it is how
		// the CLI refuses to invoke a binary that is not the runtime the
		// manifest describes.
		//
		// The project already excludes gosec's G204 for this ("CLI spawns build
		// tools from extension manifests — not user input"). G702 is its
		// interprocedural taint sibling. Suppressed HERE rather than added to the
		// project excludes, so the analyzer stays armed for every other exec in
		// the CLI.
		//nolint:gosec // G702: validated, manifest-declared runtime path; executing it is the point.
		cmd := exec.CommandContext(ctx, executable, "__putnami", "runtime-info")
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		err := cmd.Start()
		if err == nil || !isTextFileBusy(err) || attempt == attempts-1 {
			return cmd, err
		}
		select {
		case <-ctx.Done():
			return cmd, err
		case <-time.After(delay):
			delay *= 2
		}
	}
	panic("unreachable runtime handshake start retry loop")
}

func runRuntimeHandshake(
	ctx context.Context,
	executable string,
	ext *extension.ExtensionDescription,
	spans *preparationSpans,
	clock runtimeHandshakeClock,
) error {
	started := time.Now()
	var stdout, stderr bytes.Buffer
	cmd, err := startRuntimeHandshake(ctx, executable, &stdout, &stderr)
	defer func() { spans.add(PreparationVerification, time.Since(started), spawnCPU(cmd.ProcessState)) }()
	if err != nil {
		return runtimeFailure(extensionproto.FailureRuntimeHandshakeFailed,
			"extension %q runtime-info failed: %v: stderr=%q: %s",
			ext.Name, err, strings.TrimSpace(stderr.String()), describeRuntimeExecutable(executable))
	}
	running := time.Now()
	expired, release := clock(cmd.Process)
	defer release()
	// os.Process tolerates a Kill concurrent with Wait: exec.CommandContext
	// cancels a running command exactly this way. The buffered channel lets the
	// waiter finish even if nothing receives, although every path below does.
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	deadlineFired := false
	var runErr error
	select {
	case runErr = <-waited:
	case <-expired:
		deadlineFired = true
		// A Kill error means the runtime ended at the same instant. Wait reports
		// how it ended, and an answer that arrived in time is still honored below.
		_ = cmd.Process.Kill()
		runErr = <-waited
	}
	if runErr != nil {
		// A canceled caller context kills the process too. That is an
		// interruption, not a runtime that ran out of time.
		if deadlineFired && ctx.Err() == nil {
			return runtimeFailure(extensionproto.FailureRuntimeHandshakeTimeout,
				"extension %q runtime-info timed out: the runtime ran %s without answering and was stopped "+
					"(deadline %s, counted from process start; starting the process took %s and is not counted; "+
					"CPU used %s). A runtime starved of CPU or blocked is not a malformed build: rerun when the "+
					"machine is less loaded, and if it repeats run `%s __putnami runtime-info` by hand: stderr=%q: %s",
				ext.Name, time.Since(running).Round(time.Millisecond), runtimeHandshakeDeadline,
				running.Sub(started).Round(time.Millisecond), spawnCPU(cmd.ProcessState),
				executable, strings.TrimSpace(stderr.String()), describeRuntimeExecutable(executable))
		}
		return runtimeFailure(extensionproto.FailureRuntimeHandshakeFailed,
			"extension %q runtime-info failed: %v: stderr=%q: %s",
			ext.Name, runErr, strings.TrimSpace(stderr.String()), describeRuntimeExecutable(executable))
	}
	if stdout.Len() > 64*1024 {
		return runtimeFailure(extensionproto.FailureRuntimeHandshakeFailed,
			"extension %q runtime-info exceeded 64 KiB", ext.Name)
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	var info runtimeproto.Info
	if err := decoder.Decode(&info); err != nil {
		return runtimeFailure(extensionproto.FailureRuntimeHandshakeFailed,
			"extension %q returned malformed runtime-info: %v: %s",
			ext.Name, err, describeRuntimeExecutable(executable))
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return runtimeFailure(extensionproto.FailureRuntimeHandshakeFailed,
			"extension %q runtime-info contains trailing data", ext.Name)
	}
	wantPlatform := runtime.GOOS + "/" + runtime.GOARCH
	if info.Extension != ext.Name || info.Version != ext.Version ||
		info.Platform != wantPlatform ||
		info.CLIContract != protocolcli.CurrentContract ||
		info.RuntimeProtocol != runtimeproto.MaxKnownProtocolVersion ||
		info.RuntimeABI != runtimeproto.RuntimeABIVersion {
		return runtimeFailure(extensionproto.FailureRuntimeIdentityMismatch,
			"extension %q runtime-info mismatch: got %+v; want identity %q@%q, platform %q, CLI contract %d, runtime protocol %d, ABI %d",
			ext.Name, info, ext.Name, ext.Version, wantPlatform, protocolcli.CurrentContract,
			runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
	}
	return nil
}

// hashRuntimeReplacementTree fingerprints exactly the entries stageRuntimeSourceView
// stages: both read the tree through collectRuntimeTree, so the digest can never
// accept a tree the staging walk refuses, nor cover bytes the build never sees.
func hashRuntimeReplacementTree(h hash.Hash, label, root string) error {
	entries, err := collectRuntimeTree(root)
	if err != nil {
		return fmt.Errorf("walk runtime replacement %s: %w", label, err)
	}
	for _, entry := range entries {
		rel := entry.rel
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		hashField(h, label+"/"+rel+"/mode", []byte(fmt.Sprintf("%o", info.Mode()&(os.ModeType|0o111))))
		if entry.symlink {
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			hashField(h, label+"/"+rel, []byte(target))
			continue
		}
		if !info.Mode().IsRegular() {
			// The entry changed type under the walk. Refuse rather than fold a
			// mode-only field into a digest whose staging would refuse the tree.
			return fmt.Errorf("module replacement input %q is not a regular file", rel)
		}
		file, err := os.Open(full)
		if err != nil {
			return err
		}
		if err := hashStream(h, label+"/"+rel, info.Size(), file); err != nil {
			_ = file.Close()
			return fmt.Errorf("hash runtime replacement %s/%s: %w", label, rel, err)
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func hashField(h hash.Hash, label string, value []byte) {
	_ = hashStream(h, label, int64(len(value)), bytes.NewReader(value))
}

func hashStream(h hash.Hash, label string, size int64, reader io.Reader) error {
	if err := binary.Write(h, binary.LittleEndian, uint64(len(label))); err != nil {
		return err
	}
	if _, err := io.WriteString(h, label); err != nil {
		return err
	}
	if err := binary.Write(h, binary.LittleEndian, uint64(size)); err != nil {
		return err
	}
	_, err := io.Copy(h, reader)
	return err
}

// localDirectivePaths extracts local filesystem operands from simple Go
// replace directives without invoking the toolchain. Comments and both
// block/single-line forms are supported.
func localDirectivePaths(filename, directive string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var paths []string
	inBlock := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "//", 2)[0])
		if line == "" {
			continue
		}
		if inBlock {
			if line == ")" {
				inBlock = false
				continue
			}
			if local := directiveLocalOperand(line, directive); local != "" {
				paths = append(paths, local)
			}
			continue
		}
		prefix := directive + " "
		if line == directive+" (" {
			inBlock = true
			continue
		}
		if strings.HasPrefix(line, prefix) {
			if local := directiveLocalOperand(strings.TrimSpace(strings.TrimPrefix(line, prefix)), directive); local != "" {
				paths = append(paths, local)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return dedupeStrings(paths), nil
}

func directiveLocalOperand(line, directive string) string {
	fields := strings.Fields(line)
	if directive == "use" {
		if len(fields) > 0 && isLocalPath(fields[0]) {
			return fields[0]
		}
		return ""
	}
	for i, field := range fields {
		if field == "=>" && i+1 < len(fields) && isLocalPath(fields[i+1]) {
			return fields[i+1]
		}
	}
	return ""
}

func isLocalPath(value string) bool {
	return filepath.IsAbs(value) || value == "." || strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../")
}

func dedupeStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func setEnv(env []string, key, value string) []string {
	return envkeys.Host.Set(env, key, value)
}
