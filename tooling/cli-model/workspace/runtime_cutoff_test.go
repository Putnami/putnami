package workspace

import (
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// An extension whose prepared runtime is byte for byte the one the diff's base
// prepares moved nothing its consumers execute. A change to a library it
// imports then reaches the extension itself, through the dependency edge, and
// none of its consumers; the other extensions the change rebuilt still reach
// theirs.
func TestTraceChangeImpact_AnUnchangedRuntimeReachesNoConsumer(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "an-unchanged-runtime-fires-no-consumer-edge", "an-unchanged-runtime-reaches-no-consumer")
	ws := extensionImpactWorkspace()
	origin := []string{"tooling/extension-sdk/sdk.go"}

	before := TraceChangeImpact(ws, origin, ChangeImpactOptions{})
	if got, want := before.Trace.ScopeOf("/apps/web"), []string{"/go/extension", "/python/extension", "/typescript/extension"}; !slices.Equal(got, want) {
		t.Fatalf("without a proof, /apps/web is scoped to %v, want %v", got, want)
	}
	if got, want := before.Trace.RuntimeEdges, []string{"/go/extension", "/python/extension", "/typescript/extension"}; !slices.Equal(got, want) {
		t.Errorf("the runtime edges that fired are %v, want %v", got, want)
	}

	partial := TraceChangeImpact(ws, origin, ChangeImpactOptions{
		UnchangedRuntimes: map[string]bool{"/go/extension": true},
	})
	if got, want := partial.Trace.ScopeOf("/apps/web"), []string{"/python/extension", "/typescript/extension"}; !slices.Equal(got, want) {
		t.Errorf("with /go/extension unchanged, /apps/web is scoped to %v, want %v", got, want)
	}

	all := TraceChangeImpact(ws, origin, ChangeImpactOptions{UnchangedRuntimes: map[string]bool{
		"/go/extension": true, "/typescript/extension": true, "/python/extension": true,
	}})
	if len(all.Trace.RuntimeEdges) != 0 {
		t.Errorf("with every runtime unchanged, runtime edges still fired from %v", all.Trace.RuntimeEdges)
	}
	got := ownerProjectIDs(all.Projects)
	if slices.Contains(got, "/apps/web") {
		t.Errorf("with every runtime unchanged, the change still selected /apps/web: %v", got)
	}
	for _, ext := range []string{"/go/extension", "/typescript/extension", "/python/extension", "/tooling/extension-sdk"} {
		if !slices.Contains(got, ext) {
			t.Errorf("with every runtime unchanged, %s left the selection %v: its own tasks still read the change", ext, got)
		}
	}
}

// A changed file of the extension project itself keeps the edge firing, proof
// or not: the extension root holds files its tasks read at run time beside the
// runtime it prepares.
func TestTraceChangeImpact_AChangedExtensionIsNeverCutOff(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "an-unchanged-runtime-fires-no-consumer-edge", "a-changed-extension-is-never-cut-off")
	ws := extensionImpactWorkspace()
	r := TraceChangeImpact(ws, []string{"go/extension/internal/run.go"}, ChangeImpactOptions{
		UnchangedRuntimes: map[string]bool{"/go/extension": true},
	})
	if got, want := r.Trace.ScopeOf("/apps/web"), []string{"/go/extension"}; !slices.Equal(got, want) {
		t.Errorf("a change inside /go/extension scoped /apps/web to %v, want %v", got, want)
	}
}
