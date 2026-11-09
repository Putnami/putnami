package php

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
)

const (
	// ServeJobName is the job of the serve command.
	ServeJobName = "serve"
	// RunJobName is the job of the run command.
	RunJobName = "run"
	// DepsUpgradeJobName is the job of the deps-upgrade command.
	DepsUpgradeJobName = "deps-upgrade"
)

const (
	// CodeNoFrontController is the diagnostic of an application without
	// public/index.php.
	CodeNoFrontController = "php.no_front_controller"
	// CodeRunFailed is the diagnostic of a process the job could not start.
	CodeRunFailed = "php.run_failed"
	// CodeComposerUpdateFailed is the diagnostic of a failed composer update.
	CodeComposerUpdateFailed = "php.composer_update_failed"
)

const (
	defaultServePort  = 3000
	defaultEntrypoint = "bin/console"
	frontController   = "public/index.php"
	// exitCodeKey is the result member that carries a workload's exit code.
	exitCodeKey = "exit-code"
)

// routerScript sends every path that is no file of the document root to the
// front controller. Without a router, the built-in server answers 404 for a
// path whose last segment holds a dot.
const routerScript = `<?php
$root = $_SERVER['DOCUMENT_ROOT'];
$path = rawurldecode(parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH) ?: '/');
if ($path !== '/' && is_file($root . $path)) {
    return false;
}
$_SERVER['SCRIPT_NAME'] = $_SERVER['PHP_SELF'] = '/index.php';
$_SERVER['SCRIPT_FILENAME'] = $root . '/index.php';
chdir($root);
require $root . '/index.php';
`

// ServeJob runs the PHP built-in server on an application until the user stops
// it, and announces it ready once it accepts connections. Port 0 selects a free
// port.
func ServeJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		port := cli.FlagInt(cli.ParseFlags(args), "port", ctx.Params.Int("port", defaultServePort))
		root, err := stack.Selected(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		if _, err := os.Stat(filepath.Join(root.Project, filepath.FromSlash(frontController))); err != nil {
			emit.DiagnosticWithCode("error", fmt.Sprintf("%s has no %s: the server has no front controller", root.Project, frontController), "", 0, 0, CodeNoFrontController)
			return "FAILED", nil, errors.New(CodeNoFrontController)
		}
		if port == 0 {
			if port, err = stacks.FreePort(); err != nil {
				emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
				return "FAILED", nil, fmt.Errorf("%s: %w", CodeRunFailed, err)
			}
		}
		router, err := os.MkdirTemp("", "putnami-php-serve-")
		if err == nil {
			defer func() { _ = os.RemoveAll(router) }()
			err = os.WriteFile(filepath.Join(router, "router.php"), []byte(routerScript), 0o600)
		}
		if err != nil {
			emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
			return "FAILED", nil, fmt.Errorf("%s: %w", CodeRunFailed, err)
		}
		address := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
		emit.Info("serving " + root.Project + " on http://" + net.JoinHostPort("localhost", strconv.Itoa(port)))
		env := append(slices.Clip(root.Env), "PORT="+strconv.Itoa(port))
		serving, done := context.WithCancel(context.Background())
		defer done()
		go stacks.AnnounceReady(serving, emit, port)
		code, stopped, err := stacks.RunForeground(root, root.Program("php"), env, emit, "-S", address, "-t", "public", filepath.Join(router, "router.php"))
		if err != nil {
			emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
			return "FAILED", nil, fmt.Errorf("%s: %w", CodeRunFailed, err)
		}
		if code != 0 && !stopped {
			return "FAILED", map[string]any{exitCodeKey: code}, nil
		}
		return "OK", nil, nil
	}
}

// RunJob runs the application's entrypoint once with the args parameter and
// answers its exit code.
func RunJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		entrypoint := ctx.Params.String("entrypoint")
		if entrypoint == "" {
			entrypoint = defaultEntrypoint
		}
		arguments := strings.Fields(ctx.Params.String("args"))
		root, err := stack.Selected(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		code, _, err := stacks.RunForeground(root, root.Program("php"), root.Env, nil, append([]string{entrypoint}, arguments...)...)
		if err != nil {
			emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
			return "FAILED", nil, fmt.Errorf("%s: %w", CodeRunFailed, err)
		}
		data := map[string]any{exitCodeKey: code}
		if code != 0 {
			// A nil error: the job ran, and the SDK keeps the result data that
			// carries the exit code only when there is none.
			return "FAILED", data, nil
		}
		return "OK", data, nil
	}
}

// DepsUpgradeJob runs composer update at each stack root.
func DepsUpgradeJob(stack stacks.Stack) cli.JobFunc {
	return stack.WorkspaceRootJob(stacks.FailOnMissingToolchain, upgradeRoot)
}

func upgradeRoot(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	if offlineDependencies() {
		err := errors.New("offline dependencies: composer update needs the network")
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeComposerUpdateFailed)
		return fmt.Errorf("%s: %w", CodeComposerUpdateFailed, err)
	}
	if err := prepareWindowsPHP(root, emit); err != nil {
		return err
	}
	phar, err := provideComposer(ctx)
	if err == nil {
		emit.Info("$ composer update in " + root.Dir)
		err = runComposer(ctx, root, composerEnv(root.Env), phar, "update", "--no-interaction", "--no-progress")
	}
	if err != nil {
		emit.DiagnosticWithCode("error", fmt.Sprintf("composer update in %s: %v", root.Dir, err), "", 0, 0, CodeComposerUpdateFailed)
		return fmt.Errorf("%s: %w", CodeComposerUpdateFailed, err)
	}
	return nil
}
