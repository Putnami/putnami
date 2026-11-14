package main

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/jsonl"
)

// importedConfigsWorkspace is devDepsWorkspace with the member web's
// putnami.json config, and the extension's shipped configuration files.
func importedConfigsWorkspace(t *testing.T, config string) (wsDir string, install func()) {
	t.Helper()
	ctx, _ := devDepsWorkspace(t, config)
	writeProbeFile(t, filepath.Join(ctx.Extension.Root, "config", "tsconfig.json"), `{"compilerOptions":{"strict":true}}`)
	writeProbeFile(t, filepath.Join(ctx.Extension.Root, "config", "biome.json"), `{"root":false,"linter":{"enabled":true}}`)
	return ctx.WorkspaceRoot, func() {
		t.Helper()
		for _, step := range []func() error{
			func() error { return ensureWorkspaceDevDeps(ctx, jsonl.New()) },
			func() error { return ensureWorkspaceTsConfig(ctx, jsonl.New()) },
			func() error { return ensureWorkspaceBiomeConfig(ctx, jsonl.New()) },
		} {
			if err := step(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// An import whose lints stay on the repository's tool, and whose moved test
// type-checks nothing, writes no root biome.json and no root tsconfig.json:
// a tool such as ultracite picks its linter from the first configuration it
// finds, and the test reads its own configuration.
func TestAnImportWhoseLintsStayOnTheToolWritesNoRootConfig(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-biome-default",
		"a-workspace-whose-lints-stay-on-the-tool-gets-no-biome-config")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-writes-the-tsconfig-a-task-reads",
		"a-workspace-whose-jobs-read-no-tsconfig-gets-no-root-tsconfig")
	root, install := importedConfigsWorkspace(t, `{"extensions":["@putnami/turbo","@putnami/typescript"],`+
		`"disable":{"jobs":["lint","@putnami/turbo:test","@putnami/typescript:build"]},`+
		`"options":{"@putnami/typescript:test":{"types":false}}}`)
	writeProbeFile(t, filepath.Join(root, "web", "tsconfig.json"), `{"compilerOptions":{"strict":true}}`)
	install()
	for _, file := range []string{"biome.json", "tsconfig.json"} {
		if exists(t, filepath.Join(root, file)) {
			t.Errorf("the install wrote a root %s, which no job of the member reads", file)
		}
	}
}

// A member that moved only its lint, and lints with ESLint, needs the
// TypeScript parser its validate reads and no Bun types: it neither builds
// nor type-checks a test on this extension, and its package builds with the
// build it does not run. A moved test with types false needs none either.
func TestAMemberThatOnlyLintsNeedsNoBunTypes(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-adds-what-a-task-needs",
		"a-member-that-only-lints-needs-no-bun-types")
	for name, config := range map[string]string{
		"lint moved": `{"extensions":["@putnami/turbo","@putnami/typescript"],` +
			`"disable":{"jobs":["@putnami/turbo:lint","@putnami/typescript:build","@putnami/typescript:test"]}}`,
		"test moved without types": `{"extensions":["@putnami/turbo","@putnami/typescript"],` +
			`"disable":{"jobs":["@putnami/typescript:lint","@putnami/typescript:build","@putnami/turbo:test"]},` +
			`"options":{"@putnami/typescript:test":{"types":false}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, _ := devDepsWorkspace(t, config)
			if err := ensureWorkspaceDevDeps(ctx, jsonl.New()); err != nil {
				t.Fatal(err)
			}
			want := "{\n  \"name\": \"shop\",\n  \"private\": true,\n  \"devDependencies\": {\n    \"eslint\": \"^9.0.0\",\n    \"typescript\": \"^6.0.2\",\n    \"zod\": \"^3.0.0\"\n  }\n}\n"
			if got := readTextFile(t, filepath.Join(ctx.WorkspaceRoot, "package.json")); got != want {
				t.Fatalf("package.json =\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// A member that holds no tsconfig of its own and builds on this extension or
// type-checks its tests, or holds one that extends the root's, as an import
// writes it, reads the root tsconfig.json, so the install writes it. A member that lints with
// Biome gets the root biome.json.
func TestAJobThatReadsTheTSConfigGetsTheRootTSConfig(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-writes-the-tsconfig-a-task-reads",
		"a-job-that-reads-the-tsconfig-gets-the-root-tsconfig")
	cases := []struct {
		name, config, file, tsconfig string
		biome                        bool
	}{
		{name: "build moved", config: `{"extensions":["@putnami/turbo","@putnami/typescript"],"disable":{"jobs":["lint","test","@putnami/turbo:build"]}}`},
		{name: "test type-checks", config: `{"extensions":["@putnami/turbo","@putnami/typescript"],"disable":{"jobs":["lint","build","@putnami/turbo:test"]}}`},
		{
			name:     "a Biome lint moved in a project whose tsconfig extends the root's",
			config:   `{"extensions":["@putnami/turbo","@putnami/typescript"],"disable":{"jobs":["build","test","@putnami/turbo:lint"]},"options":{"@putnami/typescript":{"lint":{"linter":"biome"}}}}`,
			tsconfig: "{\n  \"extends\": \"../tsconfig.json\"\n}\n",
			biome:    true,
		},
		{
			name:     "an extends without its extension",
			config:   `{"extensions":["@putnami/turbo","@putnami/typescript"],"disable":{"jobs":["build","test","lint"]}}`,
			tsconfig: `{"extends": ["../tsconfig"]}`,
		},
		{
			name:     "a tsconfig.lib.json that extends the root's",
			config:   `{"extensions":["@putnami/turbo","@putnami/typescript"],"disable":{"jobs":["build","test","lint"]}}`,
			file:     "tsconfig.lib.json",
			tsconfig: `{"extends": "../tsconfig.json"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, install := importedConfigsWorkspace(t, c.config)
			if c.tsconfig != "" {
				file := c.file
				if file == "" {
					file = "tsconfig.json"
				}
				writeProbeFile(t, filepath.Join(root, "web", file), c.tsconfig)
			}
			install()
			if !exists(t, filepath.Join(root, "tsconfig.json")) {
				t.Error("the install wrote no root tsconfig.json, which the member reads")
			}
			if got := exists(t, filepath.Join(root, "biome.json")); got != c.biome {
				t.Errorf("root biome.json written = %v, want %v", got, c.biome)
			}
		})
	}
}

// A member that builds on this extension and type-checks its tests with its
// own tsconfig.json, which extends no root configuration, reads no root
// tsconfig.json, so the install writes none.
func TestAProjectWithItsOwnTSConfigReadsNoRootTSConfig(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-writes-the-tsconfig-a-task-reads",
		"a-project-with-its-own-tsconfig-reads-no-root-tsconfig")
	root, install := importedConfigsWorkspace(t, `{"extensions":["@putnami/turbo","@putnami/typescript"],"disable":{"jobs":["lint","@putnami/turbo:build","@putnami/turbo:test"]}}`)
	writeProbeFile(t, filepath.Join(root, "web", "tsconfig.json"), `{"extends": "@acme/tsconfig/base.json", "compilerOptions": {"strict": true}}`)
	install()
	if exists(t, filepath.Join(root, "tsconfig.json")) {
		t.Error("the install wrote a root tsconfig.json, which the member's own configuration replaces")
	}
}

// A workspace whose only TypeScript project is its root, as a single-package
// repository, has no member to read jobs from: it gets the root tsconfig.json
// and biome.json an unknown membership gets.
func TestARootOnlyWorkspaceGetsTheRootConfigs(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-writes-the-tsconfig-a-task-reads",
		"a-root-only-workspace-gets-the-root-configs")
	ctx, _ := devDepsWorkspace(t, "")
	ctx.WorkspaceProjects = ctx.WorkspaceProjects[:1]
	writeProbeFile(t, filepath.Join(ctx.Extension.Root, "config", "tsconfig.json"), `{"compilerOptions":{"strict":true}}`)
	writeProbeFile(t, filepath.Join(ctx.Extension.Root, "config", "biome.json"), `{"root":false}`)
	if err := ensureWorkspaceTsConfig(ctx, jsonl.New()); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkspaceBiomeConfig(ctx, jsonl.New()); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"tsconfig.json", "biome.json"} {
		if !exists(t, filepath.Join(ctx.WorkspaceRoot, file)) {
			t.Errorf("the install wrote no root %s", file)
		}
	}
}
