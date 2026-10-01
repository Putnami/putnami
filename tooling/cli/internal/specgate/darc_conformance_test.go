package specgate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The cli domain's DARC declarations are contracts about THIS package's
// behavior. This test joins the declared halves to the code constants the gate
// actually runs on, so the manifest cannot rot into fiction:
//
//   - the verification-observations import is a per-session SNAPSHOT carried
//     as a file artifact — the same artifact ID the collector scans for — and
//     a missing observation is declared fail-closed, because the gate never
//     infers support: a check a consulted source did not report is sanctioned,
//     and a check no source could be consulted for is reported unobserved and
//     warned, never read as verified;
//   - the workspace-probe projection is ACTIVE, and its local-model field
//     names, its writer and its freshness bound are joined to the constants
//     the recorded index is actually read with. The declaration used to say
//     `planned` precisely because the file carried none of them; holding the
//     two together is what keeps the flip from rotting back into a claim.
//
// The manifest is decoded through a minimal local view rather than the
// architecture protocol package: core deliberately does not depend on that
// module, and `architecture validate` in the required gate already enforces
// the full strict contract over this same file.
func TestDARCDeclarationsMatchTheGateTheyDescribe(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.architecture.json"))
	if err != nil {
		t.Fatalf("read the cli domain manifest: %v", err)
	}
	var manifest struct {
		Imports []struct {
			ID        string `json:"id"`
			Mode      string `json:"mode"`
			Status    string `json:"status"`
			Transport *struct {
				Kind     string `json:"kind"`
				Contract string `json:"contract"`
			} `json:"transport"`
			Consistency *struct {
				MaxStaleness string `json:"maxStaleness"`
				OnMissing    string `json:"onMissing"`
				OnStale      string `json:"onStale"`
			} `json:"consistency"`
			LocalModel *struct {
				ProvenanceField string `json:"provenanceField"`
				ObservedAtField string `json:"observedAtField"`
				FreshnessField  string `json:"freshnessField"`
				Writer          string `json:"writer"`
			} `json:"localModel"`
		} `json:"imports"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode the cli domain manifest: %v", err)
	}

	byID := make(map[string]int, len(manifest.Imports))
	for index, imported := range manifest.Imports {
		byID[imported.ID] = index
	}

	index, found := byID["cli.verification-observations.v1"]
	if !found {
		t.Fatal("the cli domain no longer declares its verification-observations import")
	}
	observations := manifest.Imports[index]
	if observations.Mode != "snapshot" || observations.Status != "active" {
		t.Errorf("observations import = mode %q status %q, want an active snapshot", observations.Mode, observations.Status)
	}
	if observations.Transport == nil || observations.Transport.Kind != "file" {
		t.Errorf("observations transport = %+v, want a file artifact", observations.Transport)
	}
	// One artifact, two spellings: the wire artifact ID is one dashed token,
	// the DARC contract ID is namespace-dotted and versioned. The test derives
	// one from the other so renaming either side breaks loudly here instead of
	// silently unlinking the contract from the collector.
	wantContract := "putnami." + strings.TrimPrefix(features.VerificationReportArtifactID, "putnami-") + ".v1"
	if observations.Transport != nil && observations.Transport.Contract != wantContract {
		t.Errorf("observations transport contract = %q, want %q (derived from features.VerificationReportArtifactID)",
			observations.Transport.Contract, wantContract)
	}
	if observations.Consistency == nil || observations.Consistency.OnMissing != "fail-closed" {
		t.Errorf("observations onMissing = %+v, want fail-closed: a missing observation never grants support — it sanctions when a source was consulted and warns when none could be", observations.Consistency)
	}

	index, found = byID["cli.workspace-probe-view.v1"]
	if !found {
		t.Fatal("the cli domain no longer declares its workspace-probe projection")
	}
	probe := manifest.Imports[index]
	if probe.Mode != "projection" || probe.Status != "active" {
		t.Errorf("probe import = mode %q status %q, want an active projection", probe.Mode, probe.Status)
	}
	if probe.LocalModel == nil {
		t.Fatal("the probe projection declares no local model")
	}
	// The three local-model fields, joined to the constants the recorded index
	// is read with. A rename on either side breaks here instead of silently
	// unlinking the contract from the code that keeps it.
	for _, field := range []struct {
		name      string
		declared  string
		enforcing string
	}{
		{"provenanceField", probe.LocalModel.ProvenanceField, workspace.RecordedProvenanceField},
		{"observedAtField", probe.LocalModel.ObservedAtField, workspace.RecordedObservedAtField},
		{"freshnessField", probe.LocalModel.FreshnessField, workspace.RecordedFreshnessField},
	} {
		if field.declared != field.enforcing {
			t.Errorf("probe projection %s = %q, want %q (workspace.Recorded*Field)", field.name, field.declared, field.enforcing)
		}
	}
	if probe.LocalModel.Writer != workspace.RecordedWriter {
		t.Errorf("probe projection writer = %q, want %q", probe.LocalModel.Writer, workspace.RecordedWriter)
	}
	if probe.Consistency == nil {
		t.Fatal("the probe projection declares no consistency block")
	}
	// The declared bound and the bound the reader enforces are one value. A
	// declaration that promised an hour while the code allowed a day would be a
	// contract nothing keeps.
	bound, err := time.ParseDuration(probe.Consistency.MaxStaleness)
	if err != nil || bound != workspace.RecordedMaxStaleness {
		t.Errorf("probe maxStaleness = %q, want %s (workspace.RecordedMaxStaleness)",
			probe.Consistency.MaxStaleness, workspace.RecordedMaxStaleness)
	}
	// The two behaviors the read surfaces implement: an absent copy refuses, a
	// stale one is answered from and marked.
	if probe.Consistency.OnMissing != "fail-closed" {
		t.Errorf("probe onMissing = %q, want fail-closed: a graph tool with no recorded copy must refuse, not answer an empty graph",
			probe.Consistency.OnMissing)
	}
	if probe.Consistency.OnStale != "use-stale" {
		t.Errorf("probe onStale = %q, want use-stale: an old copy is answered from and stamped, never withheld", probe.Consistency.OnStale)
	}
	if (workspace.RecordedView{Freshness: workspace.RecordedAbsent}).Usable() {
		t.Error("an absent recorded view reports itself usable; fail-closed would then answer from nothing")
	}
	if !(workspace.RecordedView{Freshness: workspace.RecordedStale}).Usable() {
		t.Error("a stale recorded view reports itself unusable; use-stale would then withhold an answer it should serve")
	}
}
