package workspace

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestACallerAnswersTheConfigurationBytes(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-caller-may-answer-the-configuration-bytes", "a-caller-answers-the-configuration-bytes")
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	write := func(rel, content string) string {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	workspaceFile := write(WorkspaceConfigFilename, `{"options":{"tool":{"level":"strict"}}}`)
	scopeFile := write("libs/"+ConfigFilename, `{"tags":["shared"]}`)
	projectFile := write("libs/one/"+ConfigFilename, `{"name":"one","tags":["leaf"]}`)
	project := filepath.Join(root, "libs", "one")

	type loaded struct {
		Workspace *Config
		Scope     *ScopeConfig
		Project   *ProjectConfig
	}
	load := func() loaded {
		return loaded{Load(root), LoadScopeChain(root, project), LoadProjectConfig(project)}
	}
	read := load()

	var mu sync.Mutex
	asked := map[string]int{}
	restore := UseConfigReader(func(path string) ([]byte, error) {
		mu.Lock()
		asked[path]++
		mu.Unlock()
		return os.ReadFile(path)
	})
	answered := load()
	restore()
	if !reflect.DeepEqual(read, answered) {
		t.Fatalf("a configuration loaded through a reader differs:\nread:     %+v\nanswered: %+v", read, answered)
	}
	for _, path := range []string{workspaceFile, scopeFile, projectFile} {
		if asked[path] == 0 {
			t.Errorf("the reader was not asked for %s; asked %v", path, asked)
		}
	}

	// A reader's answer is what the loaders decode, and once restored every
	// file is read again.
	restore = UseConfigReader(func(path string) ([]byte, error) {
		if path == projectFile {
			return []byte(`{"name":"answered"}`), nil
		}
		return os.ReadFile(path)
	})
	if got := LoadProjectConfig(project); got == nil || got.Name != "answered" {
		t.Fatalf("the loader did not decode the reader's answer: %+v", got)
	}
	restore()
	if got := LoadProjectConfig(project); got == nil || got.Name != "one" {
		t.Fatalf("a restored reader still answered: %+v", got)
	}
}
