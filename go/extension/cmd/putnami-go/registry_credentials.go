package main

import (
	"context"
	"os"

	"go.putnami.dev/go/extension/internal/toolchain"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
)

// moduleFetchingJobs names the jobs whose `go` commands can reach the module
// origin over the network: they resolve, download, or build against the
// workspace's own published modules.
//
// The dispatch table is the single choke point. Every `go` command this binary
// runs happens inside one of these handlers, and each handler builds its
// environment with toolchain.GoCommandEnv, so hooking here covers `go build`,
// `go mod tidy`, `go mod download`, `go test`, the linters' package loads, and
// the tool installs at once — without putting a subprocess call inside an
// environment builder that pure unit tests invoke thousands of times.
//
// The remaining jobs are excluded because they never fetch: config-extract and
// config-merge read files, the cache phases manage local state, and the infra
// and test-environment jobs are the SDK's own bodies.
var moduleFetchingJobs = map[string]bool{
	"build":          true,
	"build-generate": true,
	"build-describe": true,
	"test":           true,
	"lint":           true,
	"serve":          true,
	"run":            true,
	"package":        true,
	"publish":        true,
	"workspace-sync": true,
}

// ensureGoRegistryCredential is a package var so tests exercise every outcome
// class without a `putnami` binary.
var ensureGoRegistryCredential = registrycred.EnsureNativeCredential

// withDeclaredRegistries wraps EVERY job so the project's effective
// `registries.go` entry — the module origin and the proxy chain — is in force
// before the job builds a single Go environment.
//
// The entry reaches an extension through the job context and only through it,
// because it is deliberately kept out of the resolved parameter bag the cache
// key hashes: pointing a workspace at a mirror must not evict a store that
// holds the same bytes. The dispatch table is therefore the one place that can
// hand it to the environment builder, and it wraps every job rather than the
// module-fetching subset: the entry is also what a project OVERRIDES, so a job
// that only reads the declaration (publish resolving its origin) needs it too.
func withDeclaredRegistries(job cli.JobFunc) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if ctx != nil {
			toolchain.UseContextRegistries(ctx.Params["registries"])
		}
		return job(ctx, emit, args)
	}
}

// withRegistryCredential wraps a job so the user's credential for the module
// origin is refreshed before the job runs any `go` command.
//
// The refresh is best effort by construction: it returns an outcome, never an
// error, and the job runs either way.
func withRegistryCredential(name string, job cli.JobFunc) cli.JobFunc {
	if !moduleFetchingJobs[name] {
		return job
	}
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		refreshModuleOriginCredential(ctx, emit)
		return job(ctx, emit, args)
	}
}

// refreshModuleOriginCredential asks @putnami/cloud to write the user's .netrc
// entry for the declared Go module origin.
//
// Nothing wrote a credential for private Go modules before: the extension
// pointed NETRC at ~/.putnami/.netrc, a path no code has ever created, so a
// `go` command fetching from the origin presented nothing. GoCommandEnv now
// leaves NETRC alone, `go` reads the standard ~/.netrc, and the cloud writes
// the `machine <host>` entry there.
//
// A caller that pinned NETRC is refreshed too: the cloud writes the entry into
// the file NETRC names, the one `go` reads, so skipping the refresh would leave
// that file's entry to expire.
//
// On a hosted run the engine sets the offline signal
// (registrycred.OfflineDependencies) in every job, and the refresh is skipped:
// no go command of a job downloads a module there, so no credential is needed,
// and no job may write one where a repository-controlled process could read
// it. Only workspace-fetch reads a credential on such a run, from the
// descriptor the engine hands it.
func refreshModuleOriginCredential(ctx *pctx.Context, emit *jsonl.Emitter) {
	if registrycred.OfflineDependencies() {
		return
	}
	// os.Environ() carries the same declaration inputs GoCommandEnv reads
	// (GO_REGISTRY_URL, PUTNAMI_WORKSPACE_ROOT), and GoCommandEnv rewrites
	// neither, so the host derived here is the host the built environment
	// resolves to. The job context supplies the workspace root when the
	// environment does not.
	host := toolchain.RegistryCredentialHost(os.Environ(), ctx.WorkspaceRoot)
	if host == "" {
		return
	}
	outcome := ensureGoRegistryCredential(context.Background(), ctx.WorkspaceRoot, host)
	if outcome.Message != "" {
		emit.Log(outcome.Level, outcome.Message)
	}
}
