//go:build !putnami_runtime_codegen

package main

import (
	"go.putnami.dev/go/extension/internal/jobs/apicheck"
	"go.putnami.dev/go/extension/internal/jobs/build"
	"go.putnami.dev/go/extension/internal/jobs/configextract"
	"go.putnami.dev/go/extension/internal/jobs/configmerge"
	"go.putnami.dev/go/extension/internal/jobs/lint"
	"go.putnami.dev/go/extension/internal/jobs/pkg"
	"go.putnami.dev/go/extension/internal/jobs/publish"
	"go.putnami.dev/go/extension/internal/jobs/run"
	"go.putnami.dev/go/extension/internal/jobs/serve"
	"go.putnami.dev/go/extension/internal/jobs/test"
	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/dbtestenv"
	"go.putnami.dev/sdk/extension/docslinks"
	"go.putnami.dev/sdk/extension/infraagg"
)

// generalHandlers are the jobs of the runtime's main executable: every job
// but the codegen entry's (handlers_codegen.go). The codegen entry, built with
// the putnami_runtime_codegen tag, leaves them out (handlers_general_off.go).
func generalHandlers() map[string]cli.JobFunc {
	return map[string]cli.JobFunc{
		"build": build.Run,
		// Infra aggregation is the SDK's shared task body. Go passes no runtime compatibility hook: a Go workload serves
		// whatever the platform defaults to, so it constrains nothing.
		"build-infra": infraagg.Job(infraagg.Options{}),
		// The deployment declaration is the same aggregate in canonical bytes,
		// under the same hook as build-infra so the two never disagree.
		"package-deployment": infraagg.DeploymentJob(infraagg.Options{}),
		// The documentation link check is the SDK's shared task body too, so a
		// broken link fails lint the same way in every language.
		"lint-docs": docslinks.Job(),
		// The database test environment is the SDK's shared task body: policy,
		// closure discovery, the Postgres container and the invocation-scoped
		// sensitive binding are identical for every language, so three copies of
		// them would drift.
		"test-env-up":    dbtestenv.UpJob(),
		"test-env-down":  dbtestenv.DownJob(),
		"test":           test.Run,
		"test-race":      test.RunRace,
		"lint":           lint.Run,
		"validate-api":   apicheck.Run,
		"serve":          serve.Run,
		"run":            run.Run,
		"package":        pkg.Run,
		"publish":        publish.Run,
		"config-extract": configextract.Run,
		"config-merge":   configmerge.Run,
		"workspace-sync": runWorkspaceSync,
	}
}
