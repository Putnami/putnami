package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	runner "go.putnami.dev/protocol/runner"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/doctor"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
)

// This file is the BOUND-REQUEST ADAPTER over engine.Run: the
// executing side of portable execution. A runner provider materializes the
// submitted snapshot, launches the snapshot's pinned entrypoint with no
// arguments and BoundRequestEnv naming the bound request, and this adapter
// turns the typed request — never argv — into the engine request. Everything
// the run does is the ordinary engine lifecycle; the request only freezes what
// the submitting engine already resolved.

// runBoundRequest executes one bound portable request and returns the gate's
// exit code. It refuses every argument except the run credential's
// descriptor, which Capture has already read: raw argv is never a second
// execution authority beside the typed request.
//
// boot serves the lock-pinned extension downloads of the first-use bootstrap
// and ends before the request's credential provider is installed.
func (a *App) runBoundRequest(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, requestPath, providersEnv string, boot *bootstrapProvider) int {
	// The channel is consumed HERE and nowhere below: every task subprocess,
	// hook and bootstrap job inherits this process environment, and a nested
	// CLI invocation that still saw the variable would be hijacked into a
	// second bound execution of the same snapshot.
	_ = os.Unsetenv(runnerprovider.BoundRequestEnv)
	if len(runcredential.WithoutFlag(args)) != 0 {
		iox.Fprintf(os.Stderr, "putnami: %s names a bound execution request; arguments are not accepted beside it, except %s\n", runnerprovider.BoundRequestEnv, runcredential.Flag)
		return ExitUsage
	}
	if wsRoot == "" {
		iox.Fprintf(os.Stderr, "putnami: bound execution request needs a workspace (looking for %s)\n", wsproto.WorkspaceConfigFilename)
		return ExitError
	}
	request, err := loadBoundRequest(requestPath)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: bound execution request: %v\n", err)
		return ExitUsage
	}
	if request.Invocation.Cwd != "." {
		if err := os.Chdir(filepath.Join(wsRoot, filepath.FromSlash(request.Invocation.Cwd))); err != nil {
			iox.Fprintf(os.Stderr, "putnami: bound execution request cwd: %v\n", err)
			return ExitUsage
		}
	}
	params, err := runner.NativeParams(request.Invocation.Params)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: bound execution request: %v\n", err)
		return ExitUsage
	}
	global := boundGlobalFlags(request)
	// The same first-use bootstrap the terminal path runs: the snapshot carries
	// no installed workspace state, so dependencies and lock-pinned extensions
	// are restored through the ordinary lifecycle before the engine plans.
	bootstrapGlobal := global
	engine.ApplyEnvOverrides(&bootstrapGlobal, cfg)
	boot.ensureArtifactsAndClose(ctx, wsRoot, cfg, true)
	ensureArtifactsForProcessMode(ctx, wsRoot, cfg, false)
	// The request's providers, never PUTNAMI_PROVIDERS, decide which purposes
	// the credential provider serves in the executing engine.
	providers, source := boundRequestProviders(&request, providersEnv, os.Stderr)
	if err := guardCredentials(providers); err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return ExitError
	}
	stopProviders, err := installCredentialProviders(providers, source, wsRoot, cfg, nil, os.Stderr)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return ExitError
	}
	defer stopProviders()
	// A hosted request reads the remote cache through one provider, which the
	// bootstrap starts before the implicit install's first repository code.
	hostedCache := &engine.HostedRemoteCache{}
	defer hostedCache.Close()
	lifecycle.EnsureWorkspaceBootstrap(ctx, wsRoot, cfg, lifecycle.BootstrapOptions{
		Command: request.Invocation.Commands[0], Output: bootstrapGlobal.Output,
		Display: bootstrapLifecycleDisplay(bootstrapGlobal), RunJob: RunWorkspaceJob,
		BeforeRepositoryCode: func(ctx context.Context) error {
			_, err := hostedCache.Start(ctx, &engine.Request{WorkspaceRoot: wsRoot, Config: cfg, Global: global})
			return err
		},
	})
	result, _ := engine.New().Run(ctx, engine.Request{
		WorkspaceRoot:     wsRoot,
		Config:            cfg,
		Commands:          append([]string{}, request.Invocation.Commands...),
		Global:            global,
		CommandParams:     params,
		Hooks:             cfg.Hooks,
		HostedRemoteCache: hostedCache,
		// No observer (ADR 0001 §4): the submitter's placement is the user's
		// run, and the executing side must not report a second session for it.
		Preflight:       doctor.DoctorPreflight,
		VersionSnapshot: boundVersionSnapshot(request),
		Portable:        &engine.PortableExecution{Request: request},
	}, nil)
	return result.ExitCode
}

// boundRequestProviders returns the invocation providers the executing engine
// enables for request, and their source. They are the request's
// invocation.providers, except publish when the request carries no
// invocation.publication: such a request plans no publication, so the publish
// purpose stays off, and stderr says so.
func boundRequestProviders(request *runner.ExecutionRequest, providersEnv string, stderr io.Writer) ([]string, string) {
	providers, source, _ := invocationProviders(request, nil, providersEnv)
	if request.Invocation.Publication != nil || !slices.Contains(providers, runner.InvocationProviderPublish) {
		return providers, source
	}
	iox.Fprintf(stderr, "putnami: %s names %s without invocation.publication; the publish purpose stays off\n",
		source, runner.InvocationProviderPublish)
	return slices.DeleteFunc(providers, func(provider string) bool {
		return provider == runner.InvocationProviderPublish
	}), source
}

func loadBoundRequest(path string) (runner.ExecutionRequest, error) {
	info, err := os.Stat(path)
	if err != nil {
		return runner.ExecutionRequest{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > runner.MaxRequestBytes {
		return runner.ExecutionRequest{}, fmt.Errorf("%s is not a bounded regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return runner.ExecutionRequest{}, err
	}
	return runner.ParseExecutionRequest(data)
}

// boundGlobalFlags is the typed request's flag block projected back onto the
// engine's run-shaping flags. Placement is remote by construction; selection
// flags are inert because the engine plans the frozen selection.
func boundGlobalFlags(request runner.ExecutionRequest) GlobalFlags {
	flags := request.Invocation.Flags
	budgets := map[string]int{}
	for name, units := range flags.ResourceBudgets {
		budgets[name] = units
	}
	return GlobalFlags{
		Where: "remote", Verbose: flags.Verbose, Debug: flags.Debug, Quiet: flags.Quiet,
		NoCache: flags.NoCache, NoCacheExplicit: flags.NoCacheExplicit, RetryFailed: flags.RetryFailed,
		CacheTrust: flags.CacheTrust, MaxParallel: flags.MaxParallel, MaxParallelMode: flags.MaxParallelMode,
		ResourceBudgets: budgets, CPUBudgetPolicy: flags.CPUBudgetPolicy, Retry: flags.Retry,
		ContinueOnErr: flags.ContinueOnError, Output: flags.Output, ImpactedStrict: flags.ImpactedStrict,
		EnvProfile: flags.Profile, Projects: strings.Join(request.Selection.Projects, ","),
		Providers: append([]string(nil), request.Invocation.Providers...),
	}
}

// boundVersionSnapshot restores the tree state the submitter stamped so the
// version-derived inputs of every task match the submitting run's.
func boundVersionSnapshot(request runner.ExecutionRequest) *git.VersionInfo {
	for _, version := range request.Source.Versions {
		if version.Line == "" {
			return &git.VersionInfo{Base: version.Base, Full: version.Full, SHA: version.SHA, Branch: version.Branch,
				Suffix: version.Suffix, IsDirty: version.Dirty, Tagged: version.Tagged, Tag: version.Tag}
		}
	}
	return nil
}
