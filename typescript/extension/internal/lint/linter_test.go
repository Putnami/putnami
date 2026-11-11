package lint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const toolchainFeature = "typescript/typescript-project-toolchain"

func writeProjectFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestChooseLinter_TheProjectOptionChooses(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "lint-chooses-the-linter", "the-project-option-chooses-the-linter")
	project := t.TempDir()
	writeProjectFile(t, project, "package.json", `{"scripts": {"lint": "biome check ."}}`)

	option, err := ReadLinterOption(json.RawMessage(`{"linter": "eslint"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ChooseLinter(option, project); err != nil || got != LinterESLint {
		t.Errorf("ChooseLinter(eslint option, biome script) = %q, %v, want eslint", got, err)
	}

	writeProjectFile(t, project, "package.json", `{"scripts": {"lint": "eslint ."}}`)
	if got, err := ChooseLinter(LinterBiome, project); err != nil || got != LinterBiome {
		t.Errorf("ChooseLinter(biome option, eslint script) = %q, %v, want biome", got, err)
	}
}

func TestChooseLinter_AnESLintLintScriptChoosesESLint(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "lint-chooses-the-linter", "an-eslint-lint-script-chooses-eslint")
	for _, script := range []string{
		"eslint .",
		"eslint",
		"npx eslint src --max-warnings 0",
		"bunx eslint .",
		"pnpm exec eslint .",
		"yarn eslint .",
		"node_modules/.bin/eslint .",
		"eslint . && prettier --check .",
	} {
		project := t.TempDir()
		writeProjectFile(t, project, "package.json", `{"scripts": {"lint": `+quote(script)+`}}`)
		if got, err := ChooseLinter("", project); err != nil || got != LinterESLint {
			t.Errorf("lint script %q: ChooseLinter() = %q, %v, want eslint", script, got, err)
		}
	}
	for _, script := range []string{"biome check .", "bunx @biomejs/biome lint", "tsc --noEmit", "prettier --check . && eslint .", "echo eslint"} {
		project := t.TempDir()
		writeProjectFile(t, project, "package.json", `{"scripts": {"lint": `+quote(script)+`}}`)
		if got, err := ChooseLinter("", project); err != nil || got != LinterBiome {
			t.Errorf("lint script %q: ChooseLinter() = %q, %v, want biome", script, got, err)
		}
	}
}

// A workspace install writes a root biome.json whatever the projects lint
// with, so neither Biome's nor ESLint's configuration file chooses.
func TestChooseLinter_AConfigurationFileAloneNeverChooses(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "lint-chooses-the-linter", "a-configuration-file-alone-never-chooses")
	project := t.TempDir()
	writeProjectFile(t, project, "eslint.config.mjs", "export default [];\n")
	writeProjectFile(t, project, ".eslintrc.json", "{}\n")
	writeProjectFile(t, project, "package.json", `{"name": "app"}`)
	if got, err := ChooseLinter("", project); err != nil || got != LinterBiome {
		t.Errorf("ChooseLinter() with ESLint files and no option or script = %q, %v, want biome", got, err)
	}

	writeProjectFile(t, project, "biome.json", "{}\n")
	writeProjectFile(t, project, "package.json", `{"scripts": {"lint": "eslint ."}}`)
	if got, err := ChooseLinter("", project); err != nil || got != LinterESLint {
		t.Errorf("ChooseLinter() with biome.json and an eslint script = %q, %v, want eslint", got, err)
	}
}

func TestChooseLinter_AnUnknownLinterFailsLint(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "lint-chooses-the-linter", "an-unknown-linter-fails-lint")
	project := t.TempDir()
	if _, err := ChooseLinter("oxlint", project); err == nil || !strings.Contains(err.Error(), CodeLinterUnknown) || !strings.Contains(err.Error(), "oxlint") {
		t.Errorf("ChooseLinter(oxlint) error = %v, want %s naming the value", err, CodeLinterUnknown)
	}
	if _, err := ReadLinterOption(json.RawMessage(`"eslint"`)); err == nil || !strings.Contains(err.Error(), CodeLinterUnknown) {
		t.Errorf("ReadLinterOption(a string) error = %v, want %s", err, CodeLinterUnknown)
	}
	for _, raw := range []string{"", "null", "{}"} {
		if got, err := ReadLinterOption(json.RawMessage(raw)); err != nil || got != "" {
			t.Errorf("ReadLinterOption(%q) = %q, %v, want no option", raw, got, err)
		}
	}
}

func TestIgnoredScriptPart_NamesWhatTheESLintLintDoesNotRun(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "lint-runs-eslint", "ignored-lint-script-arguments-are-named")
	for script, want := range map[string]string{
		"eslint .":   "",
		"npx eslint": "",
		"eslint --ext .ts,.tsx src --max-warnings 0": "--ext .ts,.tsx src --max-warnings 0",
		"pnpm exec eslint . --max-warnings 0":        "--max-warnings 0",
		"eslint . && prettier --check .":             "&& prettier --check .",
		"biome check .":                              "",
	} {
		project := t.TempDir()
		writeProjectFile(t, project, "package.json", `{"scripts": {"lint": `+quote(script)+`}}`)
		if got := IgnoredScriptPart(project); got != want {
			t.Errorf("IgnoredScriptPart(%q) = %q, want %q", script, got, want)
		}
	}
}

func quote(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}
