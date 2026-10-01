package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/hometest"
)

func TestParseInitFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want initFlags
	}{
		{
			name: "defaults",
			args: nil,
			want: initFlags{extension: "ts"},
		},
		{
			name: "extension go",
			args: []string{"--extension", "go"},
			want: initFlags{extension: "go"},
		},
		{
			name: "all flags",
			args: []string{"--force", "--workspace", "my-ws", "--project", "my-app", "--project-path", "apps/my-app", "--extension", "py"},
			want: initFlags{force: true, workspace: "my-ws", project: "my-app", projectPath: "apps/my-app", extension: "py"},
		},
		{
			name: "workspace flag",
			args: []string{"--workspace", "explicit"},
			want: initFlags{workspace: "explicit", extension: "ts"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseInitFlags(tt.args)
			if got != tt.want {
				t.Errorf("parseInitFlags(%v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

// stubExtensionInstall makes the init-time extension install fail instantly.
// The real install resolves against the registry — slow, and the test outcome
// would depend on whether the environment is online/logged in. Tests that only
// assert the files written before installation deliberately ignore the error.
func stubExtensionInstall(t *testing.T) {
	t.Helper()
	orig := installExtension
	installExtension = func(context.Context, string, *wsproto.Config, []string, string) error {
		return errors.New("extension install disabled in test")
	}
	t.Cleanup(func() { installExtension = orig })
}

func TestWorkspaceInitCreatesConfig(t *testing.T) {
	dir := t.TempDir()
	stubExtensionInstall(t)

	// Change to temp dir for the test
	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ctx := context.Background()
	// The stubbed extension install must fail the command, while preserving the
	// workspace config that was successfully written before that required stage.
	err := WorkspaceInit(ctx, "", []string{"--workspace", "test-ws", "--extension", "go"}, LifecycleEnv{})
	if err == nil {
		t.Fatal("expected the stubbed extension install to fail workspace init")
	}

	// Verify putnami.workspace.json was created with correct content
	data, err := os.ReadFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename))
	if err != nil {
		t.Fatalf("expected %s to exist: %v", wsproto.WorkspaceConfigFilename, err)
	}

	var cfg initConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("invalid JSON in config: %v", err)
	}

	if cfg.Name != "test-ws" {
		t.Errorf("workspace name = %q, want %q", cfg.Name, "test-ws")
	}
	if strings.Join(cfg.Extensions, ",") != "@putnami/go,@putnami/contributor" {
		t.Errorf("extensions = %v, want the language extension and the starter's agent-content extension", cfg.Extensions)
	}
	if strings.Join(cfg.AgentArtifacts, ",") != "extension:@putnami/contributor" {
		t.Errorf("agentArtifacts = %v, want the starter's opt-in", cfg.AgentArtifacts)
	}
	if strings.Contains(string(data), "@putnami/cloud") {
		t.Fatalf("core workspace init added the optional cloud extension:\n%s", data)
	}
}

func TestWorkspaceInitRequiredStageFailuresStopBeforeReady(t *testing.T) {
	tests := []struct {
		name           string
		stage          string
		dependencyCall int
		projectPath    string
		wantMessage    string
		wantNext       string
	}{
		{
			name:        "extension install",
			stage:       "extension",
			wantMessage: "install extension @putnami/go",
			wantNext:    "putnami extensions install",
		},
		{
			name:           "initial dependency install",
			stage:          "dependencies",
			dependencyCall: 1,
			wantMessage:    "install workspace dependencies",
			wantNext:       "putnami deps install",
		},
		{
			name:        "template install",
			stage:       "template",
			wantMessage: "install template go-server",
			wantNext:    "putnami templates install",
		},
		{
			// A failed create can leave the project directory behind, which only
			// --force lets the retry render over.
			name:        "project creation",
			stage:       "project",
			wantMessage: "create project my-app",
			wantNext:    "putnami projects create my-app --template go-server --force",
		},
		{
			name:        "project creation at a custom path",
			stage:       "project",
			projectPath: "apps/my-app",
			wantMessage: "create project my-app",
			wantNext:    "putnami projects create my-app --template go-server --path apps/my-app --force",
		},
		{
			name:           "post-project dependency install",
			stage:          "dependencies",
			dependencyCall: 2,
			wantMessage:    "install workspace dependencies after creating my-app",
			wantNext:       "putnami deps install",
		},
		{
			name:        "lock metadata refresh",
			stage:       "lock",
			wantMessage: "refresh lock metadata",
			wantNext:    "putnami install",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			failure := stubWorkspaceInitStages(t, tt.stage, tt.dependencyCall)
			t.Chdir(dir)

			args := []string{
				"--workspace", "test-ws",
				"--project", "my-app",
				"--extension", "go",
			}
			if tt.projectPath != "" {
				args = append(args, "--project-path", tt.projectPath)
			}
			output, err := captureStdout(t, func() error {
				return WorkspaceInit(context.Background(), "", args, LifecycleEnv{})
			})
			if err == nil {
				t.Fatalf("WorkspaceInit returned nil after %s failure", tt.stage)
			}
			if !errors.Is(err, failure) {
				t.Errorf("WorkspaceInit error does not preserve stage cause: %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantMessage) {
				t.Errorf("WorkspaceInit error = %q, want it to contain %q", err, tt.wantMessage)
			}
			if next := protocolcli.SuggestedNext(err); next != tt.wantNext {
				t.Errorf("suggested next command = %q, want %q", next, tt.wantNext)
			}
			if strings.Contains(output, "Workspace ready") {
				t.Fatalf("failed init emitted a partial-success message:\n%s", output)
			}
		})
	}
}

// TestWorkspaceInitRecordsToolchainPinsAfterItsLastInstall: init ends with the
// lock refresh `putnami install` ends with, after the dependency install that
// writes the files the pins derive from, so the commands it prints next find
// their toolchains pinned.
func TestWorkspaceInitRecordsToolchainPinsAfterItsLastInstall(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "init-records-pins", "init-refreshes-the-lock-after-its-last-dependency-install")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "without a project", args: []string{"--workspace", "test-ws", "--extension", "go"},
			want: "dependencies,lock"},
		{name: "with a project", args: []string{"--workspace", "test-ws", "--project", "my-app", "--extension", "go"},
			want: "dependencies,project,dependencies,lock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			stubWorkspaceInitStages(t, "", 0)
			t.Chdir(dir)
			var stages []string
			installInitDependencies = func(context.Context, string, *wsproto.Config, string, string, LifecycleEnv) error {
				stages = append(stages, "dependencies")
				return nil
			}
			createInitProject = func(context.Context, string, *wsproto.Config, []string, bool, LifecycleEnv) error {
				stages = append(stages, "project")
				return nil
			}
			var refreshedRoot, refreshedVersion string
			refreshInitLockMetadata = func(_ context.Context, root, cliVersion string) (bool, error) {
				stages = append(stages, "lock")
				refreshedRoot, refreshedVersion = root, cliVersion
				return true, nil
			}

			output, err := captureStdout(t, func() error {
				return WorkspaceInit(context.Background(), "", tt.args, LifecycleEnv{CLIVersion: "1.2.3"})
			})
			if err != nil {
				t.Fatalf("WorkspaceInit: %v\n%s", err, output)
			}
			if got := strings.Join(stages, ","); got != tt.want {
				t.Fatalf("init stages = %s, want %s", got, tt.want)
			}
			if filepath.Base(refreshedRoot) != filepath.Base(dir) || refreshedVersion != "1.2.3" {
				t.Fatalf("refresh ran for %q at CLI %q, want the new workspace at the running CLI's version", refreshedRoot, refreshedVersion)
			}
			if !strings.Contains(output, "Workspace ready") {
				t.Fatalf("init did not finish:\n%s", output)
			}
		})
	}
}

// TestWorkspaceInitListsTheProjectOnce: the project create that init runs lists
// the project in the workspace config. Init then lists it once, in the config
// and in the root package.json workspaces, and that holds when the dependency
// install that follows fails and leaves the files as init wrote them.
func TestWorkspaceInitListsTheProjectOnce(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "init-lists-members-once", "init-lists-the-created-project-once")
	dir := t.TempDir()
	failure := stubWorkspaceInitStages(t, "dependencies", 2)
	t.Chdir(dir)
	createInitProject = func(_ context.Context, wsRoot string, _ *wsproto.Config, _ []string, _ bool, _ LifecycleEnv) error {
		// What the project create leaves: the project on disk, and listed in
		// the workspace config.
		if err := os.MkdirAll(filepath.Join(wsRoot, "webapp"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(wsRoot, "webapp", "package.json"), []byte(`{"name":"webapp"}`), 0o644); err != nil {
			return err
		}
		return updateConfigMembership(wsRoot, nil, []string{"webapp"})
	}

	_, err := captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), "", []string{"--workspace", "test-ws", "--project", "webapp", "--extension", "ts"}, LifecycleEnv{})
	})
	if !errors.Is(err, failure) {
		t.Fatalf("WorkspaceInit = %v, want the failure of the dependency install after the project create", err)
	}

	var manifest struct {
		Workspaces []string `json:"workspaces"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(manifest.Workspaces, []string{"webapp"}) {
		t.Errorf("package.json workspaces = %v, want webapp once", manifest.Workspaces)
	}
	if includes := wsproto.Load(dir).Includes; !slices.Equal(includes, []string{"webapp"}) {
		t.Errorf("workspace includes = %v, want webapp once", includes)
	}
}

// TestWorkspaceInitWritesAnEmptyWorkspacesList: a workspace whose projects hold
// no package.json, such as one created for Go, gets an empty workspaces array
// in its root package.json, never null.
func TestWorkspaceInitWritesAnEmptyWorkspacesList(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "init-lists-members-once", "init-writes-an-empty-workspaces-list-not-null")
	dir := t.TempDir()
	stubWorkspaceInitStages(t, "", 0)
	t.Chdir(dir)
	createInitProject = func(_ context.Context, wsRoot string, _ *wsproto.Config, _ []string, _ bool, _ LifecycleEnv) error {
		if err := os.MkdirAll(filepath.Join(wsRoot, "api"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(wsRoot, "api", "go.mod"), []byte("module example.test/api\n"), 0o644); err != nil {
			return err
		}
		return updateConfigMembership(wsRoot, nil, []string{"api"})
	}

	output, err := captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), "", []string{"--workspace", "test-ws", "--project", "api", "--extension", "go"}, LifecycleEnv{})
	})
	if err != nil {
		t.Fatalf("WorkspaceInit: %v\n%s", err, output)
	}

	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if got := string(manifest["workspaces"]); got != "[]" {
		t.Errorf("package.json workspaces = %s, want []\n%s", got, data)
	}
}

func stubWorkspaceInitStages(t *testing.T, failingStage string, failingDependencyCall int) error {
	t.Helper()
	// Every built-in starter opts into the contributor extension's agent
	// content, and installing it needs a registry. A stage test never does, so
	// the starters opt into nothing here; a test about agent content opts one
	// back in with starterOptsInto after this call.
	for starter := range initExtensions {
		starterOptsInto(t, starter, "")
	}
	origExtension := installExtension
	origDependencies := installInitDependencies
	origTemplate := installInitTemplate
	origProject := createInitProject
	origRefresh := refreshInitLockMetadata
	t.Cleanup(func() {
		installExtension = origExtension
		installInitDependencies = origDependencies
		installInitTemplate = origTemplate
		createInitProject = origProject
		refreshInitLockMetadata = origRefresh
	})

	failure := errors.New(failingStage + " failed")
	installExtension = func(context.Context, string, *wsproto.Config, []string, string) error {
		if failingStage == "extension" {
			return failure
		}
		return nil
	}
	dependencyCalls := 0
	installInitDependencies = func(context.Context, string, *wsproto.Config, string, string, LifecycleEnv) error {
		dependencyCalls++
		if failingStage == "dependencies" && dependencyCalls == failingDependencyCall {
			return failure
		}
		return nil
	}
	installInitTemplate = func(context.Context, string, *wsproto.Config, []string, string) error {
		if failingStage == "template" {
			return failure
		}
		return nil
	}
	createInitProject = func(context.Context, string, *wsproto.Config, []string, bool, LifecycleEnv) error {
		if failingStage == "project" {
			return failure
		}
		return nil
	}
	refreshInitLockMetadata = func(context.Context, string, string) (bool, error) {
		if failingStage == "lock" {
			return false, failure
		}
		return false, nil
	}
	return failure
}

func TestWorkspaceInitCreatesGitignore(t *testing.T) {
	dir := t.TempDir()
	stubExtensionInstall(t)

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ctx := context.Background()
	_ = WorkspaceInit(ctx, "", []string{"--extension", "go"}, LifecycleEnv{})

	gitignorePath := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		t.Error(".gitignore should have been created")
	}
}

// TestWorkspaceInitWritesAReadme: a new workspace documents itself from its
// first commit, and init --force keeps a README the team already has.
func TestWorkspaceInitWritesAReadme(t *testing.T) {
	dir := t.TempDir()
	stubExtensionInstall(t)

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	_ = WorkspaceInit(context.Background(), "", []string{"--extension", "go", "--workspace", "shop"}, LifecycleEnv{})

	data, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("README.md should have been created: %v", err)
	}
	for _, want := range []string{"# shop\n", "putnami lint,test,build --impacted", "README.md"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("README.md = %q, want it to contain %q", data, want)
		}
	}

	existing := "# Our shop\n"
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = WorkspaceInit(context.Background(), dir, []string{"--force", "--extension", "go"}, LifecycleEnv{})
	if data, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(data) != existing {
		t.Fatalf("README.md = %q, want the existing file untouched", data)
	}
}

// TestWorkspaceInitWritesTheLFPolicy: a new workspace carries the
// .gitattributes rule that checks text out with LF on every platform (D-W4).
func TestWorkspaceInitWritesTheLFPolicy(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "lf-checkout-policy", "init-writes-the-lf-policy")
	dir := t.TempDir()
	stubExtensionInstall(t)

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	_ = WorkspaceInit(context.Background(), "", []string{"--extension", "go"}, LifecycleEnv{})

	data, err := os.ReadFile(filepath.Join(dir, ".gitattributes"))
	if err != nil {
		t.Fatalf(".gitattributes should have been created: %v", err)
	}
	if string(data) != "* text=auto eol=lf\n" {
		t.Fatalf(".gitattributes = %q, want the LF policy", data)
	}
}

// TestWorkspaceInitKeepsAnExistingGitattributes: init --force never rewrites
// a .gitattributes the team already has.
func TestWorkspaceInitKeepsAnExistingGitattributes(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "lf-checkout-policy", "init-keeps-an-existing-gitattributes")
	dir := t.TempDir()
	stubExtensionInstall(t)
	existing := "*.png binary\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	_ = WorkspaceInit(context.Background(), dir, []string{"--force", "--extension", "go"}, LifecycleEnv{})

	data, err := os.ReadFile(filepath.Join(dir, ".gitattributes"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != existing {
		t.Fatalf(".gitattributes = %q, want the existing file untouched", data)
	}
}

// Acceptance criterion, as amended by ADR 0040: `putnami init` in an
// empty directory writes the guidance block into AGENTS.md and CLAUDE.md and
// registers the putnami MCP server in .mcp.json, so the first agent session
// reaches it with no manual step.
func TestWorkspaceInitWritesTheGuidanceBlockAndTheMCPRegistrationForAgents(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "generated-context", "init-writes-the-guidance-block")
	spectest.Proves(t, "cli/context-mcp-discovery", "implicit-mcp-registration", "init-registers-the-mcp-server")
	dir := t.TempDir()
	stubExtensionInstall(t)

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	_ = WorkspaceInit(context.Background(), "", []string{"--extension", "go"}, LifecycleEnv{})

	agents, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if !strings.Contains(string(agents), "<!-- putnami:guidance v2 begin sha256:") {
		t.Fatalf("AGENTS.md carries no guidance block:\n%s", agents)
	}
	claude, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	if !strings.Contains(string(claude), "@AGENTS.md") {
		t.Fatalf("CLAUDE.md does not import AGENTS.md:\n%s", claude)
	}
	registration, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("putnami init registered no MCP server: %v", err)
	}
	if !strings.Contains(string(registration), `"putnami"`) || !strings.Contains(string(registration), `"mcp"`) {
		t.Fatalf(".mcp.json does not register the putnami server:\n%s", registration)
	}
}

func TestWorkspaceInitSkipsIfExists(t *testing.T) {
	dir := t.TempDir()

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ctx := context.Background()

	// Should not error when workspace already exists (just prints message)
	err := WorkspaceInit(ctx, dir, nil, LifecycleEnv{})
	if err != nil {
		t.Errorf("expected nil error for existing workspace, got: %v", err)
	}

	// Config should NOT be created (we skipped)
	if _, err := os.Stat(filepath.Join(dir, wsproto.WorkspaceConfigFilename)); !os.IsNotExist(err) {
		t.Error("config should not be created when workspace exists and --force not set")
	}
}

func TestWorkspaceInitForce(t *testing.T) {
	dir := t.TempDir()
	stubExtensionInstall(t)

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ctx := context.Background()

	// With --force, it should proceed even if workspace exists
	_ = WorkspaceInit(ctx, dir, []string{"--force", "--extension", "ts"}, LifecycleEnv{})

	// Config should be created
	if _, err := os.Stat(filepath.Join(dir, wsproto.WorkspaceConfigFilename)); os.IsNotExist(err) {
		t.Error("config should be created with --force")
	}
}

func TestWorkspaceInitDefaultName(t *testing.T) {
	dir := t.TempDir()
	stubExtensionInstall(t)

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ctx := context.Background()
	_ = WorkspaceInit(ctx, "", []string{"--extension", "ts"}, LifecycleEnv{})

	data, err := os.ReadFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename))
	if err != nil {
		t.Fatalf("expected config to exist: %v", err)
	}

	var cfg initConfig
	json.Unmarshal(data, &cfg)

	// Default name should be the directory basename
	expected := filepath.Base(dir)
	if cfg.Name != expected {
		t.Errorf("workspace name = %q, want %q", cfg.Name, expected)
	}
}

func TestWorkspaceInitUnknownExtension(t *testing.T) {
	dir := t.TempDir()

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ctx := context.Background()
	err := WorkspaceInit(ctx, "", []string{"--extension", "rust"}, LifecycleEnv{})
	if err == nil {
		t.Error("expected error for unknown extension")
	}
}

func TestWorkspaceInitProjectPathRequiresProject(t *testing.T) {
	dir := t.TempDir()

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ctx := context.Background()
	err := WorkspaceInit(ctx, "", []string{"--project-path", "apps/my-app"}, LifecycleEnv{})
	if err == nil {
		t.Fatal("expected error when --project-path is used without --project")
	}
	if _, statErr := os.Stat(filepath.Join(dir, wsproto.WorkspaceConfigFilename)); !os.IsNotExist(statErr) {
		t.Error("workspace should not be initialized on invalid --project-path usage")
	}
}

func TestWorkspaceInitRejectsProjectPathOutsideWorkspace(t *testing.T) {
	dir := t.TempDir()

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ctx := context.Background()
	err := WorkspaceInit(ctx, "", []string{"--project", "my-app", "--project-path", "../my-app"}, LifecycleEnv{})
	if err == nil {
		t.Fatal("expected error for project path escaping the workspace")
	}
	if _, statErr := os.Stat(filepath.Join(dir, wsproto.WorkspaceConfigFilename)); !os.IsNotExist(statErr) {
		t.Error("workspace should not be initialized when project path is invalid")
	}
}

// registriesOf returns the registries entries the workspace at dir declares,
// read back through the loader every command uses, as compact JSON.
func registriesOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	cfg := wsproto.Load(dir)
	out := make(map[string]string, len(cfg.Registries))
	for ecosystem := range cfg.Registries {
		entry, _ := cfg.RegistryEntry(ecosystem)
		compact, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("registries.%s: %v", ecosystem, err)
		}
		out[ecosystem] = string(compact)
	}
	return out
}

// The TypeScript starter's framework dependencies are @putnami npm packages, so
// init declares the registry that serves them; the workspace install writes the
// .npmrc scope line from that entry. The Go starter declares no npm registry.
func TestWorkspaceInitDeclaresTheStarterRegistries(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "starters-declare-their-registries",
		"the-typescript-starter-declares-the-putnami-npm-scope")
	for _, tc := range []struct {
		extension string
		want      map[string]string
	}{
		{extension: "ts", want: map[string]string{"npm": `{"scopes":{"@putnami":"https://npm.putnami.dev"}}`}},
		{extension: "go", want: map[string]string{}},
	} {
		t.Run(tc.extension, func(t *testing.T) {
			dir := t.TempDir()
			hometest.Temp(t)
			stubExtensionInstall(t)
			t.Chdir(dir)

			_ = WorkspaceInit(context.Background(), "", []string{"--extension", tc.extension}, LifecycleEnv{})

			got := registriesOf(t, dir)
			if len(got) != len(tc.want) {
				t.Fatalf("registries = %v, want %v", got, tc.want)
			}
			for ecosystem, entry := range tc.want {
				if got[ecosystem] != entry {
					t.Errorf("registries.%s = %s, want %s", ecosystem, got[ecosystem], entry)
				}
			}
		})
	}
}

// Re-initializing rewrites the workspace config, but an entry the workspace
// already declares is the user's: it is kept whole, and the starter adds only
// the ecosystems the workspace did not declare.
func TestWorkspaceInitForceKeepsTheDeclaredRegistries(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "starters-declare-their-registries",
		"reinit-keeps-a-declared-registries-entry")
	dir := t.TempDir()
	hometest.Temp(t)
	stubExtensionInstall(t)
	t.Chdir(dir)
	mirror := `{"scopes":{"@putnami":"https://npm.mirror.example"},"publish":"https://npm.mirror.example"}`
	writeRawFile(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename),
		`{"name":"ws","registries":{"npm":`+mirror+`,"oci":{"publish":"oci.example/team"}}}`)

	_ = WorkspaceInit(context.Background(), dir, []string{"--force", "--extension", "ts"}, LifecycleEnv{})

	got := registriesOf(t, dir)
	want := map[string]string{
		"npm": `{"scopes":{"@putnami":"https://npm.mirror.example"},"publish":"https://npm.mirror.example"}`,
		"oci": `{"publish":"oci.example/team"}`,
	}
	if len(got) != len(want) || got["npm"] != want["npm"] || got["oci"] != want["oci"] {
		t.Fatalf("registries = %v, want the declared entries kept whole: %v", got, want)
	}
}
