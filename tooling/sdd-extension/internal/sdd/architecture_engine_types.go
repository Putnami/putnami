// Architecture engine types adapt the pure ARC/DARC protocol to repository
// discovery, the Putnami project graph, and an immutable Git baseline.
package sdd

import (
	"time"

	archproto "go.putnami.dev/protocol/architecture"
	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	// MaximumManifestBytes bounds every recursively discovered authored file.
	MaximumManifestBytes = 1 << 20
	// MaximumManifests bounds recursive authority discovery.
	MaximumManifests = 256

	ErrorCodeReadFailure            = "architecture.read_failure"
	ErrorCodeDiscoveryLimit         = "architecture.discovery_limit"
	ErrorCodeIncompleteProjectGraph = "architecture.incomplete_project_graph"
	ErrorCodeBaselineUnavailable    = "architecture.baseline_unavailable"
)

// EvaluationOptions selects the immutable baseline used for the shrink-only
// check and the explicit date used to evaluate waiver expiry.
type EvaluationOptions struct {
	BaselineRef string
	Today       time.Time
	// WorktreeOnly evaluates the current worktree and reads NO git history: the
	// frozen adoption baseline is not compared, so every finding is reported at
	// its worktree disposition and the shrink-only ratchet does not run.
	//
	// It exists for the cached DAG task (D9). That task declares three
	// file patterns as its inputs; comparing against a baseline commit would
	// make its verdict depend on git — which ref `origin/HEAD` points at, what
	// the branch forked from — none of which is in the key. A cached answer
	// that silently depends on an unkeyed input is the one thing a cache must
	// never store, so the job path drops the comparison instead of widening the
	// key to "the repository".
	//
	// Shrink-only enforcement stays an interactive and review concern:
	// `architecture validate --baseline <ref>` is unchanged and still ratchets.
	//
	// This member is the extension's own, and is deliberately absent from the
	// CLI copy of this engine: nothing in core runs an architecture evaluation
	// from a cached task, so a mirrored option there would be dead code in a
	// package slated for deletion.
	WorktreeOnly bool
}

// Result preserves structural diagnostics even when no snapshot can be published.
type Result struct {
	Snapshot    *archproto.Snapshot
	Diagnostics []diag.Diagnostic
	Baseline    BaselineStatus
}

// BaselineStatus explains whether shrink-only comparison ran and against which commit.
type BaselineStatus struct {
	Requested string `json:"requested,omitempty"`
	Commit    string `json:"commit,omitempty"`
	Compared  bool   `json:"compared"`
	PriorFile bool   `json:"priorFile"`
}

// DiscoveryResult is one deterministic pass over authored architecture files.
type DiscoveryResult struct {
	Sources     []archproto.ManifestSource
	Baseline    *archproto.Baseline
	Waivers     *archproto.WaiverFile
	Diagnostics []diag.Diagnostic
}
