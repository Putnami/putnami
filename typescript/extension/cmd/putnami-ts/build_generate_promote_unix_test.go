//go:build unix

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Concurrent writers each stage a uniquely named file and rename it over the
// committed path: every writer succeeds, the committed file holds one writer's
// complete bytes, and no staged file is left behind.
func TestPromoteSchemaArtifactIsAtomicUnderConcurrentWriters(t *testing.T) {
	projectPath := t.TempDir()
	bodies := [][]byte{
		canonicalRouteInventory(t, webRoutesFragment),
		canonicalRouteInventory(t, webRoutesFragment, applicationRoutesFragment),
	}
	emitted := make([]string, len(bodies))
	for i, body := range bodies {
		path := filepath.Join(projectPath, ".gen", fmt.Sprintf("emitted-%d.json", i))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		emitted[i] = path
	}

	const writers = 32
	var wg sync.WaitGroup
	failures := make(chan error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(source string) {
			defer wg.Done()
			<-start
			if _, err := promoteSchemaArtifact(projectPath, source, HTTPRoutesRelPath); err != nil {
				failures <- err
			}
		}(emitted[i%len(emitted)])
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("concurrent promote: %v", err)
	}

	got := readCommittedRoutes(t, projectPath)
	if string(got) != string(bodies[0]) && string(got) != string(bodies[1]) {
		t.Fatalf("committed inventory is not one writer's complete bytes:\n%s", got)
	}
	entries, err := os.ReadDir(filepath.Dir(committedRoutesPath(projectPath)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "http-routes.json" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("schema/ holds %v, want only http-routes.json", names)
	}
}
