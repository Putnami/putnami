package darc

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.putnami.dev/app"
	archproto "go.putnami.dev/protocol/architecture"
)

// referenceContract is the shape the protocol requires of a reference import:
// no carrier machinery at all, and a minimized fact list.
func referenceContract(mutate ...func(*archproto.Import)) archproto.Import {
	contract := archproto.Import{
		ID:            "observability.protocol-contracts.v1",
		Version:       1,
		From:          archproto.ExportReference{Domain: "protocols", Export: "protocols.wire-contracts.v1"},
		As:            "observability.protocol-contracts",
		Mode:          archproto.ModeReference,
		Status:        archproto.StatusActive,
		Facts:         []string{"wire_contract_definitions"},
		Justification: "The workload consumes the shared strict contract packages, never a private parse.",
	}
	for _, apply := range mutate {
		apply(&contract)
	}
	return contract
}

// TestReferenceEnforcesMinimization is the one obligation a reference carries:
// an import names the exact facts it consumes, because a fact nobody uses is a
// permission nobody needed. Reaching past the declared surface fails where it
// happens.
func TestReferenceEnforcesMinimization(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "darc-runtime-evidence",
		"a-reference-refuses-a-fact-its-import-does-not-name")
	reference, err := NewReference(referenceContract())
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := reference.Fact("wire_contract_definitions")
	if err != nil {
		t.Fatalf("the declared fact was refused: %v", err)
	}
	if provenance != "protocols.wire-contracts.v1" {
		t.Errorf("provenance = %q, want the producer export the import names", provenance)
	}
	if _, err := reference.Fact("retention_policy"); !errors.Is(err, ErrFactNotImported) {
		t.Errorf("err = %v, want ErrFactNotImported for a fact outside the minimized list", err)
	}
	if got := reference.Facts(); !reflect.DeepEqual(got, []string{"wire_contract_definitions"}) {
		t.Errorf("facts = %v, want the minimized list", got)
	}

	if _, err := NewReference(referenceContract(func(i *archproto.Import) {
		i.Status = archproto.StatusPlanned
	})); !errors.Is(err, ErrNotActive) {
		t.Errorf("err = %v, want ErrNotActive: a planned reference is a target", err)
	}
	if _, err := NewReference(referenceContract(func(i *archproto.Import) {
		i.Mode = archproto.ModeCommand
	})); err == nil {
		t.Error("a command contract was accepted by the reference component")
	}
}

// TestEvidenceCarriesTheContractVerbatim pins the row a component contributes.
// Every value comes from the contract the component was built with — and that
// contract already passed the protocol's validation — so a row can never claim a
// mode or a behavior the gate would reject.
func TestEvidenceCarriesTheContractVerbatim(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "darc-runtime-evidence",
		"an-evidence-row-carries-the-declared-contract-verbatim")
	projection := bootstrapping(t, projectionContract(), newTestClock())
	row := Evidence(projection)

	want := app.DomainAccessContract{
		Import: "observability.workspace-context.v1",
		Mode:   "projection",
		Status: "active",
		Transports: []app.DomainAccessTransport{
			{Role: "bootstrap", Kind: "api", Contract: "runtime.workspace-bindings.v1", Availability: "active"},
			{Role: "updates", Kind: "event", Contract: "runtime.workspace-binding-changed.v1", Availability: "active"},
		},
		Enforced: app.DomainAccessEnforcement{
			MaxStaleness: "5m",
			OnMissing:    "fail-closed",
			OnStale:      "use-stale",
			Ordering:     "source-version",
			LateEvents:   "ignore-older",
			Deletion:     "tombstone",
			Writer:       "observability.workspace-context-projector",
			Rebuild:      "bootstrap",
		},
	}
	if !reflect.DeepEqual(row, want) {
		t.Errorf("evidence row:\n%+v\nwant:\n%+v", row, want)
	}
}

// TestEvidenceStatesOnlyWhatTheModeCarries pins that a mode's absent halves stay
// absent. A reference declares no carrier and no consistency, and a row that
// filled those in with empty strings would report parameters nobody declared.
func TestEvidenceStatesOnlyWhatTheModeCarries(t *testing.T) {
	reference, err := NewReference(referenceContract())
	if err != nil {
		t.Fatal(err)
	}
	row := Evidence(reference)
	if len(row.Transports) != 0 {
		t.Errorf("transports = %+v, want none: a reference declares no carrier", row.Transports)
	}
	if row.Enforced != (app.DomainAccessEnforcement{}) {
		t.Errorf("enforced = %+v, want empty: a reference enforces none of these parameters", row.Enforced)
	}

	command, err := NewCommand(commandContract(), func(context.Context, usageRecord) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	row = Evidence(command)
	if len(row.Transports) != 1 || row.Transports[0].Role != "transport" {
		t.Errorf("transports = %+v, want the single declared carrier", row.Transports)
	}
	if row.Enforced != (app.DomainAccessEnforcement{}) {
		t.Errorf("enforced = %+v, want empty: this command declares no consistency block", row.Enforced)
	}
}

// TestPluginReportsEveryComponentOnce pins the describe carrier: deterministic
// order, and one row per (import, mode) however many times a component is
// registered.
func TestPluginReportsEveryComponentOnce(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "darc-runtime-evidence",
		"a-registered-component-contributes-exactly-one-row")
	projection := bootstrapping(t, projectionContract(), newTestClock())
	reference, err := NewReference(referenceContract())
	if err != nil {
		t.Fatal(err)
	}
	command, err := NewCommand(commandContract(), func(context.Context, usageRecord) error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	plugin := NewPlugin("contracts", command, projection).Register(reference, projection, nil)
	if got := plugin.Name(); got != "contracts" {
		t.Errorf("name = %q, want the plugin identity", got)
	}
	rows := plugin.DomainAccessContracts()
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want one per distinct component: %+v", len(rows), rows)
	}
	wantOrder := []string{
		"cli.usage-telemetry.v1",
		"observability.protocol-contracts.v1",
		"observability.workspace-context.v1",
	}
	for index, want := range wantOrder {
		if rows[index].Import != want {
			t.Errorf("row %d = %q, want %q (rows are ordered by import then mode)", index, rows[index].Import, want)
		}
	}

	// The plugin is what the describe pass walks, so it must satisfy the
	// app-owned seam: a compile-time assertion here is what keeps a rename in
	// app from silently unlinking the evidence channel.
	var _ app.DomainAccessContributor = plugin
	var _ app.Plugin = plugin
}

// TestPluginTakesNoPartInTheLifecycle pins that the describe carrier is exactly
// that. A plugin that quietly became a Configurer or a Starter would give a
// build-time inventory a runtime cost nobody asked for.
func TestPluginTakesNoPartInTheLifecycle(t *testing.T) {
	plugin := NewPlugin("contracts")
	if _, isConfigurer := any(plugin).(app.Configurer); isConfigurer {
		t.Error("the evidence plugin configures; it must only describe")
	}
	if _, isStarter := any(plugin).(app.Starter); isStarter {
		t.Error("the evidence plugin starts; it must only describe")
	}
	if _, isStopper := any(plugin).(app.Stopper); isStopper {
		t.Error("the evidence plugin stops; it must only describe")
	}
	if _, isProvider := any(plugin).(app.Provider); isProvider {
		t.Error("the evidence plugin provides into DI; it must only describe")
	}
	if got := plugin.DomainAccessContracts(); len(got) != 0 {
		t.Errorf("rows = %+v, want none from an empty plugin", got)
	}
}

// --- helpers the moved behavior tests used to provide ------------------------
//
// The behavior tests moved to `go.putnami.dev/protocol/architecture/darc` with
// the components they exercise, and took their shared fixtures with them. The
// evidence tests keep local copies: the contracts below are test DATA, not a
// shared vocabulary, and duplicating a fixture is cheaper than a cross-module
// test dependency.

// usageRecord is one command payload with no meaning of its own.
type usageRecord struct {
	Command string
}

// commandContract is the shape the protocol requires of a command import: one
// transport, and no projection machinery.
func commandContract(mutate ...func(*archproto.Import)) archproto.Import {
	contract := archproto.Import{
		ID:      "cli.usage-telemetry.v1",
		Version: 1,
		From:    archproto.ExportReference{Domain: "observability", Export: "observability.usage-ingest.v1"},
		As:      "cli.usage-telemetry",
		Mode:    archproto.ModeCommand,
		Status:  archproto.StatusActive,
		Facts:   []string{"usage_ingest"},
		Transport: &archproto.Transport{
			Kind: archproto.TransportEvent, Contract: "putnami.cli-usage.otlp-logs.v1",
			Availability: archproto.StatusActive,
		},
		Justification: "After the one-time notice, the CLI fire-and-forgets anonymous usage records to the ingest.",
	}
	for _, apply := range mutate {
		apply(&contract)
	}
	return contract
}

// workspaceContext is a projected value with no meaning of its own.
type workspaceContext struct {
	Region string
}

// projectionContract is the complete shape the protocol requires of a
// projection, so the evidence row it produces exercises every carried member.
func projectionContract(mutate ...func(*archproto.Import)) archproto.Import {
	contract := archproto.Import{
		ID:      "observability.workspace-context.v1",
		Version: 1,
		From:    archproto.ExportReference{Domain: "runtime", Export: "runtime.workspace-binding.v1"},
		As:      "observability.workspace-context",
		Mode:    archproto.ModeProjection,
		Status:  archproto.StatusActive,
		Facts:   []string{"workspace_id", "region", "source_version"},
		Bootstrap: &archproto.Transport{
			Kind: archproto.TransportAPI, Contract: "runtime.workspace-bindings.v1",
			Availability: archproto.StatusActive,
		},
		Updates: &archproto.Transport{
			Kind: archproto.TransportEvent, Contract: "runtime.workspace-binding-changed.v1",
			Availability: archproto.StatusActive,
		},
		Consistency: &archproto.Consistency{
			MaxStaleness:   "5m",
			OnMissing:      archproto.FailureFailClosed,
			OnStale:        archproto.FailureUseStale,
			Ordering:       archproto.OrderingSourceVersion,
			SourceVersion:  "source_version",
			IdempotencyKey: "event_id",
			LateEvents:     archproto.LateEventIgnoreOlder,
		},
		Deletion: &archproto.Deletion{Strategy: archproto.DeletionTombstone, TombstoneField: "deleted_at"},
		LocalModel: &archproto.LocalModel{
			Name:            "observability.workspace-context",
			Kind:            archproto.LocalModelProjection,
			SourceIdentity:  "workspace_id",
			ProjectedFields: []string{"workspace_id", "region", "source_version"},
			ProvenanceField: "source_contract",
			ObservedAtField: "observed_at",
			FreshnessField:  "freshness_state",
			Writer:          "observability.workspace-context-projector",
			Rebuildable:     true,
			Rebuild:         archproto.RebuildBootstrap,
		},
		Justification: "Local routing facts without making Runtime a per-request lookup service.",
	}
	for _, apply := range mutate {
		apply(&contract)
	}
	return contract
}

// testClock is a hand-wound clock, so nothing here depends on wall time.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
}

// bootstrapping builds a projection with a bootstrap source and rebuilds it.
func bootstrapping(t *testing.T, contract archproto.Import, clock *testClock, updates ...Update[workspaceContext]) *Projection[workspaceContext] {
	t.Helper()
	projection, err := NewProjection[workspaceContext](contract,
		WithClock[workspaceContext](clock.Now),
		WithBootstrap(func(context.Context) ([]Update[workspaceContext], error) { return updates, nil }))
	if err != nil {
		t.Fatalf("build the projection: %v", err)
	}
	if err := projection.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild the projection: %v", err)
	}
	return projection
}
