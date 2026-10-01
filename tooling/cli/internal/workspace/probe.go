package workspace

import (
	"errors"
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

// Workspace probe wiring.
//
// Core owns identity: it assigns canonical paths and IDs, it decides the merge,
// and it computes the digest that feeds workspace/plan identity and every cache
// key that observes project metadata or dependency edges. Providers own
// language knowledge and answer through ProbeProvider. This file is the seam
// between the two, plus the failure policy that decides which commands a broken
// provider is allowed to take down.

// ProbeProvider is one metadata provider core can ask about a workspace. The
// production implementation runs an extension; tests and the protocol's
// fixture-provider conformance suite implement it directly.
type ProbeProvider interface {
	// Name is the provider's extension name. It keys the provider-owned
	// metadata bucket in the merged view, so two providers must not share one.
	Name() string
	// Probe answers a request. A failure must be a *wsproto.ProbeFailure so the
	// cause survives to the diagnostic.
	Probe(wsproto.ProbeRequest) (wsproto.ProbeResult, error)
}

// ProbeOutcome is the result of asking every provider.
type ProbeOutcome struct {
	// Results are the validated, per-provider answers in provider-name order.
	Results []wsproto.ProbeResult
	// Merged is the merged view keyed by canonical project path.
	Merged map[string]wsproto.MergedProject
	// Digest is the normalized aggregate probe digest.
	Digest string
	// Diagnostics are advisory findings gathered from every provider.
	Diagnostics []diag.Diagnostic
}

// RunProbe asks every provider and merges the answers.
//
// Providers are asked in sorted name order and their results are merged
// order-independently, so the outcome — including which conflict is reported
// first — never depends on extension load order. Any provider failure is
// returned as a typed *wsproto.ProbeFailure: a partially probed workspace is
// not a workspace, because a missing provider silently drops dependency edges
// and a dropped edge is a wrong build, not a slower one.
func RunProbe(providers []ProbeProvider, request wsproto.ProbeRequest,
	explicit map[string]wsproto.ExplicitProject) (*ProbeOutcome, error) {
	ordered := append([]ProbeProvider(nil), providers...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Name() < ordered[j].Name() })

	outcome := &ProbeOutcome{Merged: map[string]wsproto.MergedProject{}}
	for _, provider := range ordered {
		perProvider := request
		perProvider.Version = wsproto.ProbeProtocolVersion
		perProvider.Extension = provider.Name()

		result, err := provider.Probe(perProvider)
		if err != nil {
			// A provider that already classified its own failure keeps that
			// cause; anything else is classified as transport rather than
			// traveling to the diagnostic as an unclassified string.
			var failure *wsproto.ProbeFailure
			if !errors.As(err, &failure) {
				failure = wsproto.NewProbeFailure(wsproto.ProbeFailureTransport, provider.Name(), "%v", err)
			}
			return nil, failure
		}
		if result.Extension == "" {
			result.Extension = provider.Name()
		}
		if result.Extension != provider.Name() {
			return nil, wsproto.NewProbeFailure(wsproto.ProbeFailureTransport, provider.Name(),
				"probe answer is attributed to %q; a misrouted result cannot be bucketed", result.Extension)
		}
		if diags := wsproto.ValidateProbeResult(&result); diag.HasErrors(diags) {
			failure := wsproto.NewProbeFailure(wsproto.ProbeFailureInvalidResult, provider.Name(),
				"probe answer rejected by the v%d contract", wsproto.ProbeProtocolVersion)
			failure.Diagnostics = diag.Errors(diags)
			return nil, failure
		}
		outcome.Diagnostics = append(outcome.Diagnostics, result.Diagnostics...)
		outcome.Results = append(outcome.Results, result)
	}

	merged, mergeDiags := wsproto.MergeProbeResults(outcome.Results, explicit)
	if diag.HasErrors(mergeDiags) {
		failure := wsproto.NewProbeFailure(wsproto.ProbeFailureConflict, "",
			"providers reported irreconcilable project metadata")
		failure.Diagnostics = diag.Errors(mergeDiags)
		return nil, failure
	}
	outcome.Merged = merged
	outcome.Diagnostics = append(outcome.Diagnostics, mergeDiags...)
	outcome.Digest = wsproto.ProbeWorkspaceDigest(outcome.Results)
	return outcome, nil
}

// RefusesAUsableGraph reports whether failure is a verdict on a graph the
// providers answered completely: an import visibility or declared-edge
// refusal. The synchronization that raised it wrote its snapshot first, so the
// recorded view is usable by a reader; only planning over it is refused.
func RefusesAUsableGraph(failure *wsproto.ProbeFailure) bool {
	if failure == nil {
		return false
	}
	return failure.Kind == wsproto.ProbeFailureVisibility || failure.Kind == wsproto.ProbeFailureDeclaredEdge
}

// ReadableRefusal answers a read-only graph preparation that synchronization
// refused. A refused graph is still a complete, recorded answer: a reader
// serves it with the refusal's findings, and only planning over it is refused.
// Any other failure stays an error.
func ReadableRefusal(err error) (*SyncOutcome, error) {
	failure := &wsproto.ProbeFailure{}
	if errors.As(err, &failure) && RefusesAUsableGraph(failure) {
		return &SyncOutcome{Diagnostics: failure.Diagnostics}, nil
	}
	return nil, err
}

// RequireProbe is the failure policy: a probe failure takes down every
// graph-dependent command, and exactly the recovery commands survive it.
//
// The asymmetry is the point. A command that plans or executes over the project
// graph cannot run on a half-known workspace — it would silently build the
// wrong thing. The commands that can REPAIR the situation (install a missing
// extension, re-scan the tree, read help) must stay reachable, or a broken
// provider becomes an unrecoverable workspace. Membership is declared on the
// command catalog, not duplicated here.
func RequireProbe(commandPath string, failure *wsproto.ProbeFailure) error {
	if failure == nil {
		return nil
	}
	if commandmeta.IsRecoveryCommand(commandPath) {
		return nil
	}
	return failure
}
