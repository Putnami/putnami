package main

import (
	"go.putnami.dev/protocol/features/spectest"

	"os"
	"path/filepath"
	"strings"
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	protocolcli "go.putnami.dev/protocol/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
	"go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// `architecture init` and `architecture sync` end to end.
//
// Both run through the real dispatch — the same interactiveSubcommands table,
// the same wire, the same envelope — because what they refuse is as much a
// user-visible contract as what they write, and a refusal asserted against an
// engine call would not prove the CLI path reaches it.
//
// Neither has a recorded parity answer: both are additions after the extraction, so there
// is no built-in implementation whose bytes they could match. See
// doc/05-parity.md.

// authoringWorkspace is two domains over three projects, with the graph edges
// the detector reads:
//
//	/billing  -> /shipping   declared and bound      (billing.shipping-label.v1)
//	/billing  -> /warehouse  observed, NOT declared  (no import from warehouse)
//
// The third project is deliberately in a domain billing has no contract with,
// because "sync refuses to grant what nobody declared" is the property that
// needs a real undeclared edge.
func authoringWorkspace(t *testing.T) (string, []*wsview.Project) {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"authoring","includes":["billing","shipping","warehouse"]}`)
	writeFixture(t, filepath.Join(root, "billing", "putnami.json"), `{"name":"billing"}`)
	writeFixture(t, filepath.Join(root, "shipping", "putnami.json"), `{"name":"shipping"}`)
	writeFixture(t, filepath.Join(root, "warehouse", "putnami.json"), `{"name":"warehouse"}`)

	writeFixture(t, filepath.Join(root, "billing", archproto.ManifestFilename), `{
  "protocolVersion": 1,
  "domain": "billing",
  "owner": "billing-team",
  "projects": ["/billing"],
  "exports": [],
  "imports": [
    {
      "id": "billing.shipping-label.v1",
      "version": 1,
      "from": {"domain": "shipping", "export": "shipping.label.v1"},
      "as": "billing.shipping-label",
      "mode": "reference",
      "status": "active",
      "facts": ["id"],
      "justification": "Billing stores the authoritative shipping label identity only",
      "bindings": [
        {"kind": "project-dependency", "consumerProject": "/billing", "producerProject": "/shipping"}
      ]
    }
  ]
}
`)
	writeFixture(t, filepath.Join(root, "shipping", archproto.ManifestFilename), `{
  "protocolVersion": 1,
  "domain": "shipping",
  "owner": "shipping-team",
  "projects": ["/shipping"],
  "exports": [
    {
      "id": "shipping.label.v1",
      "version": 1,
      "status": "active",
      "description": "Stable shipping label reference.",
      "facts": [{"name": "id", "authority": "shipping", "classification": "internal", "personalData": "none"}],
      "modes": ["reference"],
      "compatibility": {"strategy": "additive", "minimumConsumerVersion": 1}
    }
  ],
  "imports": []
}
`)
	writeFixture(t, filepath.Join(root, "warehouse", archproto.ManifestFilename), `{
  "protocolVersion": 1,
  "domain": "warehouse",
  "owner": "warehouse-team",
  "projects": ["/warehouse"],
  "exports": [],
  "imports": []
}
`)

	return root, []*wsview.Project{
		{ID: "/billing", Name: "billing", SourceName: "billing", Path: "billing",
			Dependencies: []string{"/shipping", "/warehouse"}},
		{ID: "/shipping", Name: "shipping", SourceName: "shipping", Path: "shipping"},
		{ID: "/warehouse", Name: "warehouse", SourceName: "warehouse", Path: "warehouse"},
	}
}

func allSelection(projects []*wsview.Project) *pctx.Selection {
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.ID)
	}
	return &pctx.Selection{Mode: pctx.SelectionModeAll, ProjectIDs: ids}
}

// TestArchitectureInitScaffoldsOnlyWhatWasStated is the scaffold golden: the
// document a scaffold writes carries the domain, the owner and the selected
// projects, and NOTHING else — no export, no import, no binding. Each of those
// is an agreement between two domains, and inventing one would put a contract
// nobody negotiated into a durable artifact.
func TestArchitectureInitScaffoldsOnlyWhatWasStated(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "authoring-never-grants-a-permission",
		"init-scaffolds-only-what-was-stated")
	root, projects := authoringWorkspace(t)
	selection := &pctx.Selection{
		Mode:       pctx.SelectionModeProjects,
		Scoped:     true,
		ProjectIDs: []string{"/warehouse"},
	}
	// The warehouse manifest is what this test creates, so it must not exist yet.
	if err := os.Remove(filepath.Join(root, "warehouse", archproto.ManifestFilename)); err != nil {
		t.Fatal(err)
	}

	ctx := wireContext(root, projects, selection, map[string]any{"output": "json", "owner": "warehouse-team"})
	stdout, _, code := runSubcommand(t, ctx, "architecture", "init", "warehouse")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("exit = %d, want success\n%s", code, stdout)
	}
	_, report := decodeEnvelope[sdd.ArchitectureInitReport](t, stdout)
	if !report.Created {
		t.Fatalf("report = %+v, want a created manifest", report)
	}
	if got, want := report.Path, "warehouse/"+archproto.ManifestFilename; got != want {
		t.Errorf("path = %q, want %q (the first mapped project's directory)", got, want)
	}

	want := `{
  "$schema": "https://putnami.dev/schemas/putnami-architecture.json",
  "protocolVersion": 1,
  "domain": "warehouse",
  "owner": "warehouse-team",
  "projects": [
    "/warehouse"
  ],
  "exports": [],
  "imports": []
}
`
	if report.Contents != want {
		t.Errorf("scaffold contents:\n%s\nwant:\n%s", report.Contents, want)
	}
	written, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(report.Path)))
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != want {
		t.Errorf("the file on disk is not the reported document:\n%s", written)
	}
}

// TestArchitectureInitRefusesWhatItCannotDecide walks the four refusals, each
// naming a decision the scaffold will not make for its caller.
func TestArchitectureInitRefusesWhatItCannotDecide(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "authoring-never-grants-a-permission",
		"init-refuses-an-already-declared-domain")
	root, projects := authoringWorkspace(t)

	t.Run("a domain another manifest already declares", func(t *testing.T) {
		ctx := wireContext(root, projects, allSelection(projects), jsonParams())
		stdout, _, code := runSubcommand(t, ctx, "architecture", "init", "billing")
		if code == protocolcli.ExitSuccess {
			t.Fatalf("a duplicate domain was accepted\n%s", stdout)
		}
		if !strings.Contains(stdout, "billing/"+archproto.ManifestFilename) {
			t.Errorf("the refusal does not name the manifest that owns the identity:\n%s", stdout)
		}
	})

	t.Run("a file already at the target path", func(t *testing.T) {
		ctx := wireContext(root, projects, &pctx.Selection{
			Mode: pctx.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/warehouse"},
		}, jsonParams())
		// A different domain identity, but the same directory: the exclusive
		// create is what refuses, not a check that raced it.
		stdout, _, code := runSubcommand(t, ctx, "architecture", "init", "warehouse-annex")
		if code == protocolcli.ExitSuccess {
			t.Fatalf("an existing file was overwritten\n%s", stdout)
		}
		if !strings.Contains(stdout, "never overwrites") {
			t.Errorf("the refusal is not the never-overwrite one:\n%s", stdout)
		}
	})

	t.Run("a domain id the protocol does not accept", func(t *testing.T) {
		ctx := wireContext(root, projects, allSelection(projects), jsonParams())
		stdout, _, code := runSubcommand(t, ctx, "architecture", "init", "Warehouse")
		if code == protocolcli.ExitSuccess {
			t.Fatalf("an invalid domain id was accepted\n%s", stdout)
		}
		if !strings.Contains(stdout, archproto.ErrorCodeInvalidDomain) {
			t.Errorf("the refusal does not carry the protocol's own diagnostic code:\n%s", stdout)
		}
	})

	t.Run("selection modes that cannot mean a membership", func(t *testing.T) {
		impacted := wireContext(root, projects, &pctx.Selection{
			Mode: pctx.SelectionModeImpacted, Scoped: true, ProjectIDs: []string{"/warehouse"},
		}, jsonParams())
		if _, _, code := runSubcommand(t, impacted, "architecture", "init", "annex"); code != protocolcli.ExitUsage {
			t.Errorf("--impacted exit = %d, want a usage refusal: an impacted set is what changed, not what a domain contains", code)
		}
		all := wireContext(root, projects, allSelection(projects), map[string]any{"output": "json", "all": true})
		if _, _, code := runSubcommand(t, all, "architecture", "init", "annex"); code != protocolcli.ExitUsage {
			t.Errorf("--all exit = %d, want a usage refusal: one domain does not own every project in the workspace", code)
		}
	})
}

// TestArchitectureInitDryRunWritesNothing pins that --dry-run shows the exact
// bytes the write path produces and leaves the tree alone.
func TestArchitectureInitDryRunWritesNothing(t *testing.T) {
	root, projects := authoringWorkspace(t)
	target := filepath.Join(root, "billing", "annex", archproto.ManifestFilename)

	ctx := wireContext(root, projects, allSelection(projects),
		map[string]any{"output": "json", "dry-run": true, "at": "billing/annex"})
	stdout, _, code := runSubcommand(t, ctx, "architecture", "init", "annex")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("exit = %d, want success\n%s", code, stdout)
	}
	_, report := decodeEnvelope[sdd.ArchitectureInitReport](t, stdout)
	if report.Created || !report.DryRun {
		t.Errorf("report = %+v, want a dry run that created nothing", report)
	}
	if report.Contents == "" {
		t.Error("a dry run printed no document; --dry-run must show the exact bytes the write path produces")
	}
	if len(report.Projects) != 0 {
		t.Errorf("projects = %v, want none: an unscoped run maps no project", report.Projects)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("--dry-run wrote %s", target)
	}
}

// TestArchitectureSyncSuggestsTheMechanicalHalf is the sync suggestion: a stale
// project entry and a stale binding are proposed for removal, and nothing is
// written without --apply.
func TestArchitectureSyncSuggestsTheMechanicalHalf(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "authoring-never-grants-a-permission",
		"sync-removes-a-stale-project-and-a-stale-binding")
	root, projects := authoringWorkspace(t)
	// Billing claims a project the workspace does not contain and a binding the
	// graph no longer observes. Both are hard `architecture validate` errors
	// today, and neither removal takes a decision.
	writeFixture(t, filepath.Join(root, "billing", archproto.ManifestFilename), `{
  "protocolVersion": 1,
  "domain": "billing",
  "owner": "billing-team",
  "projects": ["/billing", "/billing-legacy"],
  "exports": [],
  "imports": [
    {
      "id": "billing.shipping-label.v1",
      "version": 1,
      "from": {"domain": "shipping", "export": "shipping.label.v1"},
      "as": "billing.shipping-label",
      "mode": "reference",
      "status": "active",
      "facts": ["id"],
      "justification": "Billing stores the authoritative shipping label identity only",
      "bindings": [
        {"kind": "project-dependency", "consumerProject": "/billing", "producerProject": "/shipping"},
        {"kind": "project-dependency", "consumerProject": "/billing-legacy", "producerProject": "/shipping"}
      ]
    }
  ]
}
`)
	before, err := os.ReadFile(filepath.Join(root, "billing", archproto.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}

	ctx := wireContext(root, projects, allSelection(projects), jsonParams())
	stdout, _, code := runSubcommand(t, ctx, "architecture", "sync")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("exit = %d, want success\n%s", code, stdout)
	}
	_, report := decodeEnvelope[sdd.ArchitectureSyncReport](t, stdout)
	if report.Applied {
		t.Error("a run without --apply reported itself as applied")
	}
	if got := report.Summary.RemovedProjects; got != 1 {
		t.Errorf("removed projects = %d, want 1 (/billing-legacy is not in the workspace)", got)
	}
	if got := report.Summary.RemovedBindings; got != 1 {
		t.Errorf("removed bindings = %d, want 1 (/billing-legacy -> /shipping is not observed)", got)
	}
	if got := report.Summary.Written; got != 0 {
		t.Errorf("written = %d, want 0 without --apply", got)
	}
	after, err := os.ReadFile(filepath.Join(root, "billing", archproto.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("a suggestion run rewrote the manifest")
	}

	applied := wireContext(root, projects, allSelection(projects), map[string]any{"output": "json", "apply": true})
	stdout, _, code = runSubcommand(t, applied, "architecture", "sync")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("--apply exit = %d, want success\n%s", code, stdout)
	}
	_, report = decodeEnvelope[sdd.ArchitectureSyncReport](t, stdout)
	if got := report.Summary.Written; got != 1 {
		t.Errorf("written = %d, want 1 with --apply", got)
	}
	written, err := os.ReadFile(filepath.Join(root, "billing", archproto.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	manifest, diagnostics := archproto.ParseAndValidateManifest(written)
	if manifest == nil || len(diagnostics) > 0 {
		t.Fatalf("the written manifest does not validate: %v\n%s", diagnostics, written)
	}
	if len(manifest.Projects) != 1 || manifest.Projects[0] != "/billing" {
		t.Errorf("projects = %v, want only /billing", manifest.Projects)
	}
	if len(manifest.Imports[0].Bindings) != 1 {
		t.Errorf("bindings = %+v, want only the observed one", manifest.Imports[0].Bindings)
	}
}

// TestArchitectureSyncRefusesToGrantAnUndeclaredRelation is the red line as a
// test.
//
// /billing depends on /warehouse and billing declares no import from the
// warehouse domain. Sync must report the edge and change nothing: writing the
// binding would authorize the dependency, and writing the import would invent a
// contract between two domains. The undeclared relation stays a failing
// `architecture validate` finding, which is where a verdict belongs.
func TestArchitectureSyncRefusesToGrantAnUndeclaredRelation(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "authoring-never-grants-a-permission",
		"sync-refuses-to-grant-an-undeclared-relation")
	root, projects := authoringWorkspace(t)
	before, err := os.ReadFile(filepath.Join(root, "billing", archproto.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}

	ctx := wireContext(root, projects, allSelection(projects), map[string]any{"output": "json", "apply": true})
	stdout, _, code := runSubcommand(t, ctx, "architecture", "sync")
	if code != protocolcli.ExitSuccess {
		t.Fatalf("exit = %d, want success\n%s", code, stdout)
	}
	_, report := decodeEnvelope[sdd.ArchitectureSyncReport](t, stdout)
	if got := report.Summary.AddedBindings; got != 0 {
		t.Fatalf("added bindings = %d, want 0: an undeclared relation is never auto-granted", got)
	}
	if len(report.Refusals) != 1 {
		t.Fatalf("refusals = %+v, want exactly one for /billing -> /warehouse", report.Refusals)
	}
	refusal := report.Refusals[0]
	if refusal.Reason != sdd.SyncRefusalNoDeclaredImport {
		t.Errorf("reason = %q, want %q", refusal.Reason, sdd.SyncRefusalNoDeclaredImport)
	}
	if refusal.ConsumerProject != "/billing" || refusal.ProducerProject != "/warehouse" {
		t.Errorf("refusal edge = %s -> %s, want /billing -> /warehouse", refusal.ConsumerProject, refusal.ProducerProject)
	}
	after, err := os.ReadFile(filepath.Join(root, "billing", archproto.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("sync --apply rewrote a manifest whose only pending change it refused to make:\n%s", after)
	}
}

// TestArchitectureSyncAttachesOnlyToADeclaredContract pins the other side of the
// red line: with the relation already declared, the exact project edge is a
// suggestion sync WILL make — and the git diff of the applied run is where it is
// authorized. A planned import still refuses, because the protocol forbids a
// binding on a target and promoting it is a human's decision.
func TestArchitectureSyncAttachesOnlyToADeclaredContract(t *testing.T) {
	root, projects := authoringWorkspace(t)
	billing := filepath.Join(root, "billing", archproto.ManifestFilename)
	warehouseImport := func(status string) string {
		return `{
  "protocolVersion": 1,
  "domain": "billing",
  "owner": "billing-team",
  "projects": ["/billing"],
  "exports": [],
  "imports": [
    {
      "id": "billing.shipping-label.v1",
      "version": 1,
      "from": {"domain": "shipping", "export": "shipping.label.v1"},
      "as": "billing.shipping-label",
      "mode": "reference",
      "status": "active",
      "facts": ["id"],
      "justification": "Billing stores the authoritative shipping label identity only",
      "bindings": [
        {"kind": "project-dependency", "consumerProject": "/billing", "producerProject": "/shipping"}
      ]
    },
    {
      "id": "billing.stock-level.v1",
      "version": 1,
      "from": {"domain": "warehouse", "export": "warehouse.stock.v1"},
      "as": "billing.stock-level",
      "mode": "reference",
      "status": "` + status + `",
      "facts": ["sku"],
      "justification": "Billing keeps the warehouse stock identity to price a backorder"
    }
  ]
}
`
	}
	writeFixture(t, filepath.Join(root, "warehouse", archproto.ManifestFilename), `{
  "protocolVersion": 1,
  "domain": "warehouse",
  "owner": "warehouse-team",
  "projects": ["/warehouse"],
  "exports": [
    {
      "id": "warehouse.stock.v1",
      "version": 1,
      "status": "active",
      "description": "Stable stock identity.",
      "facts": [{"name": "sku", "authority": "warehouse", "classification": "internal", "personalData": "none"}],
      "modes": ["reference"],
      "compatibility": {"strategy": "additive", "minimumConsumerVersion": 1}
    }
  ],
  "imports": []
}
`)

	writeFixture(t, billing, warehouseImport("planned"))
	ctx := wireContext(root, projects, allSelection(projects), jsonParams())
	stdout, _, _ := runSubcommand(t, ctx, "architecture", "sync")
	_, report := decodeEnvelope[sdd.ArchitectureSyncReport](t, stdout)
	if report.Summary.AddedBindings != 0 {
		t.Errorf("added bindings = %d, want 0: a planned target cannot carry a current binding", report.Summary.AddedBindings)
	}
	if len(report.Refusals) != 1 || report.Refusals[0].Reason != sdd.SyncRefusalPlannedImportOnly {
		t.Fatalf("refusals = %+v, want one %q", report.Refusals, sdd.SyncRefusalPlannedImportOnly)
	}

	writeFixture(t, billing, warehouseImport("active"))
	stdout, _, _ = runSubcommand(t, ctx, "architecture", "sync")
	_, report = decodeEnvelope[sdd.ArchitectureSyncReport](t, stdout)
	if len(report.Refusals) != 0 {
		t.Errorf("refusals = %+v, want none once the contract is declared", report.Refusals)
	}
	if report.Summary.AddedBindings != 1 {
		t.Fatalf("added bindings = %d, want 1", report.Summary.AddedBindings)
	}
	added := report.Manifests[0].AddedBindings[0]
	if added.Import != "billing.stock-level.v1" || added.ConsumerProject != "/billing" || added.ProducerProject != "/warehouse" {
		t.Errorf("added binding = %+v, want /billing -> /warehouse under billing.stock-level.v1", added)
	}
}

// TestArchitectureSyncRejectsProjectSelection pins that the reconciliation is
// whole-workspace by construction: a binding is compared against the WHOLE
// resolved graph, so a narrowed run could only be silently ignored.
func TestArchitectureSyncRejectsProjectSelection(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "authoring-never-grants-a-permission",
		"sync-rejects-project-selection")
	root, projects := authoringWorkspace(t)
	ctx := wireContext(root, projects, &pctx.Selection{
		Mode: pctx.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/billing"},
	}, jsonParams())
	stdout, _, code := runSubcommand(t, ctx, "architecture", "sync")
	if code != protocolcli.ExitUsage {
		t.Fatalf("exit = %d, want a usage refusal\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "exact target") {
		t.Errorf("the refusal is not the shared selection one:\n%s", stdout)
	}
}

// TestArchitectureSyncFailsClosedOnAPartialView pins the fail-closed guard sync
// shares with validate. Over a partial project view "this binding is no longer
// observed" is unanswerable, and sync would DELETE a reviewed permission on the
// strength of an answer it could not give.
func TestArchitectureSyncFailsClosedOnAPartialView(t *testing.T) {
	root, projects := authoringWorkspace(t)
	ctx := wireContext(root, nil, allSelection(projects), jsonParams())
	ctx.SelectedProjects = []pctx.ProjectRef{{ID: "/billing", Name: "billing", Path: "billing"}}
	stdout, _, code := runSubcommand(t, ctx, "architecture", "sync")
	if code == protocolcli.ExitSuccess {
		t.Fatalf("a partial view produced a reconciliation\n%s", stdout)
	}
	if !strings.Contains(stdout, "complete project view") {
		t.Errorf("the refusal does not name the partial view:\n%s", stdout)
	}
}

// TestArchitectureSyncRefusesAnInvalidRepository pins that a suggestion is never
// derived from a document the protocol rejects: that would be a guess about what
// the author meant.
func TestArchitectureSyncRefusesAnInvalidRepository(t *testing.T) {
	root, projects := authoringWorkspace(t)
	writeFixture(t, filepath.Join(root, "warehouse", archproto.ManifestFilename),
		`{"protocolVersion": 1, "domain": "warehouse", "owner": "warehouse-team", "projects": [], "exports": [], "imports": [], "surprise": true}`+"\n")
	ctx := wireContext(root, projects, allSelection(projects), jsonParams())
	stdout, _, code := runSubcommand(t, ctx, "architecture", "sync")
	if code == protocolcli.ExitSuccess {
		t.Fatalf("an invalid repository produced a suggestion\n%s", stdout)
	}
	if !strings.Contains(stdout, archproto.ErrorCodeUnknownField) {
		t.Errorf("the refusal does not carry the protocol's own diagnostic:\n%s", stdout)
	}
}
