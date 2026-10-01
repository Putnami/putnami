package workspace

import (
	"path/filepath"
	"testing"
)

// Workspace-input patterns match slash paths with path.Match on every host, so
// `*` never crosses a `/`, as it would under filepath.Match on Windows.
func TestFirstMatchingFilePattern_StarStopsAtASlash(t *testing.T) {
	if file, _, ok := firstMatchingFilePattern([]string{"config/tools/biome.json"}, []string{"config/*.json"}); ok {
		t.Errorf("config/*.json matched %s: `*` crossed a /", file)
	}
	file, pattern, ok := firstMatchingFilePattern([]string{"config/biome.json"}, []string{"config/*.json"})
	if !ok || file != "config/biome.json" || pattern != "config/*.json" {
		t.Errorf("config/*.json on config/biome.json = (%q, %q, %v), want a match", file, pattern, ok)
	}
}

// Watch reports host-native relative paths. Each probe reads them in slash
// form, and the seed keeps the caller's spelling.
func TestFirstMatchingFilePattern_ReadsNativePathsInSlashForm(t *testing.T) {
	native := filepath.Join("config", "tools", "biome.json")
	for _, want := range []string{"config/tools/*.json", "**/biome.json", "**.json"} {
		file, pattern, ok := firstMatchingFilePattern([]string{native}, []string{want})
		if !ok || file != native || pattern != want {
			t.Errorf("%s on %s = (%q, %q, %v), want a match that returns the native path", want, native, file, pattern, ok)
		}
	}
}
