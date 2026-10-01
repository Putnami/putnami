package workspace

import (
	"errors"
	"os"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// THE WEDGE, AND WHY IT IS NOT ONE ANY MORE.
//
// Two workspaces fail Load unconditionally: one where two projects answer to the
// same name, and one whose dependency edges close a cycle. Neither is
// recoverable downstream, so both are fatal — and that is correct as long as the
// authored configuration is what produced them.
//
// A merged PROVIDER view can produce either as well, and that changed the shape
// of the failure completely: the view was adopted before the checks ran and
// persisted without them, so one bad answer made the index describe a workspace
// its own loader refuses. Every repair path — `projects sync`, `install`, even
// `projects list` — loads the workspace first, and loading re-adopted the
// recorded view before refusing it. Fixing the manifests did not help. The only
// escape was deleting a file no diagnostic named.
//
// The tests below pin both halves of the repair: a recorded view that cannot be
// adopted degrades to the un-adopted resolution with a warning that names the
// file, and the run that CREATES such a view fails before anything reaches disk.

// probeAnswer builds one normalized provider answer.
func probeAnswer(extension string, projects ...wsproto.ProbeProject) wsproto.ProbeResult {
	result := wsproto.ProbeResult{
		Version:   wsproto.ProbeProtocolVersion,
		Extension: extension,
		Projects:  projects,
	}
	wsproto.NormalizeProbeResult(&result)
	return result
}

// recordProviderView persists an index whose sole provider answered result.
func recordProviderView(t *testing.T, ws *Workspace, result wsproto.ProbeResult) {
	t.Helper()
	snapshot := NewSnapshotWithProviders(ws, ws.ProbeDigest(), []SnapshotProvider{{
		Extension: result.Extension,
		Digest:    wsproto.ProbeResultDigest(result),
		Result:    result,
	}})
	if err := WriteSnapshot(ws.Root, snapshot, SnapshotWritePolicy{}); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	InvalidateLoadCache(ws.Root)
}

// unloadableViews are the two answers Load refuses, over syncFixture's tree.
func unloadableViews() []struct {
	name   string
	result wsproto.ProbeResult
	reason string
} {
	return []struct {
		name   string
		result wsproto.ProbeResult
		reason string
	}{
		{
			name: "duplicate resolved names",
			result: probeAnswer("@fixture/lang",
				wsproto.ProbeProject{Path: "web", SourceName: "@acme/same"},
				wsproto.ProbeProject{Path: "svc", SourceName: "@acme/same"}),
			reason: "duplicate project name",
		},
		{
			name: "dependency cycle",
			result: probeAnswer("@fixture/lang",
				wsproto.ProbeProject{Path: "web", SourceName: "@acme/web", Dependencies: []string{"svc"}},
				wsproto.ProbeProject{Path: "svc", SourceName: "@acme/svc", Dependencies: []string{"web"}}),
			reason: "dependency cycle",
		},
	}
}

func TestLoad_RecordedViewThatCannotBeAdoptedDegradesInsteadOfWedging(t *testing.T) {
	for _, view := range unloadableViews() {
		t.Run(view.name, func(t *testing.T) {
			ws := syncFixture(t)
			root := ws.Root
			recordProviderView(t, ws, view.result)

			reloaded, err := Load(root)
			if err != nil {
				t.Fatalf("Load refused the workspace instead of degrading; every repair command loads first, "+
					"so this is the wedge: %v", err)
			}

			warning := strings.Join(reloaded.Warnings, "\n")
			if !strings.Contains(warning, view.reason) {
				t.Errorf("warnings do not say WHY the index was rejected (%q):\n%s", view.reason, warning)
			}
			if !strings.Contains(warning, WorkspaceIndexFilename) {
				t.Errorf("warnings do not name the file to rebuild or delete:\n%s", warning)
			}
			if !reloaded.HasWarningCode(WarningCodeProviderViewUnavailable) {
				t.Error("rejected provider view omitted its stable warning code")
			}

			// Degraded means UN-ADOPTED, not half-adopted: the projects resolve
			// exactly as they would on a checkout that has never been probed.
			if reloaded.HasProbeView() {
				t.Error("the rejected view was adopted anyway")
			}
			if got := reloaded.ProjectByID("/web"); got == nil || got.Name != "web" {
				t.Errorf("web = %+v, want the authored/basename resolution", got)
			}
			if cycle := reloaded.Graph.FindCycle(); cycle != nil {
				t.Errorf("the degraded graph still carries a cycle: %v", cycle)
			}

			// And the repair actually repairs: with the index gone the workspace
			// loads clean, which is what makes `projects sync` a sufficient remedy.
			if err := os.Remove(SnapshotPath(root)); err != nil {
				t.Fatal(err)
			}
			InvalidateLoadCache(root)
			repaired, err := Load(root)
			if err != nil {
				t.Fatalf("load after removing the index: %v", err)
			}
			if repaired.HasProbeView() {
				t.Error("a workspace with no index reports an adopted view")
			}
		})
	}
}

// The other half: the run that RESOLVES such a view must not record it. Adopting
// it in memory is survivable — the run reports a typed failure and the recovery
// commands are allowed through — but persisting it hands the same failure to
// every later command, including the ones that could fix it.
func TestSynchronize_ViewThatCannotBeAdoptedIsNeverPersisted(t *testing.T) {
	for _, view := range unloadableViews() {
		t.Run(view.name, func(t *testing.T) {
			ws := syncFixture(t)
			answer := view.result
			provider := &countingProvider{
				name:   "@fixture/lang",
				answer: func(wsproto.ProbeRequest) wsproto.ProbeResult { return answer },
			}
			bindings := []ProviderBinding{{
				Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json", "go.mod"}),
				Provider: provider,
			}}

			outcome, err := Synchronize(SyncRequest{
				Workspace: ws, Providers: bindings, Reason: wsproto.ProbeReasonLoad,
			})
			if err == nil {
				t.Fatalf("Synchronize accepted an unloadable view; outcome persisted=%v", outcome.Persisted)
			}
			var failure *wsproto.ProbeFailure
			if !errors.As(err, &failure) {
				t.Fatalf("err = %T(%v), want a typed *wsproto.ProbeFailure so the failure policy can classify it", err, err)
			}
			if failure.Kind != wsproto.ProbeFailureConflict {
				t.Errorf("failure kind = %q, want %q", failure.Kind, wsproto.ProbeFailureConflict)
			}
			if !strings.Contains(failure.Message, view.reason) {
				t.Errorf("failure message does not name the cause (%q): %s", view.reason, failure.Message)
			}

			if _, statErr := os.Stat(SnapshotPath(ws.Root)); !os.IsNotExist(statErr) {
				t.Fatalf("the unloadable view reached %s; the next command would inherit the wedge",
					SnapshotPath(ws.Root))
			}
			if ws.HasProbeView() {
				t.Error("the rejected view was left adopted on the live workspace")
			}
			if cycle := ws.Graph.FindCycle(); cycle != nil {
				t.Errorf("the live workspace was left with a cycle: %v", cycle)
			}
		})
	}
}

// A valid index must survive a later run that resolves an unloadable view: the
// refusal is "do not write this", not "throw away what was already true".
func TestSynchronize_UnloadableViewLeavesAValidIndexIntact(t *testing.T) {
	ws := syncFixture(t)
	good := probeAnswer("@fixture/lang",
		wsproto.ProbeProject{Path: "web", SourceName: "@acme/web"},
		wsproto.ProbeProject{Path: "svc", SourceName: "@acme/svc"})
	answer := good
	provider := &countingProvider{
		name:   "@fixture/lang",
		answer: func(wsproto.ProbeRequest) wsproto.ProbeResult { return answer },
	}
	bindings := []ProviderBinding{{
		Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json", "go.mod"}),
		Provider: provider,
	}}

	first := syncOnce(t, ws, bindings)
	if !first.Persisted {
		t.Fatal("the first sync did not persist the index")
	}
	before, err := os.ReadFile(SnapshotPath(ws.Root))
	if err != nil {
		t.Fatal(err)
	}

	// The provider changes its mind and reports a colliding name, and a metadata
	// input moves so the snapshot is genuinely stale and the probe re-runs.
	answer = unloadableViews()[0].result
	writeFileAt(t, ws.Root+"/web/package.json", `{"name":"@acme/web","version":"2"}`)
	if _, err := Synchronize(SyncRequest{
		Workspace: ws, Providers: bindings, Reason: wsproto.ProbeReasonLoad,
	}); err == nil {
		t.Fatal("Synchronize accepted an unloadable view")
	}

	after, err := os.ReadFile(SnapshotPath(ws.Root))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the recorded index was rewritten by a run whose view was refused")
	}
	if !ws.HasProbeView() {
		t.Error("the previously adopted, valid view was dropped")
	}
	if got := ws.ProjectByID("/web"); got == nil || got.Name != "@acme/web" {
		t.Errorf("web = %+v, want the last GOOD view's identity", got)
	}
}

// Keeping a stale index because nobody answered for it is the right call and an
// invisible one: without a diagnostic it looks exactly like a run that had
// nothing to do, so a workspace can stay on a months-old provider view because
// an extension has been failing to resolve the whole time.
func TestSynchronize_NoBindingsReportsThatTheRecordedAnswersWereKept(t *testing.T) {
	ws := syncFixture(t)
	provider := &countingProvider{name: "@fixture/lang", answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
		return probeAnswer("@fixture/lang", wsproto.ProbeProject{Path: "web", SourceName: "@acme/web"})
	}}
	bindings := []ProviderBinding{{
		Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: provider,
	}}
	syncOnce(t, ws, bindings)

	InvalidateLoadCache(ws.Root)
	degraded, err := Load(ws.Root)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	outcome := syncOnce(t, degraded, nil)

	reported := ""
	for _, d := range outcome.Diagnostics {
		reported += d.String() + "\n"
	}
	if !strings.Contains(reported, "@fixture/lang") {
		t.Errorf("a run with no adapters kept a recorded provider view without saying so:\n%s", reported)
	}
	if !strings.Contains(reported, WorkspaceIndexFilename) {
		t.Errorf("the diagnostic does not name the index it preserved:\n%s", reported)
	}
}

// A workspace with no index at all is a legitimate state, but a silent one: the
// read-only surfaces that serve it (`projects list`, `deps`, the MCP graph
// tools, `--impacted`) would report a graph with zero provider-derived edges and
// no indication that anything is missing.
func TestLoad_WarnsWhenMembersHaveNoProviderView(t *testing.T) {
	ws := syncFixture(t) // web/package.json and svc/go.mod, neither with a putnami.json

	warning := strings.Join(ws.Warnings, "\n")
	if !strings.Contains(warning, WorkspaceIndexFilename) {
		t.Fatalf("a workspace with no index and provider-only members warned nothing:\n%v", ws.Warnings)
	}
	if !strings.Contains(warning, "web") && !strings.Contains(warning, "svc") {
		t.Errorf("the warning does not name any of the affected projects:\n%s", warning)
	}
	if !ws.HasWarningCode(WarningCodeProviderViewUnavailable) {
		t.Error("missing provider view omitted its stable warning code")
	}

	// A workspace core can fully describe on its own says nothing: a Putnami-only
	// workspace has no provider view to miss.
	root := t.TempDir()
	writeFileAt(t, root+"/putnami.workspace.json", `{"includes":["tool"]}`)
	writeFileAt(t, root+"/tool/putnami.json", `{"name":"tool"}`)
	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })
	core, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if core.HasWarningCode(WarningCodeProviderViewUnavailable) {
		t.Error("fully authored workspace received a false provider-view warning code")
	}
	for _, w := range core.Warnings {
		if strings.Contains(w, WorkspaceIndexFilename) {
			t.Errorf("a Putnami-only workspace was told its index is missing: %s", w)
		}
	}
}
