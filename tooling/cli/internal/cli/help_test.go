package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
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

func TestRelatedCommands_CacheVerifyHasRelated(t *testing.T) {
	t.Parallel()
	related, ok := relatedCommands["cache verify"]
	if !ok {
		t.Fatal("cache verify should have related commands")
	}
	if len(related) == 0 {
		t.Error("cache verify should have at least one related command")
	}
	if _, ok := relatedCommands["build"]; ok {
		t.Error("build has related commands in the catalog, want only a vocabulary to relate a job command")
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
	if !strings.Contains(output, "--channel <channel>") {
		t.Error("expected --channel in structured help")
	}
	if !strings.Contains(output, "putnami init --project my-app --channel canary") {
		t.Error("expected workspace init channel example")
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

	var err error
	output := captureStructuredHelp(t, func() {
		err = printDeclaredJobCommandHelp("deploy", exts, scopeOf("deploy"))
	})
	if err != nil {
		t.Fatal(err)
	}

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

// TestJobCommandHelpRefusesAnUndeclaredCommand pins that the help of a
// command no loaded extension declares is the usage error a run of it ends
// with, an alias expansion of several commands included.
func TestJobCommandHelpRefusesAnUndeclaredCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		command string
		scope   *helpScope
		want    string
	}{
		{"build", nil, "no extension of this workspace declares build;"},
		{"build", scopeOf("lint"), "no extension of this workspace declares build;"},
		{"lint,test,build", scopeOf("lint"), "no extension of this workspace declares test, build;"},
	} {
		err := printDeclaredJobCommandHelp(tc.command, nil, tc.scope)
		if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage || !strings.Contains(err.Error(), tc.want) ||
			!strings.Contains(err.Error(), "putnami extensions install") {
			t.Errorf("help of %s = %v, want a usage error containing %q", tc.command, err, tc.want)
		}
	}
}

func TestPrintSubcommandHelp_IncludesRootStructuredCommandHelp(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{"install", "putnami install [--latest]"},
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

func TestPrintSubcommandHelp_RootListsItsSubcommands(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "a-bare-cache-runs-nothing",
		"root-help-lists-its-subcommands")
	tests := []struct {
		command string
		want    []string
	}{
		{"cache", []string{
			"Subcommands:",
			"  clean   Clear this repo's shared build cache\n",
			"  gc      Garbage-collect global caches to their budgets\n",
			"  verify  Verify cache determinism and restore equivalence\n",
		}},
		{"config", []string{"Subcommands:", "  show  Show effective configuration (default)\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			output := captureStructuredHelp(t, func() {
				PrintSubcommandHelp(tt.command, "")
			})
			for _, want := range tt.want {
				if !strings.Contains(output, want) {
					t.Errorf("putnami %s --help lacks %q, got:\n%s", tt.command, want, output)
				}
			}
		})
	}
	leaf := captureStructuredHelp(t, func() {
		PrintSubcommandHelp("cache", "clean")
	})
	if strings.Contains(leaf, "Subcommands:") {
		t.Errorf("putnami cache clean --help lists subcommands, got:\n%s", leaf)
	}
}

// deployVocabularyScope is the help scope of a workspace whose vocabulary
// declares deploy with the aliases d and D, and whose language extension
// declares deploy.
func deployVocabularyScope() *helpScope {
	dev := &extension.ExtensionDescription{Name: "@acme/dev", Verbs: []extension.VerbDefinition{
		{Name: "deploy", Description: "Deploy the project.", Aliases: []string{"d", "D"}},
	}}
	lang := &extension.ExtensionDescription{Name: "@acme/lang", Commands: map[string]string{"deploy": ""}}
	return helpScopeOf(&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{lang, dev}}, nil)
}

func TestPrintHelpMarkdown_IncludesStructuredCommandDetails(t *testing.T) {
	output := captureStructuredHelp(t, func() {
		printHelpMarkdown(os.Stdout, deployVocabularyScope())
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
		t.Error("expected the vocabulary's deploy aliases in markdown help")
	}
	if !strings.Contains(output, "| `putnami deploy` | Deploy the project |") {
		t.Error("expected the vocabulary's deploy verb in markdown help")
	}
	bare := captureStructuredHelp(t, PrintHelpMarkdown)
	if strings.Contains(bare, "## Aliases") || strings.Contains(bare, "`putnami deploy`") || !strings.Contains(bare, "No extension is loaded.") {
		t.Errorf("markdown help without an extension lists a job command or an alias:\n%s", bare)
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

// Serve's help offered `-w, --watch (default: true)` and no
// spelling that turns the flag off.
func TestExtensionFlagHelpLine_ListsTheNegationOfAFlagOnByDefault(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		flag extension.FlagDefinition
		want string
	}{
		{"watch", extension.FlagDefinition{Type: "boolean", Short: "-w", Default: true, Description: "Restart on changes"},
			"  -w, --watch, --no-watch  Restart on changes (default: true)"},
		{"race", extension.FlagDefinition{Type: "boolean", Default: false, Description: "Race detector"},
			"  --race  Race detector (default: false)"},
		{"port", extension.FlagDefinition{Type: "number", Default: 3000}, "  --port  port (default: 3000)"},
		{"fix", extension.FlagDefinition{Type: "boolean", Default: true, CIDefault: false, Description: "Apply fixes"},
			"  --fix, --no-fix  Apply fixes (default: true; false when CI is set)"},
	} {
		if got := extensionFlagHelpLine(tc.name, tc.flag); got != tc.want {
			t.Errorf("extensionFlagHelpLine(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
