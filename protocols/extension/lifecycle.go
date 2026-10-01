// Typed failure codes for the three lifecycle primitives (runtime.go,
// workspace.go, and invocation-scoped sensitive outputs).
//
// The codes in this file are RUNTIME failures — what the CLI reports when
// preparing a runtime, probing a workspace, or provisioning an
// invocation-scoped resource goes wrong. They are deliberately separate from
// the validation codes the strict path emits ("required-field",
// "invalid-enum", …): a validation code says a manifest is wrong, these say a
// correct manifest could not be executed right now.
//
// They are declared HERE, in the protocol, rather than in the CLI package that
// will emit them, for the same reason the shapes are: the three lifecycles have
// consumers in three languages, and a failure an SDK cannot name is a failure
// an SDK cannot handle. The vocabularies are closed, so a consumer can switch
// on them exhaustively, and IsLifecycleFailureCode is what a conformance suite
// asserts against.
//
// Two rules apply to every code:
//
//   - ACTIONABLE. A code exists only if a caller can do something different
//     when it sees it. "Something failed" is not a code; "the prepare command
//     exited non-zero" and "the prepared tree has no executable at the declared
//     path" are, because the first is the extension author's bug and the second
//     is a packaging bug.
//   - NEVER SILENTLY RECOVERED. Preparation failure in particular must not fall
//     back to `go run`, a shell wrapper, or name-based classification: the
//     fallback is what the runtime primitive exists to delete, and a fallback
//     that fires only on failure is the hardest kind to notice.
//
// doc/07-lifecycles.md carries the same table with each code's meaning and
// remedy, and a drift test pins the two together.

package extension

import "sort"

// LifecyclePrimitiveID identifies one of the three generic lifecycle surfaces
// core may consume from an extension. The vocabulary is deliberately closed:
// provider behavior that does not fit one of these surfaces remains an
// ordinary typed task rather than growing a fourth core lifecycle.
type LifecyclePrimitiveID string

const (
	// LifecyclePrimitiveInvocation covers invocation-scoped outputs and the
	// runOn/finalizes vocabulary that guarantees their cleanup.
	LifecyclePrimitiveInvocation LifecyclePrimitiveID = "invocation"
	// LifecyclePrimitiveRuntime covers extension runtime preparation.
	LifecyclePrimitiveRuntime LifecyclePrimitiveID = "runtime"
	// LifecyclePrimitiveWorkspace covers workspace probing and synchronization.
	LifecyclePrimitiveWorkspace LifecyclePrimitiveID = "workspace"
)

// ValidLifecyclePrimitives is the closed lifecycle-primitive registry in
// canonical (sorted) order. ManifestLifecyclePrimitives iterates this registry
// directly, so it is part of real protocol classification rather than a
// parallel documentation inventory.
var ValidLifecyclePrimitives = []LifecyclePrimitiveID{
	LifecyclePrimitiveInvocation,
	LifecyclePrimitiveRuntime,
	LifecyclePrimitiveWorkspace,
}

// ManifestLifecyclePrimitives returns the lifecycle primitives whose protocol
// surfaces the manifest uses, in registry order.
//
// Explicit runOn vocabulary counts even when its value is success (or invalid):
// the field is v3 vocabulary and strict validation reports invalid values
// separately. Likewise, a finalizes relation is classified before validation
// establishes that it belongs to a runOn: finally step.
func ManifestLifecyclePrimitives(m *Manifest) []LifecyclePrimitiveID {
	if m == nil {
		return nil
	}

	declared := map[LifecyclePrimitiveID]bool{
		LifecyclePrimitiveRuntime:   m.DeclaresRuntime(),
		LifecyclePrimitiveWorkspace: m.DeclaresWorkspaceAdapter(),
	}
	for _, task := range m.Tasks {
		if task.Declares == nil {
			continue
		}
		for _, output := range task.Declares.Outputs {
			if output.IsInvocationScoped() {
				declared[LifecyclePrimitiveInvocation] = true
				break
			}
		}
	}
	if !declared[LifecyclePrimitiveInvocation] {
		for _, command := range m.Commands {
			for _, step := range command.Run {
				if step.RunOn != "" || step.Finalizes != nil {
					declared[LifecyclePrimitiveInvocation] = true
					break
				}
			}
			if declared[LifecyclePrimitiveInvocation] {
				break
			}
		}
	}

	primitives := make([]LifecyclePrimitiveID, 0, len(ValidLifecyclePrimitives))
	for _, primitive := range ValidLifecyclePrimitives {
		if declared[primitive] {
			primitives = append(primitives, primitive)
		}
	}
	return primitives
}

// Runtime-preparation failures. The lifecycle is
// absent → pending → preparing → ready | failed; every terminal failure carries
// one of these codes.
const (
	// FailureRuntimeNotDeclared: a task referenced {extensionRuntime} but the
	// extension's manifest declares no `runtime` section. Remedy: declare
	// runtime.executable, or name the command explicitly in the task.
	FailureRuntimeNotDeclared = "runtime.not_declared"
	// FailureRuntimeExecutableMissing: the extension's installed tree (or its
	// platform archive) has no file at runtime.executable. Remedy: a packaging
	// fix in the extension — the archive did not ship the declared executable
	// for this platform.
	FailureRuntimeExecutableMissing = "runtime.executable_missing"
	// FailureRuntimePrepareFailed: the prepare command exited non-zero. The
	// command's own diagnostics are the cause; the CLI adds no interpretation
	// and never retries with a different toolchain.
	FailureRuntimePrepareFailed = "runtime.prepare_failed"
	// FailureRuntimePrepareOutputMissing: the prepare command succeeded but
	// wrote no executable at {runtimeOutput}/<executable>. Remedy: the prepare
	// and the declared executable path disagree.
	FailureRuntimePrepareOutputMissing = "runtime.prepare_output_missing"
	// FailureRuntimeHandshakeFailed: the executable exists and runs, but
	// `__putnami runtime-info` failed or returned a document that is not a
	// runtime descriptor. Remedy: rebuild the extension against a current SDK.
	FailureRuntimeHandshakeFailed = "runtime.handshake_failed"
	// FailureRuntimeHandshakeTimeout: the executable started but had not
	// answered `__putnami runtime-info` when the handshake deadline elapsed.
	// The deadline counts from the moment the operating system started the
	// process, so the time spent admitting a freshly written binary is not
	// charged to it. A runtime that never answered proves nothing about its
	// identity and is refused like any other handshake failure, but the cause is
	// a machine too loaded to schedule it or a runtime that blocks, not a
	// malformed build. Remedy: rerun on a less loaded machine; if it repeats,
	// run the executable's `__putnami runtime-info` by hand to see it block.
	FailureRuntimeHandshakeTimeout = "runtime.handshake_timeout"
	// FailureRuntimeIdentityMismatch: the handshake succeeded and disagreed
	// with the manifest — a different extension identity, platform, CLI
	// contract, or runtime protocol. Remedy: the resolved binary is not the one
	// the manifest describes; never execute it anyway.
	FailureRuntimeIdentityMismatch = "runtime.identity_mismatch"
)

// Workspace-probe failures. The lifecycle is
// unknown → probing → indexed | stale | failed; a graph-dependent command fails
// on any of these, while the recovery commands (install, extensions, projects
// sync, help, version) stay available.
const (
	// FailureWorkspaceProbeFailed: the extension's probe process exited
	// non-zero or produced no result document.
	FailureWorkspaceProbeFailed = "workspace.probe_failed"
	// FailureWorkspaceProbeInvalidResult: the probe returned a document that
	// does not conform to the probe-result contract. Remedy: an extension bug;
	// core must not guess at a partially readable result.
	FailureWorkspaceProbeInvalidResult = "workspace.probe_invalid_result"
	// FailureWorkspaceProbeConflict: two non-empty scalar contributions for one
	// project disagree and no explicit putnami.json value resolves them.
	// Remedy: state the value explicitly in the project's putnami.json.
	FailureWorkspaceProbeConflict = "workspace.probe_conflict"
	// FailureWorkspaceSnapshotInvalid: the stored workspace index is unreadable
	// or was written by an incompatible snapshot format. Remedy: `putnami
	// projects sync` rebuilds it; the failure is never repaired by guessing.
	FailureWorkspaceSnapshotInvalid = "workspace.snapshot_invalid"
)

// Invocation-scoped sensitive-artifact failures. The lifecycle is
// declared → provisioned → consumed → finalized. Successful orphan reaping is
// a recovery signal below, not a failure.
const (
	// FailureSensitiveSetupFailed: the task that provisions an
	// invocation-scoped resource failed. Consumers are blocked with this code
	// as their causal diagnostic instead of running against a resource that
	// does not exist.
	FailureSensitiveSetupFailed = "sensitive.setup_failed"
	// FailureSensitiveArtifactMissing: setup succeeded but a declared
	// invocation-scoped output is absent from the invocation scratch. Remedy:
	// the task and its declaration disagree.
	FailureSensitiveArtifactMissing = "sensitive.artifact_missing"
	// FailureSensitiveFinalizerFailed: a `runOn: finally` step failed. The
	// invocation's outcome is unchanged — a finalizer cannot rescue or condemn
	// the work it tears down — but the leak is reported, never swallowed.
	FailureSensitiveFinalizerFailed = "sensitive.finalizer_failed"
	// FailureSensitiveLeakDetected: a sensitive path or its bytes reached a
	// surface that must never carry them — an event, a result, a session
	// record, telemetry, or cache traffic. It is a fail-closed guard: the
	// invocation fails rather than publishing the value.
	FailureSensitiveLeakDetected = "sensitive.leak_detected"
)

// ValidRuntimeFailureCodes is the closed runtime-preparation failure
// vocabulary in canonical (sorted) order.
var ValidRuntimeFailureCodes = []string{
	FailureRuntimeExecutableMissing,
	FailureRuntimeHandshakeFailed,
	FailureRuntimeHandshakeTimeout,
	FailureRuntimeIdentityMismatch,
	FailureRuntimeNotDeclared,
	FailureRuntimePrepareFailed,
	FailureRuntimePrepareOutputMissing,
}

// ValidWorkspaceFailureCodes is the closed workspace-probe failure vocabulary
// in canonical (sorted) order.
var ValidWorkspaceFailureCodes = []string{
	FailureWorkspaceProbeConflict,
	FailureWorkspaceProbeFailed,
	FailureWorkspaceProbeInvalidResult,
	FailureWorkspaceSnapshotInvalid,
}

// ValidSensitiveFailureCodes is the closed sensitive-artifact failure
// vocabulary in canonical (sorted) order.
var ValidSensitiveFailureCodes = []string{
	FailureSensitiveArtifactMissing,
	FailureSensitiveFinalizerFailed,
	FailureSensitiveLeakDetected,
	FailureSensitiveSetupFailed,
}

// Lifecycle recovery signals describe successful repair of stale lifecycle
// state. They are kept separate from failures so a completed recovery cannot
// both fail and succeed the same invocation.
const (
	// RecoverySensitiveLeaseReaped means a later invocation found an orphaned
	// lease and successfully removed the resource before provisioning or reuse.
	RecoverySensitiveLeaseReaped = "sensitive.lease_reaped"
)

// ValidLifecycleRecoveryCodes is the closed successful-recovery vocabulary.
var ValidLifecycleRecoveryCodes = []string{
	RecoverySensitiveLeaseReaped,
}

// ValidLifecycleFailureCodes is every lifecycle failure code, in canonical
// (sorted) order. Three primitives, one vocabulary: the hard ratchet of
// contract v3 is that there is no fourth, so a code outside these namespaces is a
// design question and not a spelling one.
var ValidLifecycleFailureCodes = sortedUnion(
	ValidRuntimeFailureCodes,
	ValidWorkspaceFailureCodes,
	ValidSensitiveFailureCodes,
)

// IsLifecycleFailureCode reports whether code belongs to the closed lifecycle
// failure vocabulary.
func IsLifecycleFailureCode(code string) bool {
	for _, candidate := range ValidLifecycleFailureCodes {
		if candidate == code {
			return true
		}
	}
	return false
}

// IsLifecycleRecoveryCode reports whether code belongs to the closed
// successful lifecycle-recovery vocabulary.
func IsLifecycleRecoveryCode(code string) bool {
	for _, candidate := range ValidLifecycleRecoveryCodes {
		if candidate == code {
			return true
		}
	}
	return false
}

func sortedUnion(sets ...[]string) []string {
	total := 0
	for _, set := range sets {
		total += len(set)
	}
	out := make([]string, 0, total)
	for _, set := range sets {
		out = append(out, set...)
	}
	sort.Strings(out)
	return out
}
