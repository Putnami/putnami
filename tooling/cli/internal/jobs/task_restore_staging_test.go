//go:build unix

package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// TestDeclaredRestoreStagesUnderTheScratchRoot pins where a declared restore
// stages: under the workspace scratch root, never beside its destination. A
// provider's clients/ directory holds one generated client per language, and
// the generator of one language walks clients/ while the other language's
// output is restored from cache. So while clients/ts is restored again and
// again, a watcher listing clients/ must never see anything but ts, and the
// scratch root keeps no staging leftover.
func TestDeclaredRestoreStagesUnderTheScratchRoot(t *testing.T) {
	const restores = 100

	job := declaredJob("clientgen~generate-ts", "generate-ts", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"client": dirDeclaration(extension.OutputRootProject, "clients/ts", false),
		},
	})
	f := newCaptureFixture(t, job)
	clients := filepath.Join(f.ws.Root, captureTestProject, "clients")
	writeFileAt(t, filepath.Join(clients, "ts", "client.putnami.json"), "{}\n")
	writeFileAt(t, filepath.Join(clients, "ts", "src", "index.ts"), "export {};\n")
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("store declared entry")
	}

	var stop atomic.Bool
	var listings atomic.Int64
	var watcher sync.WaitGroup
	warm := make(chan struct{})
	failures := make(chan error, 1)
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		for !stop.Load() {
			entries, err := os.ReadDir(clients)
			if listings.Add(1) == 1 {
				close(warm)
			}
			for _, e := range entries {
				if err == nil && e.Name() != "ts" {
					err = fmt.Errorf("clients/ held %s during a restore", e.Name())
				}
			}
			if err != nil {
				failures <- err
				return
			}
		}
	}()
	<-warm // the watcher is listing before the first restore starts

	for range restores {
		f.hitOrFail(t, job, captureHashA)
	}
	stop.Store(true)
	watcher.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}

	if got := readFileAt(t, filepath.Join(clients, "ts", "src", "index.ts")); got != "export {};\n" {
		t.Errorf("restored client = %q", got)
	}
	scratch, err := os.ReadDir(store.ResolveScratchRoot(f.ws.Root))
	if err != nil {
		t.Fatalf("read the scratch root: %v", err)
	}
	for _, e := range scratch {
		if strings.Contains(e.Name(), ".tmp-materialize-") {
			t.Errorf("staging leftover in the scratch root: %s", e.Name())
		}
	}
	t.Logf("%d restores; %d concurrent listings of clients/", restores, listings.Load())
}
