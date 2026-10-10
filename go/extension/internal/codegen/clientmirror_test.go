package codegen

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// stageClient writes a config.json and staged client files under genDir.
func stageClient(t *testing.T, genDir, config string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(genDir, "clientgen", "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "clientgen", "config.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(genDir, "clientgen", "go", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const goTarget = `{"targets":["go"],"ts":{"output":"clients/ts","packageName":""},"go":{"output":"clients/go","modulePath":"example.com/c","packageName":"client","clientName":"Client"}}`

func TestMirrorGeneratedClients_WritesIntoProjectTree(t *testing.T) {
	proj := t.TempDir()
	genDir := filepath.Join(proj, ".gen")
	stageClient(t, genDir, goTarget, map[string]string{
		"client.gen.go": "package client\n",
		"go.mod":        "module example.com/c\n",
	})

	artifacts, outputs, err := mirrorGeneratedClients(proj, genDir)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}

	for _, rel := range []string{"clients/go/client.gen.go", "clients/go/go.mod"} {
		if _, err := os.Stat(filepath.Join(proj, filepath.FromSlash(rel))); err != nil {
			t.Errorf("expected %s in project tree: %v", rel, err)
		}
	}
	if len(artifacts) != 2 {
		t.Errorf("artifacts = %v, want 2 entries", artifacts)
	}
	if len(outputs) != 1 || outputs[0] != "clients/go" {
		t.Errorf("outputs = %v, want [clients/go]", outputs)
	}
}

func TestMirrorGeneratedClients_PreservesExistingGoMod(t *testing.T) {
	proj := t.TempDir()
	genDir := filepath.Join(proj, ".gen")
	stageClient(t, genDir, goTarget, map[string]string{
		"client.gen.go": "package client\n// regenerated\n",
		"go.mod":        "module example.com/scaffold\n",
	})

	// A committed go.mod already exists in the target.
	target := filepath.Join(proj, "clients", "go")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	committed := "module example.com/committed\n\ngo 1.25\n"
	if err := os.WriteFile(filepath.Join(target, "go.mod"), []byte(committed), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := mirrorGeneratedClients(proj, genDir); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	// go.mod is preserved (scaffold-once) — no churn to the committed module file.
	gomod, _ := os.ReadFile(filepath.Join(target, "go.mod"))
	if string(gomod) != committed {
		t.Errorf("go.mod was overwritten; got %q want %q", gomod, committed)
	}
	// client.gen.go is always refreshed.
	client, _ := os.ReadFile(filepath.Join(target, "client.gen.go"))
	if string(client) != "package client\n// regenerated\n" {
		t.Errorf("client.gen.go not refreshed: %q", client)
	}
}

func TestMirrorGeneratedClients_UsesConfiguredOutput(t *testing.T) {
	proj := t.TempDir()
	genDir := filepath.Join(proj, ".gen")
	const config = `{"targets":["go"],"go":{"output":"internal/api/client"}}`
	stageClient(t, genDir, config, map[string]string{
		"client.gen.go": "package client\n",
	})

	artifacts, outputs, err := mirrorGeneratedClients(proj, genDir)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}

	if _, err := os.Stat(filepath.Join(proj, "internal", "api", "client", "client.gen.go")); err != nil {
		t.Fatalf("configured output was not written: %v", err)
	}
	if len(outputs) != 1 || outputs[0] != "internal/api/client" {
		t.Errorf("outputs = %v, want [internal/api/client]", outputs)
	}
	if len(artifacts) != 1 || artifacts[0] != "internal/api/client/client.gen.go" {
		t.Errorf("artifacts = %v", artifacts)
	}
}

func TestMirrorGeneratedClients_NoConfig(t *testing.T) {
	proj := t.TempDir()
	artifacts, outputs, err := mirrorGeneratedClients(proj, filepath.Join(proj, ".gen"))
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if artifacts != nil {
		t.Errorf("expected no artifacts without a config, got %v", artifacts)
	}
	if outputs != nil {
		t.Errorf("expected no outputs without a config, got %v", outputs)
	}
}

func TestMirrorGeneratedClients_ConfigButNoStagedClient(t *testing.T) {
	proj := t.TempDir()
	genDir := filepath.Join(proj, ".gen")
	// Contract present (e.g. no routes), but no staged go client dir.
	if err := os.MkdirAll(filepath.Join(genDir, "clientgen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "clientgen", "config.json"), []byte(goTarget), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(proj, "clients", "go")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "client.gen.go"), []byte("package stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	artifacts, outputs, err := mirrorGeneratedClients(proj, genDir)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if artifacts != nil {
		t.Errorf("expected no artifacts when nothing was staged, got %v", artifacts)
	}
	if len(outputs) != 1 || outputs[0] != "clients/go" {
		t.Errorf("outputs = %v, want [clients/go]", outputs)
	}
	if _, err := os.Stat(filepath.Join(proj, "clients", "go")); !os.IsNotExist(err) {
		t.Errorf("no clients/ dir should be created, got err=%v", err)
	}
}

func TestMirrorGeneratedClients_RejectsEscapingOutput(t *testing.T) {
	proj := t.TempDir()
	genDir := filepath.Join(proj, ".gen")
	const config = `{"targets":["go"],"go":{"output":"../outside"}}`
	stageClient(t, genDir, config, map[string]string{
		"client.gen.go": "package client\n",
	})

	if _, _, err := mirrorGeneratedClients(proj, genDir); err == nil {
		t.Fatal("expected escaping output to fail")
	}
}

func writeTargetFile(t *testing.T, target, name, body string) {
	t.Helper()
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A committed putnami.json (the client module is a workspace project of its
// own) and a go.sum (a `go mod tidy` product the stage never carries) survive
// a mirror byte-for-byte, while a stale generated file is dropped.
func TestMirrorGeneratedClients_KeepsOwnedFilesWithStagedClient(t *testing.T) {
	proj := t.TempDir()
	genDir := filepath.Join(proj, ".gen")
	stageClient(t, genDir, goTarget, map[string]string{
		"client.gen.go": "package client\n// regenerated\n",
		"go.mod":        "module example.com/scaffold\n",
	})
	target := filepath.Join(proj, "clients", "go")
	const project = "{\n  \"name\": \"example.com/committed\",\n  \"extensions\": [\"@putnami/go\"]\n}\n"
	const gosum = "go.putnami.dev/client v0.0.1 h1:abc=\n"
	writeTargetFile(t, target, "putnami.json", project)
	writeTargetFile(t, target, "go.sum", gosum)
	writeTargetFile(t, target, "old.gen.go", "package stale\n")

	artifacts, _, err := mirrorGeneratedClients(proj, genDir)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}

	if got, _ := os.ReadFile(filepath.Join(target, "putnami.json")); string(got) != project {
		t.Errorf("putnami.json was not kept byte-for-byte; got %q want %q", got, project)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "go.sum")); string(got) != gosum {
		t.Errorf("go.sum was not kept byte-for-byte; got %q want %q", got, gosum)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "go.mod")); string(got) != "module example.com/scaffold\n" {
		t.Errorf("absent go.mod was not scaffolded from the stage; got %q", got)
	}
	if _, err := os.Stat(filepath.Join(target, "old.gen.go")); !os.IsNotExist(err) {
		t.Errorf("stale generated file survived the mirror, err=%v", err)
	}
	want := []string{"clients/go/client.gen.go", "clients/go/go.mod", "clients/go/go.sum", "clients/go/putnami.json"}
	if !slices.Equal(artifacts, want) {
		t.Errorf("artifacts = %v, want %v", artifacts, want)
	}
}

// A client directory that is a workspace project has its own .gen, which the
// engine and that project's tasks write. The mirror keeps it, and it is not an
// artifact of the provider's build.
func TestMirrorGeneratedClients_KeepsClientProjectGenDir(t *testing.T) {
	proj := t.TempDir()
	genDir := filepath.Join(proj, ".gen")
	stageClient(t, genDir, goTarget, map[string]string{"client.gen.go": "package client\n"})
	target := filepath.Join(proj, "clients", "go")
	const stamp = "{\"version\":\"0.0.0\"}\n"
	writeTargetFile(t, target, "putnami.json", "{}\n")
	writeTargetFile(t, filepath.Join(target, ".gen"), "version.json", stamp)

	artifacts, _, err := mirrorGeneratedClients(proj, genDir)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(target, ".gen", "version.json")); string(got) != stamp {
		t.Errorf("client project .gen/version.json was not kept; got %q want %q", got, stamp)
	}
	want := []string{"clients/go/client.gen.go", "clients/go/putnami.json"}
	if !slices.Equal(artifacts, want) {
		t.Errorf("artifacts = %v, want %v", artifacts, want)
	}
}

// A staged putnami.json is scaffolded when the target has none and never
// overwrites the copy a later run finds, exactly like go.mod.
func TestMirrorGeneratedClients_ScaffoldsPutnamiJSONOnce(t *testing.T) {
	proj := t.TempDir()
	genDir := filepath.Join(proj, ".gen")
	const scaffold = "{\n  \"name\": \"example.com/c\"\n}\n"
	stageClient(t, genDir, goTarget, map[string]string{
		"client.gen.go": "package client\n",
		"go.mod":        "module example.com/c\n",
		"putnami.json":  scaffold,
	})
	target := filepath.Join(proj, "clients", "go")

	artifacts, _, err := mirrorGeneratedClients(proj, genDir)
	if err != nil {
		t.Fatalf("first mirror: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "putnami.json")); string(got) != scaffold {
		t.Fatalf("putnami.json was not scaffolded; got %q want %q", got, scaffold)
	}
	want := []string{"clients/go/client.gen.go", "clients/go/go.mod", "clients/go/putnami.json"}
	if !slices.Equal(artifacts, want) {
		t.Errorf("artifacts = %v, want %v", artifacts, want)
	}

	// The project edits its metadata, and the next describe stages a different
	// scaffold: the edited copy wins.
	const edited = "{\n  \"name\": \"example.com/c\",\n  \"tags\": [\"go\", \"edited\"]\n}\n"
	writeTargetFile(t, target, "putnami.json", edited)
	stageClient(t, genDir, goTarget, map[string]string{
		"client.gen.go": "package client\n// regenerated\n",
		"putnami.json":  "{\n  \"name\": \"example.com/rescaffold\"\n}\n",
	})
	if _, _, err := mirrorGeneratedClients(proj, genDir); err != nil {
		t.Fatalf("second mirror: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "putnami.json")); string(got) != edited {
		t.Errorf("putnami.json was overwritten; got %q want %q", got, edited)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "client.gen.go")); string(got) != "package client\n// regenerated\n" {
		t.Errorf("client.gen.go not refreshed: %q", got)
	}
}

// Contract present but no client staged: the generated entries go, the owned
// files stay, and the directory is kept because it is not empty.
func TestMirrorGeneratedClients_NoStagedClientKeepsOwnedFiles(t *testing.T) {
	proj := t.TempDir()
	genDir := filepath.Join(proj, ".gen")
	if err := os.MkdirAll(filepath.Join(genDir, "clientgen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "clientgen", "config.json"), []byte(goTarget), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(proj, "clients", "go")
	const gomod = "module example.com/committed\n\ngo 1.25\n"
	const project = "{\n  \"name\": \"example.com/committed\"\n}\n"
	writeTargetFile(t, target, "go.mod", gomod)
	writeTargetFile(t, target, "putnami.json", project)
	writeTargetFile(t, target, "client.gen.go", "package stale\n")
	if err := os.MkdirAll(filepath.Join(target, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTargetFile(t, filepath.Join(target, "nested"), "extra.gen.go", "package stale\n")

	artifacts, outputs, err := mirrorGeneratedClients(proj, genDir)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if len(outputs) != 1 || outputs[0] != "clients/go" {
		t.Errorf("outputs = %v, want [clients/go]", outputs)
	}
	want := []string{"clients/go/go.mod", "clients/go/putnami.json"}
	if !slices.Equal(artifacts, want) {
		t.Errorf("artifacts = %v, want %v", artifacts, want)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "go.mod")); string(got) != gomod {
		t.Errorf("go.mod was not kept; got %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "putnami.json")); string(got) != project {
		t.Errorf("putnami.json was not kept; got %q", got)
	}
	for _, stale := range []string{"client.gen.go", "nested"} {
		if _, err := os.Stat(filepath.Join(target, stale)); !os.IsNotExist(err) {
			t.Errorf("generated entry %s survived, err=%v", stale, err)
		}
	}
}

// A rerun with the same stage leaves an unchanged generated file untouched,
// replaces a changed one, drops what the stage no longer carries, and leaves no
// temporary file behind.
func TestMirrorGeneratedClients_ReplacesOnlyChangedEntries(t *testing.T) {
	proj := t.TempDir()
	genDir := filepath.Join(proj, ".gen")
	stageClient(t, genDir, goTarget, map[string]string{
		"client.gen.go": "package client\n",
		"types.gen.go":  "package client\n// v1\n",
	})
	target := filepath.Join(proj, "clients", "go")
	writeTargetFile(t, filepath.Join(target, "old"), "stale.gen.go", "package stale\n")
	writeTargetFile(t, target, "gone.gen.go", "package stale\n")
	if _, _, err := mirrorGeneratedClients(proj, genDir); err != nil {
		t.Fatalf("first mirror: %v", err)
	}
	before, err := os.Stat(filepath.Join(target, "client.gen.go"))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(genDir, "clientgen", "go", "types.gen.go")); err != nil {
		t.Fatal(err)
	}
	stageClient(t, genDir, goTarget, map[string]string{
		"client.gen.go": "package client\n",
		"models.gen.go": "package client\n// v2\n",
	})
	artifacts, _, err := mirrorGeneratedClients(proj, genDir)
	if err != nil {
		t.Fatalf("second mirror: %v", err)
	}
	after, err := os.Stat(filepath.Join(target, "client.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("unchanged client.gen.go was rewritten")
	}
	want := []string{"clients/go/client.gen.go", "clients/go/models.gen.go"}
	if !slices.Equal(artifacts, want) {
		t.Errorf("artifacts = %v, want %v", artifacts, want)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"client.gen.go", "models.gen.go"}) {
		t.Errorf("target entries = %v, want only the staged files", names)
	}
}

// A stale directory where a staged file goes, and a stale file where a staged
// file's parent directory goes, give way to the staged layout.
func TestMirrorGeneratedClients_ReplacesStaleEntryKinds(t *testing.T) {
	proj := t.TempDir()
	genDir := filepath.Join(proj, ".gen")
	stageClient(t, genDir, goTarget, map[string]string{"client.gen.go": "package client\n"})
	writeTargetFile(t, filepath.Join(genDir, "clientgen", "go", "sub"), "sub.gen.go", "package sub\n")
	target := filepath.Join(proj, "clients", "go")
	writeTargetFile(t, filepath.Join(target, "client.gen.go"), "inner.go", "package stale\n")
	writeTargetFile(t, target, "sub", "stale\n")

	artifacts, _, err := mirrorGeneratedClients(proj, genDir)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "client.gen.go")); string(got) != "package client\n" {
		t.Errorf("client.gen.go = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "sub", "sub.gen.go")); string(got) != "package sub\n" {
		t.Errorf("sub/sub.gen.go = %q", got)
	}
	want := []string{"clients/go/client.gen.go", "clients/go/sub/sub.gen.go"}
	if !slices.Equal(artifacts, want) {
		t.Errorf("artifacts = %v, want %v", artifacts, want)
	}
}

// An importer that compiles while describe runs must always find the client
// file: the mirror replaces it, it never deletes it first.
func TestMirrorGeneratedClients_ClientFileNeverAbsent(t *testing.T) {
	proj := t.TempDir()
	genDir := filepath.Join(proj, ".gen")
	stageClient(t, genDir, goTarget, map[string]string{"client.gen.go": "package client\n"})
	if _, _, err := mirrorGeneratedClients(proj, genDir); err != nil {
		t.Fatalf("first mirror: %v", err)
	}
	clientFile := filepath.Join(proj, "clients", "go", "client.gen.go")

	stop := make(chan struct{})
	missing := make(chan error, 1)
	go func() {
		defer close(missing)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := os.Lstat(clientFile); err != nil {
				missing <- err
				return
			}
		}
	}()
	for i := range 200 {
		stageClient(t, genDir, goTarget, map[string]string{
			"client.gen.go": "package client\n// run " + strconv.Itoa(i) + "\n",
		})
		if _, _, err := mirrorGeneratedClients(proj, genDir); err != nil {
			close(stop)
			t.Fatalf("mirror %d: %v", i, err)
		}
	}
	close(stop)
	if err := <-missing; err != nil {
		t.Fatalf("client.gen.go was absent during a mirror: %v", err)
	}
}
