package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// writeTSXLibrary lays out a React component library the way a published
// TSX package looks: a default and a browser export, react as a dependency,
// a bunfig.toml that only configures tests, and a module that reads NODE_ENV.
func writeTSXLibrary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"package.json": `{
			"name": "tsx-lib",
			"dependencies": {"react": "^19.0.0"},
			"exports": {".": {"browser": "./src/index.browser.tsx", "default": "./src/index.tsx"}}
		}`,
		"tsconfig.json":         `{"compilerOptions": {"jsx": "react-jsx"}}`,
		"bunfig.toml":           "[test]\npreload = [\"./test/setup.ts\"]\n",
		"src/icon.tsx":          "export const Icon = () => <svg width={16}><path d=\"M0 0\" /></svg>;\n",
		"src/mode.ts":           "export const isDevelopment = () => process.env.NODE_ENV !== 'production';\n",
		"src/index.tsx":         "export { Icon } from './icon';\nexport { isDevelopment } from './mode';\n",
		"src/index.browser.tsx": "export { Icon } from './icon';\nexport { isDevelopment } from './mode';\n",
		"test/setup.ts":         "export {};\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestRunTranspile_TSXLibraryEmitsProductionJSXRuntime builds a TSX library
// with the real bun under a development NODE_ENV and a bunfig.toml, the two
// conditions under which bun otherwise emits jsxDEV. Every emitted module
// must import the production automatic runtime and keep NODE_ENV a runtime
// read.
func TestRunTranspile_TSXLibraryEmitsProductionJSXRuntime(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "environment-independent-library-output", "a-tsx-library-built-under-a-development-environment-imports-the-production-jsx-runtime")
	bunBin, err := toolchain.ResolveBun()
	if err != nil {
		t.Skip("bun is not installed")
	}
	t.Setenv("NODE_ENV", "development")

	dir := writeTSXLibrary(t)
	outDir := filepath.Join(dir, "lib")
	files, errors, err := RunTranspile(bunBin, dir, outDir, TranspileParams{Target: "bun", Minify: true, Splitting: true})
	if err != nil {
		t.Fatalf("RunTranspile: %v", err)
	}
	if len(errors) != 0 {
		t.Fatalf("RunTranspile reported errors: %v", errors)
	}

	var entries []string
	for _, file := range files {
		if strings.HasSuffix(file, ".js") {
			entries = append(entries, file)
		}
	}
	if len(entries) == 0 {
		t.Fatalf("no JavaScript emitted: %v", files)
	}

	sawRuntime := false
	sawEnvRead := false
	for _, file := range entries {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		rel, _ := filepath.Rel(outDir, file)
		if strings.Contains(out, "jsx-dev-runtime") || strings.Contains(out, "jsxDEV") {
			t.Errorf("%s uses the development JSX runtime:\n%s", rel, out)
		}
		if strings.Contains(out, "react/jsx-runtime") {
			sawRuntime = true
		}
		if strings.Contains(out, "process.env.NODE_ENV") {
			sawEnvRead = true
		}
	}
	if !sawRuntime {
		t.Errorf("no emitted module imports react/jsx-runtime: %v", entries)
	}
	if !sawEnvRead {
		t.Errorf("NODE_ENV was inlined at build time; no emitted module reads process.env.NODE_ENV: %v", entries)
	}
}

func TestBuildTranspileArgs_DisablesBuilderEnvironment(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "environment-independent-library-output", "the-transpile-step-never-inlines-the-builder-environment")
	for _, target := range []string{"bun", "node", "browser"} {
		args := buildTranspileArgs("/proj", "/out", TranspileParams{Target: target}, []string{"src/index.tsx"}, nil, "")
		if !containsArg(args, "--env=disable") {
			t.Errorf("target %s: expected --env=disable, got %v", target, args)
		}
	}
}
