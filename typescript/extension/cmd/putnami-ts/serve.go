package main

import (
	"path/filepath"
	"runtime"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/errs"
	"go.putnami.dev/typescript/extension/internal/serve"
)

func runServe(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)

	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}

	params := serve.Params{
		Entrypoint:  ctx.Params.String("entrypoint"),
		Watch:       ctx.Params.Bool("watch", true),
		KillPort:    ctx.Params.Bool("kill-port", false, "killPort"),
		Port:        ctx.Params.Int("port", 0),
		Inspect:     ctx.Params.Bool("inspect", false),
		InspectWait: ctx.Params.Bool("inspect-wait", false, "inspectWait"),
		InspectBrk:  ctx.Params.Bool("inspect-brk", false, "inspectBrk"),
		Debug:       ctx.Params.Bool("debug", false),
	}

	// Resolve entrypoint
	entrypoint := params.Entrypoint
	if entrypoint == "" {
		emit.PhaseStart("resolve-entrypoint")
		ep, err := serve.ResolveEntrypoint(projectPath, "")
		if err != nil {
			emit.PhaseEnd("resolve-entrypoint", "failed")
			emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, errs.CodeOf(err).String())
			return "FAILED", nil, nil
		}
		entrypoint = ep
		emit.PhaseEnd("resolve-entrypoint", "success")
	}

	port := serve.ResolvePort(params.Port)

	// Kill port
	if params.KillPort {
		emit.PhaseStart("kill-port")
		status := "skipped"
		if err := serve.KillPortUnavailable(runtime.GOOS); err != nil {
			emit.Diagnostic("warning", err.Error(), "", 0)
		} else if serve.KillProcessOnPort(port) {
			status = "success"
		}
		emit.PhaseEnd("kill-port", status)
	}

	// Serve
	emit.PhaseStart("serve")
	ok, err := serve.RunBunServe(emit, bunBin, projectPath, entrypoint, port, params)
	if err != nil {
		emit.PhaseEnd("serve", "failed")
		return "FAILED", nil, err
	}

	if ok {
		emit.PhaseEnd("serve", "success")
		return "OK", nil, nil
	}

	emit.PhaseEnd("serve", "failed")
	return "FAILED", nil, nil
}
