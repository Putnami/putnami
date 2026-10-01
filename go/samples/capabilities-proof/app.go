// Package capproof is a proof workload for the Go capability-manifest emitter.
// It composes every v2 collection under one native feature scope so
// describe mode emits representative capability and design documents.
package capproof

import (
	"context"
	"os"
	"path/filepath"

	"go.putnami.dev/app"
	"go.putnami.dev/config"
	"go.putnami.dev/migration"
	protocaps "go.putnami.dev/protocol/capabilities"
	infra "go.putnami.dev/protocol/infra"
)

type proofOptions struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token" sensitive:"true"`
}

type configPlugin struct{}

func (*configPlugin) Name() string { return "proof-config" }
func (*configPlugin) ConfigDefinitions() []config.Descriptor {
	return []config.Descriptor{config.Config[proofOptions]("proof").Descriptor()}
}

// iamSource is a migration source that declares the database it migrates, so it
// drives both the migrations and infraRequirements capability kinds.
type iamSource struct{}

func (iamSource) Kind() migration.Kind { return migration.KindSQL }
func (iamSource) Namespace() string    { return "iam" }
func (iamSource) InfraDatabases() []infra.Database {
	return []infra.Database{{
		Name:    "default",
		Engine:  infra.EnginePostgres,
		Schemas: []string{"public"},
	}}
}

// migrationPlugin contributes the iam migration source.
type migrationPlugin struct{}

func (*migrationPlugin) Name() string { return "iam-feature" }
func (*migrationPlugin) MigrationSources() []migration.Source {
	return []migration.Source{iamSource{}}
}
func (*migrationPlugin) RequiredCapabilities() []app.CapabilityRequirement {
	return []app.CapabilityRequirement{{
		Name: "sql",
		Requires: []protocaps.CapabilityKind{
			protocaps.CapabilityKindConfig,
			protocaps.CapabilityKindSchema,
			protocaps.CapabilityKindDiscoverer,
			protocaps.CapabilityKindDatasource,
			protocaps.CapabilityKindMigration,
			protocaps.CapabilityKindInfra,
			protocaps.CapabilityKindHealth,
			protocaps.CapabilityKindReadiness,
			protocaps.CapabilityKindLifecycle,
			protocaps.CapabilityKindPackage,
		},
	}}
}

// diskHealthPlugin feeds the /healthz liveness probe.
type diskHealthPlugin struct{}

func (*diskHealthPlugin) Name() string                      { return "diskSpace" }
func (*diskHealthPlugin) CheckHealth(context.Context) error { return nil }

// databaseReadinessPlugin feeds the /readyz readiness probe.
type databaseReadinessPlugin struct{}

func (*databaseReadinessPlugin) Name() string                         { return "primaryDatabase" }
func (*databaseReadinessPlugin) CheckReadiness(context.Context) error { return nil }

// connectionPoolPlugin is both a Starter and a Stopper, so it appears twice in
// the lifecycle hooks (starter + stopper phases).
type connectionPoolPlugin struct{}

func (*connectionPoolPlugin) Name() string                             { return "connectionPool" }
func (*connectionPoolPlugin) Start(context.Context, *app.Module) error { return nil }
func (*connectionPoolPlugin) Stop(context.Context, *app.Module) error  { return nil }

type inventoryPlugin struct{}

func (*inventoryPlugin) Name() string { return "inventory" }
func (*inventoryPlugin) CapabilitySchemas() []app.CapabilitySchema {
	return []app.CapabilitySchema{{Name: "proofRoute", Kind: protocaps.SchemaKindRoute, Path: "/proof"}, {Name: "proofOpenAPI", Kind: protocaps.SchemaKindOpenAPI, Path: "schema/openapi.json"}}
}
func (*inventoryPlugin) CapabilityDiscoverers() []app.CapabilityDiscoverer { return nil }
func (*inventoryPlugin) Describe(ctx *app.DescribeContext) error {
	if err := os.MkdirAll(filepath.Join(ctx.OutputDir, "schema"), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ctx.OutputDir, "schema", "openapi.json"), []byte("{}\n"), 0o600); err != nil {
		return err
	}
	return infra.WriteSidecarIn(ctx.OutputDir, "inventory", infra.PerProjectManifest{
		Events:        &infra.Events{Publishes: []string{"proof.created"}},
		Storage:       []infra.StorageBucket{{Name: "proof-artifacts"}},
		Secrets:       []string{"proof.external-secret"},
		ScheduledJobs: []infra.ScheduledJob{{Name: "proof-cleanup", Schedule: "0 2 * * *"}},
	})
}

// BuildApp assembles the proof application. app_test.go runs it through
// Describe to emit the capability manifest; the app never starts servers.
func BuildApp() *app.Application {
	a := app.New("capabilities-proof")
	a.Feature(app.Feature{
		ID:      "capabilities/source-bound-manifest",
		Name:    "Indexed capability provenance",
		Outcome: "Indexers can resolve exact native capability provenance without volatile manifest hashes",
		Owner:   "frameworks",
	})
	a.Use(&configPlugin{})
	a.Use(&migrationPlugin{})
	a.Use(&diskHealthPlugin{})
	a.Use(&databaseReadinessPlugin{})
	a.Use(&connectionPoolPlugin{})
	a.Use(&inventoryPlugin{})
	return a
}
