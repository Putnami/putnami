package cli

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// This pins the rendered command surface so every later
// change can prove it regenerated the same bytes from the new CommandCatalog.
//
// Hard constraint: these goldens are IN-PROCESS ONLY. The
// suite runs with race: true (tooling/cli/putnami.json) where one subprocess
// re-exec costs ≈1s, so every golden here calls the rendering
// function directly instead of shelling out to the built binary.
//
// Deliberately NOT pinned: invalid input. Bad flag values and unknown flags are
// usage errors (flags.go, parse.go), not rendered surfaces. See
// doc/adr/0001-cli-foundation-boundaries.md.

// The environment opt-in reaches this existing generator through ./putnamiw.
var updateSurfaceGoldens = flag.Bool("update-surface-goldens", os.Getenv("PUTNAMI_UPDATE_SURFACE_GOLDENS") == "1",
	"rewrite internal/cli/testdata/surface/*.golden from the current renderers")

// goldenVersion replaces the linker-stamped CLI version while a golden renders,
// so the pinned bytes do not depend on how the test binary was built. Package
// cli's tests never call t.Parallel, so mutating the package var is safe here
// (captureStdout already relies on the same property for os.Stdout).
const goldenVersion = "0.0.0-golden"

func withGoldenVersion(t *testing.T) {
	t.Helper()
	original := Version
	Version = goldenVersion
	t.Cleanup(func() { Version = original })
}

// assertGolden compares got against testdata/surface/<name>.golden, rewriting
// the file instead when -update-surface-goldens is set.
func assertGolden(t *testing.T, name, got string) {
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
		name, firstGoldenDiff(string(want), got))
}

// firstGoldenDiff reports the first differing line so a failure names the drift
// instead of dumping two full renderings into the test log.
func firstGoldenDiff(want, got string) string {
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

// captureStructuredHelp keeps direct help-renderer assertions and goldens in
// memory. The general captureStdout helper intentionally exercises the real
// process stream for tests which need it; these render checks only need the
// bytes emitted through the iox stdout seam.
func captureStructuredHelp(t *testing.T, fn func()) string {
	t.Helper()

	output, err := iox.CaptureStdout(func() error {
		fn()
		return nil
	})
	if err != nil {
		t.Fatalf("capture structured help: %v", err)
	}
	return output
}

func TestStructuredHelpCaptureMatchesProcessStdout(t *testing.T) {
	want := captureStdout(t, func() {
		PrintSubcommandHelp("workspace", "init")
	})
	got := captureStructuredHelp(t, func() {
		PrintSubcommandHelp("workspace", "init")
	})
	if got != want {
		t.Fatalf("in-memory structured-help capture changed rendered bytes:\n%s", firstGoldenDiff(want, got))
	}
}

func TestGoldenTopLevelHelp(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-help", "the-rendered-help-surface-is-pinned")
	withGoldenVersion(t)
	assertGolden(t, "help-top-level", captureStructuredHelp(t, PrintHelp))
}

func TestGoldenHelpMan(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-help", "the-rendered-help-surface-is-pinned")
	withGoldenVersion(t)
	assertGolden(t, "help-man", captureStructuredHelp(t, PrintHelpMan))
}

func TestGoldenHelpMarkdown(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-help", "the-rendered-help-surface-is-pinned")
	withGoldenVersion(t)
	assertGolden(t, "help-markdown", captureStructuredHelp(t, PrintHelpMarkdown))
}

// TestGoldenJobCommandHelp pins the per-command help layout for a job command,
// including how extension-declared flags (with and without a short form or a
// default) merge into the rendering.
func TestGoldenJobCommandHelp(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-help", "the-rendered-help-surface-is-pinned")
	withGoldenVersion(t)
	flags := map[string]extension.FlagDefinition{
		"environment":  {Type: "string", Description: "Deployment environment"},
		"confirm":      {Type: "boolean", Short: "-c", Description: "Confirm deployment"},
		"replicas":     {Type: "number", Description: "Replica count", Default: 3},
		"undocumented": {Type: "boolean"},
	}
	assertGolden(t, "help-command-deploy", captureStructuredHelp(t, func() {
		PrintCommandHelp("deploy", flags)
	}))
}

// structuredHelpSubcommands lists the (command, subcommand) pairs the
// structured-help renderer must keep covering. It mirrors the keys of
// commands.structuredCommandHelp; the commands-side data golden
// (internal/commands/testdata/surface/structured-help-table.golden) is what
// pins the table content itself.
var structuredHelpSubcommands = []string{
	"architecture inspect",
	"architecture snapshot",
	"architecture validate",
	"cache clean",
	"cache gc",
	"cache verify",
	"channel set",
	"channel status",
	"ci explain",
	"ci fmt",
	"ci init",
	"ci validate",
	"config set",
	"config show",
	"contracts check",
	"contracts generate",
	"context generate",
	"context map",
	"deps install",
	"dev extension",
	"dev template",
	"extensions install",
	"extensions list",
	"extensions remove",
	"extensions update",
	"features diff",
	"features inspect",
	"features snapshot",
	"features validate",
	"infra plan",
	"mcp install",
	"migrate vnext",
	"projects create",
	"projects describe",
	"projects list",
	"projects sync",
	"projects tag",
	"scopes list",
	"sessions inspect",
	"sessions list",
	"sessions replay",
	"specs init",
	"specs inspect",
	"specs list",
	"specs validate",
	"telemetry off",
	"telemetry on",
	"telemetry show",
	"telemetry status",
	"templates install",
	"templates list",
	"templates remove",
	"templates update",
	"version get",
	"version list",
	"version tag",
	"version use",
	"workspace describe",
	"workspace init",
}

// TestGoldenStructuredCommandHelp renders every registered command root plus
// every pinned subcommand through the production renderer into one golden.
// Command roots are read from commandRegistry, so registering a new structured
// command forces a golden update — that review is the point.
func TestGoldenStructuredCommandHelp(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-help", "the-rendered-help-surface-is-pinned")
	withGoldenVersion(t)

	roots := make([]string, 0, len(commandRegistry))
	for name := range commandRegistry {
		roots = append(roots, name)
	}
	sort.Strings(roots)

	output := captureStructuredHelp(t, func() {
		render := func(command, subcommand string) {
			label := command
			if subcommand != "" {
				label = command + " " + subcommand
			}
			iox.Fprintln(os.Stdout, "═══ "+label+" ═══")
			PrintSubcommandHelp(command, subcommand)
		}

		for _, root := range roots {
			render(root, "")
		}
		for _, pair := range structuredHelpSubcommands {
			command, subcommand, found := strings.Cut(pair, " ")
			if !found {
				t.Fatalf("structuredHelpSubcommands entry %q must be %q", pair, "<command> <subcommand>")
			}
			render(command, subcommand)
		}
	})

	assertGolden(t, "help-structured-commands", output)
}
