package app

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"os"
	"path/filepath"
	"reflect"

	"go.putnami.dev/errors"
	"go.putnami.dev/migration"
	protomig "go.putnami.dev/protocol/migration"
)

// This file holds the build-time "describe" phase: the machinery that runs
// Configure on every plugin and invokes Describer.Describe to extract
// proto/openapi/migration/infra artifacts without starting servers. It is kept
// separate from application.go so the runtime lifecycle (Start/Stop/Validate/
// Migrate/invoke/signal handling) reads on its own, free of the codegen path.

// describeEnvVar is the environment variable that triggers describe mode.
// When set in the environment of an app process, ListenAndServe runs the
// describe phase instead of the normal start sequence and exits.
const describeEnvVar = "PUTNAMI_DESCRIBE"

// describeOutEnvVar names the directory artifacts are written to. Defaults
// to ".gen" relative to the working directory when unset.
const describeOutEnvVar = "PUTNAMI_DESCRIBE_OUT"

// Describe runs Configure on every plugin, then invokes Describe on plugins
// that implement the Describer interface. No servers or background workers
// start. Used at build time to extract proto/openapi/etc. without running
// the application.
//
// outputDir is the absolute directory artifacts are written to. targets
// filters which describers run; pass nil or {"all"} to run every describer.
func (a *Application) Describe(outputDir string, targets []string) error {
	if outputDir == "" {
		return errors.Newf(CodeConfigure, "describe: outputDir is required")
	}
	//nolint:gosec // outputDir is framework-controlled (describe CLI / PUTNAMI_DESCRIBE_OUT), not attacker-supplied
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return errors.Wrapf(err, CodeConfigure, "describe: prepare output dir")
	}
	dctx := &DescribeContext{OutputDir: outputDir, Targets: targets}
	if dctx.Wants(describerNameCapabilities) {
		if err := removeCapabilityManifest(outputDir); err != nil {
			return err
		}
	}

	ctx := context.Background()
	allPlugins := a.CollectPlugins()

	if err := a.runModulePreConfigure(ctx); err != nil {
		return err
	}

	// Lazy container start: describe must never reach external systems (a DB
	// pool, etc.) to harvest metadata, so non-lazy singletons are not resolved.
	if err := a.buildContainer(allPlugins, false); err != nil {
		return err
	}
	defer a.closeContainer()

	if err := a.configurePlugins(ctx, allPlugins); err != nil {
		return err
	}

	if err := a.runModulePostConfigure(ctx); err != nil {
		return err
	}

	if err := a.collectMigrationSources(); err != nil {
		return err
	}

	for _, d := range a.describers() {
		if !wantsDescriber(dctx, d) {
			continue
		}
		if err := d.Describe(dctx); err != nil {
			return errors.Wrapf(err, CodeConfigure, "describe failed",
				errors.String("plugin", d.Name()))
		}
	}

	// Built-in migration describer: emits the per-app registry view as
	// .gen/migrations.json so the build artifact captures the migration
	// surface every cmd would actually apply. Same content as the
	// migrate CLI's `inspect` subcommand. Filtered by "migrations" /
	// "all" via DescribeContext.Wants.
	if dctx.Wants(describerNameMigrations) {
		if err := a.describeMigrations(dctx); err != nil {
			return errors.Wrapf(err, CodeConfigure, "describe failed",
				errors.String("plugin", describerNameMigrations))
		}
	}

	// Built-in migration-bundle describer: emits a self-contained, immutable
	// migration bundle (.gen/migration-bundle/) — the canonical manifest plus
	// materialized payloads — for the artifact-first publish/remote-execute
	// flow. Pure build step: runners load definitions from their authoring
	// source, never a live database. Filtered by "migration-bundle" / "all".
	// It runs before the infra describer: a bundle the runner would refuse
	// fails the build before any infra fragment names its schemas.
	if dctx.Wants(describerNameBundle) {
		if err := a.describeMigrationBundle(dctx); err != nil {
			return errors.Wrapf(err, CodeConfigure, "describe failed",
				errors.String("plugin", describerNameBundle))
		}
	}

	// Built-in infra-requirements describer: the migration registry is the
	// authoritative source for the schemas each declared database owns, so it
	// emits a framework-generated scratch fragment (.gen/infra/migration.json)
	// that the Go generator syncs into committed infra/requirements.json.
	// Filtered by "infra-requirements" / "all" via Wants.
	if dctx.Wants(describerNameInfra) && a.migrationRegistry != nil {
		if err := a.migrationRegistry.WriteInfraRequirements(dctx.OutputDir); err != nil {
			return errors.Wrapf(err, CodeConfigure, "describe failed",
				errors.String("plugin", describerNameInfra))
		}
	}

	// Built-in config describer: aggregates the config blocks every
	// ConfigContributor in the module tree owns and emits them as
	// .gen/config-deps.json. The build's config merge step folds these
	// library-owned blocks into the workload's published schema/config.json, so
	// a workload publishes its dependencies' config (and secrets) transitively.
	// Filtered by "config" / "all" via Wants.
	if dctx.Wants(describerNameConfig) {
		if err := a.describeConfig(dctx); err != nil {
			return errors.Wrapf(err, CodeConfigure, "describe failed",
				errors.String("plugin", describerNameConfig))
		}
	}

	// Built-in capabilities describer: aggregates every contribution kind the
	// app package can observe (config, migrations, infra, health/readiness,
	// lifecycle) into a single deterministic manifest at
	// .gen/schema/capabilities.json, promoted by the codegen committer to the
	// tracked tree at schema/capabilities.json. Runs last so it reflects the
	// fully collected describe state. Filtered by "capabilities" / "all".
	if dctx.Wants(describerNameCapabilities) {
		if err := a.describeCapabilities(dctx); err != nil {
			return errors.Wrapf(err, CodeConfigure, "describe failed",
				errors.String("plugin", describerNameCapabilities))
		}
	}

	// Built-in design describer: only applications with a native Feature
	// declaration emit this disposable graph. The graph is written atomically so
	// one describe pass always publishes a converged projection.
	if dctx.Wants(describerNameDesign) {
		graph, err := a.buildDesignGraph()
		if err != nil {
			return errors.Wrapf(err, CodeConfigure, "describe failed",
				errors.String("plugin", describerNameDesign))
		}
		if graph != nil {
			if err := writeDesignGraph(dctx.OutputDir, graph); err != nil {
				return errors.Wrapf(err, CodeConfigure, "describe failed",
					errors.String("plugin", describerNameDesign))
			}
		} else if err := removeDesignGraph(dctx.OutputDir); err != nil {
			return errors.Wrapf(err, CodeConfigure, "describe failed",
				errors.String("plugin", describerNameDesign))
		}
	}
	return nil
}

func wantsDescriber(ctx *DescribeContext, describer Describer) bool {
	if ctx.Wants(describer.Name()) {
		return true
	}
	targeter, ok := describer.(AdditionalDescribeTargeter)
	if !ok {
		return false
	}
	for _, target := range targeter.AdditionalDescribeTargets() {
		if ctx.Wants(target) {
			return true
		}
	}
	return false
}

// describers returns every Describer to run in the describe phase: lifecycle
// plugins discovered through the module tree via Collect, followed by the
// describe-only contributors registered with UseForDescribe. A contributor
// that is also registered via Use (and so already discovered by Collect) is
// de-duplicated by pointer identity, so its sidecar is written once.
func (a *Application) describers() []Describer {
	result := Collect[Describer](a.Module)
	// Dedup is by pointer identity, mirroring Collect; the rare non-pointer
	// Describer is never deduped (no framework producer is a value type).
	seen := make(map[uintptr]struct{}, len(result))
	for _, d := range result {
		if rv := reflect.ValueOf(d); rv.Kind() == reflect.Pointer && !rv.IsNil() {
			seen[rv.Pointer()] = struct{}{}
		}
	}
	for _, d := range a.collectDescribeOnly() {
		if rv := reflect.ValueOf(d); rv.Kind() == reflect.Pointer && !rv.IsNil() {
			if _, dup := seen[rv.Pointer()]; dup {
				continue
			}
			seen[rv.Pointer()] = struct{}{}
		}
		result = append(result, d)
	}
	return result
}

const describerNameMigrations = "migrations"

const describerNameInfra = "infra-requirements"

const describerNameBundle = "migration-bundle"

// describeMigrationBundle assembles a migration bundle and writes it to
// <OutputDir>/migration-bundle/. For each kind it prefers the registered
// runner when it implements protomig.BundleContributor (the runner sees every
// source of its kind); otherwise it falls back to the contributed sources
// themselves. The fallback is what lets `putnami build` emit a faithful bundle
// without per-workload describe wiring: a database plugin registered via
// UseForDescribe never registers a runner, and a workload on a non-Postgres
// runtime backend has no SQL runner either, yet the sources are contributed
// regardless of the backend. Contributors materialize their definitions and
// payloads from the authoring source without touching a live system, so this
// stays a pure, reproducible build step. Apps with no bundle migrations
// contribute nothing and no artifact is written. Sources that put one
// datasource in two schemas fail the describe, and nothing is written.
func (a *Application) describeMigrationBundle(dctx *DescribeContext) error {
	if a.migrationRegistry == nil {
		return nil
	}

	var (
		ops      []protomig.BundleOperation
		payloads []protomig.BundlePayload
	)
	for _, k := range a.migrationRegistry.Kinds() {
		var contributors []protomig.BundleContributor
		if run, ok := a.migrationRegistry.Runner(k); ok {
			if c, ok := run.(protomig.BundleContributor); ok {
				contributors = append(contributors, c)
			}
		}
		if len(contributors) == 0 {
			for _, s := range a.migrationRegistry.Sources(k) {
				if c, ok := s.(protomig.BundleContributor); ok {
					contributors = append(contributors, c)
				}
			}
		}
		for _, c := range contributors {
			kops, kpayloads, err := c.MigrationBundleOperations()
			if err != nil {
				return err
			}
			ops = append(ops, kops...)
			payloads = append(payloads, kpayloads...)
		}
	}

	if len(ops) == 0 {
		return os.RemoveAll(filepath.Join(dctx.OutputDir, "migration-bundle"))
	}
	// The runner refuses one datasource in two schemas at apply time; failing
	// here keeps a workload that cannot migrate from being packaged at all, and
	// removing the bundle an earlier build left keeps it from being packaged
	// instead.
	if err := migration.CheckBundleSchemas(ops); err != nil {
		if rmErr := os.RemoveAll(filepath.Join(dctx.OutputDir, "migration-bundle")); rmErr != nil {
			return stderrors.Join(err, rmErr)
		}
		return err
	}

	// The build job's authored project identity is the publication identity.
	// Runtime application names may be shorter (for example "data-api"). Keep
	// programmatic Describe calls independent of the invoking job's environment.
	appName := a.name
	if os.Getenv(describeEnvVar) != "" {
		if projectName := os.Getenv("PUTNAMI_PROJECT_NAME"); projectName != "" {
			appName = projectName
		}
	}
	bundle := protomig.Bundle{
		Protocol:   protomig.BundleProtocol,
		AppName:    appName,
		Operations: ops,
	}
	return protomig.WriteBundle(filepath.Join(dctx.OutputDir, "migration-bundle"), bundle, payloads)
}

// migrationsDescribeFilename is the registry view's name inside the describe
// output directory. The Go extension declares <project>/.gen/migrations.json as
// a build-describe output, so this file's presence has to be a function of what
// the current sources contribute and nothing else.
const migrationsDescribeFilename = "migrations.json"

// describeMigrations writes the registry-side view to
// <OutputDir>/migrations.json. No DB access; pure in-memory dump of
// what feature plugins contributed at boot — useful for CI assertions
// (e.g., `jq '.kinds[] | select(.kind=="sql") | .sources | length'`).
//
// An app that contributes nothing has any earlier view REMOVED, the way
// describeMigrationBundle removes a stale bundle. The file is captured by
// build-describe's declared output, so leaving one behind would publish, under
// this run's key, a view the current sources do not produce — and every later
// cache hit would restore it.
func (a *Application) describeMigrations(dctx *DescribeContext) error {
	if a.migrationRegistry == nil {
		return removeMigrationsDescribeView(dctx.OutputDir)
	}
	view := migrationDescribeView{Kinds: []migrationDescribeKind{}}
	for _, k := range a.migrationRegistry.Kinds() {
		mk := migrationDescribeKind{
			Kind:    string(k),
			Sources: []migrationDescribeSource{},
		}
		if _, ok := a.migrationRegistry.Runner(k); ok {
			mk.RunnerRegistered = true
		}
		for _, s := range a.migrationRegistry.Sources(k) {
			mk.Sources = append(mk.Sources, migrationDescribeSource{
				Namespace: s.Namespace(),
			})
		}
		view.Kinds = append(view.Kinds, mk)
	}

	if len(view.Kinds) == 0 {
		// No contributions at all → write nothing, and drop an earlier run's
		// view rather than littering the artifact directory with an empty file
		// for an app that does not use migrations.
		return removeMigrationsDescribeView(dctx.OutputDir)
	}

	out := filepath.Join(dctx.OutputDir, migrationsDescribeFilename)
	f, err := os.Create(out) //nolint:gosec // OutputDir is framework-controlled
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(view)
}

// removeMigrationsDescribeView drops a stale registry view. A missing file is
// the expected case, not an error.
func removeMigrationsDescribeView(outputDir string) error {
	//nolint:gosec // OutputDir is framework-controlled and the name is a constant
	if err := os.Remove(filepath.Join(outputDir, migrationsDescribeFilename)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

type migrationDescribeView struct {
	Kinds []migrationDescribeKind `json:"kinds"`
}

type migrationDescribeKind struct {
	Kind             string                    `json:"kind"`
	RunnerRegistered bool                      `json:"runnerRegistered"`
	Sources          []migrationDescribeSource `json:"sources"`
}

type migrationDescribeSource struct {
	Namespace string `json:"namespace"`
}
