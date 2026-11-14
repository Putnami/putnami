package workspace

import (
	"encoding/json"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const layoutSchema = "https://putnami.dev/schemas/putnami-project.json"

// decodeConfig reads a configuration file the way a writer does before it
// edits a member.
func decodeConfig(t *testing.T, data string) map[string]any {
	t.Helper()
	doc := map[string]any{}
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func renderConfig(t *testing.T, doc any, existing string) string {
	t.Helper()
	var prior []byte
	if existing != "" {
		prior = []byte(existing)
	}
	out, err := RenderConfigFile(doc, prior)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// changedLines answers the lines of after that before does not hold at the
// same position, and the count of lines the two differ by.
func changedLines(before, after string) []string {
	b, a := strings.Split(before, "\n"), strings.Split(after, "\n")
	var changed []string
	for i, line := range a {
		if i >= len(b) || b[i] != line {
			changed = append(changed, line)
		}
	}
	return changed
}

func TestRenderConfigFile_ANewFileTakesTheFormattersLayout(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-written-file-keeps-the-repository-layout", "a-new-file-takes-the-formatters-layout")
	doc := map[string]any{
		"$schema":      layoutSchema,
		"dependencies": []string{"/packages/pricing", "/packages/ui"},
		"disable":      map[string]any{"jobs": []string{"lint"}},
		"extensions":   []string{"@putnami/turbo"},
		"includes":     []string{"apps/dashboard", "apps/website", "packages/analytics", "packages/ui"},
		"name":         "web",
		"options":      map[string]any{},
		"tags":         []string{},
	}
	// Biome and Prettier write this file unchanged with their default
	// settings, Biome given two-space indentation: an object that breaks after
	// its brace stays one member per line, and an array of scalars fits on one
	// line of 80 columns when it can. oxfmt, whose default line is 100
	// columns, keeps every line but joins includes, 86 columns on one line.
	want := `{
  "$schema": "https://putnami.dev/schemas/putnami-project.json",
  "dependencies": ["/packages/pricing", "/packages/ui"],
  "disable": {
    "jobs": ["lint"]
  },
  "extensions": ["@putnami/turbo"],
  "includes": [
    "apps/dashboard",
    "apps/website",
    "packages/analytics",
    "packages/ui"
  ],
  "name": "web",
  "options": {},
  "tags": []
}
`
	if got := renderConfig(t, doc, ""); got != want {
		t.Fatalf("new file =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderConfigFile_ANewFileIsIndentedAsThePackageJSONBesideIt(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-written-file-keeps-the-repository-layout", "a-new-file-takes-the-formatters-layout")
	doc := map[string]any{"disable": map[string]any{"jobs": []string{"lint"}}, "tags": []string{"turbo"}}
	besides := map[string]string{
		"tabs":       "{\n\t\"name\": \"web\",\n\t\"scripts\": {\n\t\t\"lint\": \"biome check .\"\n\t}\n}\n",
		"four":       "{\n    \"name\": \"web\"\n}\n",
		"one line":   `{"name": "web"}`,
		"not a file": "",
	}
	wants := map[string]string{
		"tabs":       "{\n\t\"disable\": {\n\t\t\"jobs\": [\"lint\"]\n\t},\n\t\"tags\": [\"turbo\"]\n}\n",
		"four":       "{\n    \"disable\": {\n        \"jobs\": [\"lint\"]\n    },\n    \"tags\": [\"turbo\"]\n}\n",
		"one line":   "{\n  \"disable\": {\n    \"jobs\": [\"lint\"]\n  },\n  \"tags\": [\"turbo\"]\n}\n",
		"not a file": "{\n  \"disable\": {\n    \"jobs\": [\"lint\"]\n  },\n  \"tags\": [\"turbo\"]\n}\n",
	}
	for name, beside := range besides {
		t.Run(name, func(t *testing.T) {
			out, err := RenderConfigFileBeside(doc, nil, []byte(beside))
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != wants[name] {
				t.Fatalf("beside %q =\n%s\nwant\n%s", beside, out, wants[name])
			}
		})
	}
}

func TestRenderConfigFile_AnUpdateChangesOnlyTheValuesItChanges(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-written-file-keeps-the-repository-layout", "an-update-changes-only-the-values-it-changes")
	cases := map[string]struct {
		existing string
		edit     func(doc map[string]any)
		want     string
	}{
		"biome adds a dependency": {
			existing: "{\n" +
				"\t\"$schema\": \"" + layoutSchema + "\",\n" +
				"\t\"dependencies\": [\"/packages/pricing\"],\n" +
				"\t\"disable\": { \"jobs\": [\"lint\"] },\n" +
				"\t\"extensions\": [\"@putnami/turbo\"],\n" +
				"\t\"tags\": [\"turbo\"]\n" +
				"}\n",
			edit: func(doc map[string]any) {
				doc["dependencies"] = []string{"/packages/pricing", "/packages/ui"}
			},
			want: "\t\"dependencies\": [\"/packages/pricing\", \"/packages/ui\"],",
		},
		"prettier adds a dependency to unsorted members": {
			existing: "{\n" +
				"  \"name\": \"web\",\n" +
				"  \"dependencies\": [\"/packages/a\"],\n" +
				"  \"tags\": [\"app\"],\n" +
				"  \"$schema\": \"" + layoutSchema + "\"\n" +
				"}\n",
			edit: func(doc map[string]any) {
				doc["dependencies"] = []string{"/packages/a", "/packages/b"}
			},
			want: "  \"dependencies\": [\"/packages/a\", \"/packages/b\"],",
		},
		"a one-line object grows in place": {
			existing: "{\n" +
				"  \"disable\": { \"jobs\": [\"lint\"] },\n" +
				"  \"name\": \"web\"\n" +
				"}\n",
			edit: func(doc map[string]any) {
				doc["disable"] = map[string]any{"jobs": []string{"lint", "test"}}
			},
			want: "  \"disable\": { \"jobs\": [\"lint\", \"test\"] },",
		},
		"an escape the file wrote stays": {
			existing: "{\n" +
				"  \"commands\": { \"build\": \"tsc \\u0026\\u0026 vite\" },\n" +
				"  \"tags\": [\"app\"]\n" +
				"}\n",
			edit: func(doc map[string]any) {
				doc["tags"] = []string{"app", "web"}
			},
			want: "  \"tags\": [\"app\", \"web\"]",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc := decodeConfig(t, tc.existing)
			tc.edit(doc)
			got := renderConfig(t, doc, tc.existing)
			changed := changedLines(tc.existing, got)
			if len(changed) != 1 || changed[0] != tc.want || strings.Count(got, "\n") != strings.Count(tc.existing, "\n") {
				t.Fatalf("rendered =\n%s\nchanged lines %q, want only %q", got, changed, tc.want)
			}
		})
	}
}

func TestRenderConfigFile_ANewMemberFollowsTheMemberBeforeIt(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-written-file-keeps-the-repository-layout", "an-update-changes-only-the-values-it-changes")
	existing := "{\n" +
		"  \"name\": \"web\",\n" +
		"  \"tags\": [\"app\"],\n" +
		"  \"dependencies\": [\"/packages/a\"]\n" +
		"}\n"
	doc := decodeConfig(t, existing)
	doc["extensions"] = []string{"@putnami/turbo"}
	want := "{\n" +
		"  \"name\": \"web\",\n" +
		"  \"tags\": [\"app\"],\n" +
		"  \"dependencies\": [\"/packages/a\"],\n" +
		"  \"extensions\": [\"@putnami/turbo\"]\n" +
		"}\n"
	if got := renderConfig(t, doc, existing); got != want {
		t.Fatalf("rendered =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderConfigFile_AnArrayThatNoLongerFitsBreaks(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-written-file-keeps-the-repository-layout", "an-update-changes-only-the-values-it-changes")
	existing := "{\n  \"dependencies\": [\"/packages/analytics\", \"/packages/pricing\"],\n  \"name\": \"web\"\n}\n"
	doc := decodeConfig(t, existing)
	doc["dependencies"] = []string{"/packages/analytics", "/packages/pricing", "/packages/ui-components"}
	want := "{\n" +
		"  \"dependencies\": [\n" +
		"    \"/packages/analytics\",\n" +
		"    \"/packages/pricing\",\n" +
		"    \"/packages/ui-components\"\n" +
		"  ],\n" +
		"  \"name\": \"web\"\n" +
		"}\n"
	if got := renderConfig(t, doc, existing); got != want {
		t.Fatalf("rendered =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderConfigFile_LineEndingsStay(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-written-file-keeps-the-repository-layout", "an-update-changes-only-the-values-it-changes")
	existing := "{\r\n  \"disable\": {\r\n    \"jobs\": [\"lint\"]\r\n  },\r\n  \"tags\": [\"app\"]\r\n}\r\n"
	doc := decodeConfig(t, existing)
	doc["name"] = "web"
	want := "{\r\n  \"disable\": {\r\n    \"jobs\": [\"lint\"]\r\n  },\r\n  \"name\": \"web\",\r\n  \"tags\": [\"app\"]\r\n}\r\n"
	if got := renderConfig(t, doc, existing); got != want {
		t.Fatalf("rendered = %q\nwant %q", got, want)
	}
}

func TestRenderConfigFile_AnEqualFileKeepsItsBytes(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-written-file-keeps-the-repository-layout", "an-equal-file-keeps-its-bytes")
	for name, existing := range map[string]string{
		"biome":          "{\n\t\"extensions\": [\"@putnami/turbo\"],\n\t\"includes\": [\"apps/*\"]\n}\n",
		"one line":       `{"name": "demo", "options": {"@putnami/turbo": {"mode": "delegate"}}}`,
		"unsorted":       "{\n  \"name\": \"web\",\n  \"$schema\": \"" + layoutSchema + "\"\n}",
		"escaped":        "{\n  \"command\": \"a \\u0026\\u0026 b\"\n}\n",
		"putnami layout": "{\n  \"tags\": [\n    \"turbo\"\n  ]\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if got := renderConfig(t, decodeConfig(t, existing), existing); got != existing {
				t.Fatalf("rendered =\n%s\nwant the file unchanged:\n%s", got, existing)
			}
		})
	}
}

func TestRenderConfigFile_AnExpandedFileStaysExpanded(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-written-file-keeps-the-repository-layout", "an-expanded-file-stays-expanded")
	existing := "{\n" +
		"  \"dependencies\": [\n" +
		"    \"/packages/a\"\n" +
		"  ],\n" +
		"  \"tags\": [\n" +
		"    \"turbo\"\n" +
		"  ]\n" +
		"}\n"
	doc := decodeConfig(t, existing)
	doc["dependencies"] = []string{"/packages/a", "/packages/b"}
	doc["extensions"] = []string{"@putnami/turbo"}
	want := "{\n" +
		"  \"dependencies\": [\n" +
		"    \"/packages/a\",\n" +
		"    \"/packages/b\"\n" +
		"  ],\n" +
		"  \"extensions\": [\n" +
		"    \"@putnami/turbo\"\n" +
		"  ],\n" +
		"  \"tags\": [\n" +
		"    \"turbo\"\n" +
		"  ]\n" +
		"}\n"
	if got := renderConfig(t, doc, existing); got != want {
		t.Fatalf("rendered =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderConfigFile_AFileThatIsNotAnObjectIsReplacedAsNew(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-written-file-keeps-the-repository-layout", "a-new-file-takes-the-formatters-layout")
	doc := map[string]any{"tags": []string{"app"}}
	want := "{\n  \"tags\": [\"app\"]\n}\n"
	for _, existing := range []string{"[1, 2]", "{not json", "\n"} {
		if got := renderConfig(t, doc, existing); got != want {
			t.Errorf("over %q rendered =\n%s\nwant\n%s", existing, got, want)
		}
	}
	if _, err := RenderConfigFile([]string{"a"}, nil); err == nil {
		t.Error("an array rendered as a configuration file")
	}
}

func TestRenderConfigFile_AShrunkArrayStaysExpandedInAnExpandedFile(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-written-file-keeps-the-repository-layout", "an-expanded-file-stays-expanded")
	existing := "{\n" +
		"  \"dependencies\": [\n" +
		"    \"/packages/analytics-dashboard\",\n" +
		"    \"/packages/pricing-engine\",\n" +
		"    \"/packages/ui-components\"\n" +
		"  ],\n" +
		"  \"tags\": [\n" +
		"    \"turbo\"\n" +
		"  ]\n" +
		"}\n"
	doc := decodeConfig(t, existing)
	doc["dependencies"] = []string{"/packages/pricing-engine"}
	want := "{\n" +
		"  \"dependencies\": [\n" +
		"    \"/packages/pricing-engine\"\n" +
		"  ],\n" +
		"  \"tags\": [\n" +
		"    \"turbo\"\n" +
		"  ]\n" +
		"}\n"
	if got := renderConfig(t, doc, existing); got != want {
		t.Fatalf("rendered =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderConfigFile_AJoinedArrayKeepsTheEscapesOfItsItems(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-written-file-keeps-the-repository-layout", "an-update-changes-only-the-values-it-changes")
	existing := "{\n  \"commands\": [\"a \\u0026\\u0026 b\"]\n}\n"
	doc := decodeConfig(t, existing)
	doc["commands"] = []string{"a && b", "c"}
	want := "{\n  \"commands\": [\"a \\u0026\\u0026 b\", \"c\"]\n}\n"
	if got := renderConfig(t, doc, existing); got != want {
		t.Fatalf("rendered = %q\nwant %q", got, want)
	}
}
