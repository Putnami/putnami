package architecture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	"go.putnami.dev/protocol/features/spectest"
)

// validDomain is one authoring that exercises every shape the package offers:
// owned concepts, an export with facts and modes, and all five import modes,
// including a projection with the whole consistency/deletion/local-model
// contract. Each rejection case below is one compiling mutation of it.
func validDomain() *Builder {
	return NewDomain("observability", "observability").
		Projects("/sites/telemetry.putnami.dev").
		Owns(
			Concept("observability.usage-aggregates", archproto.OwnershipModel,
				"Daily anonymous CLI-usage aggregates; retention and bucketing are local authority."),
			Concept("observability.retention-policy", archproto.OwnershipFact,
				"Observability-owned retention configuration; never projected from another domain."),
		).
		Export(archproto.Export{
			ID:          "observability.usage-ingest.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "The anonymous CLI-usage ingest.",
			Facts: []archproto.Fact{
				Fact("usage_ingest", "observability", archproto.ClassificationInternal, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeCommand},
			Compatibility: Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Import(
			Reference("observability.protocol-contracts.v1", 1, From("protocols", "protocols.wire-contracts.v1"),
				As("observability.protocol-contracts"),
				Status(archproto.StatusActive),
				Facts("wire_contract_definitions"),
				Justification("The workload consumes the shared strict contract packages, never a private parse."),
				Bind("/sites/telemetry.putnami.dev", "/protocols/telemetry"),
			),
			Query("observability.runtime-placement.v1", 1, From("runtime", "runtime.placement.v1"),
				Carried(archproto.TransportAPI, "runtime.placement.v1", archproto.StatusPlanned),
				As("observability.runtime-placement"),
				Facts("region"),
				Guarantees(archproto.Consistency{
					MaxStaleness:  "5m",
					OnMissing:     archproto.FailureFailClosed,
					OnStale:       archproto.FailureFailClosed,
					Ordering:      archproto.OrderingNone,
					SourceVersion: "source_version",
					LateEvents:    archproto.LateEventReject,
				}),
				Justification("A rare fresh read of the placement a workload was assigned."),
			),
			Snapshot("observability.release-manifest.v1", 1, From("runtime", "runtime.release.v1"),
				Carried(archproto.TransportFile, "runtime.release-manifest.v1", archproto.StatusPlanned),
				As("observability.release-manifest"),
				Facts("release_id"),
				Guarantees(archproto.Consistency{
					MaxStaleness:  "1h",
					OnMissing:     archproto.FailureFailClosed,
					OnStale:       archproto.FailureFailClosed,
					Ordering:      archproto.OrderingNone,
					SourceVersion: "release_id",
					LateEvents:    archproto.LateEventReject,
				}),
				Justification("An immutable per-release attachment, replaced rather than updated."),
			),
			Command("observability.retention-sweep.v1", 1, From("runtime", "runtime.sweep.v1"),
				Carried(archproto.TransportEvent, "runtime.retention-sweep.v1", archproto.StatusPlanned),
				As("observability.retention-sweep"),
				Facts("sweep_request"),
				Justification("Observability asks Runtime to reclaim expired storage; Runtime decides whether it happens."),
			),
			Projection("observability.workspace-context.v1", 1, From("runtime", "runtime.workspace-binding.v1"),
				Carried(archproto.TransportAPI, "runtime.workspace-bindings.v1", archproto.StatusPlanned),
				Carried(archproto.TransportEvent, "runtime.workspace-binding-changed.v1", archproto.StatusPlanned),
				As("observability.workspace-context"),
				Facts("workspace_id", "region", "source_version"),
				Guarantees(archproto.Consistency{
					MaxStaleness:   "5m",
					OnMissing:      archproto.FailureFailClosed,
					OnStale:        archproto.FailureUseStale,
					Ordering:       archproto.OrderingSourceVersion,
					SourceVersion:  "source_version",
					IdempotencyKey: "event_id",
					LateEvents:     archproto.LateEventIgnoreOlder,
				}),
				Tombstones("deleted_at"),
				Model(archproto.LocalModel{
					Name:            "observability.workspace-context",
					Kind:            archproto.LocalModelProjection,
					SourceIdentity:  "workspace_id",
					ProjectedFields: []string{"workspace_id", "region", "source_version"},
					LocalFields:     []string{"retention_policy"},
					ProvenanceField: "source_contract",
					ObservedAtField: "observed_at",
					FreshnessField:  "freshness_state",
					Writer:          "observability.workspace-context-projector",
					Rebuildable:     true,
					Rebuild:         archproto.RebuildBootstrapAndReplay,
				}),
				Justification("Local routing facts without making Runtime a per-request lookup service."),
			),
		)
}

// TestCanonicalBytesRoundTripThroughTheStrictReader is the round trip the whole
// package rests on: what the builder emits must be exactly what the gate's own
// strict reader accepts and re-renders, or a committed projection of an
// authoring would be a file `architecture validate` rejects.
func TestCanonicalBytesRoundTripThroughTheStrictReader(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "architecture-authored-in-code",
		"the-canonical-rendering-round-trips-through-the-strict-reader")
	data, err := validDomain().CanonicalBytes()
	if err != nil {
		t.Fatalf("a valid authoring was rejected: %v", err)
	}
	if !strings.HasSuffix(string(data), "}\n") {
		t.Errorf("canonical bytes do not end in one trailing newline: %q", tail(string(data)))
	}
	if !strings.Contains(string(data), "\n  \"protocolVersion\": 1,") {
		t.Error("canonical bytes are not two-space indented")
	}

	parsed, diagnostics := archproto.ParseAndValidateManifest(data)
	if parsed == nil || len(diagnostics) > 0 {
		t.Fatalf("the strict reader rejected the builder's own output: %v", diagnostics)
	}
	rendered, err := archproto.MarshalManifest(parsed)
	if err != nil {
		t.Fatalf("re-render: %v", err)
	}
	if string(rendered) != string(data) {
		t.Errorf("the round trip is not a fixed point:\n emitted:\n%s\n re-rendered:\n%s", data, rendered)
	}
}

func TestReferenceFactOmitsOnlyReferenceOnlyDataMetadata(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "architecture-authored-in-code",
		"reference-only-facts-do-not-require-placeholder-data-metadata")

	referenceOnly := NewDomain("protocols", "protocols").Export(archproto.Export{
		ID:          "protocols.wire-contracts.v1",
		Version:     1,
		Status:      archproto.StatusActive,
		Description: "Shared code contracts.",
		Facts: []archproto.Fact{
			ReferenceFact("wire_contract_definitions", "protocols"),
		},
		Modes:         []archproto.AccessMode{archproto.ModeReference},
		Compatibility: Compatible(archproto.CompatibilityAdditive, 1),
	})
	data, err := referenceOnly.CanonicalBytes()
	if err != nil {
		t.Fatalf("reference-only authoring was rejected: %v", err)
	}
	if strings.Contains(string(data), `"classification"`) || strings.Contains(string(data), `"personalData"`) {
		t.Fatalf("reference-only authoring emitted placeholder metadata:\n%s", data)
	}

	mixed := NewDomain("protocols", "protocols").Export(archproto.Export{
		ID:          "protocols.wire-contracts.v1",
		Version:     1,
		Status:      archproto.StatusActive,
		Description: "Shared code and query contracts.",
		Facts: []archproto.Fact{
			ReferenceFact("wire_contract_definitions", "protocols"),
		},
		Modes:         []archproto.AccessMode{archproto.ModeReference, archproto.ModeQuery},
		Compatibility: Compatible(archproto.CompatibilityAdditive, 1),
	})
	_, err = mixed.Build()
	violation := &ValidationError{}
	if !errors.As(err, &violation) || !contains(violation.Codes(), archproto.ErrorCodeInvalidFact) {
		t.Fatalf("mixed-mode ReferenceFact error = %v, want %s", err, archproto.ErrorCodeInvalidFact)
	}
}

// TestCanonicalBytesIgnoreAuthoringOrder pins that authoring order is not
// observable in the projection. Two programs that declare the same domain must
// commit the same file, or "the builder is the author" would depend on which
// line someone happened to write first.
func TestCanonicalBytesIgnoreAuthoringOrder(t *testing.T) {
	forward, err := validDomain().CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	shuffled := validDomain()
	reversed := NewDomain("observability", "observability").
		Projects(shuffled.manifest.Projects...).
		Owns(shuffled.manifest.Owns[1], shuffled.manifest.Owns[0]).
		Export(shuffled.manifest.Exports...)
	for index := len(shuffled.manifest.Imports) - 1; index >= 0; index-- {
		reversed = reversed.Import(shuffled.manifest.Imports[index])
	}
	backward, err := reversed.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(forward) != string(backward) {
		t.Error("authoring order changed the projection; the canonical writer is not canonicalizing")
	}
}

// TestBuildRefusesAnInvalidAuthoring is the heart of "fails at authoring, not in
// a consumer's plan": each case is one compiling mutation of the valid domain
// that breaks exactly one protocol invariant, and each must be rejected with the
// code automation keys on.
func TestBuildRefusesAnInvalidAuthoring(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "architecture-authored-in-code",
		"building-refuses-an-invalid-domain-manifest")
	cases := []struct {
		name   string
		mutate func(*Builder)
		code   string
	}{
		{
			name:   "IDENTITY: a domain that is not a lower-case semantic ID",
			mutate: func(b *Builder) { b.manifest.Domain = "Observability" },
			code:   archproto.ErrorCodeInvalidDomain,
		},
		{
			name:   "IDENTITY: a project that is not one absolute Putnami project ID",
			mutate: func(b *Builder) { b.Projects("sites/telemetry.putnami.dev") },
			code:   archproto.ErrorCodeInvalidProject,
		},
		{
			name:   "IDENTITY: the same project twice",
			mutate: func(b *Builder) { b.Projects("/sites/telemetry.putnami.dev") },
			code:   archproto.ErrorCodeDuplicateProject,
		},
		{
			name: "IDENTITY: an export ID whose version suffix contradicts its version",
			mutate: func(b *Builder) {
				b.manifest.Exports[0].Version = 2
			},
			code: archproto.ErrorCodeInvalidID,
		},
		{
			name: "IDENTITY: two exports under one ID",
			mutate: func(b *Builder) {
				b.Export(b.manifest.Exports[0])
			},
			code: archproto.ErrorCodeDuplicateExport,
		},
		{
			name: "IDENTITY: an export that exposes nothing",
			mutate: func(b *Builder) {
				b.manifest.Exports[0].Facts = nil
			},
			code: archproto.ErrorCodeInvalidFact,
		},
		{
			name: "IDENTITY: an export that allows no access mode",
			mutate: func(b *Builder) {
				b.manifest.Exports[0].Modes = nil
			},
			code: archproto.ErrorCodeInvalidMode,
		},
		{
			name: "MODE SEMANTICS: a projection with no updates carrier",
			mutate: func(b *Builder) {
				b.manifest.Imports[4].Updates = nil
			},
			code: archproto.ErrorCodeInvalidProjection,
		},
		{
			name: "MODE SEMANTICS: a projection whose projected fields are not its imported facts",
			mutate: func(b *Builder) {
				b.manifest.Imports[4].LocalModel.ProjectedFields = []string{"workspace_id"}
			},
			code: archproto.ErrorCodeInvalidProjection,
		},
		{
			name: "MODE SEMANTICS: a query with no transport",
			mutate: func(b *Builder) {
				b.manifest.Imports[1].Transport = nil
			},
			code: archproto.ErrorCodeInvalidTransport,
		},
		{
			name: "MODE SEMANTICS: an unbounded freshness window",
			mutate: func(b *Builder) {
				b.manifest.Imports[1].Consistency.MaxStaleness = "eventually"
			},
			code: archproto.ErrorCodeInvalidConsistency,
		},
		{
			name: "MODE SEMANTICS: a tombstone field on a strategy that has none",
			mutate: func(b *Builder) {
				b.manifest.Imports[4].Deletion = &archproto.Deletion{
					Strategy:       archproto.DeletionHardDelete,
					TombstoneField: "deleted_at",
				}
			},
			code: archproto.ErrorCodeInvalidDeletion,
		},
		{
			name: "PLANNED IS NOT OBSERVED: a planned import that claims a current binding",
			mutate: func(b *Builder) {
				b.manifest.Imports[0].Status = archproto.StatusPlanned
			},
			code: archproto.ErrorCodeInvalidBinding,
		},
		{
			name: "PLANNED IS NOT OBSERVED: an active import carried by a planned transport",
			mutate: func(b *Builder) {
				b.manifest.Imports[1].Status = archproto.StatusActive
			},
			code: archproto.ErrorCodeInvalidStatus,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			builder := validDomain()
			testCase.mutate(builder)
			_, err := builder.Build()
			if err == nil {
				t.Fatalf("the authoring was accepted; want %s", testCase.code)
			}
			violation := &ValidationError{}
			ok := errors.As(err, &violation)
			if !ok {
				t.Fatalf("error = %T, want *ValidationError", err)
			}
			if !contains(violation.Codes(), testCase.code) {
				t.Fatalf("codes = %v, want one %s\n%s", violation.Codes(), testCase.code, violation)
			}
		})
	}
}

// TestImportConstructorsCannotStateAModeTheyDoNotMean pins the reason there are
// five constructors instead of one mode option: the members a mode may carry are
// set by the constructor, so the combinations the protocol rejects are not
// writable through this package at all.
func TestImportConstructorsCannotStateAModeTheyDoNotMean(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "architecture-authored-in-code",
		"the-import-constructors-cannot-state-a-mode-they-do-not-mean")
	reference := Reference("d.a.v1", 1, From("p", "p.e.v1"))
	if reference.Mode != archproto.ModeReference || reference.Transport != nil ||
		reference.Bootstrap != nil || reference.Updates != nil {
		t.Errorf("reference = %+v, want no carrier at all", reference)
	}
	for name, imported := range map[string]archproto.Import{
		"query":    Query("d.a.v1", 1, From("p", "p.e.v1"), NoCarrier(archproto.StatusActive)),
		"snapshot": Snapshot("d.a.v1", 1, From("p", "p.e.v1"), NoCarrier(archproto.StatusActive)),
		"command":  Command("d.a.v1", 1, From("p", "p.e.v1"), NoCarrier(archproto.StatusActive)),
	} {
		if imported.Transport == nil || imported.Bootstrap != nil || imported.Updates != nil {
			t.Errorf("%s = %+v, want exactly one transport", name, imported)
		}
	}
	projection := Projection("d.a.v1", 1, From("p", "p.e.v1"),
		NoCarrier(archproto.StatusActive), NoCarrier(archproto.StatusActive))
	if projection.Transport != nil || projection.Bootstrap == nil || projection.Updates == nil {
		t.Errorf("projection = %+v, want bootstrap and updates and no ambiguous transport", projection)
	}
	if got := NoCarrier(archproto.StatusActive).Contract; got != "" {
		t.Errorf("NoCarrier named contract %q; a none transport cannot carry one", got)
	}
	if projection.Status != archproto.StatusPlanned {
		t.Errorf("default status = %q, want planned: an authoring that forgets to say otherwise describes an intention",
			projection.Status)
	}
}

// TestAuthoringNeverMintsAPermission is the red line, stated as a test.
//
// Bind is the ONLY way a binding enters a manifest through this package, it
// takes two exact project IDs from the caller, and it reads nothing — no
// workspace, no graph, no detector. There is no constructor that turns an
// observed edge into an authorization, because that is the "debt becomes
// permission" anti-pattern ADR 0001 forbids.
func TestAuthoringNeverMintsAPermission(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "architecture-authored-in-code",
		"authoring-never-mints-a-permission")
	built, err := validDomain().Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, imported := range built.Imports {
		for _, binding := range imported.Bindings {
			if binding.Kind != archproto.BindingProjectDependency {
				t.Errorf("import %s authorized binding kind %q; only an exact project dependency is authorizable in v1",
					imported.ID, binding.Kind)
			}
		}
	}
	bare, err := NewDomain("observability", "observability").
		Projects("/sites/telemetry.putnami.dev").
		Import(Reference("observability.protocol-contracts.v1", 1, From("protocols", "protocols.wire-contracts.v1"),
			As("observability.protocol-contracts"),
			Status(archproto.StatusActive),
			Facts("wire_contract_definitions"),
			Justification("Nothing here authorizes a project edge unless the author wrote Bind."),
		)).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(bare.Imports[0].Bindings) != 0 {
		t.Errorf("an import authored without Bind carries %d binding(s); a declaration must never grow a permission by itself",
			len(bare.Imports[0].Bindings))
	}
}

// TestBuildDoesNotAliasTheBuilder pins that a built manifest is independent:
// mutating it must not reach back into the builder, or a program that edits its
// result would corrupt what it writes next.
func TestBuildDoesNotAliasTheBuilder(t *testing.T) {
	builder := validDomain()
	first, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	first.Projects[0] = "/elsewhere"
	first.Imports[0].Bindings = append(first.Imports[0].Bindings, archproto.Binding{
		Kind:            archproto.BindingProjectDependency,
		ConsumerProject: "/sites/telemetry.putnami.dev",
		ProducerProject: "/protocols/storage",
	})

	second, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Projects[0]; got != "/sites/telemetry.putnami.dev" {
		t.Errorf("editing a built manifest reached the builder: project = %q", got)
	}
	if got := len(second.Imports[0].Bindings); got != 1 {
		t.Errorf("editing a built manifest added %d binding(s) to the builder", got-1)
	}
}

// TestPinHoldsTheCommittedFileToTheAuthoring exercises both directions of the
// pin over a real file: the projection of an authoring passes, and a one-token
// tamper of that same file fails. The second half is what keeps the first from
// being vacuous — a comparison that passed on everything would look identical to
// a passing one.
func TestPinHoldsTheCommittedFileToTheAuthoring(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "architecture-authored-in-code",
		"the-pin-fails-on-a-hand-edited-manifest")
	data, err := validDomain().CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), archproto.ManifestFilename)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	clean := &recordingReporter{}
	pin(clean, path, validDomain)
	if len(clean.failures) != 0 {
		t.Fatalf("the projection of the authoring did not pass its own pin:\n%s",
			strings.Join(clean.failures, "\n"))
	}

	edited := strings.Replace(string(data), `"maxStaleness": "5m"`, `"maxStaleness": "6m"`, 1)
	if edited == string(data) {
		t.Fatal("the mutation did not apply; this harness is no longer checking anything")
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	tampered := &recordingReporter{}
	pin(tampered, path, validDomain)
	if len(tampered.failures) == 0 {
		t.Fatal("a hand-edited manifest still matched the authored one; the comparison is not comparing")
	}
	if !strings.Contains(tampered.failures[0], "is not the document the builder authors") {
		t.Errorf("failure = %q, want the one-author message", tampered.failures[0])
	}
}

// TestPinRefusesAManifestTheGateWouldReject pins that the committed side is read
// through the protocol's strict reader, not merely compared: a file the gate
// would refuse must fail here first, in the project that owns it.
func TestPinRefusesAManifestTheGateWouldReject(t *testing.T) {
	path := filepath.Join(t.TempDir(), archproto.ManifestFilename)
	if err := os.WriteFile(path, []byte(`{"protocolVersion": 1, "domain": "x", "surprise": true}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := &recordingReporter{}
	pin(broken, path, validDomain)
	if len(broken.failures) == 0 {
		t.Fatal("a manifest with an unknown field passed the pin")
	}
	if !strings.Contains(broken.failures[0], "is not a valid architecture manifest") {
		t.Errorf("failure = %q, want the strict-reader refusal", broken.failures[0])
	}
}

// recordingReporter stands in for *testing.T so this package can assert that the
// pin FAILS. testing.TB cannot be implemented outside the standard library, and
// a pin nobody proved can fail is not a pin.
type recordingReporter struct{ failures []string }

func (r *recordingReporter) Helper() {}

func (r *recordingReporter) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func (r *recordingReporter) Fatalf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func tail(text string) string {
	if len(text) <= 16 {
		return text
	}
	return text[len(text)-16:]
}
