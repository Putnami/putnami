package qualify

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	qualifyproto "go.putnami.dev/protocol/qualify"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/compose"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// LocalOptions configure a local target: the workload compose serves on this
// machine, together with the workloads it runs with.
type LocalOptions struct {
	// WorkspaceRoot is the workspace the composition runs in. The worktree that
	// contains it is what the verdict binds to.
	WorkspaceRoot string
	// Config is the loaded workspace configuration; nil loads it.
	Config *wsproto.Config
	// Project is the workload to qualify. It is the composition's target.
	Project *workspace.Project
	// ReadyTimeout bounds each member's typed ready event; zero is compose's
	// default.
	ReadyTimeout time.Duration
	// Output renders the members' serve logs; nil discards them.
	Output jobs.Renderer
	// Preparation renders the engine run that prepares the serve pipelines;
	// nil reports only failures, on stderr.
	Preparation jobs.Renderer
}

// localTarget is a workload composed on this machine. It runs production-mode
// without watch — a watched target would rebuild itself mid-smoke, and the
// verdict could no longer name one tree.
//
// The binding is read when the target is built, before anything is prepared or
// started, and proven again in the version-binding phase: see Worktree.
type localTarget struct {
	options compose.Options
	project string
	root    string
	binding qualifyproto.Binding

	mu sync.Mutex
	// composition is the running composition; nil until Open succeeded.
	composition *compose.Composition
	// baseURL is the target member's stable proxy URL.
	baseURL string
	// failedID is the id of a composition whose start failed after its lease
	// was created; empty otherwise.
	failedID string
	// failedCleanup is the teardown report of a start that failed after it had
	// acquired something. Nil when nothing was acquired.
	failedCleanup *qualifyproto.Cleanup
}

// NewLocalTarget returns the target that composes opts.Project on this machine.
//
// It fingerprints the worktree first, and fails when it cannot: a local verdict
// states which tree it proved, and a target that cannot name its tree would
// produce a verdict that proves nothing.
func NewLocalTarget(opts LocalOptions) (Target, error) {
	if opts.Project == nil {
		return nil, errors.New("--target local needs a project to compose")
	}
	tree, err := git.FingerprintTree(opts.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("--target local binds its verdict to the worktree it serves, and this worktree cannot be fingerprinted: %w", err)
	}
	return &localTarget{
		options: compose.Options{
			WorkspaceRoot: opts.WorkspaceRoot,
			Config:        opts.Config,
			Target:        opts.Project,
			ProxyPort:     0,
			WatchTarget:   false,
			ReadyTimeout:  opts.ReadyTimeout,
			Output:        opts.Output,
			Preparation:   opts.Preparation,
		},
		project: opts.Project.ID,
		root:    opts.WorkspaceRoot,
		binding: treeBindingOf(tree),
	}, nil
}

// treeBindingOf is the one projection of a worktree fingerprint onto the wire
// binding, so the promise and the proof are built the same way.
func treeBindingOf(tree git.TreeFingerprint) qualifyproto.Binding {
	return qualifyproto.Binding{
		Kind:        qualifyproto.BindingTree,
		Fingerprint: tree.Fingerprint,
		Dirty:       tree.Dirty,
		HeadSHA:     tree.HeadSHA,
	}
}

// Describe names the target and the binding it promises. Before Open it is the
// bare local target; afterwards it also carries the composition id and the
// proxy URL the smoke ran against.
func (t *localTarget) Describe() (qualifyproto.Target, qualifyproto.Binding) {
	t.mu.Lock()
	defer t.mu.Unlock()
	target := qualifyproto.Target{Kind: qualifyproto.TargetLocal, URL: t.baseURL}
	if t.composition != nil {
		target.CompositionID = t.composition.ID()
	} else if t.failedID != "" {
		target.CompositionID = t.failedID
	}
	return target, t.binding
}

// Open composes the workload and returns the target member's proxy URL.
//
// A composition failure is composition_failed, never target_unreachable: the
// workload was never reached because it was never served, and the diagnostic
// carries compose's code, the member it concerns and the phase it failed in.
// Whatever the failed start had acquired is torn down by compose before Open
// returns; its report is kept for Close.
func (t *localTarget) Open(ctx context.Context) (string, qualifyproto.Binding, error) {
	_, binding := t.Describe()
	composition, err := compose.Up(ctx, t.options)
	if err != nil {
		t.recordFailedStart(err)
		return "", binding, &TargetError{
			State: qualifyproto.StateCompositionFailed,
			Code:  qualifyproto.PhaseCodeCompositionFailed,
			Err:   err,
		}
	}
	endpoint, ok := composition.Endpoint(t.project)
	t.mu.Lock()
	t.composition, t.baseURL = composition, endpoint
	t.mu.Unlock()
	if !ok {
		return "", binding, &TargetError{
			State: qualifyproto.StateCompositionFailed,
			Code:  qualifyproto.PhaseCodeCompositionFailed,
			Err:   fmt.Errorf("the composition serves no endpoint for %s", t.project),
		}
	}
	return endpoint, binding, nil
}

// recordFailedStart keeps what compose reported about its own teardown, so the
// verdict's cleanup states what a failed start left behind rather than assuming
// it left nothing.
func (t *localTarget) recordFailedStart(err error) {
	var composeErr *compose.Error
	if !errors.As(err, &composeErr) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failedID = composeErr.ID
	if composeErr.Cleanup == nil {
		return
	}
	t.failedCleanup = &qualifyproto.Cleanup{State: composeErr.Cleanup.State, Leftovers: composeErr.Cleanup.Leftovers}
}

// Worktree re-reads the worktree the composition serves. It is what makes a
// local verdict bind to an exact tree rather than to a tree that was there once:
// a preparation step, a generator or an editor that moved a byte between the
// target opening and the workload answering leaves the running workload and the
// recorded fingerprint describing different sources, which the version-binding
// phase reports as digest_mismatch.
func (t *localTarget) Worktree(context.Context) (qualifyproto.Binding, error) {
	tree, err := git.FingerprintTree(t.root)
	if err != nil {
		return qualifyproto.Binding{}, err
	}
	return treeBindingOf(tree), nil
}

// AwaitStartup waits for the target member's application to report completed
// startup: the typed ready event with target workload that compose records
// (compose.Composition.WaitStartup). The composition's own readiness is only
// the member's first claim, a listening port, which an application announces
// before its remaining starters and start hooks ran. A member that exits first
// is composition_failed, naming the member and the phase.
func (t *localTarget) AwaitStartup(ctx context.Context) error {
	t.mu.Lock()
	composition := t.composition
	t.mu.Unlock()
	if composition == nil {
		return &TargetError{
			State: qualifyproto.StateCompositionFailed,
			Code:  qualifyproto.PhaseCodeCompositionFailed,
			Err:   fmt.Errorf("no composition serves %s", t.project),
		}
	}
	err := composition.WaitStartup(ctx, t.project)
	var composeErr *compose.Error
	if errors.As(err, &composeErr) {
		return &TargetError{
			State: qualifyproto.StateCompositionFailed,
			Code:  qualifyproto.PhaseCodeCompositionFailed,
			Err:   err,
		}
	}
	return err
}

// Close stops the composition: its members, proxies and databases. What survives
// is named in the cleanup, which makes the verdict a non-pass — a run that
// leaked a process or a database proved what it did at the cost of the next one.
func (t *localTarget) Close(ctx context.Context) *qualifyproto.Cleanup {
	t.mu.Lock()
	composition, failed := t.composition, t.failedCleanup
	t.mu.Unlock()
	switch {
	case composition != nil:
		report := composition.Close(ctx)
		return &qualifyproto.Cleanup{State: report.State, Leftovers: report.Leftovers}
	case failed != nil:
		return failed
	default:
		// Nothing was acquired: the run was canceled before Open, or the
		// composition was refused while planning.
		return &qualifyproto.Cleanup{State: qualifyproto.CleanupClean}
	}
}
