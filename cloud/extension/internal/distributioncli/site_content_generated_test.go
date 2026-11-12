package distributioncli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// writeGeneratedSiteProject writes a site project with siteContent as its
// site-content.json, or no such file when siteContent is nil, and returns the
// project directory.
func writeGeneratedSiteProject(t *testing.T, siteContent any) string {
	t.Helper()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "putnami.json"), []byte(`{"name":"docs/example.dev","publish":["site-content"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if siteContent != nil {
		data, err := json.Marshal(siteContent)
		if err != nil {
			t.Fatal(err)
		}
		writeSiteContentDeclaration(t, project, string(data))
	}
	return project
}

func writeSiteContentDeclaration(t *testing.T, project, document string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(project, SiteContentDeclarationFile), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
}

func generatedSections(commands map[string][]string) map[string]any {
	generated := map[string]any{}
	for section, command := range commands {
		generated[section] = map[string]any{"command": command}
	}
	return map[string]any{"generated": generated}
}

// generateIn runs generate-site-content from project, as the build step does,
// and returns its JSON result, the lines it streamed and its error.
func generateIn(t *testing.T, project string, args ...string) (map[string]any, []string, error) {
	t.Helper()
	t.Chdir(project)
	var result map[string]any
	var lines []string
	err := GenerateSiteContent(map[string]any{"json": true}, args, "", clicore.IO{
		JSON:   func(data any) { result = resultPayloadMap(t, data) },
		Stdout: func(line string) { lines = append(lines, line) },
	})
	return result, lines, err
}

func readGeneratedTree(t *testing.T, project string) map[string]string {
	t.Helper()
	files := map[string]string{}
	root := filepath.Join(project, ".gen", "site-content")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: a file the test's generator wrote
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return files
}

// The command receives the absolute section directory wherever {output}
// appears in an argument, runs in the project directory, and its output
// streams line by line. Sections run in name order, into a directory that
// holds only the declared sections.
func TestGenerateSiteContentReplacesTheOutputPlaceholder(t *testing.T) {
	project := writeGeneratedSiteProject(t, generatedSections(map[string][]string{
		"api": {"sh", "-c", `mkdir -p "$1/types" && printf '# API\n' > "$1/index.md" && printf '# User\n' > "$1/types/user.md" && echo "wrote api from $(basename "$PWD")"`, "sh", "{output}"},
		"cli": {"sh", "-c", `printf '# CLI\n' > "${1#--out=}/index.md" && echo wrote cli`, "sh", "--out={output}"},
	}))
	stale := filepath.Join(project, ".gen", "site-content", "removed", "index.md")
	if err := os.MkdirAll(filepath.Dir(stale), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("# Removed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, lines, err := generateIn(t, project)
	if err != nil {
		t.Fatalf("generate-site-content: %v", err)
	}
	want := map[string]string{"api/index.md": "# API\n", "api/types/user.md": "# User\n", "cli/index.md": "# CLI\n"}
	if got := readGeneratedTree(t, project); !reflect.DeepEqual(got, want) {
		t.Fatalf(".gen/site-content = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(lines, []string{"wrote api from " + filepath.Base(project), "wrote cli"}) {
		t.Fatalf("streamed lines = %q, want each generator's output in section order", lines)
	}
	if result["status"] != "generated" || result["files"] != 3 {
		t.Fatalf("result = %v, want generated with 3 files", result)
	}
}

// A command that fails, that cannot start, or that writes no regular file
// fails the step and names the section.
func TestGenerateSiteContentRefusesAFailedOrEmptyGenerator(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command []string
		want    []string
	}{
		{name: "a failing command", command: []string{"sh", "-c", `echo "cannot read the schema" >&2; exit 3`, "sh", "{output}"},
			want: []string{`generate site-content section "api": sh exited with status 3`, "cannot read the schema"}},
		{name: "a program not on PATH", command: []string{"putnami-no-such-generator", "{output}"},
			want: []string{`section "api": putnami-no-such-generator is not on the job's PATH`}},
		{name: "a command that writes nothing", command: []string{"sh", "-c", "true", "sh", "{output}"},
			want: []string{"wrote no file to", "a generated section must ship content"}},
		{name: "a command that writes only a symbolic link", command: []string{"sh", "-c", `ln -s /etc/hosts "$1/hosts.md"`, "sh", "{output}"},
			want: []string{"wrote no file to"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := writeGeneratedSiteProject(t, generatedSections(map[string][]string{"api": tc.command}))
			result, _, err := generateIn(t, project)
			if err == nil || clicore.ExitCode(err) != clicore.ExitFailure {
				t.Fatalf("generate-site-content = (%v, %v), want a failure", err, result)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want %q", err, want)
				}
			}
		})
	}
}

// A project that declares no generated section succeeds with nothing to do,
// and a section an earlier declaration generated does not survive.
func TestGenerateSiteContentWithNoDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name        string
		siteContent any
	}{
		{name: "no declaration file"},
		{name: "no generated section", siteContent: map[string]any{"generated": map[string]any{}}},
		{name: "an empty declaration", siteContent: map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := writeGeneratedSiteProject(t, tc.siteContent)
			stale := filepath.Join(project, ".gen", "site-content", "api", "index.md")
			if err := os.MkdirAll(filepath.Dir(stale), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stale, []byte("# API\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			result, _, err := generateIn(t, project)
			if err != nil || result["status"] != "skipped" {
				t.Fatalf("generate-site-content = (%v, %v), want skipped", err, result)
			}
			if got := readGeneratedTree(t, project); len(got) != 0 {
				t.Fatalf(".gen/site-content = %v, want it removed", got)
			}
		})
	}

	t.Run("no project file", func(t *testing.T) {
		result, _, err := generateIn(t, t.TempDir())
		if err != nil || result["status"] != "skipped" {
			t.Fatalf("generate-site-content = (%v, %v), want skipped", err, result)
		}
	})
}

// A declaration that cannot generate a section fails before any command runs.
func TestGenerateSiteContentRefusesAnInvalidDeclaration(t *testing.T) {
	output := []string{"sh", "-c", `printf x > "$1/index.md"`, "sh", "{output}"}
	for _, tc := range []struct {
		name        string
		siteContent any
		want        string
	}{
		{name: "a misspelled field", siteContent: map[string]any{"generate": map[string]any{}}, want: `unknown field "generate"`},
		{name: "a misspelled command field", siteContent: map[string]any{"generated": map[string]any{"api": map[string]any{"cmd": output}}}, want: `unknown field "cmd"`},
		{name: "a value that is not an object", siteContent: true, want: "it must be one object whose only member is generated"},
		{name: "a list", siteContent: []any{}, want: "it must be one object whose only member is generated"},
		{name: "generated that is not an object", siteContent: map[string]any{"generated": []any{}}, want: "cannot unmarshal array"},
		{name: "a section that is not an object", siteContent: map[string]any{"generated": map[string]any{"api": output}}, want: "cannot unmarshal array"},
		{name: "an empty command", siteContent: generatedSections(map[string][]string{"api": {}}), want: "command must be a non-empty list"},
		{name: "an unnamed program", siteContent: generatedSections(map[string][]string{"api": {" ", "{output}"}}), want: "command must be a non-empty list"},
		{name: "no output placeholder", siteContent: generatedSections(map[string][]string{"api": {"sh", "-c", "true"}}), want: "command must pass {output}"},
		{name: "a name that is not a slug", siteContent: generatedSections(map[string][]string{"API": output}), want: "is not a lowercase URL-safe slug"},
		{name: "a dot name", siteContent: generatedSections(map[string][]string{".hidden": output}), want: "must be one directory name"},
		{name: "a nested name", siteContent: generatedSections(map[string][]string{"api/v1": output}), want: "must be one directory name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := writeGeneratedSiteProject(t, tc.siteContent)
			_, _, err := generateIn(t, project)
			if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("generate-site-content = %v, want a usage error with %q", err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(project, ".gen", "site-content")); !os.IsNotExist(statErr) {
				t.Fatalf("an invalid declaration created .gen/site-content: %v", statErr)
			}
		})
	}

	t.Run("a positional argument", func(t *testing.T) {
		project := writeGeneratedSiteProject(t, generatedSections(map[string][]string{"api": output}))
		_, _, err := generateIn(t, project, "docs/other.dev")
		if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "takes no argument") {
			t.Fatalf("generate-site-content = %v, want a usage error", err)
		}
	})

	for _, tc := range []struct{ name, document, want string }{
		{name: "a file that is not JSON", document: "{", want: "unexpected EOF"},
		{name: "an empty file", document: "", want: "it must be one object"},
		{name: "null", document: "null", want: "it must be one object"},
		{name: "a second value after the object", document: `{"generated":{}} {}`, want: "data follows the object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := writeGeneratedSiteProject(t, nil)
			writeSiteContentDeclaration(t, project, tc.document)
			_, _, err := generateIn(t, project)
			if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "site-content.json") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("generate-site-content = %v, want a usage error on site-content.json with %q", err, tc.want)
			}
		})
	}

	// putnami.json declares no generated section: the declaration has one
	// source, site-content.json.
	t.Run("a declaration in putnami.json", func(t *testing.T) {
		project := t.TempDir()
		document := `{"name":"docs/example.dev","publish":["site-content"],"options":{"@putnami/cloud":{"site-content":{"generated":{"api":{"command":["sh","-c","printf x > \"$1/index.md\"","sh","{output}"]}}}}}}`
		if err := os.WriteFile(filepath.Join(project, "putnami.json"), []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		result, _, err := generateIn(t, project)
		if err != nil || result["status"] != "skipped" {
			t.Fatalf("generate-site-content = (%v, %v), want skipped: putnami.json declares no generated section", err, result)
		}
	})
}

// DeriveSiteContentSections reads a declared generated section from
// .gen/site-content/<section> on disk, beside the section directories, and
// leaves an undeclared directory there out.
func TestDeriveSiteContentSectionsReadsADeclaredGeneratedSection(t *testing.T) {
	site := writeGeneratedSiteProject(t, generatedSections(map[string][]string{"api": {"generate", "{output}"}}))
	for _, rel := range []string{"guides/index.md", ".gen/site-content/api/index.md", ".gen/site-content/undeclared/index.md"} {
		path := filepath.Join(site, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# Page\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sections, err := DeriveSiteContentSections(site, "example.dev", "", map[string]string{})
	if err != nil || len(sections) != 2 {
		t.Fatalf("sections = (%+v, %v), want api and guides", sections, err)
	}
	api := sections[0]
	if api.Section != "api" || api.Dir != filepath.Join(site, ".gen", "site-content", "api") || api.Mount != "/docs/api" || !api.generated {
		t.Fatalf("generated section = %+v, want api read from .gen/site-content/api", api)
	}
	bundle, shipped, err := AssembleSiteContentSection(api, "abc123")
	if err != nil || !shipped || !reflect.DeepEqual(bundle.FilePaths(), []string{"docs/api/index.md"}) {
		t.Fatalf("assembled = (%v, %t, %v), want docs/api/index.md", bundle.FilePaths(), shipped, err)
	}

	if err := os.RemoveAll(filepath.Join(site, ".gen", "site-content", "api", "index.md")); err != nil {
		t.Fatal(err)
	}
	if err := emptySectionError(api); !strings.Contains(err.Error(), "check its generator command") {
		t.Fatalf("empty generated section error = %v", err)
	}
}
