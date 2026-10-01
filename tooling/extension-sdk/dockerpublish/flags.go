package dockerpublish

import (
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// CommonFlags holds the flags shared by all publish channels.
//
// A channel is not a publish input: the release set names the channels it moves
// and moves them once, after every member has a verified digest. A publisher
// only decides whether it uploads.
type CommonFlags struct {
	DryRun bool
}

// resolveCommonFlags parses the shared publish flags from CLI args (argv) and
// context params, so the same orchestration works whether the caller passes flags
// as argv (CI) or via ctx.Params (the ecosystem extensions' planner).
func resolveCommonFlags(flags map[string]string, params pctx.Params) CommonFlags {
	return CommonFlags{
		DryRun: resolveFlagBool(flags, "dry-run", params, false, "dryRun"),
	}
}

// resolveFlagBool resolves a bool from CLI flags (argv), then params (kebab key +
// optional camelCase fallbacks).
//
//nolint:unparam // defaultVal is kept for API symmetry with the other flag readers
func resolveFlagBool(flags map[string]string, flagKey string, params pctx.Params, defaultVal bool, paramKeys ...string) bool {
	if _, ok := flags[flagKey]; ok {
		return cli.FlagBool(flags, flagKey, defaultVal)
	}
	for _, k := range paramKeys {
		if _, ok := params[k]; ok {
			return params.Bool(k, defaultVal)
		}
	}
	if _, ok := params[flagKey]; ok {
		return params.Bool(flagKey, defaultVal)
	}
	return defaultVal
}

// skipIfNoProject returns true (and emits a summary) when there is no project
// context.
func skipIfNoProject(ctx *pctx.Context, emit *jsonl.Emitter) bool {
	if ctx.Project.Name == "" {
		emit.Summary("Skipped: no project context")
		return true
	}
	return false
}
