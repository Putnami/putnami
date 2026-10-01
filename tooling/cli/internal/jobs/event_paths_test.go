package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestNormalizeWorkspaceDisplayPath(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	projectRel := filepath.Join("platform", "workloads", "migrate-runner")
	projectRoot := filepath.Join(wsRoot, projectRel)
	sourceRel := filepath.Join(projectRel, "cmd", "run", "pull.go")
	sourceAbs := filepath.Join(wsRoot, sourceRel)
	if err := os.MkdirAll(filepath.Dir(sourceAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceAbs, []byte("package run\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "cwd relative traversal",
			path: filepath.ToSlash(filepath.Join(".", "..", "..", "..", sourceRel)),
			want: filepath.ToSlash(sourceRel),
		},
		{
			name: "project relative",
			path: filepath.ToSlash(filepath.Join("cmd", "run", "pull.go")),
			want: filepath.ToSlash(sourceRel),
		},
		{
			name: "workspace relative",
			path: filepath.ToSlash(sourceRel),
			want: filepath.ToSlash(sourceRel),
		},
		{
			name: "workspace relative missing file",
			path: filepath.ToSlash(filepath.Join(projectRel, "cmd", "run", "missing.go")),
			want: filepath.ToSlash(filepath.Join(projectRel, "cmd", "run", "missing.go")),
		},
		{
			name: "module path containing project path",
			path: "go.putnami.dev/" + filepath.ToSlash(filepath.Join(projectRel, "cmd", "run", "pull.go")),
			want: filepath.ToSlash(sourceRel),
		},
		{
			name: "absolute",
			path: sourceAbs,
			want: filepath.ToSlash(sourceRel),
		},
		{
			name: "uri",
			path: "file:///" + filepath.ToSlash(sourceAbs),
			want: "file:///" + filepath.ToSlash(sourceAbs),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeWorkspaceDisplayPath(tt.path, wsRoot, projectRoot, projectRoot)
			if got != tt.want {
				t.Fatalf("normalizeWorkspaceDisplayPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestReadJSONLEvents_NormalizesDiagnosticPathsBeforeStoringAndForwarding(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/diagnostics-results", "located-diagnostics", "locations-normalize-and-survive-to-results")
	wsRoot := t.TempDir()
	projectRel := filepath.Join("packages", "app")
	projectRoot := filepath.Join(wsRoot, projectRel)
	sourceAbs := filepath.Join(projectRoot, "src", "main.ts")
	if err := os.MkdirAll(filepath.Dir(sourceAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceAbs, []byte("export {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	line := `{"v":2,"type":"diagnostic","severity":"error","message":"bad","location":{"file":"./src/main.ts","line":3}}`
	var forwarded []RawJobEvent
	events, _ := readJSONLEvents(strings.NewReader(line), func(event RawJobEvent) {
		forwarded = append(forwarded, event)
	}, workspacePathEventMapper(wsRoot, projectRoot, projectRoot))

	if len(events) != 1 || len(forwarded) != 1 {
		t.Fatalf("events=%d forwarded=%d, want 1 each", len(events), len(forwarded))
	}
	for _, event := range []RawJobEvent{events[0], forwarded[0]} {
		loc := event.Data["location"].(map[string]any)
		if got, want := loc["file"], filepath.ToSlash(filepath.Join(projectRel, "src", "main.ts")); got != want {
			t.Fatalf("location.file = %v, want %q", got, want)
		}
	}
}
