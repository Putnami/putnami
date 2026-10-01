// Package publish implements the Go extension's publish channels: go-module (the
// proprietary gomod-write protocol) and docker (the shared, ecosystem-agnostic
// orchestration). go-module publishing is Go-specific so it lives here; docker is
// shared with the TypeScript extension via go.putnami.dev/sdk/extension/dockerpublish.
package publish

import (
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dockerpublish"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Run dispatches a publish job to the requested channel. Each manifest run step
// invokes this with a single channel flag (--go or --docker), so exactly one
// branch fires per invocation; an invocation with neither is a no-op skip.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	flags := cli.ParseFlags(args)
	switch {
	case cli.FlagBool(flags, "go", false):
		return goModule(ctx, emit, args)
	case cli.FlagBool(flags, "docker", false):
		return dockerpublish.Publish(ctx, emit, args)
	default:
		return "SKIP", nil, nil
	}
}
