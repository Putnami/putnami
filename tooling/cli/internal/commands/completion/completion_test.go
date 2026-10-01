package completion

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/hometest"
)

// completionBinaryEnv makes this test binary stand in for the putnami binary
// RefreshShellCompletions runs, on every platform: `completion <shell>` prints
// "generated-<shell>", and anything else exits 2.
const completionBinaryEnv = "PUTNAMI_COMPLETION_TEST_BINARY"

func TestMain(m *testing.M) {
	if os.Getenv(completionBinaryEnv) != "" {
		if len(os.Args) > 2 && os.Args[1] == "completion" {
			fmt.Printf("generated-%s\n", os.Args[2])
			os.Exit(0)
		}
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func TestCompletionBash(t *testing.T) {
	var buf bytes.Buffer
	cfg := &wsproto.Config{}

	CompletionBash(&buf, "", cfg)
	output := buf.String()

	if !strings.Contains(output, "_putnami_completions") {
		t.Error("expected bash completion function name")
	}
	if !strings.Contains(output, "complete -F _putnami_completions putnami") {
		t.Error("expected complete registration")
	}
	if !strings.Contains(output, "build") {
		t.Error("expected 'build' command in completions")
	}
	if !strings.Contains(output, "deploy") {
		t.Error("expected protocol-level 'deploy' command in completions")
	}
	if !strings.Contains(output, " d ") {
		t.Error("expected deploy alias in completions")
	}
	if !strings.Contains(output, "extensions") {
		t.Error("expected 'extensions' structured command")
	}
	if !strings.Contains(output, "upgrade") {
		t.Error("expected 'upgrade' structured command")
	}
	if !strings.Contains(output, "'mcp') printf '%s\\n' 'install'") {
		t.Error("expected mcp subcommands in bash completion")
	}
	if !strings.Contains(output, "--verbose") {
		t.Error("expected --verbose flag")
	}
	if !strings.Contains(output, "--project-path") {
		t.Error("expected workspace init structured flag")
	}
	if !strings.Contains(output, "--path") {
		t.Error("expected projects create structured flag")
	}
	if !strings.Contains(output, "go py ts") {
		t.Error("expected workspace init extension values")
	}
}

func TestCompletionZsh(t *testing.T) {
	var buf bytes.Buffer
	cfg := &wsproto.Config{}

	CompletionZsh(&buf, "", cfg)
	output := buf.String()

	if !strings.Contains(output, "#compdef putnami") {
		t.Error("expected zsh compdef header")
	}
	if !strings.Contains(output, "_putnami") {
		t.Error("expected _putnami function")
	}
	if !strings.Contains(output, "'build:Compile or transpile project sources into runnable or distributable outputs.'") {
		t.Error("expected build command with description")
	}
	// The apostrophe in "workload's" must arrive POSIX-escaped: emitted raw it
	// closes the _describe entry early and corrupts the rest of the array. This
	// assertion previously pinned the raw form — it encoded the bug.
	if !strings.Contains(output, `'deploy:Converge a cloud environment on the workload'\''s aggregated infra manifest.'`) {
		t.Error("expected deploy command with escaped protocol description")
	}
	if !strings.Contains(output, "'--verbose[Show job results and diagnostics]'") {
		t.Error("expected verbose flag with description")
	}
	if !strings.Contains(output, "'--project-path[Workspace-relative path for the initial project]:path:'") {
		t.Error("expected workspace init flag in zsh completion")
	}
	if !strings.Contains(output, "'--path[Workspace-relative project path]:path:'") {
		t.Error("expected projects create flag in zsh completion")
	}
	if !strings.Contains(output, "'ts:TypeScript'") {
		t.Error("expected workspace init extension values in zsh completion")
	}
	if !strings.Contains(output, "'upgrade:Upgrade CLI, extensions, and dependencies'") {
		t.Error("expected upgrade command in zsh completion")
	}
	// The menu description is the catalog Summary since A1b, so it is the same
	// prose `putnami help` shows rather than a third wording.
	if !strings.Contains(output, "'mcp:Run the MCP server over stdio for AI agent harnesses'") || !strings.Contains(output, "'install'") {
		t.Error("expected mcp install in zsh completion")
	}
}

func TestCompletionFish(t *testing.T) {
	var buf bytes.Buffer
	cfg := &wsproto.Config{}

	CompletionFish(&buf, "", cfg)
	output := buf.String()

	if !strings.Contains(output, "complete -c putnami") {
		t.Error("expected fish complete command")
	}
	if !strings.Contains(output, "build") {
		t.Error("expected 'build' command")
	}
	if !strings.Contains(output, "deploy") {
		t.Error("expected 'deploy' command")
	}
	if !strings.Contains(output, "extensions") {
		t.Error("expected 'extensions' command")
	}
	if !strings.Contains(output, "upgrade") {
		t.Error("expected 'upgrade' command")
	}
	if !strings.Contains(output, "__putnami_complete_children mcp' -a 'install'") {
		t.Error("expected mcp install in fish completion")
	}
	if !strings.Contains(output, "verbose") {
		t.Error("expected 'verbose' flag")
	}
	if !strings.Contains(output, "-l 'project-path'") {
		t.Error("expected workspace init structured flag in fish completion")
	}
	if !strings.Contains(output, "-l 'path'") {
		t.Error("expected projects create structured flag in fish completion")
	}
	if !strings.Contains(output, "-l 'extension'") || !strings.Contains(output, "-a 'go py ts'") {
		t.Error("expected workspace init extension values in fish completion")
	}
}

func TestGatherCompletionContext(t *testing.T) {
	cfg := &wsproto.Config{}
	ctx := gatherCompletionContext("", cfg)

	// Should have default commands
	if !sharedtest.Contains(ctx.commands, "build") {
		t.Error("expected 'build' in default commands")
	}
	if !sharedtest.Contains(ctx.commands, "test") {
		t.Error("expected 'test' in default commands")
	}
	if !sharedtest.Contains(ctx.commands, "deploy") {
		t.Error("expected protocol-level 'deploy' in default commands")
	}
	if !sharedtest.Contains(ctx.commands, "package") {
		t.Error("expected protocol-level 'package' in default commands")
	}
	if !sharedtest.Contains(ctx.commands, "d") || !sharedtest.Contains(ctx.commands, "D") {
		t.Error("expected deploy aliases in default commands")
	}

	// Should have structured commands
	if !sharedtest.Contains(ctx.structured, "extensions") {
		t.Error("expected 'extensions' in structured commands")
	}
	if !sharedtest.Contains(ctx.structured, "migrate") {
		t.Error("expected 'migrate' in structured commands")
	}
	if !sharedtest.Contains(ctx.structured, "sessions") {
		t.Error("expected 'sessions' in structured commands")
	}
	if !sharedtest.Contains(ctx.structured, "completion") {
		t.Error("expected 'completion' in structured commands")
	}
	if !sharedtest.Contains(ctx.structured, "upgrade") {
		t.Error("expected 'upgrade' in structured commands")
	}
	if !sharedtest.Contains(ctx.structured, "mcp") {
		t.Error("expected 'mcp' in structured commands")
	}

	// Should have subcommands
	if subs, ok := ctx.subcommands["migrate"]; !ok || len(subs) == 0 {
		t.Error("expected migrate subcommands")
	}
	if subs, ok := ctx.subcommands["sessions"]; !ok || len(subs) == 0 {
		t.Error("expected sessions subcommands")
	}
	assertStringSet(t, ctx.subcommands["sessions"], []string{"export", "inspect", "list", "replay", "summary"})
	assertStringSet(t, ctx.subcommands["mcp"], []string{"install"})
	if !containsStructuredFlag(ctx.structuredFlags["sessions export"], "--since") {
		t.Error("expected sessions export structured flag --since")
	}
	for _, flag := range []string{"--since", "--command"} {
		if !containsStructuredFlag(ctx.structuredFlags["sessions summary"], flag) {
			t.Errorf("expected sessions summary structured flag %s", flag)
		}
	}

	// Should have flags
	if !sharedtest.Contains(ctx.globalFlagNames(), "--verbose") {
		t.Error("expected --verbose in global flags")
	}
	if len(ctx.structuredFlags["workspace init"]) == 0 {
		t.Error("expected workspace init structured flags")
	}
	if len(ctx.structuredFlags["projects create"]) == 0 {
		t.Error("expected projects create structured flags")
	}
	if !containsStructuredFlag(ctx.structuredFlags["upgrade"], "--deps") {
		t.Error("expected upgrade structured flags")
	}
	if len(ctx.structuredFlagValues["workspace init --extension"]) != 3 {
		t.Error("expected workspace init extension values")
	}
}

func TestCompletionHidesInternalExtensionCommands(t *testing.T) {
	dir := writeInternalCommandCompletionWorkspace(t)
	ctx := gatherCompletionContext(dir, &wsproto.Config{})

	if sharedtest.Contains(ctx.commands, "deps-upgrade") {
		t.Fatalf("internal command leaked into completion commands: %v", ctx.commands)
	}
	if sharedtest.Contains(ctx.commands, "workspace-install") {
		t.Fatalf("internal command leaked into completion commands: %v", ctx.commands)
	}
	if !sharedtest.Contains(ctx.commands, "build") {
		t.Fatalf("expected public build command in completions: %v", ctx.commands)
	}
	if len(ctx.structuredFlags["deps-upgrade"]) != 0 {
		t.Fatalf("internal command flags leaked: %+v", ctx.structuredFlags["deps-upgrade"])
	}
	if !sharedtest.Contains(ctx.structured, "upgrade") {
		t.Fatalf("expected upgrade structured command: %v", ctx.structured)
	}

	var bash bytes.Buffer
	CompletionBash(&bash, dir, &wsproto.Config{})
	if strings.Contains(bash.String(), "deps-upgrade") {
		t.Error("expected bash completion to hide deps-upgrade")
	}
	if !strings.Contains(bash.String(), "upgrade") {
		t.Error("expected bash completion to include upgrade")
	}

	var zsh bytes.Buffer
	CompletionZsh(&zsh, dir, &wsproto.Config{})
	if strings.Contains(zsh.String(), "deps-upgrade") {
		t.Error("expected zsh completion to hide deps-upgrade")
	}
	if !strings.Contains(zsh.String(), "'upgrade:Upgrade CLI, extensions, and dependencies'") {
		t.Error("expected zsh completion to include upgrade")
	}

	var fish bytes.Buffer
	CompletionFish(&fish, dir, &wsproto.Config{})
	if strings.Contains(fish.String(), "deps-upgrade") {
		t.Error("expected fish completion to hide deps-upgrade")
	}
	if !strings.Contains(fish.String(), "-a 'upgrade'") {
		t.Error("expected fish completion to include upgrade")
	}
}

func TestGatherCompletionContext_FlatJobFlagsAndAliases(t *testing.T) {
	dir := writeJobFlagCompletionWorkspace(t)
	ctx := gatherCompletionContext(dir, &wsproto.Config{})

	deployFlags := ctx.structuredFlags["deploy"]
	if !containsStructuredFlag(deployFlags, "--environment") {
		t.Fatalf("expected deploy to include --environment, got %+v", deployFlags)
	}
	if !containsStructuredFlag(deployFlags, "--confirm") {
		t.Fatalf("expected deploy to include --confirm, got %+v", deployFlags)
	}
	for _, alias := range []string{"d", "D"} {
		if !sharedtest.Contains(ctx.commands, alias) {
			t.Fatalf("expected alias %q in commands: %v", alias, ctx.commands)
		}
		if !containsStructuredFlag(ctx.structuredFlags[alias], "--environment") {
			t.Fatalf("expected alias %q to expose deploy flags, got %+v", alias, ctx.structuredFlags[alias])
		}
	}

	var bash bytes.Buffer
	CompletionBash(&bash, dir, &wsproto.Config{})
	if !strings.Contains(bash.String(), "'deploy') printf '%s\\n' '--confirm --environment'") {
		t.Error("expected bash completion to include deploy command flags")
	}

	var zsh bytes.Buffer
	CompletionZsh(&zsh, dir, &wsproto.Config{})
	if !strings.Contains(zsh.String(), "'--environment[Deployment environment]:value:'") {
		t.Error("expected zsh completion to include deploy string flag")
	}

	var fish bytes.Buffer
	CompletionFish(&fish, dir, &wsproto.Config{})
	if !strings.Contains(fish.String(), "__putnami_seen_path deploy' -l 'environment'") {
		t.Error("expected fish completion to include deploy string flag")
	}
}

func TestGatherCompletionContext_DiscoversTemplateValues(t *testing.T) {
	dir := t.TempDir()

	// Create domain-based template directories with putnami.template.json
	alphaDir := filepath.Join(dir, "mydomain", "templates", "alpha-template")
	if err := os.MkdirAll(alphaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alphaDir, "putnami.template.json"), []byte(`{
		"name": "alpha-template",
		"description": "Alpha template"
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	betaDir := filepath.Join(dir, "mydomain", "templates", "beta-template")
	if err := os.MkdirAll(betaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(betaDir, "putnami.template.json"), []byte(`{
		"name": "beta-template",
		"description": "Beta template"
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &wsproto.Config{}
	ctx := gatherCompletionContext(dir, cfg)

	values := ctx.structuredFlagValues["projects create --template"]
	if len(values) != 2 {
		t.Fatalf("expected 2 template values, got %d", len(values))
	}
	if values[0].Value != "alpha-template" || values[0].Description != "Alpha template" {
		t.Errorf("first template = %+v, want alpha-template/Alpha template", values[0])
	}
	if values[1].Value != "beta-template" || values[1].Description != "Beta template" {
		t.Errorf("second template = %+v, want beta-template/Beta template", values[1])
	}
}

func TestGatherCompletionContext_ExtensionNestedCommandGroups(t *testing.T) {
	dir := writeNestedCompletionExtensionWorkspace(t)
	ctx := gatherCompletionContext(dir, &wsproto.Config{})

	if !sharedtest.Contains(ctx.structured, "cloud") {
		t.Fatalf("expected cloud structured command, got %v", ctx.structured)
	}

	assertStringSet(t, ctx.subcommands["cloud"], []string{"config", "secrets"})
	assertStringSet(t, ctx.subcommands["cloud config"], []string{"list", "resolve", "show"})
	assertStringSet(t, ctx.subcommands["cloud secrets"], []string{"delete", "list", "reveal", "set"})

	configShowFlags := ctx.structuredFlags["cloud config show"]
	if !containsStructuredFlag(configShowFlags, "--with-secrets") {
		t.Fatalf("expected cloud config show to include --with-secrets, got %+v", configShowFlags)
	}
	if !containsStructuredFlag(configShowFlags, "--env") {
		t.Fatalf("expected cloud config show to inherit --env from cloud-config, got %+v", configShowFlags)
	}
	// The group-level shared flag must cascade to every (including nested)
	// subcommand's completion.
	if !containsStructuredFlag(configShowFlags, "--region") {
		t.Fatalf("expected cloud config show to inherit --region from the cloud group, got %+v", configShowFlags)
	}
	if !ctx.knownSubcommandPaths["cloud config show"] {
		t.Fatalf("expected cloud config show to be a known completion path")
	}
}

func TestCompletionScripts_ExtensionNestedCommandGroups(t *testing.T) {
	dir := writeNestedCompletionExtensionWorkspace(t)
	cfg := &wsproto.Config{}

	var bash bytes.Buffer
	CompletionBash(&bash, dir, cfg)
	bashOut := bash.String()
	for _, want := range []string{
		"'cloud') printf '%s\\n' 'config secrets'",
		"'cloud config') printf '%s\\n' 'list resolve show'",
		"'cloud secrets') printf '%s\\n' 'delete list reveal set'",
		"'cloud config show') printf '%s\\n' '--env --region --with-secrets'",
	} {
		if !strings.Contains(bashOut, want) {
			t.Errorf("bash: expected %q in completion script", want)
		}
	}

	var zsh bytes.Buffer
	CompletionZsh(&zsh, dir, cfg)
	zshOut := zsh.String()
	for _, want := range []string{
		"'cloud config')",
		"'show'",
		"'--with-secrets[Include secret values]'",
	} {
		if !strings.Contains(zshOut, want) {
			t.Errorf("zsh: expected %q in completion script", want)
		}
	}

	var fish bytes.Buffer
	CompletionFish(&fish, dir, cfg)
	fishOut := fish.String()
	for _, want := range []string{
		"__putnami_complete_children cloud config' -a 'show'",
		"__putnami_complete_children cloud secrets' -a 'reveal'",
		"__putnami_seen_path cloud config show' -l 'with-secrets'",
	} {
		if !strings.Contains(fishOut, want) {
			t.Errorf("fish: expected %q in completion script", want)
		}
	}
}

func TestGatherCompletionContext_ExtensionFlatCommandGroupStillWorks(t *testing.T) {
	dir := writeFlatCompletionExtensionWorkspace(t)
	ctx := gatherCompletionContext(dir, &wsproto.Config{})

	if !sharedtest.Contains(ctx.structured, "demo") {
		t.Fatalf("expected demo structured command, got %v", ctx.structured)
	}
	assertStringSet(t, ctx.subcommands["demo"], []string{"login", "status"})
	if len(ctx.subcommands["demo login"]) != 0 {
		t.Fatalf("flat manifest should not create nested subcommands, got %v", ctx.subcommands["demo login"])
	}
}

func TestCompletionScripts_IncludeDiscoveredTemplateValues(t *testing.T) {
	dir := t.TempDir()

	// Create domain-based template directory with putnami.template.json
	tplDir := filepath.Join(dir, "mydomain", "templates", "custom-template")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "putnami.template.json"), []byte(`{
		"name": "custom-template",
		"description": "Custom template"
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &wsproto.Config{}

	var bash bytes.Buffer
	CompletionBash(&bash, dir, cfg)
	if !strings.Contains(bash.String(), "custom-template") {
		t.Error("expected discovered template in bash completion")
	}

	var zsh bytes.Buffer
	CompletionZsh(&zsh, dir, cfg)
	if !strings.Contains(zsh.String(), "'custom-template:Custom template'") {
		t.Error("expected discovered template in zsh completion")
	}

	var fish bytes.Buffer
	CompletionFish(&fish, dir, cfg)
	if !strings.Contains(fish.String(), "-l 'template'") || !strings.Contains(fish.String(), "custom-template") {
		t.Error("expected discovered template in fish completion")
	}
}

func TestRefreshShellCompletions_OverwritesExistingZshCompletion(t *testing.T) {
	home := hometest.Temp(t)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZSH", "")
	t.Setenv("ZSH_CUSTOM", "")

	compPath := filepath.Join(home, ".zfunc", "_putnami")
	if err := os.MkdirAll(filepath.Dir(compPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compPath, []byte("old completion"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := RefreshShellCompletions(context.Background(), completionBinary(t))
	if err != nil {
		t.Fatalf("RefreshShellCompletions: %v", err)
	}
	if result.Path != compPath {
		t.Fatalf("completion path = %q, want %q", result.Path, compPath)
	}
	data, err := os.ReadFile(compPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "generated-zsh\n" {
		t.Fatalf("completion content = %q, want generated-zsh", string(data))
	}
	if !result.ZshCacheHint {
		t.Fatal("expected zsh cache hint")
	}
}

func TestRefreshShellCompletions_ReportsZshDuplicateFiles(t *testing.T) {
	home := t.TempDir()
	zshDir := filepath.Join(home, ".oh-my-zsh")
	hometest.Set(t, home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZSH", zshDir)
	t.Setenv("ZSH_CUSTOM", "")

	ohMyZshComp := filepath.Join(zshDir, "custom", "completions", "_putnami")
	zfuncComp := filepath.Join(home, ".zfunc", "_putnami")
	for _, path := range []string{ohMyZshComp, zfuncComp} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("old completion"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	result, err := RefreshShellCompletions(context.Background(), completionBinary(t))
	if err != nil {
		t.Fatalf("RefreshShellCompletions: %v", err)
	}
	if result.Path != ohMyZshComp {
		t.Fatalf("completion path = %q, want %q", result.Path, ohMyZshComp)
	}
	if len(result.ZshDuplicates) != 2 {
		t.Fatalf("expected two zsh duplicate paths, got %v", result.ZshDuplicates)
	}
}

// redirectSystemBashCompletionDir points the system bash-completion probe at dir
// for the duration of the test so the resolved path does not depend on whether
// the suite runs as root. Passing "" disables the system path entirely.
func redirectSystemBashCompletionDir(t *testing.T, dir string) {
	t.Helper()
	prev := systemBashCompletionDir
	systemBashCompletionDir = dir
	t.Cleanup(func() { systemBashCompletionDir = prev })
}

func TestRefreshShellCompletions_OverwritesExistingBashCompletion(t *testing.T) {
	home := hometest.Temp(t)
	t.Setenv("SHELL", "/bin/bash")
	// Force the per-user fallback path; otherwise a writable /etc/bash_completion.d
	// (present when the suite runs as root) would win and the assertion below would
	// depend on the runner's UID.
	redirectSystemBashCompletionDir(t, "")

	compPath := filepath.Join(home, ".local", "share", "bash-completion", "completions", "putnami")
	if err := os.MkdirAll(filepath.Dir(compPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compPath, []byte("old completion"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := RefreshShellCompletions(context.Background(), completionBinary(t))
	if err != nil {
		t.Fatalf("RefreshShellCompletions: %v", err)
	}
	if result.Path != compPath {
		t.Fatalf("completion path = %q, want %q", result.Path, compPath)
	}
	data, err := os.ReadFile(compPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "generated-bash\n" {
		t.Fatalf("completion content = %q, want generated-bash", string(data))
	}
}

func TestRefreshShellCompletions_PrefersWritableSystemBashDir(t *testing.T) {
	hometest.Temp(t)
	t.Setenv("SHELL", "/bin/bash")
	// A writable system dir takes precedence over the per-user fallback. Point the
	// probe at a temp dir so the branch is exercised deterministically without
	// touching the real /etc and without depending on the runner's UID.
	sysDir := t.TempDir()
	redirectSystemBashCompletionDir(t, sysDir)

	result, err := RefreshShellCompletions(context.Background(), completionBinary(t))
	if err != nil {
		t.Fatalf("RefreshShellCompletions: %v", err)
	}
	want := filepath.Join(sysDir, "putnami")
	if result.Path != want {
		t.Fatalf("completion path = %q, want %q", result.Path, want)
	}
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "generated-bash\n" {
		t.Fatalf("completion content = %q, want generated-bash", string(data))
	}
}

// writeCompletionWorkspace builds a temp workspace with projects whose names
// deliberately differ from their path basenames, so completion of bare names
// and short-form basenames can be exercised independently of the IDs.
func writeCompletionWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{
		"name": "test-workspace",
		"includes": ["packages/web-app", "services/api", "packages/widget"]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, p := range []struct{ path, name string }{
		{"packages/web-app", "@acme/web-app"}, // name != basename "web-app"
		{"services/api", "acme-api"},          // name != basename "api"
		{"packages/widget", "widget"},         // name == basename (dedup check)
	} {
		projDir := filepath.Join(dir, filepath.FromSlash(p.path))
		if err := os.MkdirAll(projDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(projDir, "putnami.json"),
			[]byte(`{"name":"`+p.name+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeNestedCompletionExtensionWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{
		"name": "completion-ws",
		"includes": ["cloud-extension"]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	extDir := filepath.Join(dir, "cloud-extension")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "putnami.json"), []byte(`{"name":"@putnami/cloud"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := `{
  "name": "@putnami/cloud",
  "version": "0.1.0",
  "cliContract": 4,
  "commandGroups": {
    "cloud": {
      "description": "Manage Putnami Cloud",
      "flags": {
        "region": {
          "type": "string",
          "description": "Cloud region"
        }
      },
      "subcommands": {
        "config": {
          "description": "Inspect resolved project config.",
          "command": "cloud-config",
          "subcommands": {
            "list": {
              "description": "List resolved config keys for a project/env.",
              "positionals": [{ "name": "project", "required": false }]
            },
            "show": {
              "description": "Show resolved config.",
              "positionals": [{ "name": "project", "required": false }],
              "flags": {
                "with-secrets": {
                  "type": "boolean",
                  "description": "Include secret values"
                }
              }
            },
            "resolve": {
              "description": "Resolve config metadata.",
              "positionals": [{ "name": "project", "required": false }]
            }
          }
        },
        "secrets": {
          "description": "Manage workspace and project secrets.",
          "command": "cloud-secrets",
          "subcommands": {
            "list": {
              "description": "List workspace or project secret metadata.",
              "positionals": [{ "name": "project", "required": false }]
            },
            "set": {
              "description": "Write a secret.",
              "positionals": [
                { "name": "project", "required": true },
                { "name": "key", "required": true }
              ]
            },
            "reveal": {
              "description": "Reveal plaintext secrets.",
              "positionals": [
                { "name": "project", "required": true },
                { "name": "key", "required": false }
              ],
              "flags": { "yes": { "type": "boolean", "description": "Confirm reveal" } }
            },
            "delete": {
              "description": "Delete a secret.",
              "positionals": [
                { "name": "project", "required": true },
                { "name": "key", "required": true }
              ]
            }
          }
        }
      }
    }
  },
  "commands": {
    "cloud-config": {
      "description": "Config command group target.",
      "flags": {
        "env": {
          "type": "string",
          "description": "Environment name"
        }
      },
      "run": [{ "id": "config", "task": "cloud-config-task" }]
    },
    "cloud-secrets": {
      "description": "Secrets command group target.",
      "run": [{ "id": "secrets", "task": "cloud-secrets-task" }]
    }
  },
  "tasks": {
    "cloud-config-task": { "kind": "command", "command": "echo", "cache": false },
    "cloud-secrets-task": { "kind": "command", "command": "echo", "cache": false }
  }
}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFlatCompletionExtensionWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{
		"name": "flat-completion-ws",
		"includes": ["demo-extension"]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	extDir := filepath.Join(dir, "demo-extension")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "putnami.json"), []byte(`{"name":"@putnami/demo-extension"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{
  "name": "@putnami/demo-extension",
  "version": "0.1.0",
  "cliContract": 4,
  "commandGroups": {
    "demo": {
      "subcommands": {
        "login": { "command": "demo-login" },
        "status": { "command": "demo-status" }
      }
    }
  },
  "commands": {
    "demo-login": { "run": [{ "id": "login", "task": "demo-login-task" }] },
    "demo-status": { "run": [{ "id": "status", "task": "demo-status-task" }] }
  },
  "tasks": {
    "demo-login-task": { "kind": "command", "command": "echo", "cache": false },
    "demo-status-task": { "kind": "command", "command": "echo", "cache": false }
  }
}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeJobFlagCompletionWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{
		"name": "job-flag-completion-ws",
		"includes": ["cloud-extension"]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	extDir := filepath.Join(dir, "cloud-extension")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "putnami.json"), []byte(`{"name":"@putnami/cloud"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{
  "name": "@putnami/cloud",
  "version": "0.1.0",
  "cliContract": 4,
  "commands": {
    "deploy": {
      "description": "Deploy to cloud.",
      "flags": {
        "environment": {
          "type": "string",
          "description": "Deployment environment"
        },
        "confirm": {
          "type": "boolean",
          "description": "Confirm deployment"
        }
      },
      "run": [{ "id": "deploy", "task": "deploy-task" }]
    }
  },
  "tasks": {
    "deploy-task": { "kind": "command", "command": "echo", "cache": false }
  }
}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeInternalCommandCompletionWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{
		"name": "internal-command-completion-ws",
		"includes": ["language-extension"]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	extDir := filepath.Join(dir, "language-extension")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "putnami.json"), []byte(`{"name":"@putnami/language"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{
  "name": "@putnami/language",
  "version": "0.1.0",
  "cliContract": 4,
  "commands": {
    "build": {
      "description": "Build project.",
      "run": [{ "id": "build", "task": "build-task" }]
    },
    "workspace-install": {
      "description": "Install workspace dependencies.",
      "visibility": "internal",
      "run": [{ "id": "workspace-install", "task": "workspace-install-task" }]
    },
    "deps-upgrade": {
      "description": "Upgrade framework dependencies.",
      "visibility": "internal",
      "flags": {
        "putnami-version": {
          "type": "string",
          "description": "Version to require"
        }
      },
      "run": [{ "id": "deps-upgrade", "task": "deps-upgrade-task" }]
    }
  },
  "tasks": {
    "build-task": { "kind": "command", "command": "echo", "cache": false },
    "workspace-install-task": { "kind": "command", "command": "echo", "cache": false },
    "deps-upgrade-task": { "kind": "command", "command": "echo", "cache": false }
  }
}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertStringSet(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func containsStructuredFlag(flags []StructuredFlagInfo, long string) bool {
	for _, flag := range flags {
		if flag.Long == long {
			return true
		}
	}
	return false
}

// completionBinary returns the binary RefreshShellCompletions runs: this test
// binary, in the role completionBinaryEnv gives it.
func completionBinary(t *testing.T) string {
	t.Helper()
	t.Setenv(completionBinaryEnv, "1")
	// A race-instrumented binary otherwise sleeps a second as it exits.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return executable
}

func TestGatherCompletionContext_CollectsBasenames(t *testing.T) {
	dir := writeCompletionWorkspace(t)
	ctx := gatherCompletionContext(dir, &wsproto.Config{})

	if !sharedtest.Contains(ctx.projects, "@acme/web-app") || !sharedtest.Contains(ctx.projects, "acme-api") {
		t.Errorf("expected project names, got %v", ctx.projects)
	}
	if !sharedtest.Contains(ctx.projectBasenames, "web-app") || !sharedtest.Contains(ctx.projectBasenames, "api") {
		t.Errorf("expected path basenames web-app/api, got %v", ctx.projectBasenames)
	}
	// "widget" is both a name and a basename; it must appear once (as a name)
	// and be deduplicated out of the basenames list.
	if !sharedtest.Contains(ctx.projects, "widget") {
		t.Errorf("expected name 'widget', got %v", ctx.projects)
	}
	if sharedtest.Contains(ctx.projectBasenames, "widget") {
		t.Errorf("basename 'widget' should be deduped against names, got %v", ctx.projectBasenames)
	}
}

// TestCompletionScripts_OfferNamesAndBasenamesAsTargets guards the reported
// bug: typing a bare project name or path basename as a positional target
// produced no completions because only IDs (which all start with "/") and
// aliases were offered.
func TestCompletionScripts_OfferNamesAndBasenamesAsTargets(t *testing.T) {
	dir := writeCompletionWorkspace(t)
	cfg := &wsproto.Config{}

	var bash bytes.Buffer
	CompletionBash(&bash, dir, cfg)
	bashOut := bash.String()
	if !strings.Contains(bashOut, `local project_basenames="`) {
		t.Error("bash: missing project_basenames variable")
	}
	for _, want := range []string{"web-app", "api", "acme-api"} {
		if !strings.Contains(bashOut, want) {
			t.Errorf("bash: expected target %q in completion", want)
		}
	}
	// The positional branch must complete the full target set, not just IDs.
	if !strings.Contains(bashOut, `compgen -W "${project_targets} ${global_flags}"`) {
		t.Error("bash: positional target should use project_targets (ids+names+basenames+aliases)")
	}
	if strings.Contains(bashOut, `compgen -W "${projects} ${project_ids}"`) {
		t.Error("bash: --exclude should not suggest project IDs")
	}

	var zsh bytes.Buffer
	CompletionZsh(&zsh, dir, cfg)
	zshOut := zsh.String()
	if !strings.Contains(zshOut, "project_basenames=(") {
		t.Error("zsh: missing project_basenames array")
	}
	if !strings.Contains(zshOut, "_describe -t project-names 'project name' project_basenames") {
		t.Error("zsh: positional target should describe project_basenames")
	}
	if !strings.Contains(zshOut, "_describe -t project-names 'project name' projects") {
		t.Error("zsh: positional target should describe project names")
	}
	if strings.Contains(zshOut, `elif [[ "${prev}" == "--exclude" ]]; then
                        _describe -t project-names 'project name' projects
                        _describe -t project-ids`) {
		t.Error("zsh: --exclude should not suggest project IDs")
	}

	var fish bytes.Buffer
	CompletionFish(&fish, dir, cfg)
	fishOut := fish.String()
	for _, want := range []string{"web-app", "api", "acme-api"} {
		if !strings.Contains(fishOut, want) {
			t.Errorf("fish: expected target %q in completion", want)
		}
	}
	if strings.Contains(fishOut, "Project names/IDs (for --exclude flag value)") {
		t.Error("fish: --exclude should not suggest project IDs")
	}
}
