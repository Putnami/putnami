package jobs

import "time"

// PreparationPhase is one OWNERSHIP CLASS of preparation work — the vocabulary
// the stage's design requires it to be decomposed into. It is a
// CLOSED set: a step that fits none of these is a step whose cost nobody can
// act on, so the answer is to classify it, not to invent a sixth bucket.
type PreparationPhase string

const (
	// PreparationNetwork is time spent talking to a remote: registry metadata,
	// artifact downloads, integrity fetches.
	//
	// The runtime-synchronization stage instrumented here performs NO network
	// I/O of its own — installed extension binaries are materialized by the
	// lock-driven ensure pass at CLI bootstrap, and a prepare command's own
	// module downloads happen inside its subprocess and are therefore counted
	// under PreparationGeneration, where the measurement can actually see them.
	// The class exists in the vocabulary so the fetch half can be routed here
	// later without a schema change; a phase nothing entered is simply absent
	// from the report rather than published as a measured zero.
	PreparationNetwork PreparationPhase = "network"
	// PreparationResolution is deciding WHAT is needed and under which content
	// identity: enumerating declared runtime inputs, walking the local module
	// replacement closure, and hashing both into the runtime digest.
	PreparationResolution PreparationPhase = "resolution"
	// PreparationVerification is proving that what is on disk is what was
	// promised: the executable is a real non-symlink executable file, the inputs
	// did not move under the preparation, and the runtime answers the identity
	// and ABI handshake.
	PreparationVerification PreparationPhase = "verification"
	// PreparationGeneration is running a declared prepare command — the compile
	// that turns extension source into a runtime executable. It is the only
	// phase that runs third-party code, and the only one whose cost the CLI can
	// reduce solely by not paying it twice.
	PreparationGeneration PreparationPhase = "generation"
	// PreparationMutation is the exclusive, ordered part: creating the isolated
	// staging tree, copying inputs into it, waiting for the content-addressed
	// store's per-digest ownership lock, publishing by atomic rename, and
	// reclaiming the staging tree afterwards. Everything that makes a shared
	// directory different from how it was found is here, which is what makes
	// "mutation steps stay exclusive" checkable rather than assumed.
	PreparationMutation PreparationPhase = "mutation"
)

// PreparationPhaseTotals is one ownership class's contribution to the stage.
type PreparationPhaseTotals struct {
	// Phase is the ownership class.
	Phase PreparationPhase
	// Wall is the SUM of this phase's spans. Under parallelism spans overlap, so
	// this can exceed the stage's own wall — deliberately: it is the work the
	// phase represents, and comparing it against PreparationSummary.Wall is how
	// a reader sees how much of it the parallelism actually overlapped.
	Wall time.Duration
	// CPU is measured CHILD CPU (user+system) for the spans of this phase that
	// spawned a subprocess, and zero for the phases that did not. It is never
	// derived from Wall: an in-process phase reports no CPU rather than a
	// fabricated one.
	CPU time.Duration
	// Steps is how many spans were summed, so a mean is available without
	// publishing one.
	Steps int
}

// PreparationSummary is the whole stage, decomposed. It is the value the
// scheduler attaches to the run and the session file publishes.
type PreparationSummary struct {
	// Wall is the stage's OWN wall: the sum of the top-level synchronization
	// calls, which run sequentially within a run. It is the number the critical
	// path actually pays, and the denominator every phase is read against.
	Wall time.Duration
	// Parallelism is the widest independent-step fan-out the stage was permitted
	// in this run. 1 means the phase walls cannot overlap and therefore sum to at
	// most Wall; above 1 they may overlap and a reader must not add them up as
	// elapsed time.
	Parallelism int
	// Phases are the classes that were ENTERED, in preparationPhaseOrder. A
	// class the run never entered is absent, not zero.
	Phases []PreparationPhaseTotals
}
