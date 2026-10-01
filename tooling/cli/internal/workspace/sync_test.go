package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
)

// countingProvider records how many times it was asked, which is how every
// zero-process invariant below is actually checked: "started no extension
// process" is only meaningful if something counts the starts.
type countingProvider struct {
	name    string
	calls   int
	answer  func(wsproto.ProbeRequest) wsproto.ProbeResult
	failure error
}

func (p *countingProvider) Name() string { return p.name }

func (p *countingProvider) Probe(request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	p.calls++
	if p.failure != nil {
		return wsproto.ProbeResult{}, p.failure
	}
	if p.answer != nil {
		return p.answer(request), nil
	}
	return wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: p.name}, nil
}

func scopeFor(t *testing.T, extension string, markers, inputs []string) ProviderScope {
	t.Helper()
	scope, ok := NewProviderScope(extension, &extproto.WorkspaceAdapter{Markers: markers, Inputs: inputs})
	if !ok {
		t.Fatalf("NewProviderScope(%q) rejected a valid adapter", extension)
	}
	return scope
}

func writeFileAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// syncFixture builds a two-project workspace: one npm package and one Go
// module, both declared members.
func syncFixture(t *testing.T) *Workspace {
	t.Helper()
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["web","svc"]}`)
	writeFileAt(t, filepath.Join(root, "web", "package.json"), `{"name":"@acme/web"}`)
	writeFileAt(t, filepath.Join(root, "svc", "go.mod"), "module acme/svc\n\ngo 1.25\n")

	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })
	ws, err := Load(root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	return ws
}

func syncOnce(t *testing.T, ws *Workspace, bindings []ProviderBinding) *SyncOutcome {
	t.Helper()
	outcome, err := Synchronize(SyncRequest{Workspace: ws, Providers: bindings, Reason: wsproto.ProbeReasonLoad})
	if err != nil {
		t.Fatalf("Synchronize: %v", err)
	}
	return outcome
}

// THE invariant of slice C3b: a workspace whose recorded metadata inputs still
// hash to their recorded digests starts NO provider. Everything else in this
// slice is arranged to make this true, so it is checked first.
func TestSynchronize_ValidSnapshotStartsNoProvider(t *testing.T) {
	ws := syncFixture(t)
	provider := &countingProvider{name: "@putnami/typescript"}
	bindings := []ProviderBinding{{
		Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: provider,
	}}

	first := syncOnce(t, ws, bindings)
	if first.Fresh || provider.calls != 1 {
		t.Fatalf("first sync: fresh=%v calls=%d, want a single probe", first.Fresh, provider.calls)
	}
	if !first.Persisted {
		t.Fatal("first sync did not persist the snapshot")
	}

	second := syncOnce(t, ws, bindings)
	if !second.Fresh {
		t.Fatalf("second sync over an unchanged tree was not fresh: reason=%q changed=%v", second.Reason, second.Changed)
	}
	if provider.calls != 1 {
		t.Fatalf("provider was asked %d times; a valid snapshot must start zero processes", provider.calls)
	}
	if len(second.Probed) != 0 {
		t.Fatalf("outcome reports probed=%v for a fresh snapshot", second.Probed)
	}
}

// A source-only edit is not a metadata input, so however many times it happens
// the probe never repeats. This is the watch invariant, expressed at the layer
// watch actually re-enters (every iteration re-runs Synchronize).
func TestSynchronize_SourceEditsNeverRepeatTheProbe(t *testing.T) {
	ws := syncFixture(t)
	provider := &countingProvider{name: "@putnami/typescript"}
	bindings := []ProviderBinding{{
		Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: provider,
	}}
	syncOnce(t, ws, bindings)

	for i := range 5 {
		writeFileAt(t, filepath.Join(ws.Root, "web", "src", "index.ts"), "export const n = "+string(rune('0'+i))+";")
		outcome := syncOnce(t, ws, bindings)
		if !outcome.Fresh {
			t.Fatalf("source edit %d invalidated the snapshot: %q %v", i, outcome.Reason, outcome.Changed)
		}
	}
	if provider.calls != 1 {
		t.Fatalf("provider was asked %d times across five source edits, want 1", provider.calls)
	}
}

// A provider that derives identity from source shape opts into source-set
// invalidation explicitly. Creating the first package main is the important
// case: the file had no path to record when the library snapshot was written,
// so only a recursive matched-set witness can make the owner re-probe and
// replace the recorded Type before planning.
func TestSynchronize_RecursiveClassificationWitnessReprobesItsOwner(t *testing.T) {
	ws := syncFixture(t)
	ts := &countingProvider{name: "@putnami/typescript"}
	golang := &countingProvider{name: "@putnami/go", answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
		projectType := "library"
		if _, err := os.Stat(filepath.Join(ws.Root, "svc", "cmd", "tool", "main.go")); err == nil {
			projectType = "application"
		}
		return wsproto.ProbeResult{
			Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/go",
			Projects: []wsproto.ProbeProject{{Path: "svc", SourceName: "acme/svc", Type: projectType}},
		}
	}}
	bindings := []ProviderBinding{
		{Scope: scopeFor(t, ts.name, []string{"package.json"}, []string{"package.json"}), Provider: ts},
		{Scope: scopeFor(t, golang.name, []string{"go.mod"}, []string{"go.mod", "**/*.go"}), Provider: golang},
	}

	first := syncOnce(t, ws, bindings)
	if got := first.Merged["svc"].Type; got != "library" {
		t.Fatalf("initial type = %q, want library", got)
	}
	writeFileAt(t, filepath.Join(ws.Root, "svc", "cmd", "tool", "main.go"),
		"package main\n\nfunc main() {}\n")

	second := syncOnce(t, ws, bindings)
	if second.Fresh || golang.calls != 2 {
		t.Fatalf("classification witness: fresh=%v go calls=%d, want a second Go probe", second.Fresh, golang.calls)
	}
	if ts.calls != 1 {
		t.Fatalf("unaffected provider calls = %d, want 1", ts.calls)
	}
	if got := second.Merged["svc"].Type; got != "application" {
		t.Fatalf("type after creating package main = %q, want application", got)
	}
}

// One changed manifest starts exactly one probe: the provider that DECLARED
// that file as its metadata input, and no other.
func TestSynchronize_ChangedInputProbesOnlyItsOwner(t *testing.T) {
	ws := syncFixture(t)
	ts := &countingProvider{name: "@putnami/typescript", answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
		return wsproto.ProbeResult{
			Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/typescript",
			Projects: []wsproto.ProbeProject{{Path: "web", SourceName: "@acme/web"}},
		}
	}}
	golang := &countingProvider{name: "go.putnami.dev/go", answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
		return wsproto.ProbeResult{
			Version: wsproto.ProbeProtocolVersion, Extension: "go.putnami.dev/go",
			Projects: []wsproto.ProbeProject{{Path: "svc", SourceName: "acme/svc"}},
		}
	}}
	bindings := []ProviderBinding{
		{Scope: scopeFor(t, ts.name, []string{"package.json"}, []string{"package.json"}), Provider: ts},
		{Scope: scopeFor(t, golang.name, []string{"go.mod"}, []string{"go.mod", "go.sum"}), Provider: golang},
	}
	syncOnce(t, ws, bindings)
	if ts.calls != 1 || golang.calls != 1 {
		t.Fatalf("initial probe counts = ts:%d go:%d, want 1 each", ts.calls, golang.calls)
	}

	writeFileAt(t, filepath.Join(ws.Root, "web", "package.json"), `{"name":"@acme/web","version":"2.0.0"}`)
	outcome := syncOnce(t, ws, bindings)

	if ts.calls != 2 {
		t.Errorf("the owning provider was asked %d times, want 2", ts.calls)
	}
	if golang.calls != 1 {
		t.Errorf("an unaffected provider was re-asked (%d calls); only the owner of a changed input probes", golang.calls)
	}
	if len(outcome.Probed) != 1 || outcome.Probed[0] != ts.name {
		t.Errorf("probed = %v, want exactly [%s]", outcome.Probed, ts.name)
	}
	// The unprobed provider's answer must survive: dropping it would silently
	// delete its dependency edges, which is a wrong build rather than a slow one.
	if _, ok := outcome.Merged["svc"]; !ok {
		t.Fatalf("merged view lost the unprobed provider's project: %v", outcome.Merged)
	}
	if outcome.Merged["svc"].SourceName != "acme/svc" {
		t.Errorf("carried-forward answer = %+v", outcome.Merged["svc"])
	}
}

// A changed input NO provider claims — core's own putnami.json — invalidates
// every provider: it decides membership and explicit identity, so an unasked
// provider would be answering about a workspace that no longer exists.
func TestSynchronize_UnclaimedInputChangeProbesEveryProvider(t *testing.T) {
	ws := syncFixture(t)
	ts := &countingProvider{name: "@putnami/typescript"}
	golang := &countingProvider{name: "go.putnami.dev/go"}
	bindings := []ProviderBinding{
		{Scope: scopeFor(t, ts.name, []string{"package.json"}, []string{"package.json"}), Provider: ts},
		{Scope: scopeFor(t, golang.name, []string{"go.mod"}, []string{"go.mod"}), Provider: golang},
	}
	syncOnce(t, ws, bindings)

	writeFileAt(t, filepath.Join(ws.Root, "web", wsproto.ConfigFilename), `{"name":"web","tags":["ts"]}`)
	outcome := syncOnce(t, ws, bindings)

	if ts.calls != 2 || golang.calls != 2 {
		t.Fatalf("probe counts = ts:%d go:%d; a core-owned input change must re-ask everyone", ts.calls, golang.calls)
	}
	if len(outcome.Probed) != 2 {
		t.Errorf("probed = %v, want both providers", outcome.Probed)
	}
}

// Content, and only content, decides validity. A rewrite to the same length
// within the same second is a real change that no stat-based oracle can see.
func TestSynchronize_SameSizeSameTimestampRewriteInvalidates(t *testing.T) {
	ws := syncFixture(t)
	provider := &countingProvider{name: "@putnami/typescript"}
	bindings := []ProviderBinding{{
		Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: provider,
	}}
	syncOnce(t, ws, bindings)

	manifest := filepath.Join(ws.Root, "web", "package.json")
	info, err := os.Stat(manifest)
	if err != nil {
		t.Fatal(err)
	}
	// Same byte length, different content, and the modification time pinned
	// back to what the snapshot recorded.
	writeFileAt(t, manifest, `{"name":"@acme/wob"}`)
	if newInfo, err := os.Stat(manifest); err != nil || newInfo.Size() != info.Size() {
		t.Fatalf("fixture is not a same-size rewrite (err=%v)", err)
	}
	if err := os.Chtimes(manifest, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	outcome := syncOnce(t, ws, bindings)
	if outcome.Fresh {
		t.Fatal("a same-size, same-mtime rewrite was accepted as unchanged")
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want the rewrite to have re-probed", provider.calls)
	}
}

// `--plan` and `--dry-run` may probe in memory and must never persist:
// a speculative view must not become the workspace's recorded identity.
func TestSynchronize_PlanPolicyNeverPersists(t *testing.T) {
	ws := syncFixture(t)
	provider := &countingProvider{name: "@putnami/typescript"}
	bindings := []ProviderBinding{{
		Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: provider,
	}}

	outcome, err := Synchronize(SyncRequest{
		Workspace: ws, Providers: bindings,
		Reason: wsproto.ProbeReasonPlan,
		Policy: SnapshotWritePolicy{Plan: true},
	})
	if err != nil {
		t.Fatalf("Synchronize: %v", err)
	}
	if outcome.Persisted {
		t.Error("a --plan run reported the snapshot as persisted")
	}
	if outcome.Snapshot == nil {
		t.Error("a --plan run must still report the snapshot it WOULD have written")
	}
	if _, statErr := os.Stat(SnapshotPath(ws.Root)); !os.IsNotExist(statErr) {
		t.Fatalf("a --plan run wrote %s (stat err = %v)", WorkspaceIndexFilename, statErr)
	}
}

// A provider failure travels as a typed *ProbeFailure so the caller can apply
// the recovery-command policy; flattening it to a string is what makes a broken
// workspace unrecoverable.
func TestSynchronize_ProviderFailureIsTyped(t *testing.T) {
	ws := syncFixture(t)
	provider := &countingProvider{
		name:    "@putnami/typescript",
		failure: wsproto.NewProbeFailure(wsproto.ProbeFailureTimeout, "@putnami/typescript", "took too long"),
	}
	_, err := Synchronize(SyncRequest{Workspace: ws, Providers: []ProviderBinding{{
		Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: provider,
	}}})

	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) {
		t.Fatalf("Synchronize error = %v (%T), want a *ProbeFailure", err, err)
	}
	if failure.Kind != wsproto.ProbeFailureTimeout {
		t.Errorf("failure kind = %q, want %q", failure.Kind, wsproto.ProbeFailureTimeout)
	}
	if _, statErr := os.Stat(SnapshotPath(ws.Root)); !os.IsNotExist(statErr) {
		t.Error("a failed probe wrote a snapshot; a half-known workspace must not be recorded")
	}
}

// Two providers claiming different non-empty values for one scalar is a hard
// error, not a silent pick — silently choosing one is how a workspace starts
// building differently depending on extension load order.
func TestSynchronize_MergeConflictIsTyped(t *testing.T) {
	ws := syncFixture(t)
	left := &countingProvider{name: "a-ext", answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
		return wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: "a-ext",
			Projects: []wsproto.ProbeProject{{Path: "web", SourceName: "left"}}}
	}}
	right := &countingProvider{name: "b-ext", answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
		return wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: "b-ext",
			Projects: []wsproto.ProbeProject{{Path: "web", SourceName: "right"}}}
	}}
	_, err := Synchronize(SyncRequest{Workspace: ws, Providers: []ProviderBinding{
		{Scope: scopeFor(t, left.name, []string{"package.json"}, []string{"package.json"}), Provider: left},
		{Scope: scopeFor(t, right.name, []string{"package.json"}, []string{"package.json"}), Provider: right},
	}})

	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureConflict {
		t.Fatalf("Synchronize error = %v, want a merge-conflict ProbeFailure", err)
	}
}

// A workspace with no adapter-declaring extension still gets an index: the
// snapshot is core's own record, not only the providers'.
func TestSynchronize_NoProvidersStillWritesTheIndex(t *testing.T) {
	ws := syncFixture(t)
	outcome, err := Synchronize(SyncRequest{Workspace: ws})
	if err != nil {
		t.Fatalf("Synchronize: %v", err)
	}
	if !outcome.Persisted || outcome.Snapshot == nil {
		t.Fatalf("outcome = %+v, want a persisted core-only snapshot", outcome)
	}
	again, err := Synchronize(SyncRequest{Workspace: ws})
	if err != nil {
		t.Fatalf("Synchronize: %v", err)
	}
	if !again.Fresh || again.Persisted {
		t.Errorf("an unchanged tree rewrote the index: fresh=%v persisted=%v", again.Fresh, again.Persisted)
	}
}

// A fresh snapshot must produce the SAME merged view a probing run produces.
// Without that, a consumer's behavior would depend on whether this particular
// run happened to start a provider.
func TestSynchronize_FreshSnapshotRebuildsTheMergedView(t *testing.T) {
	ws := syncFixture(t)
	provider := &countingProvider{name: "@putnami/typescript", answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
		return wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/typescript",
			Projects: []wsproto.ProbeProject{{Path: "web", SourceName: "@acme/web", Type: "library"}}}
	}}
	bindings := []ProviderBinding{{
		Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: provider,
	}}

	probed := syncOnce(t, ws, bindings)
	adopted := syncOnce(t, ws, bindings)
	if !adopted.Fresh {
		t.Fatal("second sync was not fresh")
	}
	if probed.Merged["web"].SourceName != adopted.Merged["web"].SourceName ||
		probed.Merged["web"].Type != adopted.Merged["web"].Type {
		t.Fatalf("adopted view %+v differs from the probed view %+v", adopted.Merged["web"], probed.Merged["web"])
	}
}

// The recorded snapshot is not consulted for validity by its own timestamps: a
// snapshot whose file mtime moved but whose inputs did not is still valid.
func TestSynchronize_SnapshotFileTimestampIsNotTheOracle(t *testing.T) {
	ws := syncFixture(t)
	provider := &countingProvider{name: "@putnami/typescript"}
	bindings := []ProviderBinding{{
		Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: provider,
	}}
	syncOnce(t, ws, bindings)

	future := time.Now().Add(48 * time.Hour)
	if err := os.Chtimes(SnapshotPath(ws.Root), future, future); err != nil {
		t.Fatal(err)
	}
	if outcome := syncOnce(t, ws, bindings); !outcome.Fresh {
		t.Fatalf("moving the snapshot's own mtime invalidated it: %q", outcome.Reason)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
}

// ONE TREE, ONE KEY.
//
// This is the invariant the design rests on. Since the core parsers were
// deleted, a project's identity — and therefore its metadata digest, and
// therefore every cache key that observes it — comes from the merged provider
// view. A run that PROBED and a run that replayed the RECORDED answer must
// resolve the same workspace, or the same tree would key two different ways
// depending on whether a provider happened to run. That is the cache-poisoning
// shape this repository has paid for more than once, including in
// version.json's buildTime.
//
// The check is deliberately made over a FRESH *Workspace for the replay: reusing
// the probed one would compare a value to itself.
func TestSynchronize_ProbedAndReplayedRunsResolveOneIdentity(t *testing.T) {
	ws := syncFixture(t)
	answer := func(request wsproto.ProbeRequest) wsproto.ProbeResult {
		return wsproto.ProbeResult{
			Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/typescript",
			Projects: []wsproto.ProbeProject{{
				Path: "web", SourceName: "@acme/web", Type: "library",
				Dependencies: []string{"svc"},
				Metadata:     []byte(`{"main":"dist/index.js"}`),
			}},
		}
	}
	provider := &countingProvider{name: "@putnami/typescript", answer: answer}
	bindings := []ProviderBinding{{
		Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: provider,
	}}

	probed := syncOnce(t, ws, bindings)
	if probed.Fresh || provider.calls != 1 {
		t.Fatalf("first sync: fresh=%v calls=%d, want one probe", probed.Fresh, provider.calls)
	}
	web := ws.ProjectByPath("web")
	if web == nil || web.Name != "@acme/web" || web.Type != "library" {
		t.Fatalf("probed identity = %+v, want the provider's answer adopted", web)
	}
	if len(web.Dependencies) != 1 || web.Dependencies[0] != ws.ProjectByPath("svc").Name {
		t.Fatalf("probed dependencies = %v, want the reported path resolved to a name", web.Dependencies)
	}
	probedDigest := ws.MetadataDigestFor(web)
	probedIdentity := ws.IdentityDigest()

	// A brand-new *Workspace over the same tree: Load adopts the RECORDED answer
	// from the index, and Synchronize confirms it without starting a process.
	InvalidateLoadCache(ws.Root)
	replayed, err := Load(ws.Root)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	replayedProvider := &countingProvider{name: "@putnami/typescript", answer: answer}
	replayedBindings := []ProviderBinding{{
		Scope:    scopeFor(t, replayedProvider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: replayedProvider,
	}}
	outcome := syncOnce(t, replayed, replayedBindings)
	if !outcome.Fresh || replayedProvider.calls != 0 {
		t.Fatalf("replayed sync: fresh=%v calls=%d, want a zero-process replay", outcome.Fresh, replayedProvider.calls)
	}

	replayedWeb := replayed.ProjectByPath("web")
	if replayedWeb == nil || replayedWeb.Name != web.Name || replayedWeb.Type != web.Type {
		t.Fatalf("replayed identity = %+v, want the probed one %+v", replayedWeb, web)
	}
	if got := replayed.MetadataDigestFor(replayedWeb); got != probedDigest {
		t.Errorf("metadata digest depends on whether this run probed: %q (probe) vs %q (replay)", probedDigest, got)
	}
	if got := replayed.IdentityDigest(); got != probedIdentity {
		t.Errorf("workspace identity depends on whether this run probed: %q vs %q", probedIdentity, got)
	}
}

// A run whose providers could not be resolved must NOT erase the recorded
// answers. Overwriting the index with a provider-less snapshot would delete
// every project's language identity and dependency edges, and the next load —
// which adopts whatever the index says — would resolve a different workspace for
// an unchanged tree.
func TestSynchronize_NoBindingsPreservesRecordedProviderAnswers(t *testing.T) {
	ws := syncFixture(t)
	provider := &countingProvider{name: "@putnami/typescript", answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
		return wsproto.ProbeResult{
			Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/typescript",
			Projects: []wsproto.ProbeProject{{Path: "web", SourceName: "@acme/web"}},
		}
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
	if !outcome.Fresh {
		t.Fatalf("a provider-less run rebuilt the index: %+v", outcome)
	}

	stored, err := LoadSnapshot(ws.Root)
	if err != nil || stored == nil {
		t.Fatalf("snapshot = %+v err = %v", stored, err)
	}
	if len(stored.Providers) != 1 {
		t.Fatalf("recorded providers = %d, want the answer preserved", len(stored.Providers))
	}
	if got := degraded.ProjectByPath("web").Name; got != "@acme/web" {
		t.Errorf("web name = %q, want the recorded identity still adopted", got)
	}
}

// A workspace that genuinely has no providers still gets an index: it is core's
// own, and `projects sync` is still the command that writes it.
func TestSynchronize_NoBindingsAndNoRecordedAnswersStillWritesTheIndex(t *testing.T) {
	ws := syncFixture(t)
	outcome := syncOnce(t, ws, nil)
	if !outcome.Persisted {
		t.Fatalf("a provider-less workspace lost its index: %+v", outcome)
	}
	stored, err := LoadSnapshot(ws.Root)
	if err != nil || stored == nil {
		t.Fatalf("snapshot = %+v err = %v", stored, err)
	}
	if len(stored.Projects) != 2 {
		t.Errorf("recorded projects = %d, want core's own resolution", len(stored.Projects))
	}
}

// A provider that did not resolve for THIS run must not have its recorded
// answer deleted from the index.
//
// The `providers` rebuild ranges over the CURRENT bindings, and
// snapshotStillHolds treats a shorter provider list as a reason to WRITE — so a
// recorded provider whose artifact vanished from the gitignored `.putnami/`,
// whose `deps install` was interrupted, or whose runtime was garbage-collected
// used to be silently dropped. Every project only that provider described then
// reverted to authored-config-only identity: no source name, no type, no
// metadata, and no provider-derived dependency edges, so `--impacted` computed
// over an incomplete graph while the tree never changed.
//
// This is the per-provider form of the all-or-nothing refusal above, and it is
// held to the same three obligations: keep the record, keep the VIEW consistent
// with it, and say so.
func TestSynchronize_UnresolvedProviderKeepsItsRecordedAnswer(t *testing.T) {
	ws := syncFixture(t)
	tsProvider := &countingProvider{name: "@putnami/typescript", answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
		return wsproto.ProbeResult{
			Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/typescript",
			Projects: []wsproto.ProbeProject{{Path: "web", SourceName: "@acme/web", Type: "application"}},
		}
	}}
	goProvider := &countingProvider{name: "@putnami/go", answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
		return wsproto.ProbeResult{
			Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/go",
			Projects: []wsproto.ProbeProject{{
				Path: "svc", SourceName: "acme/svc", Type: "library",
				Dependencies: []string{"web"},
			}},
		}
	}}
	tsBinding := ProviderBinding{
		Scope:    scopeFor(t, tsProvider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: tsProvider,
	}
	goBinding := ProviderBinding{
		Scope:    scopeFor(t, goProvider.name, []string{"go.mod"}, []string{"go.mod"}),
		Provider: goProvider,
	}
	syncOnce(t, ws, []ProviderBinding{tsBinding, goBinding})

	// The Go extension does not resolve this time, and a TypeScript metadata
	// input moved, so this run really does rebuild rather than stay fresh.
	writeFileAt(t, filepath.Join(ws.Root, "web", "package.json"), `{"name":"@acme/web","version":"2.0.0"}`)
	InvalidateLoadCache(ws.Root)
	degraded, err := Load(ws.Root)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	outcome := syncOnce(t, degraded, []ProviderBinding{tsBinding})

	stored, err := LoadSnapshot(ws.Root)
	if err != nil || stored == nil {
		t.Fatalf("snapshot = %+v err = %v", stored, err)
	}
	if len(stored.Providers) != 2 {
		t.Fatalf("recorded providers = %d (%v), want the unresolved provider's answer kept",
			len(stored.Providers), snapshotProviderNames(stored))
	}
	var carried *SnapshotProvider
	for i := range stored.Providers {
		if stored.Providers[i].Extension == "@putnami/go" {
			carried = &stored.Providers[i]
		}
	}
	if carried == nil {
		t.Fatalf("the unresolved provider is gone from the index: %v", snapshotProviderNames(stored))
	}
	if len(carried.Inputs) == 0 {
		t.Error("the carried provider lost its recorded inputs; the snapshot would stay valid across any change to them")
	}

	// The VIEW this run adopted must agree with what it persisted, or the next
	// load would resolve a different workspace than the run that wrote the index.
	svc := degraded.ProjectByPath("svc")
	if svc == nil || svc.Name != "acme/svc" || svc.Type != "library" {
		t.Fatalf("svc identity = %+v, want the carried-forward provider answer", svc)
	}
	if len(svc.Dependencies) != 1 || svc.Dependencies[0] != degraded.ProjectByPath("web").Name {
		t.Fatalf("svc dependencies = %v, want the carried-forward edge", svc.Dependencies)
	}

	// And it must SAY so: "the index was kept because nobody answered for it" and
	// "the index is current" are the same silence otherwise.
	var named bool
	for _, d := range outcome.Diagnostics {
		if strings.Contains(d.Message, "@putnami/go") && strings.Contains(d.Message, "kept") {
			named = true
		}
	}
	if !named {
		t.Errorf("no diagnostic names the provider whose answer was carried forward: %+v", outcome.Diagnostics)
	}
}

// The invalid-manifest warning is a statement about the tree, not an
// announcement.
//
// A provider's `invalid-manifest` diagnostic IS persisted, inside
// SnapshotProvider.Result.Diagnostics — but nothing read it back, so the
// zero-probe path produced an empty outcome. Run 1 after corrupting a manifest
// printed the warning and recorded the new (invalid) digest; runs 2..n printed
// NOTHING while the project stayed degraded and renamed to its directory
// basename. engine/workspace.go still promises this warning by name.
func TestSynchronize_FreshRunReplaysRecordedDiagnostics(t *testing.T) {
	ws := syncFixture(t)
	provider := &countingProvider{name: "@putnami/typescript", answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
		return wsproto.ProbeResult{
			Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/typescript",
			Projects: []wsproto.ProbeProject{{Path: "web"}},
			Diagnostics: []diag.Diagnostic{
				diag.Warningf("invalid-manifest", "web/package.json", "package.json is not valid JSON"),
			},
		}
	}}
	bindings := []ProviderBinding{{
		Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: provider,
	}}

	probed := syncOnce(t, ws, bindings)
	if !hasInvalidManifestDiagnostic(probed.Diagnostics) {
		t.Fatalf("the probing run did not report the provider's finding: %+v", probed.Diagnostics)
	}

	// A brand-new workspace over the same tree: nothing probes, and the finding
	// must still be reported.
	InvalidateLoadCache(ws.Root)
	replayed, err := Load(ws.Root)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	replayedProvider := &countingProvider{name: "@putnami/typescript"}
	outcome := syncOnce(t, replayed, []ProviderBinding{{
		Scope:    scopeFor(t, replayedProvider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: replayedProvider,
	}})
	if !outcome.Fresh || replayedProvider.calls != 0 {
		t.Fatalf("replay: fresh=%v calls=%d, want a zero-process replay", outcome.Fresh, replayedProvider.calls)
	}
	if !hasInvalidManifestDiagnostic(outcome.Diagnostics) {
		t.Fatalf("a zero-probe run went silent about a degraded project: %+v", outcome.Diagnostics)
	}
}

// A provider that ACTUALLY probed contributes its fresh findings and must not
// also contribute its recorded copy, or every probing run would print each
// finding twice.
func TestSynchronize_ProbingRunDoesNotDoublePrintARecordedDiagnostic(t *testing.T) {
	ws := syncFixture(t)
	answer := func(wsproto.ProbeRequest) wsproto.ProbeResult {
		return wsproto.ProbeResult{
			Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/typescript",
			Projects: []wsproto.ProbeProject{{Path: "web"}},
			Diagnostics: []diag.Diagnostic{
				diag.Warningf("invalid-manifest", "web/package.json", "package.json is not valid JSON"),
			},
		}
	}
	provider := &countingProvider{name: "@putnami/typescript", answer: answer}
	bindings := []ProviderBinding{{
		Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
		Provider: provider,
	}}
	syncOnce(t, ws, bindings)

	// Move the provider's own metadata input so this run re-probes it.
	writeFileAt(t, filepath.Join(ws.Root, "web", "package.json"), `{"name":"@acme/web","version":"3.0.0"}`)
	outcome := syncOnce(t, ws, bindings)
	if len(outcome.Probed) != 1 {
		t.Fatalf("probed = %v, want the provider re-asked", outcome.Probed)
	}
	if got := countInvalidManifestDiagnostics(outcome.Diagnostics); got != 1 {
		t.Fatalf("the same finding was reported %d times on one probing run: %+v", got, outcome.Diagnostics)
	}
}

func hasInvalidManifestDiagnostic(diagnostics []diag.Diagnostic) bool {
	return countInvalidManifestDiagnostics(diagnostics) > 0
}

func countInvalidManifestDiagnostics(diagnostics []diag.Diagnostic) int {
	count := 0
	for _, d := range diagnostics {
		if d.Code == "invalid-manifest" {
			count++
		}
	}
	return count
}
