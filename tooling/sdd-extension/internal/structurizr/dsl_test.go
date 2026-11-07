package structurizr

import (
	"strings"
	"testing"
)

func elementByName(t *testing.T, workspace *Workspace, name string) *Element {
	t.Helper()
	for _, element := range workspace.Elements {
		if element.Name == name {
			return element
		}
	}
	t.Fatalf("no element %q", name)
	return nil
}

func TestReadBigBankDSL(t *testing.T) {
	workspace, err := Read("testdata/big-bank.dsl")
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Name != "Big Bank plc" {
		t.Errorf("name = %q", workspace.Name)
	}
	if got := len(workspace.Elements); got != 18 {
		t.Errorf("elements = %d, want 18", got)
	}
	if got := len(workspace.Relationships); got != 25 {
		t.Errorf("relationships = %d, want 25", got)
	}
	facade := elementByName(t, workspace, "Mainframe Banking System Facade")
	if facade.Kind != KindComponent || facade.Parent.Name != "API Application" || facade.Parent.Parent.Name != "Internet Banking System" {
		t.Errorf("facade = %+v", facade)
	}
	if facade.Property("path") != "api/mainframe-facade" || facade.Technology != "Spring Bean" {
		t.Errorf("facade path = %q, technology = %q", facade.Property("path"), facade.Technology)
	}
	if got := elementByName(t, workspace, "E-mail System").Property("path"); got != "email" {
		t.Errorf("unquoted property = %q", got)
	}
	spa := elementByName(t, workspace, "Single-Page Application")
	if strings.Join(spa.Tags, "|") != "Web Browser" || spa.Technology != "JavaScript and Angular" {
		t.Errorf("spa = %+v", spa)
	}
	last := workspace.Relationships[len(workspace.Relationships)-1]
	if last.Source.Name != "E-mail Component" || last.Destination.Name != "E-mail System" || last.Technology != "SMTP" {
		t.Errorf("last relationship = %+v", last)
	}
}

func TestDSLRelationshipsInsideAnElement(t *testing.T) {
	workspace, err := ParseDSL([]byte(`workspace {
    !identifiers hierarchical
    model {
        a = softwareSystem "A" {
            web = container "Web" {
                -> b.api "Calls" "HTTPS"
            }
            this -> b "Uses"
        }
        b = softwareSystem "B" {
            api = container "API" \
                "The API"
        }
    }
}`), "w.dsl")
	if err != nil {
		t.Fatal(err)
	}
	if len(workspace.Relationships) != 2 {
		t.Fatalf("relationships = %+v", workspace.Relationships)
	}
	first := workspace.Relationships[0]
	if first.Source.Name != "Web" || first.Destination.Name != "API" || first.Technology != "HTTPS" {
		t.Errorf("first = %+v", first)
	}
	if got := elementByName(t, workspace, "API").Description; got != "The API" {
		t.Errorf("continued line description = %q", got)
	}
}

func TestDSLRefusesWhatItCannotRead(t *testing.T) {
	cases := map[string]string{
		"include":   "workspace {\n  model {\n    !include other.dsl\n  }\n}",
		"extends":   "workspace extends base.dsl {\n}",
		"archetype": "workspace {\n  model {\n    archetypes {\n    }\n  }\n}",
		"unknown":   "workspace {\n  model {\n    a = softwareSystem \"A\"\n    a -> nowhere\n  }\n}",
		"nesting":   "workspace {\n  model {\n    c = container \"C\"\n  }\n}",
		"unclosed":  "workspace {\n  model {\n    a = person \"A\"\n",
		"allman":    "workspace {\n    views\n    {\n    }\n}\n",
		"textblock": "workspace {\n  model {\n    a = person \"\"\"\n",
	}
	wantLine := map[string]string{
		"include": "w.dsl:3:", "archetype": "w.dsl:3:", "unknown": "w.dsl:4:", "nesting": "w.dsl:3:",
		"unclosed": "w.dsl:4:", "allman": "w.dsl:3:", "textblock": "w.dsl:3:",
	}
	for name, source := range cases {
		_, err := ParseDSL([]byte(source), "w.dsl")
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if want := wantLine[name]; want != "" && !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: error %q does not start with %q", name, err, want)
		}
	}
}

func TestDSLAcceptsAByteOrderMark(t *testing.T) {
	workspace, err := ParseDSL([]byte("\uFEFFworkspace {\n  model {\n    a = softwareSystem \"A\"\n  }\n}\n"), "bom.dsl")
	if err != nil || len(workspace.Elements) != 1 {
		t.Fatalf("workspace = %+v, err = %v", workspace, err)
	}
}

func TestReadJSONWorkspace(t *testing.T) {
	workspace, err := ParseJSON([]byte(`{
  "name": "Small",
  "model": {
    "people": [{"id": "1", "name": "User", "relationships": [{"sourceId": "1", "destinationId": "2", "description": "Uses"}]}],
    "softwareSystems": [
      {"id": "2", "name": "Shop", "tags": "Element,Software System",
       "containers": [
         {"id": "3", "name": "Web", "technology": "Go", "properties": {"path": "web"},
          "relationships": [
            {"sourceId": "3", "destinationId": "5", "description": "Reads", "technology": "SQL"},
            {"sourceId": "3", "destinationId": "4", "description": "Implied", "linkedRelationshipId": "9"}
          ],
          "components": [{"id": "6", "name": "Handler"}]}
       ]},
      {"id": "4", "name": "Stock", "containers": [{"id": "5", "name": "DB"}]}
    ]
  }
}`), "small.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(workspace.Elements) != 6 {
		t.Errorf("elements = %d, want 6", len(workspace.Elements))
	}
	if len(workspace.Relationships) != 2 {
		t.Fatalf("relationships = %+v, want the implied one skipped", workspace.Relationships)
	}
	web := elementByName(t, workspace, "Web")
	if web.Property("path") != "web" || web.Kind != KindContainer || web.Path() != "Shop/Web" {
		t.Errorf("web = %+v", web)
	}
	if got := elementByName(t, workspace, "Shop").Tags; len(got) != 2 {
		t.Errorf("tags = %v", got)
	}
	if _, err := ParseJSON([]byte(`{"model": {"softwareSystems": [{"id": "1"}, {"id": "1"}]}}`), "dup.json"); err == nil ||
		!strings.HasPrefix(err.Error(), "dup.json:") {
		t.Errorf("a duplicate id: %v", err)
	}
	dangling := `{"model": {"softwareSystems": [{"id": "1", "relationships": [{"sourceId": "1", "destinationId": "9"}]}]}}`
	if _, err := ParseJSON([]byte(dangling), "d.json"); err == nil {
		t.Error("a relationship to an undeclared element is dropped silently")
	}
}
