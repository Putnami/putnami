package sdd

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

const surfaceFeatureManifest = `{
  "protocolVersion": 2,
  "namespace": "billing",
  "features": [{
    "id": "billing/export",
    "type": "feature",
    "name": "Export",
    "outcome": "Customers export invoices",
    "owner": "payments",
    "target": "modeled",
    "surface": %s
  }]
}`

// surfaceExtensionManifest serves the commands validate and invoices export,
// the verb build, the group path reports run daily, and the tool billing.export.
// The flat key billing-reports-run is internal, so it serves nothing. The
// members no extension protocol defines stand for a field a newer protocol
// adds: resolution ignores them.
const surfaceExtensionManifest = `{
  "name": "@example/billing",
  "introducedLater": {"anything": true},
  "commands": {
    "validate": {"description": "Validate", "introducedLater": "verification"},
    "billing-reports-run": {"description": "Run", "visibility": "internal"}
  },
  "commandGroups": {
    "reports": {"subcommands": {"run": {"command": "billing-reports-run", "subcommands": {"daily": {}}}}}
  },
  "verbs": [{"name": "build", "description": "Build"}],
  "tools": {"billing.export": {}}
}`

// surfaceWorkspaceWith writes a fixture whose app project declares one feature
// that lists surface, beside an extension manifest when one is given.
func surfaceWorkspaceWith(t *testing.T, surface string, extensionManifest string) *workspace.Workspace {
	t.Helper()
	ws := featureCommandsWorkspace(t, false)
	appDir := filepath.Join(ws.Root, "app")
	writeFixtureFile(t, filepath.Join(appDir, featureproto.ManifestFilename), strings.Replace(surfaceFeatureManifest, "%s", surface, 1))
	if extensionManifest != "" {
		writeFixtureFile(t, filepath.Join(appDir, "putnami.extension.json"), extensionManifest)
	}
	return ws
}

func TestFeatureContextReturnsTheDeclaredSurface(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "features-name-their-surface",
		"feature-context-returns-the-declared-surface")
	// The declared lists are out of order on purpose: the surface comes back in
	// canonical order, and is returned as declared even though nothing serves it.
	ws := featureCommandsWorkspace(t, false)
	writeFixtureFile(t, filepath.Join(ws.Root, "app", featureproto.ManifestFilename),
		strings.Replace(surfaceFeatureManifest, "%s", `{"commands":["validate","invoices export"],"tools":["sdd.list_specs","billing.export"]}`, 1))
	want := featureproto.FeatureSurface{Commands: []string{"invoices export", "validate"}, Tools: []string{"billing.export", "sdd.list_specs"}}

	agent, err := BuildAgentFeatureContextResult(ws, "billing/export")
	if err != nil {
		t.Fatalf("feature context: %v", err)
	}
	if len(agent.Declarations) != 1 || agent.Declarations[0].Surface == nil ||
		!slices.Equal(agent.Declarations[0].Surface.Commands, want.Commands) || !slices.Equal(agent.Declarations[0].Surface.Tools, want.Tools) {
		t.Fatalf("feature context declarations = %+v", agent.Declarations)
	}

	catalog, err := BuildFeatureCatalogResult(ws, "export", Selection{})
	if err != nil {
		t.Fatalf("features list: %v", err)
	}
	if len(catalog.Features) != 1 || len(catalog.Features[0].Declarations) != 1 || catalog.Features[0].Declarations[0].Surface == nil ||
		!slices.Equal(catalog.Features[0].Declarations[0].Surface.Tools, want.Tools) {
		t.Fatalf("features list = %+v", catalog.Features)
	}

	inspection, err := BuildFeatureContextResult(ws, "billing/export")
	if err != nil {
		t.Fatalf("features inspect: %v", err)
	}
	if inspection.Surface == nil || !slices.Equal(inspection.Surface.Commands, want.Commands) || !slices.Equal(inspection.Surface.Tools, want.Tools) {
		t.Fatalf("features inspect surface = %+v", inspection.Surface)
	}

	// A feature that declares no surface carries none.
	bare, err := BuildFeatureContextResult(featureCommandsWorkspace(t, false), "orders/checkout")
	if err != nil {
		t.Fatalf("features inspect: %v", err)
	}
	if bare.Surface != nil {
		t.Fatalf("a feature without a surface reported %+v", bare.Surface)
	}
}

func TestAnUnservedSurfaceNameFailsFeaturesValidate(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "features-name-their-surface",
		"an-unserved-surface-name-fails-features-validate")
	// The served names are a command key, two group paths, a verb and a tool.
	// The unserved ones are the internal key behind a group, a verb the manifest
	// does not declare, and a tool it does not declare.
	served := `{"commands":["validate","reports run","reports run daily","build"],"tools":["billing.export"]}`
	unserved := `{"commands":["validate","billing-reports-run","deploy"],"tools":["billing.export","billing.other"]}`

	for name, validate := range map[string]func(ws *workspace.Workspace) (FeatureValidationReport, error){
		"interactive": func(ws *workspace.Workspace) (FeatureValidationReport, error) {
			return BuildFeatureValidationResult(ws, Selection{})
		},
		"task": func(ws *workspace.Workspace) (FeatureValidationReport, error) {
			return BuildFeatureTaskValidationResult(ws, Selection{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			report, err := validate(surfaceWorkspaceWith(t, served, surfaceExtensionManifest))
			if err != nil || !report.Valid {
				t.Fatalf("served surface rejected: %v %+v", err, report.Diagnostics)
			}

			_, err = validate(surfaceWorkspaceWith(t, unserved, surfaceExtensionManifest))
			if !errors.Is(err, protocolcli.ErrInvalidConfig) {
				t.Fatalf("unserved surface error = %v, want ErrInvalidConfig", err)
			}
			failed, ok := ResultData(err).(FeatureValidationReport)
			if !ok {
				t.Fatalf("failure data = %T", ResultData(err))
			}
			got := make(map[string]string)
			for _, finding := range failed.Diagnostics {
				if finding.Code == featureproto.ErrorCodeUnresolvedSurface {
					got[finding.Field] = finding.Message
				}
			}
			for _, field := range []string{
				"app/putnami.features.json#features[0].surface.commands[1]",
				"app/putnami.features.json#features[0].surface.commands[2]",
				"app/putnami.features.json#features[0].surface.tools[1]",
			} {
				if got[field] == "" {
					t.Errorf("no %s finding at %s; findings: %v", featureproto.ErrorCodeUnresolvedSurface, field, got)
				}
			}
			if len(got) != 3 {
				t.Errorf("unresolved findings = %v, want exactly the internal key, the unserved verb and the unserved tool", got)
			}
		})
	}

	// A stale claim fails validation only: a snapshot of the same tree still
	// reads, because it reports the declaration as declared.
	if _, err := BuildFeatureSnapshotResult(surfaceWorkspaceWith(t, unserved, surfaceExtensionManifest), Selection{}); err != nil {
		t.Fatalf("snapshot of an unserved surface: %v", err)
	}

	// A manifest that cannot be parsed resolves nothing, so the claim fails.
	_, err := BuildFeatureValidationResult(surfaceWorkspaceWith(t, served, `{"commands":`), Selection{})
	if !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("unparsable extension manifest error = %v, want ErrInvalidConfig", err)
	}
}

func TestAProjectWithoutAManifestResolvesNothing(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "features-name-their-surface",
		"a-project-without-a-manifest-resolves-nothing")
	// No putnami.extension.json beside the declaration: the claim is kept as
	// authored, whatever it names, in both validation entry points.
	ws := surfaceWorkspaceWith(t, `{"commands":["not served anywhere"],"tools":["anything"]}`, "")
	if report, err := BuildFeatureValidationResult(ws, Selection{}); err != nil || !report.Valid {
		t.Fatalf("a project without an extension manifest was resolved: %v %+v", err, report.Diagnostics)
	}
	if report, err := BuildFeatureTaskValidationResult(ws, Selection{}); err != nil || !report.Valid {
		t.Fatalf("a project without an extension manifest was resolved by the task: %v %+v", err, report.Diagnostics)
	}
}
