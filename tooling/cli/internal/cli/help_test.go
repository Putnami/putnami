package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
)

type streamCaptureResult struct {
	data []byte
	err  error
}

func drainCapturedStream(r io.Reader) <-chan streamCaptureResult {
	done := make(chan streamCaptureResult, 1)
	go func() {
		data, err := io.ReadAll(r)
		done <- streamCaptureResult{data: data, err: err}
	}()
	return done
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = orig
		_ = w.Close()
		_ = r.Close()
	}()

	readDone := drainCapturedStream(r)

	fn()

	// Restore the process stream before closing the captured writer. The reader
	// must run concurrently with fn: waiting until after fn lets a sufficiently
	// large rendering fill the finite pipe buffer and block forever in Write.
	os.Stdout = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	result := <-readDone
	if result.err != nil {
		t.Fatalf("read stdout: %v", result.err)
	}
	return string(result.data)
}

func TestCaptureStdout_DrainsConcurrently(t *testing.T) {
	want := strings.Repeat("x", 256*1024)
	got := captureStdout(t, func() {
		if _, err := io.WriteString(os.Stdout, want); err != nil {
			t.Fatalf("write stdout capacity probe: %v", err)
		}
	})
	if got != want {
		t.Fatalf("captured %d bytes, want %d", len(got), len(want))
	}
}

func TestSortedExtensionFlags_Empty(t *testing.T) {
	t.Parallel()
	flags := map[string]extension.FlagDefinition{}
	names := SortedExtensionFlags(flags)
	if len(names) != 0 {
		t.Errorf("expected empty, got %v", names)
	}
}

func TestSortedExtensionFlags_Sorted(t *testing.T) {
	t.Parallel()
	flags := map[string]extension.FlagDefinition{
		"zoo":    {Type: "boolean"},
		"apple":  {Type: "string"},
		"banana": {Type: "number"},
	}
	names := SortedExtensionFlags(flags)
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %d", len(names))
	}
	if names[0] != "apple" || names[1] != "banana" || names[2] != "zoo" {
		t.Errorf("names = %v, want [apple banana zoo]", names)
	}
}

func TestAllFlagCategories_NotEmpty(t *testing.T) {
	t.Parallel()
	if len(allFlagCategories) == 0 {
		t.Error("allFlagCategories should not be empty")
	}

	// Verify expected categories exist
	found := make(map[string]bool)
	for _, cat := range allFlagCategories {
		found[cat.Name] = true
		if len(cat.Flags) == 0 {
			t.Errorf("category %q has no flags", cat.Name)
		}
	}

	for _, name := range []string{"Common", "Project Selection", "Execution", "Output", "Advanced"} {
		if !found[name] {
			t.Errorf("missing category %q", name)
		}
	}
}

func TestRelatedCommands_BuildHasRelated(t *testing.T) {
	t.Parallel()
	related, ok := relatedCommands["build"]
	if !ok {
		t.Fatal("build should have related commands")
	}
	if len(related) == 0 {
		t.Error("build should have at least one related command")
	}
}

func TestRelatedCommands_NonexistentCommand(t *testing.T) {
	t.Parallel()
	_, ok := relatedCommands["nonexistent"]
	if ok {
		t.Error("nonexistent command should not have related commands")
	}
}

func TestPrintSubcommandHelp_IncludesStructuredFlags(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-help", "command-help-includes-structured-and-extension-flags")
	output := captureStructuredHelp(t, func() {
		PrintSubcommandHelp("workspace", "init")
	})

	if !strings.Contains(output, "putnami workspace init") {
		t.Fatal("expected workspace init header")
	}
	if !strings.Contains(output, "--project-path <path>") {
		t.Error("expected --project-path in structured help")
	}
	if !strings.Contains(output, "putnami init --project my-app --project-path apps/my-app") {
		t.Error("expected workspace init example")
	}
	if !strings.Contains(output, "Global Options:") {
		t.Error("expected global options in structured help")
	}
}

func TestPrintJobCommandHelpForWorkspace_IncludesMergedExtensionFlags(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-help", "command-help-includes-structured-and-extension-flags")
	exts := []*extension.ExtensionDescription{
		{
			Name: "@putnami/cloud",
			Jobs: map[string]*extension.JobDefinition{
				"deploy": {
					Name:          "deploy",
					ExtensionName: "@putnami/cloud",
					Flags: map[string]extension.FlagDefinition{
						"environment": {
							Type:        "string",
							Description: "Deployment environment",
						},
						"confirm": {
							Type:        "boolean",
							Description: "Confirm deployment",
						},
					},
				},
			},
		},
	}

	output := captureStructuredHelp(t, func() {
		printJobCommandHelp("deploy", "/workspace", &wsproto.Config{}, exts)
	})

	if !strings.Contains(output, "putnami deploy") {
		t.Fatal("expected deploy header")
	}
	if !strings.Contains(output, "--environment") || !strings.Contains(output, "Deployment environment") {
		t.Error("expected merged string flag in job help")
	}
	if !strings.Contains(output, "--confirm") || !strings.Contains(output, "Confirm deployment") {
		t.Error("expected merged boolean flag in job help")
	}
}

func TestPrintSubcommandHelp_IncludesRootStructuredCommandHelp(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{"infra", "putnami infra plan [--output <format>]"},
		{"scopes", "putnami scopes list [--output <format>]"},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			output := captureStructuredHelp(t, func() {
				PrintSubcommandHelp(tt.command, "")
			})
			if !strings.Contains(output, tt.want) {
				t.Fatalf("expected root help for %q to contain %q, got:\n%s", tt.command, tt.want, output)
			}
			if !strings.Contains(output, "Related Commands:") {
				t.Errorf("expected related commands for %q", tt.command)
			}
		})
	}
}

func TestPrintHelpMarkdown_IncludesStructuredCommandDetails(t *testing.T) {
	output := captureStructuredHelp(t, func() {
		PrintHelpMarkdown()
	})

	if !strings.Contains(output, "### `putnami workspace init`") {
		t.Fatal("expected workspace init markdown section")
	}
	if !strings.Contains(output, "`--project-path` <path>") {
		t.Error("expected workspace init flag in markdown help")
	}
	if !strings.Contains(output, "### `putnami projects create`") {
		t.Error("expected projects create markdown section")
	}
	if !strings.Contains(output, "`d`=deploy") || !strings.Contains(output, "`D`=deploy") {
		t.Error("expected deploy aliases in markdown help")
	}
}

func TestPrintHelpMan_IncludesStructuredCommandDetails(t *testing.T) {
	output := captureStructuredHelp(t, func() {
		PrintHelpMan()
	})

	if !strings.Contains(output, ".SS workspace init") {
		t.Fatal("expected workspace init man section")
	}
	if !strings.Contains(output, ".B --project-path") {
		t.Error("expected workspace init flag in man help")
	}
	if !strings.Contains(output, ".SS projects create") {
		t.Error("expected projects create man section")
	}
}
