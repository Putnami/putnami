package main

import (
	"go.putnami.dev/go/extension/internal/toolchain"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// remoteBuildCacheParam is the option that decides whether this job's `go`
// commands may read and write the run's shared object cache. It defaults to ON;
// the camel-case spelling is the alias resolution produces for a dashed flag.
const remoteBuildCacheParam = "remote-build-cache"

// withRemoteBuildCacheOption wraps EVERY job so the `remote-build-cache` option
// is in force before the job builds its first Go environment.
//
// The dispatch table is the right place for the same reason it is for the
// declared registries next to it: the option has to reach an environment
// builder that a dozen call sites invoke — including ones inside toolchain
// resolution that have no job context — and recording it once at dispatch is
// less plumbing than a parameter on all of them. It wraps every job, not the
// compiling subset, because every job that shells out to `go` at all inherits
// the decision.
//
// The option is deliberately NOT a cache-key input. It selects where already
// identical bytes come from — a local directory or the same directory warmed
// from the object cache — so folding it into a task's identity would split the
// task cache in two for a choice that cannot change a result.
func withRemoteBuildCacheOption(job cli.JobFunc) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if ctx != nil {
			toolchain.UseRemoteBuildCacheOption(ctx.Params.Bool(remoteBuildCacheParam, true, "remoteBuildCache"))
		}
		return job(ctx, emit, args)
	}
}
