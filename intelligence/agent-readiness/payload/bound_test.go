package payload_test

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/intelligence/agent-readiness/internal/gittest"
	"go.putnami.dev/intelligence/agent-readiness/payload"
)

const (
	syntheticCommits  = 2500
	syntheticFiles    = 5000
	syntheticPackages = 50
	// Collection stays within 30 seconds and 256 MiB for this synthetic history.
	wallBound = 30 * time.Second
	heapBound = 256 << 20
)

// syntheticStream writes a fast-import stream: one commit with 5,000 files in
// 50 pnpm packages, then 2,499 commits that each change two files, spread
// over the last 180 days.
func syntheticStream(now time.Time) []byte {
	var stream bytes.Buffer
	blob := func(content string) {
		fmt.Fprintf(&stream, "data %d\n%s\n", len(content), content)
	}
	start := now.Add(-180 * 24 * time.Hour)
	step := 180 * 24 * time.Hour / syntheticCommits
	for i := 0; i < syntheticCommits; i++ {
		at := start.Add(time.Duration(i) * step).Unix()
		author := fmt.Sprintf("dev%02d@example.test", i%20)
		fmt.Fprintf(&stream, "commit refs/heads/main\nmark :%d\n", i+1)
		fmt.Fprintf(&stream, "author Dev <%s> %d +0000\ncommitter Dev <%s> %d +0000\n", author, at, author, at)
		blob(fmt.Sprintf("change %d (#%d)", i, i+1))
		if i > 0 {
			fmt.Fprintf(&stream, "from :%d\n", i)
		}
		if i == 0 {
			fmt.Fprint(&stream, "M 100644 inline pnpm-workspace.yaml\n")
			blob("packages:\n  - 'packages/*'\n")
			for p := 0; p < syntheticPackages; p++ {
				fmt.Fprintf(&stream, "M 100644 inline packages/p%02d/package.json\n", p)
				blob(fmt.Sprintf(`{"name":"p%02d"}`, p))
			}
			for f := 0; f < syntheticFiles-syntheticPackages-1; f++ {
				fmt.Fprintf(&stream, "M 100644 inline packages/p%02d/src/f%04d.ts\n", f%syntheticPackages, f)
				blob(fmt.Sprintf("export const f%d = %d;\n// line two\n// line three\n", f, f))
			}
			continue
		}
		for k := 0; k < 2; k++ {
			f := (i*7 + k*13) % (syntheticFiles - syntheticPackages - 1)
			fmt.Fprintf(&stream, "M 100644 inline packages/p%02d/src/f%04d.ts\n", f%syntheticPackages, f)
			blob(fmt.Sprintf("export const f%d = %d;\n", f, i))
		}
	}
	return stream.Bytes()
}

func TestCollectStaysWithinItsTimeAndMemoryBounds(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := gittest.New(t)
	repo.Import(syntheticStream(now))

	var peak uint64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		var stats runtime.MemStats
		for {
			runtime.ReadMemStats(&stats)
			peak = max(peak, stats.HeapAlloc)
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	result, err := payload.Collect(context.Background(), payload.Options{Dir: repo.Dir, CollectorVersion: "0.1.0", Now: now})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("collected %d commits and %d files in %s, peak heap %d MiB",
		result.Payload.Inventory.CommitsTotal, result.Payload.Inventory.Files, result.Elapsed, peak>>20)
	if result.Payload.Inventory.CommitsTotal != syntheticCommits || result.Payload.Inventory.Files != syntheticFiles {
		t.Fatalf("read %d commits and %d files, want %d and %d",
			result.Payload.Inventory.CommitsTotal, result.Payload.Inventory.Files, syntheticCommits, syntheticFiles)
	}
	if len(result.Payload.Areas) != syntheticPackages+1 {
		t.Fatalf("found %d areas, want %d packages and the root", len(result.Payload.Areas), syntheticPackages)
	}
	if result.Elapsed > wallBound {
		t.Fatalf("collection took %s, bound %s", result.Elapsed, wallBound)
	}
	if peak > heapBound {
		t.Fatalf("peak heap %d MiB, bound %d MiB", peak>>20, heapBound>>20)
	}
}
