package toml

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseReadsTablesArraysAndInlineTables(t *testing.T) {
	doc, err := Parse([]byte(`# a pyproject.toml
[project]
name = "api"   # the distribution name
version = '0.1.0'
dependencies = [
    "fastapi>=0.115",  # a comment inside an array
    "shared",
]
"quoted key" = true
dotted.key = false

[tool.uv.sources]
shared = { workspace = true }
lib = {path="../lib", editable = true,}

[tool.uv.workspace]
members = ["packages/*", 'apps/*']

[[package]]
name = "a"
size = 12
when = 1979-05-27 07:32:00Z

[[package]]
name = "b"
wheels = [
    { url = "https://example.com/b.whl", hash = "sha256:00" },
]

[package.source]
type = "directory"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := String(doc, "project", "name"); got != "api" {
		t.Errorf("project.name = %q", got)
	}
	if got := String(doc, "project", "version"); got != "0.1.0" {
		t.Errorf("project.version = %q", got)
	}
	if got := Strings(doc, "project", "dependencies"); !reflect.DeepEqual(got, []string{"fastapi>=0.115", "shared"}) {
		t.Errorf("dependencies = %v", got)
	}
	if Get(doc, "project", "quoted key") != true || Get(doc, "project", "dotted", "key") != false {
		t.Errorf("project = %v", doc["project"])
	}
	if Get(doc, "tool", "uv", "sources", "shared", "workspace") != true {
		t.Errorf("sources = %v", Get(doc, "tool", "uv", "sources"))
	}
	if String(doc, "tool", "uv", "sources", "lib", "path") != "../lib" {
		t.Errorf("lib = %v", Get(doc, "tool", "uv", "sources", "lib"))
	}
	if got := Strings(doc, "tool", "uv", "workspace", "members"); !reflect.DeepEqual(got, []string{"packages/*", "apps/*"}) {
		t.Errorf("members = %v", got)
	}
	packages := Tables(doc["package"])
	if len(packages) != 2 {
		t.Fatalf("packages = %v", doc["package"])
	}
	if packages[0]["size"] != "12" || packages[0]["when"] != "1979-05-27 07:32:00Z" {
		t.Errorf("scalars = %v", packages[0])
	}
	if String(packages[1], "source", "type") != "directory" {
		t.Errorf("[package.source] does not belong to the last package: %v", packages[1])
	}
	if wheels := Tables(packages[1]["wheels"]); len(wheels) != 1 || wheels[0]["hash"] != "sha256:00" {
		t.Errorf("wheels = %v", packages[1]["wheels"])
	}
}

func TestParseReadsStrings(t *testing.T) {
	doc, err := Parse([]byte("a = \"tab\\there \\\"q\\\" \\u00e9\"\n" +
		"b = '''\nraw \\n\nline'''\n" +
		"c = \"\"\"\nfirst \\\n   second\"\"\"\n" +
		"d = \"\"\"two \"\"quotes\"\"\"\"\"\n" +
		"e = 'C:\\path'\r\n" +
		"f = \"\\U0001F600\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"a": "tab\there \"q\" é",
		"b": "raw \\n\nline",
		"c": "first second",
		"d": `two ""quotes""`,
		"e": `C:\path`,
		"f": "😀",
	} {
		if got := String(doc, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestParseRefusesWhatIsNotTOML(t *testing.T) {
	for name, input := range map[string]string{
		"unclosed string":     "a = \"x\n",
		"unclosed array":      "a = [1, 2\n",
		"unclosed header":     "[project\nname = \"x\"\n",
		"missing equals":      "name \"x\"\n",
		"missing value":       "name =\n",
		"duplicate key":       "a = 1\na = 2\n",
		"trailing garbage":    "a = \"x\" y\n",
		"bad escape":          "a = \"\\q\"\n",
		"bad code point":      "a = \"\\uZZZZ\"\n",
		"surrogate":           "a = \"\\uD800\"\n",
		"short escape":        "a = \"\\u00",
		"table over a value":  "a = 1\n[a]\n",
		"array over a table":  "[a]\n[[a]]\n",
		"unclosed inline":     "a = { b = 1\n",
		"unclosed multi-line": "a = \"\"\"x\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(input))
			if err == nil || !strings.HasPrefix(err.Error(), "line ") {
				t.Fatalf("Parse(%q) = %v, want a line error", input, err)
			}
		})
	}
}

func TestAccessorsTolerateOtherShapes(t *testing.T) {
	doc := Table{"a": "x", "list": []any{"s", int64(1), Table{"k": "v"}}}
	if Get(doc, "a", "b") != nil || String(doc, "missing") != "" || Strings(doc, "a") != nil {
		t.Error("an absent path reads as nothing")
	}
	if got := Strings(doc, "list"); !reflect.DeepEqual(got, []string{"s"}) {
		t.Errorf("Strings = %v", got)
	}
	if got := Tables(doc["list"]); len(got) != 1 || got[0]["k"] != "v" {
		t.Errorf("Tables = %v", got)
	}
	if Tables("x") != nil {
		t.Error("a string has no tables")
	}
}
