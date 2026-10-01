package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// strictGoTargetConfig is goTargetConfig for a first-party provider, so every
// generation writes an ownership manifest the obsolete-target walk can match.
var strictGoTargetConfig = strings.Replace(goTargetConfig, "\n  \"thirdParty\": true,", "", 1)

// restoreBeside replaces parent/name the way a cache restore that stages beside
// its destination does: it populates a hidden staging tree under a unique
// name, renames the old tree aside, renames the staged tree in, and reclaims
// what it replaced. parent/name is absent between the two renames, and the
// staging and replaced trees appear and vanish beside it.
func restoreBeside(parent, name string, files map[string]string) error {
	staging, err := os.MkdirTemp(parent, "."+name+".tmp-materialize-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	for rel, content := range files {
		path := filepath.Join(staging, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	dest := filepath.Join(parent, name)
	aside := staging + ".tmp-replaced"
	if err := os.Rename(dest, aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(staging, dest); err != nil {
		return err
	}
	return os.RemoveAll(aside)
}

// TestGenerateProjectClients_WalkToleratesConcurrentRestores runs the Go
// generator of a provider while another task of the same provider restores its
// own client under clients/ in a loop. The walk that looks for obsolete Go
// targets meets staging trees that vanish, a destination that is briefly
// absent, and a replaced tree being reclaimed under it. None of that is a
// target of this generator, so no generation may fail on it. The test is
// bounded by the number of generations, never by wall-clock time.
func TestGenerateProjectClients_WalkToleratesConcurrentRestores(t *testing.T) {
	const generations = 200

	root := t.TempDir()
	writeProjectFile(t, root, ".gen/clientgen/config.json", strictGoTargetConfig)
	writeProjectFile(t, root, ".gen/schema/openapi.json", strictMinimalSpec)
	clients := filepath.Join(root, "clients")
	if err := os.MkdirAll(clients, 0o755); err != nil {
		t.Fatal(err)
	}
	restored := map[string]string{
		clientcontract.GeneratedManifestFile: `{"language":"typescript"}`,
		"src/index.ts":                       "export * from './client';\n",
		"src/client/items.ts":                "export class ItemsClient {}\n",
		"src/client/deep/nested/model.ts":    "export type Item = { id: string };\n",
	}

	var stop atomic.Bool
	var restores atomic.Int64
	warm := make(chan struct{})
	restoreErr := make(chan error, 1)
	go func() {
		defer close(restoreErr)
		for !stop.Load() {
			if err := restoreBeside(clients, "ts", restored); err != nil {
				restoreErr <- err
				return
			}
			if restores.Add(1) == 1 {
				close(warm)
			}
		}
	}()
	select {
	case <-warm:
	case err := <-restoreErr:
		t.Fatalf("concurrent restore before the first generation: %v", err)
	}

	completed := 0
	for ; completed < generations; completed++ {
		if _, err := GenerateProjectClients(root); err != nil {
			t.Errorf("generation %d with a concurrent restore: %v", completed, err)
			break
		}
	}
	stop.Store(true)
	if err := <-restoreErr; err != nil {
		t.Fatalf("concurrent restore: %v", err)
	}
	if completed == generations {
		if _, err := os.Stat(filepath.Join(root, "clients/go", clientcontract.GeneratedManifestFile)); err != nil {
			t.Fatalf("the generated Go client is missing: %v", err)
		}
	}
	t.Logf("%d generations against %d concurrent restores", completed, restores.Load())
}

// TestGenerateProjectClients_NeverTouchesRestoreStaging pins that the walk for
// obsolete Go targets never enters a directory whose name carries the restore
// staging marker. Such a tree is another process's restore in flight, not a
// target of this project, even when it holds a valid Go ownership manifest
// whose files all match.
func TestGenerateProjectClients_NeverTouchesRestoreStaging(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, ".gen/clientgen/config.json", strictGoTargetConfig)
	writeProjectFile(t, root, ".gen/schema/openapi.json", strictMinimalSpec)
	if _, err := GenerateProjectClients(root); err != nil {
		t.Fatal(err)
	}

	raw := readProjectFile(t, root, "clients/go/"+clientcontract.GeneratedManifestFile)
	manifest, diagnostics := clientcontract.ParseAndValidateGeneratedManifest([]byte(raw))
	if len(diagnostics) != 0 || manifest.Language != clientcontract.GeneratedLanguageGo {
		t.Fatalf("generated manifest is not a valid Go manifest: %v", diagnostics)
	}
	staged := "clients/.go.tmp-materialize-1234"
	owned := make([]string, 0, len(manifest.Files)+1)
	owned = append(owned, clientcontract.GeneratedManifestFile)
	for _, file := range manifest.Files {
		owned = append(owned, file.Path)
	}
	for _, rel := range owned {
		writeProjectFile(t, root, staged+"/"+rel, readProjectFile(t, root, "clients/go/"+rel))
	}

	changed := strings.Replace(strictGoTargetConfig, `"output": "clients/go"`, `"output": "clients/new-go"`, 1)
	writeProjectFile(t, root, ".gen/clientgen/config.json", changed)
	if _, err := GenerateProjectClients(root); err != nil {
		t.Fatal(err)
	}
	for _, rel := range owned {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(staged+"/"+rel))); err != nil {
			t.Errorf("the walk removed %s from a restore staging tree: %v", rel, err)
		}
	}
	// Control: the same walk removed the prior target outside the staging tree,
	// so it did run over clients/.
	if _, err := os.Stat(filepath.Join(root, "clients/go/client.gen.go")); !os.IsNotExist(err) {
		t.Fatalf("prior owned output remains, so the walk did not run: %v", err)
	}
}
