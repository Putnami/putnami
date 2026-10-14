package toolchain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

func TestReporterRuntimeToolchainProbeHonorsCallerCancellation(t *testing.T) {
	root := t.TempDir()
	bin := t.TempDir()
	ready := filepath.Join(root, "probe-ready")
	fixtureproc.Write(t, filepath.Join(bin, "compiler"), fixtureproc.Program{Record: ready, Sleep: 30 * time.Second})
	writeRuntimeToolchainLock(t, root, "compiler", "1.2.3", "test-integrity")
	ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- resolveRuntimeToolchainsContext(ctx, root, ext, []string{"compiler"}, []string{"PATH=" + bin})
	}()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
readyLoop:
	for {
		select {
		case err := <-result:
			t.Fatalf("probe never reached readiness: %v", err)
		case <-deadline.C:
			t.Fatal("probe did not start")
		case <-ticker.C:
			if _, err := os.Stat(ready); err == nil {
				break readyLoop
			}
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("probe cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("probe kept its five-second background timeout")
	}
}

func TestReporterRuntimeToolchainMutexHonorsCallerDeadline(t *testing.T) {
	runtimeToolchainMu.Lock()
	defer runtimeToolchainMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
	if err := resolveRuntimeToolchainsContext(ctx, t.TempDir(), ext, []string{"compiler"}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock ignored deadline: %v", err)
	}
	if _, err := ProviderRuntimeEnvironment(ctx, nil, ext, []string{"compiler"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("environment ignored deadline: %v", err)
	}
}
