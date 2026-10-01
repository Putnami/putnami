package commands

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/completion"
)

// This pins the generated shell-completion scripts and the
// structured-help table so later work can prove the catalog regenerates the same
// bytes.
//
// Hard constraint (epic decision R8): IN-PROCESS ONLY. CompletionBash/Zsh/Fish
// all take an io.Writer, so no golden here spawns the built binary — the suite
// runs with race: true where one re-exec costs ≈1s.
//
// Every golden renders with wsRoot == "" so no workspace, extension, or
// template discovery leaks machine state into the pinned bytes. Workspace-
// derived completion values are covered by the existing behavioral tests in
// completion_test.go.

// The environment opt-in reaches this existing generator through ./putnamiw.
var updateSurfaceGoldens = flag.Bool("update-surface-goldens", os.Getenv("PUTNAMI_UPDATE_SURFACE_GOLDENS") == "1",
	"rewrite internal/commands/testdata/surface/*.golden from the current generators")

// assertSurfaceGolden compares got against testdata/surface/<name>.golden,
// rewriting the file instead when -update-surface-goldens is set.
func assertSurfaceGolden(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join("testdata", "surface", name+".golden")
	if *updateSurfaceGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create golden dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with: PUTNAMI_UPDATE_SURFACE_GOLDENS=1 ./putnamiw test --projects @putnami/cli --run Golden --no-cache --no-enforce-coverage)", path, err)
	}
	if string(want) == got {
		return
	}
	t.Errorf("%s drifted from its golden.\n%s\nRegenerate with:\n  PUTNAMI_UPDATE_SURFACE_GOLDENS=1 ./putnamiw test --projects @putnami/cli --run Golden --no-cache --no-enforce-coverage",
		name, firstSurfaceGoldenDiff(string(want), got))
}

func firstSurfaceGoldenDiff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		var wantLine, gotLine string
		if i < len(wantLines) {
			wantLine = wantLines[i]
		}
		if i < len(gotLines) {
			gotLine = gotLines[i]
		}
		if wantLine != gotLine {
			return "first difference at line " + strconv.Itoa(i+1) + ":\n  want: " + wantLine + "\n  got:  " + gotLine
		}
	}
	return "renderings differ only in length"
}

func TestGoldenCompletionScripts(t *testing.T) {
	generators := []struct {
		name     string
		generate func(io.Writer, string, *wsproto.Config)
	}{
		{"completion-bash", completion.CompletionBash},
		{"completion-zsh", completion.CompletionZsh},
		{"completion-fish", completion.CompletionFish},
	}
	for _, generator := range generators {
		t.Run(generator.name, func(t *testing.T) {
			var buf bytes.Buffer
			generator.generate(&buf, "", &wsproto.Config{})
			assertSurfaceGolden(t, generator.name, buf.String())
		})
	}
}

// TestGoldenStructuredHelpTable pins the CONTENT of structuredCommandHelp,
// independent of any renderer: A1a must reproduce every description, usage
// line, flag, and example from the catalog. The renderer layout is pinned
// separately by internal/cli/testdata/surface/help-structured-commands.golden.
func TestGoldenStructuredHelpTable(t *testing.T) {
	table := completion.StructuredCommandHelpTable()
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var out strings.Builder
	for _, key := range keys {
		info := table[key]
		out.WriteString("═══ " + key + " ═══\n")
		out.WriteString("description: " + info.Description + "\n")
		out.WriteString("usage: " + info.Usage + "\n")
		for _, flag := range info.Flags {
			out.WriteString("flag: " + flag.Long + " | " + flag.ValueName + " | " + flag.Description + "\n")
		}
		for _, example := range info.Examples {
			out.WriteString("example: " + example + "\n")
		}
	}

	assertSurfaceGolden(t, "structured-help-table", out.String())
}
