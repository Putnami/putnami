package main

import (
	"path/filepath"
	goruntime "runtime"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/errs"
	"go.putnami.dev/typescript/extension/internal/project"
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

	runtime, err := workloadRuntime(ctx)
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, errs.CodeOf(err).String())
		return "FAILED", nil, nil
	}
	if runtime == project.RuntimeNode {
		return runNodeServe(ctx, emit, bunBin, projectPath, params)
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
	killPort(emit, params, port)

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

// killPort frees the port before a serve when the serve asks for it.
func killPort(emit *jsonl.Emitter, params serve.Params, port int) {
	if !params.KillPort {
		return
	}
	emit.PhaseStart("kill-port")
	status := "skipped"
	if err := serve.KillPortUnavailable(goruntime.GOOS); err != nil {
		emit.Diagnostic("warning", err.Error(), "", 0)
	} else if serve.KillProcessOnPort(port) {
		status = "success"
	}
	emit.PhaseEnd("kill-port", status)
}

// runNodeServe serves a project on the node runtime: it bundles the serve
// entrypoint, the ./serve export else the run entrypoint, and starts node on
// the bundle.
func runNodeServe(ctx *pctx.Context, emit *jsonl.Emitter, bunBin, projectPath string, params serve.Params) (string, map[string]any, error) {
	nodeBin, err := serve.LookNode()
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, errs.CodeOf(err).String())
		return "FAILED", nil, nil
	}
	emit.PhaseStart("resolve-entrypoint")
	entrypoint := params.Entrypoint
	if entrypoint == "" {
		if ep, err := serve.ResolveEntrypoint(projectPath, ""); err == nil {
			entrypoint = ep
		} else if ep, err := resolveRunEntrypoint(projectPath, ""); err == nil {
			entrypoint = ep
		} else {
			emit.PhaseEnd("resolve-entrypoint", "failed")
			emit.DiagnosticWithCode("error", "no serve entrypoint found; add a \"./serve\" export, set package.json \"main\", or pass --entrypoint", "", 0, 0, errs.CodeNoServeExport.String())
			return "FAILED", nil, nil
		}
	}
	emit.PhaseEnd("resolve-entrypoint", "success")

	port := serve.ResolvePort(params.Port)
	killPort(emit, params, port)

	emit.PhaseStart("serve")
	ok, err := serve.RunNodeServe(emit, serve.NodeServe{
		NodeBin:     nodeBin,
		ProjectPath: projectPath,
		Port:        port,
		Params:      params,
		Bundle:      nodeBundler(bunBin, projectPath, nodeBundleDir(ctx, "serve"), entrypoint),
	})
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
