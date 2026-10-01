package main

import (
	"os"
	"reflect"
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	protocaps "go.putnami.dev/protocol/capabilities"
)

// The evidence half of this workload's Domain Access & Replication Contracts.
//
// `darc_conformance_test.go` pins the domain's EXPORT against the ingest it
// describes. This file pins its IMPORTS against the two artifacts that have to
// agree about them:
//
//   - `putnami.architecture.json` — the reviewed declaration, the only thing
//     that authorizes a cross-domain dependency (ADR 0001);
//   - `schema/capabilities.json` — the describe pass's record that the code
//     enforces it.
//
// `putnami architecture validate` joins those two on (consumer domain, import,
// mode) alone. That leaves a real gap: a contract in code could keep the right
// id and mode while drifting on `from`, `as`, `facts`, `version`, `status` or
// its justification, and the gate would still pass. These tests close it by
// requiring the enforced contract to equal the declared import VERBATIM, so the
// comment in `darc.go` — "the contract must stay the committed declaration" — is
// a checked statement rather than a hope.
//
// Nothing here writes evidence. The rows come from the committed manifest the
// describe pass emitted; a test that manufactured one would be exactly the
// hand-written permission the protocol forbids.

// domainAccessRow is the (import, mode, status, owner) tuple a `domainAccess`
// record has to carry. The gate joins on import and mode; owner is what makes
// the row this project's statement rather than an anonymous one.
type domainAccessRow struct {
	Import string
	Mode   string
	Status string
	Owner  string
}

// wantDomainAccessRows is the exact evidence this workload owes, one row per
// active import the observability domain declares. It is spelled out rather
// than derived so that losing a component — the failure mode that makes a
// domain silently under-report — changes this list and fails here.
//
// The set is atomic on purpose: `architecture.declared_without_evidence` fires
// inside a domain that already emits, and then for EVERY active import in it.
// Observability emits, so it emits for both or the gate rejects the half.
var wantDomainAccessRows = []domainAccessRow{
	{
		Import: "observability.application-runtime.v1",
		Mode:   "reference",
		Status: "active",
		Owner:  "telemetry.putnami.dev",
	},
	{
		Import: "observability.protocol-contracts.v1",
		Mode:   "reference",
		Status: "active",
		Owner:  "telemetry.putnami.dev",
	},
}

// TestCommittedEvidenceMatchesTheEnforcedContracts reads the committed manifest
// and requires it to carry exactly the rows the registered components produce.
//
// It compares the file with the plugin rather than only with a literal, because
// the two can drift in both directions: a component removed from the plugin
// stops emitting, and a stale committed manifest keeps claiming a component
// that no longer runs.
func TestCommittedEvidenceMatchesTheEnforcedContracts(t *testing.T) {
	rows := committedDomainAccess(t)

	got := make([]domainAccessRow, 0, len(rows))
	for _, row := range rows {
		got = append(got, domainAccessRow{
			Import: row.Import,
			Mode:   row.Mode,
			Status: row.Status,
			Owner:  row.Identity.OwnerProject,
		})
		if row.Identity.Key != row.Import {
			t.Errorf("row %s: identity key = %q, want the import id", row.Import, row.Identity.Key)
		}
		if row.Identity.Subkind != row.Mode {
			t.Errorf("row %s: identity subkind = %q, want the mode %q", row.Import, row.Identity.Subkind, row.Mode)
		}
	}
	if !reflect.DeepEqual(got, wantDomainAccessRows) {
		t.Fatalf("committed domainAccess rows =\n%+v\nwant\n%+v", got, wantDomainAccessRows)
	}

	plugin, err := newDomainAccessPlugin()
	if err != nil {
		t.Fatalf("build the domain access plugin: %v", err)
	}
	emitted := make([]domainAccessRow, 0, len(wantDomainAccessRows))
	for _, contract := range plugin.DomainAccessContracts() {
		emitted = append(emitted, domainAccessRow{
			Import: contract.Import,
			Mode:   contract.Mode,
			Status: contract.Status,
			Owner:  "telemetry.putnami.dev",
		})
	}
	if !reflect.DeepEqual(emitted, got) {
		t.Errorf("the plugin emits\n%+v\nbut schema/capabilities.json carries\n%+v\n"+
			"run the describe pass and commit the result; never hand-edit the manifest", emitted, got)
	}
}

// TestEnforcedContractsAreTheDeclarationVerbatim requires every contract the
// runtime enforces to equal the reviewed import it names, field for field.
//
// Bindings are the one exception, and only because they are not part of the
// contract a running process can enforce: they authorize observable dependency
// edges and are maintained by `architecture sync` against the resolved graph.
func TestEnforcedContractsAreTheDeclarationVerbatim(t *testing.T) {
	declared := declaredImports(t)

	if len(domainAccessImports) != len(declared) {
		t.Fatalf("the code enforces %d contracts but the manifest declares %d imports; "+
			"a domain that emits evidence must implement every active import it declares",
			len(domainAccessImports), len(declared))
	}
	for _, contract := range domainAccessImports {
		want, ok := declared[contract.ID]
		if !ok {
			t.Errorf("import %s is enforced in code but not declared in putnami.architecture.json; "+
				"architecture validate reports this as architecture.evidence_without_declaration", contract.ID)
			continue
		}
		want.Bindings = nil
		if !reflect.DeepEqual(contract, want) {
			t.Errorf("import %s drifted from its declaration:\n code      = %+v\n manifest  = %+v",
				contract.ID, contract, want)
		}
	}
}

// TestModeDriftLosesItsDeclaration exercises the join the gate applies.
//
// The gate keys evidence on (consumer domain, import, mode). Changing a mode in
// code therefore does not produce a mismatched row — it produces an ORPHANED
// one, matching no declaration at all, which is the always-failing
// `architecture.evidence_without_declaration`. This test pins that behavior so
// the safety property is not just asserted in a comment.
func TestModeDriftLosesItsDeclaration(t *testing.T) {
	declared := declaredImports(t)

	for _, contract := range domainAccessImports {
		if !joinsDeclaration(declared, contract.ID, contract.Mode) {
			t.Fatalf("import %s at mode %s does not join its declaration; the baseline is already broken",
				contract.ID, contract.Mode)
		}
		drifted := contract
		drifted.Mode = archproto.ModeProjection
		if joinsDeclaration(declared, drifted.ID, drifted.Mode) {
			t.Errorf("import %s declared at mode %s also joins mode %s; the join must key on the mode",
				contract.ID, contract.Mode, drifted.Mode)
		}
	}
}

// joinsDeclaration is the gate's join, reduced to this domain: a record matches
// a declaration only when the import id AND the mode agree.
func joinsDeclaration(declared map[string]archproto.Import, id string, mode archproto.AccessMode) bool {
	found, ok := declared[id]
	return ok && found.Mode == mode
}

// declaredImports reads this domain's reviewed imports through the shared strict
// contract package — the same decode `architecture validate` performs, so a
// manifest this test accepts is one the gate accepts.
func declaredImports(t *testing.T) map[string]archproto.Import {
	t.Helper()
	data, err := os.ReadFile("putnami.architecture.json")
	if err != nil {
		t.Fatalf("read the observability domain manifest: %v", err)
	}
	manifest, diags := archproto.ParseAndValidateManifest(data)
	if manifest == nil {
		t.Fatalf("parse putnami.architecture.json: %+v", diags)
	}
	imports := make(map[string]archproto.Import, len(manifest.Imports))
	for _, declared := range manifest.Imports {
		imports[declared.ID] = declared
	}
	return imports
}

// committedDomainAccess reads the describe pass's own output. The manifest is
// parsed and validated through the capability contract rather than a local
// struct, so a row this test reads is a row the SDD extension can read back as
// architecture evidence.
func committedDomainAccess(t *testing.T) []protocaps.DomainAccessV2 {
	t.Helper()
	data, err := os.ReadFile(protocaps.CommittedPath)
	if err != nil {
		t.Fatalf("read the committed capability manifest: %v", err)
	}
	manifest, diags := protocaps.ParseAndValidateManifestV2(data)
	if manifest == nil {
		t.Fatalf("parse %s: %+v", protocaps.CommittedPath, diags)
	}
	return manifest.DomainAccess
}
