package orchestration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/fileindex"
	"go.putnami.dev/tooling/cli/internal/store"
)

// extensionCase is the fixture of one extension built here: a workspace that
// loads it from this tree, with one small project it serves, and the counts
// that workspace holds.
type extensionCase struct {
	// name is the extension, as a subtest names it.
	name string
	// extensions are the extensions built here whose processes the case's
	// ledgers hold, by name.
	extensions []string
	// links are the directories of this tree the workspace links, by their
	// path in the workspace: the extensions it loads under .ext, which the
	// project scan never enters, and the packages an install would put in
	// node_modules.
	links map[string]string
	// copies are the files of this tree the workspace holds, by their path
	// in the workspace.
	copies map[string]string
	// tools are the programs the case runs that must be on PATH.
	tools []string
	// requires are the paths of this tree the case reads that an install
	// writes, such as a package's node_modules.
	requires []string
	// provisioned returns the executables of the tools the case's extensions
	// run that a run would otherwise download: on PATH or under the machine's
	// Putnami home. Their directories go first on the case's PATH.
	provisioned func(t *testing.T) []string
	// toolchains are the pins this repository's lock gives the workspace.
	toolchains []string
	// files are the workspace's files, putnami.workspace.json included.
	files map[string]string
	// setup are the CLI invocations that prepare the workspace before any run.
	setup [][]string
	// command is the gate the case runs, on one worker and from no task
	// history (clitest.ForgetTaskHistory), so its tasks run in the same order
	// from one run to the next.
	command []string
	// cold is the ledger of a run with --no-cache, every task executed.
	cold string
	// plan is the ledger of planning the unchanged workspace, the same
	// command with --dry-run, which executes nothing.
	plan string
	// was is the cold ledger the change before the last one to move it
	// recorded: cold holds the counts rule against it (checkCountsRule).
	was string
	// settles is whether the case holds that a run after a run of the
	// unchanged workspace executes no task but everyRun.
	settles bool
	// everyRun are the keys of the tasks that execute on every run of the
	// unchanged workspace, each named with the reason it is never served from
	// the cache.
	everyRun []string
}

// registries are the registries of this repository's workspace, so a job
// resolves the credential of each host it reaches as it does here.
const registries = `{"npm": {"scopes": {"@putnami": "https://npm.putnami.dev"}},` +
	` "go": {"origin": "https://go.putnami.dev", "proxy": ["https://proxy.golang.org", "direct"]}}`

var goFiles = map[string]string{
	".gitignore": ".putnami\n.gen\n",
	"putnami.workspace.json": `{"name": "counts", "extensions": ["/.ext/go", "/.ext/dev"], "includes": ["lib"],` +
		` "registries": ` + registries + `}` + "\n",
	"lib/putnami.json": `{"name": "lib", "extensions": ["@putnami/go"]}` + "\n",
	"lib/go.mod":       "module example.com/lib\n\ngo 1.26\n",
	"lib/lib.go":       "// Package lib adds.\npackage lib\n\n// Add returns a plus b.\nfunc Add(a, b int) int { return a + b }\n",
	"lib/lib_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n" +
		"\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"1 + 2 != 3\")\n\t}\n}\n",
}

var typescriptFiles = map[string]string{
	".gitignore":   ".putnami\n.gen\nnode_modules\ndist\n*.tsbuildinfo\n",
	"package.json": `{"name": "counts", "private": true, "packageManager": "bun@1.4.5", "workspaces": ["lib"]}` + "\n",
	"putnami.workspace.json": `{"name": "counts", "extensions": ["/.ext/typescript", "/.ext/dev"], "includes": ["lib"],` +
		` "registries": ` + registries + `}` + "\n",
	// The project's files are written as its lint formats them, so the first
	// run rewrites no source and the next run finds the tree it left.
	"lib/putnami.json": `{ "name": "lib", "extensions": ["@putnami/typescript"] }` + "\n",
	"lib/package.json": "{\n\t\"name\": \"lib\",\n\t\"type\": \"module\",\n\t\"main\": \"dist/index.js\",\n" +
		"\t\"types\": \"dist/index.d.ts\",\n\t\"scripts\": {\n\t\t\"build\": \"tsc -b\"\n\t}\n}\n",
	"lib/tsconfig.json": "{\n\t\"compilerOptions\": {\n\t\t\"composite\": true,\n\t\t\"rootDir\": \"src\",\n" +
		"\t\t\"outDir\": \"dist\",\n\t\t\"module\": \"nodenext\",\n\t\t\"target\": \"es2022\",\n" +
		"\t\t\"strict\": true,\n\t\t\"types\": []\n\t},\n\t\"include\": [\"src\"]\n}\n",
	"lib/src/index.ts": "export const add = (a: number, b: number): number => a + b;\n",
}

var sddFiles = map[string]string{
	".gitignore":             ".putnami\n.gen\n",
	"putnami.workspace.json": `{"name": "counts", "extensions": ["/.ext/sdd"], "includes": ["lib"]}` + "\n",
	"lib/putnami.json":       `{"name": "lib", "extensions": ["@putnami/sdd"]}` + "\n",
	"lib/putnami.features.json": `{"protocolVersion": 2, "namespace": "counts", "features": [{"id": "counts/add",` +
		` "type": "feature", "name": "Add", "outcome": "Two numbers add", "owner": "counts", "target": "coded",` +
		` "requirements": [{"id": "adds", "stage": "coded", "evidenceKinds": ["attestation"]}]}]}` + "\n",
}

var rustFiles = map[string]string{
	".gitignore":               ".putnami\ntarget\n",
	"putnami.workspace.json":   `{"name": "counts", "extensions": ["/.ext/rust", "/.ext/dev"], "includes": ["lib"]}` + "\n",
	"lib/putnami.json":         `{"name": "lib", "extensions": ["@putnami/rust"]}` + "\n",
	"lib/.config/nextest.toml": "[profile.ci.junit]\npath = \"junit.xml\"\n",
	"lib/Cargo.toml":           "[package]\nname = \"lib\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
	"lib/Cargo.lock":           "# This file is automatically @generated by Cargo.\n# It is not intended for manual editing.\nversion = 4\n\n[[package]]\nname = \"lib\"\nversion = \"0.1.0\"\n",
	"lib/src/lib.rs": "/// Returns a plus b.\npub fn add(a: i64, b: i64) -> i64 {\n    a + b\n}\n\n#[cfg(test)]\nmod tests {\n" +
		"    #[test]\n    fn adds() {\n        assert_eq!(super::add(1, 2), 3);\n    }\n}\n",
}

// clientgenFiles is a provider whose OpenAPI contract generates a TypeScript
// client, as in the client generation cache test.
var clientgenFiles = map[string]string{
	".gitignore":             "node_modules\n.putnami\n.gen\nclients/ts/src\n",
	"biome.json":             `{"root": true, "formatter": {"indentStyle": "space", "indentWidth": 2, "lineWidth": 120}}` + "\n",
	"package.json":           `{"name": "counts", "private": true, "packageManager": "bun@1.4.5", "workspaces": ["clients/ts"]}` + "\n",
	"putnami.workspace.json": `{"name": "counts", "extensions": ["/.ext/typescript", "/.ext/dev", "/.ext/clientgen"], "includes": ["provider", "clients/ts"]}` + "\n",
	"provider/putnami.json":  `{"name": "provider", "extensions": ["@putnami/typescript", "@putnami/clientgen"]}` + "\n",
	"provider/package.json":  `{"name": "provider", "private": true}` + "\n",
	"clients/ts/putnami.json": `{"name": "client", "type": "library", "extensions": ["@putnami/typescript", "@putnami/clientgen"],` +
		` "generatedFrom": {"service": "catalog.items", "language": "ts"}}` + "\n",
	"clients/ts/package.json": `{"name": "@example/items-client", "version": "0.0.0", "main": "src/index.ts",` +
		` "exports": {".": "./src/index.ts"}, "dependencies": {"@putnami/client": "workspace:*"}}` + "\n",
}

var extensionCases = []extensionCase{
	{
		name:        "go",
		extensions:  []string{"@putnami/go"},
		provisioned: goLintTools,
		links:       map[string]string{".ext/go": "go/extension", ".ext/dev": "tooling/dev"},
		tools:       []string{"go"},
		toolchains:  []string{"go"},
		files:       goFiles,
		setup:       [][]string{{"projects", "sync"}},
		command:     []string{"lint,test,build", "--all", "--max-parallel", "1"},
		cold:        "engine job 9 · cli 1 · git 17 | engine git check-ignore 1 · for-each-ref 1 · ls-files 5 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		was:         "engine job 9 · cli 1 · git 18 | engine git check-ignore 1 · diff 1 · for-each-ref 1 · ls-files 5 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		plan:        "engine git 8 | engine git for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		settles:     true,
	},
	{
		name:        "typescript",
		extensions:  []string{"@putnami/typescript"},
		provisioned: pinnedBun,
		links: map[string]string{
			".ext/typescript": "typescript/extension", ".ext/dev": "tooling/dev",
			"node_modules/typescript": "node_modules/typescript", "node_modules/@biomejs/biome": "node_modules/@biomejs/biome",
		},
		toolchains: []string{"bun", "go"},
		files:      typescriptFiles,
		setup:      [][]string{{"projects", "sync"}},
		command:    []string{"lint,test,build", "--all", "--max-parallel", "1"},
		cold:       "engine job 8 · control 1 · git 24 | engine git check-ignore 1 · for-each-ref 1 · ls-files 11 · rev-parse 6 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs git 1 | jobs git check-ignore 1",
		was:        "engine job 8 · control 1 · git 25 | engine git check-ignore 1 · diff 1 · for-each-ref 1 · ls-files 11 · rev-parse 6 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs git 1 | jobs git check-ignore 1",
		plan:       "engine git 8 | engine git for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		settles:    true,
	},
	{
		name:       "sdd",
		extensions: []string{"@putnami/sdd"},
		links:      map[string]string{".ext/sdd": "tooling/sdd-extension"},
		toolchains: []string{"go"},
		files:      sddFiles,
		setup:      [][]string{{"projects", "sync"}},
		command:    []string{"validate", "--all", "--max-parallel", "1"},
		cold:       "engine job 13 · git 23 | engine git check-ignore 1 · for-each-ref 1 · ls-files 9 · rev-parse 7 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		was:        "engine job 13 · git 24 | engine git check-ignore 1 · diff 1 · for-each-ref 1 · ls-files 9 · rev-parse 7 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		plan:       "engine git 8 | engine git for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		settles:    true,
		// The squash-message checks read PUTNAMI_PROPOSAL_MESSAGE and Git
		// history, which no key holds (sdd ADR 0002 point 7, protocols/job ADR
		// 0003 point 4), so they are uncached.
		everyRun: []string{"counts:validate-workspace~agent-attribution", "counts:validate-workspace~breaking-marker",
			"counts:validate-workspace~feature-coverage-ratchet"},
	},
	{
		name:        "clientgen",
		extensions:  []string{"@putnami/clientgen"},
		provisioned: pinnedBun,
		links: map[string]string{
			".ext/typescript": "typescript/extension", ".ext/dev": "tooling/dev", ".ext/clientgen": "tooling/clientgen-extension",
			"clients/ts/node_modules/@putnami/client":              "typescript/framework/client",
			"clients/ts/node_modules/.bin/putnami-client-generate": "typescript/framework/client/bin/generate.ts",
		},
		requires:   []string{"typescript/framework/client/node_modules"},
		copies:     map[string]string{"provider/schema/openapi.json": "typescript/samples/10-service-to-service/schema/openapi.json"},
		toolchains: []string{"bun", "go"},
		files:      clientgenFiles,
		setup:      [][]string{{"projects", "sync"}},
		command:    []string{"client-generate", "--projects", "client", "--max-parallel", "1"},
		cold:       "engine job 1 · control 1 · git 16 | engine git diff 1 · for-each-ref 1 · ls-files 4 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		was:        "engine job 1 · control 1 · git 16 | engine git diff 1 · for-each-ref 1 · ls-files 4 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		plan:       "engine git 9 | engine git diff 1 · for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		settles:    true,
	},
	{
		name:        "rust",
		extensions:  []string{"@putnami/rust"},
		provisioned: rustTestTools,
		links:       map[string]string{".ext/rust": "rust/extension", ".ext/dev": "tooling/dev"},
		tools:       []string{"cargo"},
		toolchains:  []string{"go"},
		files:       rustFiles,
		setup:       [][]string{{"projects", "sync"}},
		command:     []string{"lint,test,build", "--all", "--max-parallel", "1"},
		cold:        "engine job 4 · git 17 | engine git check-ignore 1 · diff 1 · for-each-ref 1 · ls-files 4 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		was:         "engine job 4 · git 18 | engine git diff 1 · for-each-ref 1 · ls-files 5 · rev-parse 6 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs git 1 | jobs git ls-files 1",
		plan:        "engine git 9 | engine git diff 1 · for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
	},
	{
		name:       "shell",
		extensions: []string{"sample/shell-extension"},
		links:      map[string]string{".ext/shell": "tooling/samples/shell-extension"},
		tools:      []string{"bash", "make"},
		files:      shellFiles,
		setup:      [][]string{{"projects", "sync"}},
		command:    []string{"test", "--all", "--max-parallel", "1"},
		cold:       "engine job 2 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		was:        "engine job 2 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		plan:       "engine git 9 | engine git diff 1 · for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
	},
	{
		name:        "web",
		extensions:  []string{"@putnami/application", "@putnami/web"},
		provisioned: pinnedBun,
		links: map[string]string{
			".ext/typescript": "typescript/extension", ".ext/dev": "tooling/dev",
			".ext/application": "typescript/framework/application", ".ext/web": "typescript/framework/web",
			"node_modules/typescript": "node_modules/typescript", "node_modules/@biomejs/biome": "node_modules/@biomejs/biome",
		},
		requires:   []string{"typescript/framework/application/node_modules", "typescript/framework/web/node_modules"},
		toolchains: []string{"bun", "go"},
		files:      webFiles,
		setup:      [][]string{{"projects", "sync"}},
		command:    []string{"build", "--all", "--max-parallel", "1"},
		cold:       "engine job 5 · control 1 · git 20 | engine git for-each-ref 1 · ls-files 8 · rev-parse 6 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs git 1 | jobs git check-ignore 1",
		was:        "engine job 5 · control 1 · git 20 | engine git for-each-ref 1 · ls-files 8 · rev-parse 6 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs git 1 | jobs git check-ignore 1",
		plan:       "engine git 8 | engine git for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
	},
	{
		name:       "cloud",
		extensions: []string{"@putnami/cloud"},
		links:      map[string]string{".ext/cloud": "cloud/extension"},
		toolchains: []string{"go"},
		files:      cloudFiles,
		setup:      [][]string{{"projects", "sync"}},
		command:    []string{"validate", "--all", "--max-parallel", "1"},
		cold:       "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		was:        "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		plan:       "engine git 9 | engine git diff 1 · for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
	},
	{
		name:       "bazel",
		extensions: []string{"@putnami/bazel"},
		links:      map[string]string{".ext/bazel": "integrations/bazel", ".ext/dev": "tooling/dev"},
		toolchains: []string{"go"},
		files:      bazelFiles,
		setup:      [][]string{{"projects", "sync"}},
		command:    []string{"validate", "--all", "--max-parallel", "1"},
		cold:       "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		was:        "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		plan:       "engine git 9 | engine git diff 1 · for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
	},
	{
		name:       "pants",
		extensions: []string{"@putnami/pants"},
		links:      map[string]string{".ext/pants": "integrations/pants", ".ext/dev": "tooling/dev"},
		toolchains: []string{"go"},
		files:      pantsFiles,
		setup:      [][]string{{"projects", "sync"}},
		command:    []string{"validate", "--all", "--max-parallel", "1"},
		cold:       "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		was:        "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		plan:       "engine git 9 | engine git diff 1 · for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
	},
	{
		name:       "moon",
		extensions: []string{"@putnami/moon"},
		links:      map[string]string{".ext/moon": "integrations/moon", ".ext/dev": "tooling/dev"},
		toolchains: []string{"go"},
		files:      moonFiles,
		setup:      [][]string{{"projects", "sync"}},
		command:    []string{"validate", "--all", "--max-parallel", "1"},
		cold:       "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		was:        "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		plan:       "engine git 9 | engine git diff 1 · for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
	},
	{
		name:       "turbo",
		extensions: []string{"@putnami/turbo"},
		links:      map[string]string{".ext/turbo": "integrations/turbo", ".ext/dev": "tooling/dev"},
		toolchains: []string{"go"},
		files:      turboFiles,
		setup:      [][]string{{"projects", "sync"}},
		command:    []string{"validate", "--all", "--max-parallel", "1"},
		cold:       "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		was:        "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		plan:       "engine git 9 | engine git diff 1 · for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
	},
	{
		name:       "nx",
		extensions: []string{"@putnami/nx"},
		links:      map[string]string{".ext/nx": "integrations/nx", ".ext/dev": "tooling/dev"},
		toolchains: []string{"go"},
		files:      nxFiles,
		setup:      [][]string{{"projects", "sync"}},
		command:    []string{"validate", "--all", "--max-parallel", "1"},
		cold:       "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		was:        "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		plan:       "engine git 9 | engine git diff 1 · for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
	},
	{
		name:       "rush",
		extensions: []string{"@putnami/rush"},
		links:      map[string]string{".ext/rush": "integrations/rush", ".ext/dev": "tooling/dev"},
		toolchains: []string{"go"},
		files:      rushFiles,
		setup:      [][]string{{"projects", "sync"}},
		command:    []string{"validate", "--all", "--max-parallel", "1"},
		cold:       "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		was:        "engine job 1 · git 15 | engine git diff 1 · for-each-ref 1 · ls-files 3 · rev-parse 5 · show 1 · status 2 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
		plan:       "engine git 9 | engine git diff 1 · for-each-ref 1 · rev-parse 3 · show 1 · status 1 · symbolic-ref 1 · tag 1 | jobs none | jobs git none",
	},
}

// TestTheExtensionCounts holds, for each extension built here, what a small
// workspace it serves starts: the job and control processes of the engine,
// the git calls of the engine and of the jobs, and the CLI starts of the jobs,
// in a run that executes every task, and in planning the unchanged workspace.
func TestTheExtensionCounts(t *testing.T) {
	clitest.RequireShell(t)
	// The cases share one Putnami home, so its copies are made once.
	home := t.TempDir()
	held := 0
	for _, tc := range extensionCases {
		t.Run(tc.name, func(t *testing.T) {
			for name, value := range clitest.OuterRunEnvironment {
				t.Setenv(name, value)
			}
			clitest.UnsetHostRunEnvironment(t)
			tools := requireOffline(t, tc)
			t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
			// The engine probes each candidate of a pinned toolchain until one
			// answers with the pinned release: the pinned Go through GOROOT and
			// the case's pinned tools first on PATH make the first candidate
			// answer, whatever else the host's PATH holds. They stay where the
			// machine keeps them, outside the cases' Putnami home: each names its
			// own executable through its probe's self, so it answers from a
			// record once probed.
			goroot := goRoot(t)
			requireRecordable(t, filepath.Join(goroot, "bin", "go"))
			for _, tool := range tools {
				if filepath.Base(tool) == "bun" {
					requireRecordable(t, tool)
				}
			}
			putnamiHome(t, home)
			t.Setenv("GOROOT", goroot)
			dirs := []string{filepath.Join(goroot, "bin")}
			for _, tool := range tools {
				dirs = append(dirs, filepath.Dir(tool))
			}
			t.Setenv("PATH", strings.Join(append(dirs, os.Getenv("PATH")), string(os.PathListSeparator)))
			t.Setenv("PUTNAMI_STORE_DIR", t.TempDir())
			root := extensionWorkspace(t, tc)
			for _, args := range tc.setup {
				if code, out := clitest.RunGateArgs(t, root, args...); code != 0 {
					t.Fatalf("putnami %v exit %d:\n%s", args, code, out)
				}
			}
			// An install writes the lock again, without the pins of the
			// runtimes the workspace compiles from this tree.
			clitest.PinToolchains(t, repoRoot, root, tc.toolchains...)
			// The first run prepares the extension runtimes, so the runs counted
			// build none. It runs on every worker, in the order its tasks take
			// there.
			if code, out := clitest.RunGateArgs(t, root, everyWorker(tc.command)...); code != 0 {
				t.Fatalf("putnami %v exit %d:\n%s", tc.command, code, out)
			}
			clitest.ForgetTaskHistory(t, root)
			cold := clitest.RunCounts(t, root, append(tc.command, "--no-cache")...)
			clitest.ForgetTaskHistory(t, root)
			plan := clitest.PlanCounts(t, root, tc.command...)
			t.Logf("cold %d tasks: %s", cold.Tasks, cold.Ledger())
			t.Logf("plan: %s", plan.Ledger())
			// A run of the unchanged workspace after those is served from the
			// cache the first run filled on every worker: it executes no task
			// but those that run every time. It comes last, so the ledgers above
			// count what they counted before it.
			if tc.settles {
				executed, ended := clitest.ExecutedTasks(t, root, tc.command...)
				if ended != cold.Tasks {
					t.Errorf("a run after a run of the unchanged workspace ended %d tasks, want the %d it selects", ended, cold.Tasks)
				}
				if !slices.Equal(sorted(executed), sorted(tc.everyRun)) {
					t.Errorf("a run after a run of the unchanged workspace executed %v, want only %v", executed, tc.everyRun)
				}
			}
			for _, check := range []struct{ name, got, want string }{
				{"a run with every task executed", cold.Ledger(), tc.cold},
				{"planning the unchanged workspace", plan.Ledger(), tc.plan},
			} {
				if check.got != check.want {
					t.Errorf("%s:\n got  %s\n want %s\n%s", check.name, check.got, check.want, countsHint)
				}
			}
			if plan.Ledger() == tc.plan {
				held++
			}
			if tc.name == "go" {
				unchangedRunReadsNoSource(t, root, tc.command)
			}
		})
	}
	// Each extension's plan holds its git calls alone: the handshakes, the
	// toolchain probes and the workspace probe are read from what the runs
	// before recorded. The proof needs a case that ran.
	if held > 0 {
		spectest.Proves(t, "cli/job-planning-execution", "unchanged-planning-reads-records",
			"planning-an-unchanged-workspace-starts-only-git")
	}
}

// everyWorker is command without its --max-parallel limit.
func everyWorker(command []string) []string {
	out := make([]string, 0, len(command))
	for i := 0; i < len(command); i++ {
		if command[i] == "--max-parallel" {
			i++
			continue
		}
		out = append(out, command[i])
	}
	return out
}

// sorted is a sorted copy of keys.
func sorted(keys []string) []string {
	out := slices.Clone(keys)
	slices.Sort(out)
	return out
}

// unchangedRunReadsNoSource holds that a run of the unchanged workspace reads
// no source file: every digest of one, the basis digests of the go-embed
// answers it reads from its records included, is answered from the file
// index. The files the runs before wrote are recorded once they are older than
// the index's margin. What it reads is a build stamp each run writes again.
func unchangedRunReadsNoSource(t *testing.T, root string, command []string) {
	t.Helper()
	time.Sleep(fileindex.Margin + 100*time.Millisecond)
	clitest.RunCounts(t, root, command...)
	time.Sleep(fileindex.Margin + 100*time.Millisecond)
	var mu sync.Mutex
	var read []string
	restore := fileindex.ObserveReads(func(path, _ string) {
		mu.Lock()
		defer mu.Unlock()
		read = append(read, path)
	})
	clitest.RunCounts(t, root, command...)
	restore()
	for _, path := range read {
		rel, err := filepath.Rel(root, path)
		if err != nil || strings.HasPrefix(rel, "..") || !strings.Contains(filepath.ToSlash(rel), ".gen/") {
			t.Errorf("a run of the unchanged workspace read %s, want no file but the build stamps it writes again\n%s", path, countsHint)
		}
	}
}

// requireOffline skips the case when a tool it runs is not on PATH, when a
// path of this tree it reads is not installed, or when a tool its extensions
// provision is not on the machine yet: provisioning it would download it, and
// these tests start nothing that leaves the host.
func requireOffline(t *testing.T, tc extensionCase) (tools []string) {
	t.Helper()
	for _, tool := range append([]string{"git"}, tc.tools...) {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("the %s case runs %s, which is not on PATH: %v", tc.name, tool, err)
		}
	}
	for _, rel := range tc.requires {
		if _, err := os.Stat(filepath.Join(repoRoot, rel)); err != nil {
			t.Skipf("the %s case reads %s, which is not installed in this tree: %v", tc.name, rel, err)
		}
	}
	if tc.provisioned == nil {
		return nil
	}
	tools = tc.provisioned(t)
	for _, path := range tools {
		if _, err := os.Stat(path); err != nil {
			t.Skipf("the %s case runs %s, which is not on this machine; provisioning it would download it: %v", tc.name, path, err)
		}
	}
	return tools
}

// requireRecordable skips the case unless the toolchain executable at path is
// one the engine records once it named itself: a native file with one link.
// A host whose Go or Bun is a script or a hard link probes it on each run, so
// the counts held here do not describe it.
func requireRecordable(t *testing.T, path string) {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Skipf("the case's toolchain %s does not resolve: %v", path, err)
	}
	identity, ok := store.StatFileIdentity(real)
	if !ok || identity.Links != 1 {
		t.Skipf("the case's toolchain %s has no file identity or several links, so it is probed on each run", real)
	}
	file, err := os.Open(real)
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = file.Close() }()
	head := make([]byte, 2)
	if _, err := file.Read(head); err == nil && string(head) == "#!" {
		t.Skipf("the case's toolchain %s is a script, so it is probed on each run", real)
	}
}

// putnamiHome sets PUTNAMI_HOME to home, a Putnami home whose tools are the
// machine's and whose toolchains are empty, and points the artifact store, and
// the records a run keeps there, at home too. A home a case already filled is
// reused.
func putnamiHome(t *testing.T, home string) {
	t.Helper()
	if machineTools := toolsRoot(t); machineTools != "" {
		if _, err := os.Stat(machineTools); err == nil {
			linkOnce(t, machineTools, filepath.Join(home, "tools"))
		}
	}
	t.Setenv("PUTNAMI_HOME", home)
	t.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(home, "artifacts"))
}

// linkOnce links path to target unless path exists.
func linkOnce(t *testing.T, target, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// toolsRoot is the root of the tools extensions provision on this machine.
func toolsRoot(t *testing.T) string {
	t.Helper()
	if home := os.Getenv("PUTNAMI_HOME"); home != "" {
		return filepath.Join(home, "tools")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	return filepath.Join(home, ".putnami", "tools")
}

// pinnedBun is the bun executable of the release this repository pins: the
// bun on PATH when it is that release, or the one provisioned under the
// machine's Putnami home.
func pinnedBun(t *testing.T) []string {
	t.Helper()
	var lock struct {
		Toolchains map[string]struct {
			Version  string
			Archives map[string]struct{ Executable string }
		} `json:"toolchains"`
	}
	readJSON(t, "putnami.lock.json", &lock)
	bun := lock.Toolchains["bun"]
	if path, err := exec.LookPath("bun"); err == nil {
		if out, err := exec.Command(path, "--version").Output(); err == nil && strings.TrimSpace(string(out)) == bun.Version {
			return []string{path}
		}
	}
	archive, ok := bun.Archives[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		t.Skipf("this repository pins no Bun for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return []string{filepath.Join(filepath.Dir(toolsRoot(t)), "toolchains", "bun", "bun-"+bun.Version, archive.Executable)}
}

// goLintTools are the lint tools @putnami/go builds with the Go release this
// repository pins, at the versions go/extension/tools/versions.json names.
func goLintTools(t *testing.T) []string {
	t.Helper()
	var versions struct {
		Tools map[string]struct{ Version string } `json:"tools"`
	}
	var lock struct {
		Toolchains map[string]struct{ Version string } `json:"toolchains"`
	}
	readJSON(t, "go/extension/tools/versions.json", &versions)
	readJSON(t, "putnami.lock.json", &lock)
	minor := lock.Toolchains["go"].Version
	if parts := strings.SplitN(minor, ".", 3); len(parts) == 3 {
		minor = parts[0] + "." + parts[1]
	}
	var paths []string
	for name, tool := range versions.Tools {
		paths = append(paths, filepath.Join(toolsRoot(t), "go", name, tool.Version, "go"+minor,
			runtime.GOOS+"-"+runtime.GOARCH, name))
	}
	return paths
}

// rustTestTools are the test tools of @putnami/rust: those on PATH, which it
// prefers, or those it provisions at the versions rust/extension/provide.go
// pins.
func rustTestTools(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "rust/extension/provide.go"))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, tool := range []struct{ name, constant string }{{"cargo-nextest", "nextestVersion"}, {"cargo-llvm-cov", "llvmCovVersion"}} {
		if path, err := exec.LookPath(tool.name); err == nil {
			paths = append(paths, path)
			continue
		}
		m := regexp.MustCompile(tool.constant + `\s*=\s*"([^"]+)"`).FindSubmatch(data)
		if m == nil {
			t.Fatalf("rust/extension/provide.go declares no %s", tool.constant)
		}
		paths = append(paths, filepath.Join(toolsRoot(t), "rust", tool.name+"-"+string(m[1]), tool.name))
	}
	return paths
}

func readJSON(t *testing.T, rel string, into any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
}

// goRoot is the root of the Go toolchain this repository builds with.
func goRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("go", "env", "GOROOT")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go env GOROOT: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// extensionWorkspace writes the case's files under a new Git work tree, links
// its extensions from this tree and pins its toolchains.
func extensionWorkspace(t *testing.T, tc extensionCase) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range tc.files {
		clitest.WriteFile(t, filepath.Join(root, rel), content)
	}
	for rel, source := range tc.copies {
		data, err := os.ReadFile(filepath.Join(repoRoot, source))
		if err != nil {
			t.Fatal(err)
		}
		clitest.WriteFile(t, filepath.Join(root, rel), string(data))
	}
	for link, rel := range tc.links {
		target, err := filepath.EvalSymlinks(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Skipf("%s is not installed in this tree: %v", rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, link)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	clitest.PinToolchains(t, repoRoot, root, tc.toolchains...)
	clitest.InitGitRepo(t, root)
	return root
}
