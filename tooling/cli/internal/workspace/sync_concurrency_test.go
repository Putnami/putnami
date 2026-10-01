package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/flock"
)

type rendezvousProbeProvider struct {
	name    string
	result  wsproto.ProbeResult
	started chan<- string
	release <-chan struct{}
}

func (p *rendezvousProbeProvider) Name() string { return p.name }

func (p *rendezvousProbeProvider) Probe(wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	if p.started != nil {
		p.started <- p.name
	}
	if p.release != nil {
		<-p.release
	}
	return p.result, nil
}

func TestParallelProbesMergeDeterministically(t *testing.T) {
	spectest.Proves(t, "cli/workspace-initialization", "parallel-probes", "parallel-probes-merge-deterministically")
	workers := probeWorkerCount(3)
	if workers < 2 {
		t.Skipf("host grants %d workspace probe worker", workers)
	}

	root := t.TempDir()
	includes := make([]string, workers)
	for i := range workers {
		includes[i] = fmt.Sprintf("p%d", i)
		writeFileAt(t, filepath.Join(root, includes[i], "package.json"), fmt.Sprintf(`{"name":"@acme/%s"}`, includes[i]))
	}
	writeFileAt(t, filepath.Join(root, wsproto.WorkspaceConfigFilename),
		fmt.Sprintf(`{"includes":["%s"]}`, strings.Join(includes, `","`)))
	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })
	ws, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan string, workers)
	release := make(chan struct{})
	bindings := make([]ProviderBinding, workers)
	for i := range workers {
		name := fmt.Sprintf("@putnami/provider-%d", workers-1-i)
		path := fmt.Sprintf("p%d", workers-1-i)
		bindings[i] = ProviderBinding{
			Scope: scopeFor(t, name, []string{"package.json"}, []string{"package.json"}),
			Provider: &rendezvousProbeProvider{
				name: name, started: started, release: release,
				result: wsproto.ProbeResult{
					Version: wsproto.ProbeProtocolVersion, Extension: name,
					Projects: []wsproto.ProbeProject{{Path: path, SourceName: "@acme/" + path}},
				},
			},
		}
	}

	done := make(chan struct {
		outcome *SyncOutcome
		err     error
	}, 1)
	go func() {
		outcome, syncErr := Synchronize(SyncRequest{Context: context.Background(), Workspace: ws, Providers: bindings})
		done <- struct {
			outcome *SyncOutcome
			err     error
		}{outcome: outcome, err: syncErr}
	}()

	allStarted := true
	for range workers {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			allStarted = false
		}
		if !allStarted {
			break
		}
	}
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if !allStarted {
		t.Fatal("independent workspace providers did not overlap")
	}

	wantOrder := make([]string, workers)
	for i := range workers {
		wantOrder[i] = fmt.Sprintf("@putnami/provider-%d", i)
	}
	if got := result.outcome.Probed; strings.Join(got, "\x00") != strings.Join(wantOrder, "\x00") {
		t.Fatalf("probed order = %v, want %v", got, wantOrder)
	}
	gotProviders := make([]string, len(result.outcome.Snapshot.Providers))
	for i, provider := range result.outcome.Snapshot.Providers {
		gotProviders[i] = provider.Extension
	}
	if strings.Join(gotProviders, "\x00") != strings.Join(wantOrder, "\x00") {
		t.Fatalf("snapshot provider order = %v, want %v", gotProviders, wantOrder)
	}
	firstDigest := result.outcome.Snapshot.ProbeDigest

	// Re-probe the same answers in the opposite binding order. Full forces the
	// processes to run again; the normalized aggregate must remain identical.
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Scope.Extension < bindings[j].Scope.Extension })
	for i := range bindings {
		provider := bindings[i].Provider.(*rendezvousProbeProvider)
		provider.started = nil
		provider.release = nil
	}
	again, err := Synchronize(SyncRequest{Workspace: ws, Providers: bindings, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if again.Snapshot.ProbeDigest != firstDigest {
		t.Fatalf("parallel merge digest changed: first=%s second=%s", firstDigest, again.Snapshot.ProbeDigest)
	}
}

const workspaceSyncHelperEnv = "PUTNAMI_WORKSPACE_SYNC_HELPER"

type fileCountingProbeProvider struct {
	counter string
}

func (p *fileCountingProbeProvider) Name() string { return "@putnami/test-provider" }

func (p *fileCountingProbeProvider) Probe(wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	f, err := os.OpenFile(p.counter, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return wsproto.ProbeResult{}, err
	}
	if _, err := f.WriteString("probe\n"); err != nil {
		_ = f.Close()
		return wsproto.ProbeResult{}, err
	}
	if err := f.Close(); err != nil {
		return wsproto.ProbeResult{}, err
	}
	time.Sleep(250 * time.Millisecond)
	return wsproto.ProbeResult{
		Version: wsproto.ProbeProtocolVersion, Extension: p.Name(),
		Projects: []wsproto.ProbeProject{{Path: "app", SourceName: "@acme/app"}},
	}, nil
}

func TestConcurrentPreparationSharesOneIndexRefresh(t *testing.T) {
	spectest.Proves(t, "cli/workspace-initialization", "graph-readiness", "concurrent-preparation-shares-one-index-refresh")
	if runtime.GOOS == "windows" {
		t.Skip("flock ownership is Unix-only")
	}
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"includes":["app"]}`)
	writeFileAt(t, filepath.Join(root, "app", "package.json"), `{"name":"@acme/app"}`)
	counter := filepath.Join(t.TempDir(), "probes")

	const processes = 4
	commands := make([]*exec.Cmd, processes)
	outputs := make([]bytes.Buffer, processes)
	for i := range commands {
		commands[i] = exec.Command(os.Args[0], "-test.run=^TestWorkspaceSyncHelperProcess$")
		commands[i].Env = append(os.Environ(),
			workspaceSyncHelperEnv+"=1",
			"PUTNAMI_WORKSPACE_SYNC_ROOT="+root,
			"PUTNAMI_WORKSPACE_SYNC_COUNTER="+counter,
		)
		commands[i].Stdout = &outputs[i]
		commands[i].Stderr = &outputs[i]
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("helper %d: %v\n%s", i, err, outputs[i].String())
		}
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "probe\n"); got != 1 {
		t.Fatalf("provider probes across %d processes = %d, want 1", processes, got)
	}
	snapshot, err := LoadSnapshot(root)
	if err != nil || snapshot == nil || len(snapshot.Providers) != 1 {
		t.Fatalf("published snapshot = %+v, err=%v", snapshot, err)
	}
}

func TestWorkspaceSyncHelperProcess(t *testing.T) {
	if os.Getenv(workspaceSyncHelperEnv) != "1" {
		return
	}
	root := os.Getenv("PUTNAMI_WORKSPACE_SYNC_ROOT")
	ws, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	provider := &fileCountingProbeProvider{counter: os.Getenv("PUTNAMI_WORKSPACE_SYNC_COUNTER")}
	_, err = Synchronize(SyncRequest{
		Context: context.Background(), Workspace: ws,
		Providers: []ProviderBinding{{
			Scope:    scopeFor(t, provider.Name(), []string{"package.json"}, []string{"package.json"}),
			Provider: provider,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSynchronizeLockFailureKeepsInMemoryViewWithoutPublishing(t *testing.T) {
	ws := syncFixture(t)
	lockPath := filepath.Join(ws.Root, ".putnami", workspaceSyncLockFilename)
	if err := os.MkdirAll(lockPath, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := &countingProvider{name: "@putnami/typescript", answer: func(wsproto.ProbeRequest) wsproto.ProbeResult {
		return wsproto.ProbeResult{
			Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/typescript",
			Projects: []wsproto.ProbeProject{{Path: "web", SourceName: "@acme/web"}},
		}
	}}
	outcome, err := Synchronize(SyncRequest{
		Workspace: ws,
		Providers: []ProviderBinding{{
			Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
			Provider: provider,
		}},
		Reason: wsproto.ProbeReasonLoad,
	})
	if err != nil {
		t.Fatalf("read-only lock failure must recover in memory: %v", err)
	}
	if outcome == nil || provider.calls != 1 || outcome.Persisted {
		t.Fatalf("fallback outcome = %+v, provider calls = %d", outcome, provider.calls)
	}
	if got := outcome.Merged["web"].SourceName; got != "@acme/web" {
		t.Fatalf("in-memory provider view = %q, want @acme/web", got)
	}
	foundWarning := false
	for _, diagnostic := range outcome.Diagnostics {
		if diagnostic.Code == extproto.FailureWorkspaceSnapshotInvalid &&
			strings.Contains(diagnostic.Message, "no snapshot write") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatalf("fallback diagnostics = %+v, want %s warning", outcome.Diagnostics, extproto.FailureWorkspaceSnapshotInvalid)
	}
	if _, err := os.Stat(SnapshotPath(ws.Root)); !os.IsNotExist(err) {
		t.Fatalf("unlocked fallback published the workspace index: %v", err)
	}
}

func TestWorkspaceSyncLockContentionNotifiesAndTimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock contention is Unix-only")
	}
	root := t.TempDir()
	lockPath := filepath.Join(root, ".putnami", workspaceSyncLockFilename)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := flock.Acquire(lockPath, true, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })

	var notices []string
	started := time.Now()
	release, err := acquireWorkspaceSyncLockWithOptions(context.Background(), root, workspaceSyncLockOptions{
		maxWait: 80 * time.Millisecond, noticeAfter: 10 * time.Millisecond,
		onWait: func(message string) { notices = append(notices, message) },
	})
	if release != nil {
		release()
		t.Fatal("contended lock unexpectedly acquired")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contention error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded lock wait took %s", elapsed)
	}
	if len(notices) != 1 || notices[0] != workspaceSyncLockWaitMessage {
		t.Fatalf("wait notices = %v", notices)
	}
	writeFileAt(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"includes":["app"]}`)
	writeFileAt(t, filepath.Join(root, "app", "package.json"), `{"name":"@acme/app"}`)
	InvalidateLoadCache(root)
	ws, loadErr := Load(root)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	provider := &countingProvider{name: "@putnami/typescript"}
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	outcome, syncErr := Synchronize(SyncRequest{
		Context:   waitCtx,
		Workspace: ws,
		Providers: []ProviderBinding{{
			Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
			Provider: provider,
		}},
	})
	if outcome != nil || !errors.Is(syncErr, context.DeadlineExceeded) || provider.calls != 0 {
		t.Fatalf("timed-out synchronization = outcome %+v, err %v, provider calls %d", outcome, syncErr, provider.calls)
	}
	if _, statErr := os.Stat(SnapshotPath(root)); !os.IsNotExist(statErr) {
		t.Fatalf("timed-out synchronization published without the held lock: %v", statErr)
	}

	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	release, err = acquireWorkspaceSyncLockWithOptions(context.Background(), root, workspaceSyncLockOptions{
		maxWait: 100 * time.Millisecond, noticeAfter: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("released lock remained unavailable: %v", err)
	}
	release()
}

func TestWorkspaceSyncLockInProcessWaitHonorsCancellation(t *testing.T) {
	ws := syncFixture(t)
	root := ws.Root
	key := loadKey(root)
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	workspaceSyncGates.Store(key, gate)
	t.Cleanup(func() {
		<-gate
		workspaceSyncGates.Delete(key)
	})

	ctx, cancel := context.WithCancel(context.Background())
	var notice string
	release, err := acquireWorkspaceSyncLockWithOptions(ctx, root, workspaceSyncLockOptions{
		maxWait: 500 * time.Millisecond, noticeAfter: 10 * time.Millisecond,
		onWait: func(message string) {
			notice = message
			cancel()
		},
	})
	if release != nil {
		release()
		t.Fatal("in-process gate unexpectedly acquired")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("in-process wait error = %v, want canceled", err)
	}
	if notice != workspaceSyncLockWaitMessage {
		t.Fatalf("wait notice = %q", notice)
	}
	provider := &countingProvider{name: "@putnami/typescript"}
	outcome, syncErr := Synchronize(SyncRequest{
		Context:   ctx,
		Workspace: ws,
		Providers: []ProviderBinding{{
			Scope:    scopeFor(t, provider.name, []string{"package.json"}, []string{"package.json"}),
			Provider: provider,
		}},
	})
	if outcome != nil || !errors.Is(syncErr, context.Canceled) || provider.calls != 0 {
		t.Fatalf("canceled synchronization = outcome %+v, err %v, provider calls %d", outcome, syncErr, provider.calls)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami")); !os.IsNotExist(err) {
		t.Fatalf("canceled in-process wait touched the lock directory: %v", err)
	}
}
