package workspace

import (
	"fmt"
	"time"
)

// The local model of `cli.workspace-probe-view.v1`, the DARC projection the CLI
// declares in `tooling/cli/putnami.architecture.json`.
//
// `.putnami/workspace-index.json` is a PROJECTION: the provider extensions own
// project identity and dependency edges, and this file is the CLI's local copy
// of the answers they gave. Every read-only surface — `projects list`, `deps`,
// the MCP graph tools — answers from that copy without paying for a probe, and
// that fast path is deliberate.
//
// What was missing was the copy's honesty. A projection has to say
// how old it is and what happens when it is absent, and this one said neither:
// a workspace that had never been probed answered with an empty-but-well-formed
// graph, which reads as "there is nothing here" rather than "nobody has told me
// yet". The contract stayed `planned` for exactly that reason.
//
// The three members below are the local-model fields the contract declares —
// provenance, observation time, freshness — and this file is where they stop
// being sentences in a manifest. `darc_conformance_test.go` joins each constant
// to the declaration, so a rename on either side fails loudly.

// RecordedFreshness is how a read describes the recorded index it answered from.
type RecordedFreshness string

const (
	// RecordedAbsent means no usable copy exists. Under the contract's
	// `onMissing: fail-closed`, a surface whose answer is a projected fact must
	// refuse rather than answer from what it happens to have.
	RecordedAbsent RecordedFreshness = "absent"
	// RecordedFresh means the copy is inside the declared staleness bound.
	RecordedFresh RecordedFreshness = "fresh"
	// RecordedStale means it is outside the bound. The contract declares
	// `onStale: use-stale`, so the copy is still answered from — and says so.
	RecordedStale RecordedFreshness = "stale"
)

// RecordedMaxStaleness is the declared freshness bound of the projection.
//
// It is a day because the index is content-validated on every load that plans
// or executes: a tree that changed re-probes regardless of age, so the bound is
// not what keeps the copy correct. What it bounds is a workspace nobody has
// touched — an answer served from a week-old probe of extensions that have since
// been upgraded — which is exactly the case content validation cannot see.
const RecordedMaxStaleness = 24 * time.Hour

// The local-model field names the projection declares. They are the manifest's
// spelling, in the protocol's snake_case, and the conformance test holds the
// declaration to them.
const (
	// RecordedProvenanceField records which producer state this copy came from.
	RecordedProvenanceField = "probe_digest"
	// RecordedObservedAtField records when the producer state was observed.
	RecordedObservedAtField = "observed_at"
	// RecordedFreshnessField carries the verdict above.
	RecordedFreshnessField = "freshness_state"
)

// RecordedWriter is the sole component the local model allows to update the
// copy. Every write goes through `Synchronize`/`RefreshSnapshot` in this
// package; nothing else writes `.putnami/workspace-index.json`.
const RecordedWriter = "cli.workspace-loader"

// RecordedView is one read of the recorded index: the copy's provenance, when
// it was observed, and the freshness verdict against the declared bound.
//
// It is a value, not a handle on the file: taking it costs one bounded read and
// no probe, which is what lets a read-only command report the honesty of its own
// answer without changing what that answer costs.
type RecordedView struct {
	// Freshness is the verdict, and the field the contract calls
	// `freshness_state`.
	Freshness RecordedFreshness `json:"freshnessState"`
	// ProbeDigest is the producer state this copy carries — `probe_digest`.
	ProbeDigest string `json:"probeDigest,omitempty"`
	// ObservedAt is when that state was observed, RFC 3339 — `observed_at`.
	ObservedAt string `json:"observedAt,omitempty"`
	// AgeSeconds is how old the copy is at the moment of the read. It is
	// derived, not stored: a persisted age would be wrong the instant it was
	// written.
	AgeSeconds int64 `json:"ageSeconds,omitempty"`
	// Message states what the verdict means for the answer beside it, and — when
	// the copy is absent or stale — the command that rebuilds it.
	Message string `json:"message"`
}

// Usable reports whether an answer derived from projected facts may be
// published. It is false only for an absent copy: the contract declares
// `onStale: use-stale`, so an old copy is answered from and marked, while a
// missing one fails closed.
func (v RecordedView) Usable() bool { return v.Freshness != RecordedAbsent }

// RecordedIndexView reads the recorded index and reports its freshness at now.
//
// A missing, unreadable, or format-superseded index is `absent` — all three mean
// the same thing to a reader, which is that there is no copy to answer from. An
// index with no recorded observation time is also absent: it was written before
// the projection carried one, and guessing an age for it would be inventing the
// fact the contract requires it to state.
func RecordedIndexView(root string, now time.Time) RecordedView {
	snapshot, err := LoadSnapshot(root)
	if err != nil || snapshot == nil {
		return RecordedView{
			Freshness: RecordedAbsent,
			Message: fmt.Sprintf(
				"no recorded workspace view (%s): project identity and dependency edges come from the "+
					"provider extensions and none have been recorded — run `putnami projects sync` to build it",
				WorkspaceIndexFilename),
		}
	}
	if snapshot.Version != snapshotFormatVersion || snapshot.ObservedAt == "" {
		return RecordedView{
			Freshness:   RecordedAbsent,
			ProbeDigest: snapshot.ProbeDigest,
			Message: fmt.Sprintf(
				"the recorded workspace view (%s) predates the current index format and states no observation time — "+
					"run `putnami projects sync` to rebuild it",
				WorkspaceIndexFilename),
		}
	}
	observed, parseErr := time.Parse(time.RFC3339Nano, snapshot.ObservedAt)
	if parseErr != nil {
		return RecordedView{
			Freshness:   RecordedAbsent,
			ProbeDigest: snapshot.ProbeDigest,
			ObservedAt:  snapshot.ObservedAt,
			Message: fmt.Sprintf(
				"the recorded workspace view (%s) carries an unreadable observation time %q — "+
					"run `putnami projects sync` to rebuild it",
				WorkspaceIndexFilename, snapshot.ObservedAt),
		}
	}

	age := now.Sub(observed)
	if age < 0 {
		// A copy observed in the future is a clock that moved, not a fresh copy.
		// Reporting it as fresh would let a skewed builder serve an arbitrarily
		// old answer, so it is treated as exactly at the bound.
		age = 0
	}
	view := RecordedView{
		ProbeDigest: snapshot.ProbeDigest,
		ObservedAt:  observed.UTC().Format(time.RFC3339Nano),
		AgeSeconds:  int64(age / time.Second),
	}
	if age > RecordedMaxStaleness {
		view.Freshness = RecordedStale
		view.Message = fmt.Sprintf(
			"the recorded workspace view (%s) was observed %s ago, past the declared %s bound; "+
				"the answer is served from it anyway — run `putnami projects sync` to refresh it",
			WorkspaceIndexFilename, age.Round(time.Hour), RecordedMaxStaleness)
		return view
	}
	view.Freshness = RecordedFresh
	view.Message = fmt.Sprintf("the recorded workspace view (%s) was observed %s ago",
		WorkspaceIndexFilename, age.Round(time.Second))
	return view
}
