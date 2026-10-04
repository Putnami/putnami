package cli

import (
	"context"
	"fmt"
	"io"
	"maps"
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
// submitted snapshot, or checks out the commit a version 2 request names,
// launches the pinned entrypoint with no arguments and BoundRequestEnv naming
// the bound request, and this adapter turns the typed request — never argv —
// into the engine request. Everything the run does is the ordinary engine
// lifecycle; a version 1 request only freezes what the submitting engine
// already resolved, and a version 2 request names the selection the engine
// resolves on the checkout.

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
	// A version 2 request runs on the checkout of the commit it names, and on
	// nothing else: the check precedes the first-use bootstrap, hooks and every
	// task. It cannot precede this process, which the checkout's entrypoint
	// started, built from the checkout in a source workspace.
	if request.Commit != nil {
		if err := verifyCommitCheckout(wsRoot, request.Commit.Source.Commit); err != nil {
			iox.Fprintf(os.Stderr, "putnami: bound execution request refused: %v\n", err)
			return ExitError
		}
	}
	invocation := request.Invocation()
	// A request that may publish reads its bound commit's ancestry now, before
	// the bootstrap or an install runs repository code that could rewrite
	// refs; the engine run reuses that snapshot (engine.CaptureAncestry).
	ctx = engine.CaptureAncestry(ctx, wsRoot, invocation.Commands, &invocation)
	if invocation.Cwd != "." {
		if err := os.Chdir(filepath.Join(wsRoot, filepath.FromSlash(invocation.Cwd))); err != nil {
			iox.Fprintf(os.Stderr, "putnami: bound execution request cwd: %v\n", err)
			return ExitUsage
		}
	}
	params, err := runner.NativeParams(invocation.Params)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: bound execution request: %v\n", err)
		return ExitUsage
	}
	global := boundGlobalFlags(request)
	// The same first-use bootstrap the terminal path runs: the snapshot or the
	// checkout carries no installed workspace state, so dependencies and
	// lock-pinned extensions are restored through the ordinary lifecycle before
	// the engine plans.
	bootstrapGlobal := global
	engine.ApplyEnvOverrides(&bootstrapGlobal, cfg)
	boot.ensureArtifactsAndClose(ctx, wsRoot, cfg, true)
	ensureArtifactsForProcessMode(ctx, wsRoot, cfg, false)
	// The request's providers, never PUTNAMI_PROVIDERS, decide which purposes
	// the credential provider serves in the executing engine.
	providers, source := boundRequestProviders(&invocation, providersEnv, os.Stderr)
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
	// A hosted request reads the remote cache through one provider, and hands
	// its reporters the run credential, which the bootstrap starts before the
	// implicit install's first repository code.
	hostedCache := &engine.HostedRemoteCache{}
	defer hostedCache.Close()
	hostedReporters := &engine.HostedReporters{}
	defer hostedReporters.Close()
	lifecycle.EnsureWorkspaceBootstrap(ctx, wsRoot, cfg, lifecycle.BootstrapOptions{
		Command: invocation.Commands[0], Output: bootstrapGlobal.Output,
		Display: bootstrapLifecycleDisplay(bootstrapGlobal), RunJob: RunWorkspaceJob,
		BeforeRepositoryCode: func(ctx context.Context) error {
			req := &engine.Request{WorkspaceRoot: wsRoot, Config: cfg, Global: global}
			if _, err := hostedCache.Start(ctx, req); err != nil {
				return err
			}
			hostedReporters.Start(ctx, req)
			return nil
		},
	})
	result, _ := engine.New().Run(ctx, engine.Request{
		WorkspaceRoot:     wsRoot,
		Config:            cfg,
		Commands:          append([]string{}, invocation.Commands...),
		Global:            global,
		CommandParams:     params,
		Hooks:             cfg.Hooks,
		HostedRemoteCache: hostedCache,
		HostedReporters:   hostedReporters,
		// No observer (ADR 0001 §4): the submitter's placement is the user's
		// run, and the executing side must not report a second session for it.
		Preflight:       doctor.DoctorPreflight,
		VersionSnapshot: boundVersionSnapshot(request),
		Portable:        boundPortableExecution(request),
	}, nil)
	return result.ExitCode
}

// boundPortableExecution is the engine's binding of request: frozen for a
// version 1 request, engine-planned for a version 2 one.
func boundPortableExecution(request runner.BoundRequest) *engine.PortableExecution {
	if request.Commit != nil {
		return &engine.PortableExecution{Commit: request.Commit}
	}
	return &engine.PortableExecution{Request: *request.Snapshot}
}

// verifyCommitCheckout refuses a version 2 request on any checkout but the
// commit it names, unmodified: HEAD must equal commit, and no tracked file may
// differ from HEAD, in the index or the working tree. Untracked files are not
// checked; a provider's caches may sit beside the checkout. The request
// addresses the commit's content, so a checkout of other content is refused
// rather than run under the request's name.
func verifyCommitCheckout(wsRoot, commit string) error {
	head, err := git.HeadSHA(wsRoot)
	if err != nil {
		return fmt.Errorf("source.commit %s needs a Git checkout at %s: %w", commit, wsRoot, err)
	}
	if head != commit {
		return fmt.Errorf("the checkout's HEAD is %s, but the request names source.commit %s", head, commit)
	}
	modified, err := git.TrackedDirtyDigests(wsRoot)
	if err != nil {
		return fmt.Errorf("read the checkout's modified files: %w", err)
	}
	if len(modified) > 0 {
		paths := slices.Sorted(maps.Keys(modified))
		if len(paths) > 5 {
			paths = append(paths[:5], fmt.Sprintf("and %d more", len(paths)-5))
		}
		return fmt.Errorf("the checkout of source.commit %s modifies %d tracked file(s): %s", commit, len(modified), strings.Join(paths, ", "))
	}
	return nil
}

// boundRequestProviders returns the invocation providers the executing engine
// enables for a bound request's invocation block, and their source. They are
// its invocation.providers, except publish when it carries no
// invocation.publication: such a request plans no publication, so the publish
// purpose stays off, and stderr says so.
func boundRequestProviders(invocation *runner.InvocationBlock, providersEnv string, stderr io.Writer) ([]string, string) {
	providers, source, _ := invocationProviders(invocation, nil, providersEnv)
	if invocation.Publication != nil || !slices.Contains(providers, runner.InvocationProviderPublish) {
		return providers, source
	}
	iox.Fprintf(stderr, "putnami: %s names %s without invocation.publication; the publish purpose stays off\n",
		source, runner.InvocationProviderPublish)
	return slices.DeleteFunc(providers, func(provider string) bool {
		return provider == runner.InvocationProviderPublish
	}), source
}

// loadBoundRequest reads the bound request of either version: the channel
// dispatches on the document's version member.
func loadBoundRequest(path string) (runner.BoundRequest, error) {
	info, err := os.Stat(path)
	if err != nil {
		return runner.BoundRequest{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > runner.MaxRequestBytes {
		return runner.BoundRequest{}, fmt.Errorf("%s is not a bounded regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return runner.BoundRequest{}, err
	}
	return runner.ParseBoundRequest(data)
}

// boundGlobalFlags is the typed request's flag block projected back onto the
// engine's run-shaping flags. Placement is remote by construction. Selection
// flags are inert: the engine plans a version 1 request's frozen selection,
// and binds a version 2 request's requested selection itself
// (engine.PortableExecution).
func boundGlobalFlags(request runner.BoundRequest) GlobalFlags {
	invocation := request.Invocation()
	flags := invocation.Flags
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
		EnvProfile: flags.Profile, Projects: frozenProjects(request),
		Providers: append([]string(nil), invocation.Providers...),
	}
}

// frozenProjects is a version 1 request's frozen project ids as one selector,
// "" for a version 2 request.
func frozenProjects(request runner.BoundRequest) string {
	if request.Snapshot == nil {
		return ""
	}
	return strings.Join(request.Snapshot.Selection.Projects, ",")
}

// boundVersionSnapshot restores the tree state the submitter of a version 1
// request stamped so the version-derived inputs of every task match the
// submitting run's. A version 2 request has none: the engine stamps versions
// from the checkout's Git history, as on a local run.
func boundVersionSnapshot(request runner.BoundRequest) *git.VersionInfo {
	if request.Snapshot == nil {
		return nil
	}
	for _, version := range request.Snapshot.Source.Versions {
		if version.Line == "" {
			return &git.VersionInfo{Base: version.Base, Full: version.Full, SHA: version.SHA, Branch: version.Branch,
				Suffix: version.Suffix, IsDirty: version.Dirty, Tagged: version.Tagged, Tag: version.Tag}
		}
	}
	return nil
}
