// RECOVERABILITY, and with it the producer's whole half of the lifecycle: the
// invocation's private scratch, the lease that outlives the process, and the
// verification that decides whether the consumer frontier may run.
//
// The invariant this file owns is that nothing durable exists without a record
// naming it, and nothing is handed to a consumer that the producer did not
// actually deliver. Those are the same statement read from opposite ends:
//
//   - A SIGKILL cannot run a finalizer, so the only thing that can attribute an
//     orphaned resource afterwards is the lease that arming wrote BEFORE
//     provisioning. Reaping therefore happens on the next invocation's arm, not
//     on a timer — that is what makes "the next run after a kill recovers the
//     orphan" true rather than eventual.
//   - A declared artifact that was reserved and never written sits on disk
//     indistinguishable from one the producer created and left empty. For a
//     credential both mean the same thing, so the frontier is blocked with
//     `sensitive.artifact_missing`, and a producer that failed before its
//     subprocess ever existed blocks it with `sensitive.setup_failed`, rather
//     than letting a consumer run against a path to nothing.
//
// The leak guard is deliberately NOT here. This file decides what EXISTS;
// invocation_needles.go decides what those bytes mean for confinement and is
// entered from exactly one place in this file — provisioned's deferred
// loadNeedles, which runs on every terminal path — so the order "verify, then
// derive, on success and on failure alike" cannot be rearranged by accident.
package jobs

import (
	"fmt"
	"os"
	"sort"
	"sync"

	extensionproto "go.putnami.dev/protocol/extension"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// arm creates the invocation's private scratch, publishes its lease, reaps any
// orphan a previous kill left behind, and hands the non-secret locator to the
// producer, the listed consumers and the finalizer.
//
// It is called from the producer's prepareTask, which is the moment "the producer
// started" — the contract's trigger. Stamping the locator onto the consumer and
// finalizer plan nodes here is safe without further synchronization: every
// consumer is transitively downstream of the producer (the manifest check
// enforces it), and the finalizer is dispatched by the coordinator, so both
// reads happen after the producer's completion travels a channel.
//
// actionDigest is the producing action's cache identity. It is recorded in the
// lease so a reader can attribute an orphaned resource to the action that made
// it without reading the resource.
func (rt *invocationRuntime) arm(job *ScheduledJob, actionDigest string) (reaped []store.InvocationLease, err error) {
	if rt == nil {
		return nil, nil
	}
	rel := rt.byProducer[job.Key()]
	if rel == nil {
		return nil, nil
	}
	rel.mu.Lock()
	defer rel.mu.Unlock()
	if rel.armed {
		return nil, nil
	}

	root := ""
	if rt.ws != nil {
		root = rt.ws.Root
	}
	// Reaping happens BEFORE provisioning, not on a timer. That is what makes
	// "the next invocation after a kill recovers the orphan" true.
	reaped = store.ReapOrphanInvocations(root)

	scratch, err := store.NewInvocationScratch(root, rel.provider, actionDigest)
	if err != nil {
		// Arming is the last step before the producer's subprocess exists, so a
		// failure here is a setup failure the frontier must be told about: the
		// producer's own lifecycle never reaches closeTask, which is where a
		// producer that RAN records its verdict.
		rel.setupFailed = true
		return reaped, err
	}
	rel.scratch = scratch
	rel.armed = true

	locator := &protocoljob.Invocation{ID: scratch.ID(), ArtifactRoot: scratch.ArtifactRoot()}
	rel.producer.Invocation = locator
	rel.finalizer.Invocation = locator
	for _, consumer := range rel.consumers {
		consumer.Invocation = locator
	}

	// Reserve each declared sensitive artifact at mode 0600 before the producer
	// can create it at the process umask.
	for _, output := range sortedInvocationOutputs(rel.producer) {
		if output.PathFrom != "" {
			continue
		}
		if _, err := scratch.Prepare(output.Path, output.Sensitive); err != nil {
			rel.setupFailed = true
			return reaped, err
		}
	}
	return reaped, nil
}

// provisioned closes the producer's side of the lifecycle: it verifies that
// every required invocation-scoped artifact exists, re-asserts owner-only
// permissions on the sensitive ones, and loads the needles the leak guard
// matches against for the rest of the run.
//
// A missing required artifact is `sensitive.artifact_missing`: the task and its
// declaration disagree, and letting the consumers run against a path to nothing
// is exactly the failure the declaration exists to prevent.
//
// The needles are loaded on EVERY terminal path, verdict included. A producer
// that wrote its credential, echoed it, and then died is the shape the guard
// exists for; deriving needles only from a successful producer would leave that
// run's own output — and the held events released right after this — matched
// against an empty set.
func (rt *invocationRuntime) provisioned(job *ScheduledJob, result *JobResult) *JobError {
	if rt == nil {
		return nil
	}
	rel := rt.byProducer[job.Key()]
	if rel == nil {
		return nil
	}
	rel.mu.Lock()
	defer rel.mu.Unlock()
	if rel.scratch == nil {
		return nil
	}
	// LIFO: this runs before the unlock above, and after every return below.
	defer rel.loadNeedles()
	if result == nil || result.Status != "success" {
		rel.setupFailed = true
		return nil
	}

	for _, output := range sortedInvocationOutputs(rel.producer) {
		if output.PathFrom != "" {
			continue
		}
		if err := verifyProducedArtifact(rel.scratch, output); err != nil {
			if output.OptionalEmpty {
				continue
			}
			rel.setupFailed = true
			return &JobError{
				Code:    extensionproto.FailureSensitiveArtifactMissing,
				Message: err.Error(),
			}
		}
		if !output.Sensitive {
			continue
		}
		if err := rel.scratch.Harden(output.Path); err != nil {
			rel.setupFailed = true
			return &JobError{
				Code:    extensionproto.FailureSensitiveArtifactMissing,
				Message: err.Error(),
			}
		}
	}
	return nil
}

// observe records a participant's terminal result so blocked consumers can be
// told WHY. A producer that ended in anything other than success blocks its
// frontier with `sensitive.setup_failed`.
//
// It is called on EVERY terminal path a producer can take, and that breadth is
// the point. provisioned records the verdict of a producer that RAN, but a
// producer can fail before its subprocess exists — a preBuild hook that fails,
// a command-output directory that cannot be prepared, a scratch that cannot be
// created — and those failures never reach closeTask. Without this call on the
// ordinary completion path, `--continue-on-error` read a frontier whose setup
// never happened as an unrelated failure and let the consumers run against a
// resource that was never provisioned.
//
// A PRUNED relation is exempt: its producer is "skipped" precisely because
// every consumer was already served from cache, and marking that as a setup
// failure would block the consumers the pruning exists to serve.
func (rt *invocationRuntime) observe(job *ScheduledJob, result *JobResult) {
	if rt == nil {
		return
	}
	rel := rt.byProducer[job.Key()]
	if rel == nil || result == nil || result.Status == "success" {
		return
	}
	rel.mu.Lock()
	if !rel.pruned {
		rel.setupFailed = true
	}
	rel.mu.Unlock()
}

// blockedError is the typed causal diagnostic for a consumer the scheduler is
// about to skip because its setup failed. It returns nil for every other skip,
// which keeps the generic "dependency failed" message for ordinary DAG skips.
func (rt *invocationRuntime) blockedError(job *ScheduledJob) *JobError {
	if rt == nil {
		return nil
	}
	rel := rt.byConsumer[job.Key()]
	if rel == nil {
		return nil
	}
	rel.mu.Lock()
	defer rel.mu.Unlock()
	if !rel.setupFailed {
		return nil
	}
	message := fmt.Sprintf(
		"blocked: %s did not provision its invocation-scoped resource", rel.producer.Key())
	if rel.conflict != "" {
		message = "blocked: " + rel.conflict
	}
	return &JobError{Code: extensionproto.FailureSensitiveSetupFailed, Message: message}
}

// discard destroys the private scratch. It is idempotent and is called after the
// finalizer ran (or after it was established that none can), so a killed
// provider's own resources are the only thing left for a later reap.
func (rel *invocationRelation) discard() {
	rel.discardProducerEvents()
	rel.mu.Lock()
	scratch := rel.scratch
	rel.scratch = nil
	rel.mu.Unlock()
	_ = scratch.Discard()
}

// leases returns every live relation's lease, in creation order. Exposed for the
// session's recovery reporting and for tests; it carries no secret by
// construction (store.InvocationLease has no free-form member).
func (rt *invocationRuntime) leases() []store.InvocationLease {
	if rt == nil {
		return nil
	}
	var out []store.InvocationLease
	for _, rel := range rt.relations {
		rel.mu.Lock()
		if rel.scratch != nil {
			out = append(out, rel.scratch.Lease())
		}
		rel.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// verifyProducedArtifact reports whether the producer actually WROTE a declared
// invocation-scoped output.
//
// Existence alone is not enough for a sensitive artifact, because arming
// RESERVED it at mode 0600 before the producer ran — a reservation the producer
// never filled is on disk, empty, and indistinguishable from a file it created
// and left empty. Both mean the same thing for a credential: the consumers have
// nothing to read. A declaration that legitimately produces nothing says so with
// optionalEmpty, which the caller honors.
func verifyProducedArtifact(scratch *store.InvocationScratch, output extension.DeclaredOutput) error {
	if err := scratch.Verify(output.Path); err != nil {
		return err
	}
	if !output.Sensitive {
		return nil
	}
	path, err := scratch.Resolve(output.Path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() && info.Size() == 0 {
		return fmt.Errorf("%w: %s was reserved but never written",
			store.ErrInvocationArtifactMissing, output.Path)
	}
	return nil
}

// sortedInvocationOutputs returns the job's invocation-scoped declared outputs
// in canonical id order, so preparation, verification and hardening are
// order-independent.
func sortedInvocationOutputs(job *ScheduledJob) []extension.DeclaredOutput {
	declaration := taskDeclarationOf(job)
	if declaration == nil {
		return nil
	}
	ids := make([]string, 0, len(declaration.Outputs))
	for id, output := range declaration.Outputs {
		if output.IsInvocationScoped() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]extension.DeclaredOutput, 0, len(ids))
	for _, id := range ids {
		out = append(out, declaration.Outputs[id])
	}
	return out
}

// armInvocation creates the private scratch and publishes the lease for a
// producer of an invocation-scoped resource, and reports the orphans it reaped
// on the way.
//
// The lease records the PRODUCING ACTION's digest — the producer's own cache
// identity, derived from its declared inputs. It is computed here rather than
// read off the task lifecycle because a producer is uncacheable by contract
// (a task with an invocation-scoped output must set cache:false), so no cache
// hash was computed for it. A digest is not a secret: it is a function of the
// inputs, never of anything the producer wrote.
func (s *Scheduler) armInvocation(job *ScheduledJob, mu *sync.Mutex, hashes map[string]string) error {
	if s.invocations == nil || s.invocations.byProducer[job.Key()] == nil {
		return nil
	}
	reaped, err := s.invocations.arm(job, s.actionDigest(job, mu, hashes))
	for _, lease := range reaped {
		s.emitLeaseReaped(lease)
	}
	if err != nil {
		return fmt.Errorf("provision invocation scratch: %w", err)
	}
	return nil
}

// actionDigest is the job's input-derived cache identity, computed whether or
// not the job is cacheable. It returns "" when the digest cannot be computed;
// the lease then simply records no action, which costs a reader attribution and
// never blocks recovery.
func (s *Scheduler) actionDigest(job *ScheduledJob, mu *sync.Mutex, hashes map[string]string) string {
	if s.cache == nil {
		return ""
	}
	mu.Lock()
	hashCopy := make(map[string]string, len(job.DependsOn))
	for _, depKey := range job.DependsOn {
		if value, ok := hashes[depKey]; ok {
			hashCopy[depKey] = value
		}
	}
	mu.Unlock()
	digest, err := computeJobCacheHash(s.ws, job, s.commandParams, s.cfg.VersionInfo, s.cache, hashCopy)
	if err != nil {
		return ""
	}
	return digest
}

// emitLeaseReaped records the successful recovery of an orphaned invocation as
// a session event. It is a RECOVERY signal, not a failure: a kill left a
// resource behind and this invocation removed it before provisioning.
//
// Every member of the payload comes from the lease, which has no free-form
// member, so this surface cannot carry a credential.
func (s *Scheduler) emitLeaseReaped(lease store.InvocationLease) {
	if s.onSessionEvent == nil {
		return
	}
	s.onSessionEvent(SessionRecord{Type: "invocation:reaped", Data: map[string]any{
		"code":         extensionproto.RecoverySensitiveLeaseReaped,
		"invocationId": lease.ID,
		"pid":          lease.PID,
		"provider":     lease.Provider,
		"actionDigest": lease.ActionDigest,
		"createdAt":    lease.CreatedAt,
	}})
}
