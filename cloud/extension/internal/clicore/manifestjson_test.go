package clicore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpsertJSONFieldCreatesMissingObjects(t *testing.T) {
	got, err := UpsertJSONField([]byte("{\n  \"name\": \"acme\"\n}\n"), []string{"options", "@putnami/intelligence"}, "enabled", []byte("true"))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"name\": \"acme\",\n  \"options\": {\n    \"@putnami/intelligence\": {\n      \"enabled\": true\n    }\n  }\n}\n"
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestUpsertJSONFieldReplacesAndKeepsNeighbours(t *testing.T) {
	data := "{\n    \"options\": {\n        \"@putnami/cloud\": {\"x\": [1, \"}\", {\"y\": null}]},\n        \"@putnami/intelligence\": {\n            \"audit\": {},\n            \"enabled\": false\n        }\n    }\n}"
	got, err := UpsertJSONField([]byte(data), []string{"options", "@putnami/intelligence"}, "enabled", []byte("true"))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(data, "\"enabled\": false", "\"enabled\": true", 1); string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	got, err = UpsertJSONField(got, []string{"options", "@putnami/intelligence"}, "workspace", []byte("{\n  \"slug\": \"g\"\n}"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "\"enabled\": true,\n            \"workspace\": {\n              \"slug\": \"g\"\n            }\n        }") {
		t.Fatalf("inserted field lost the sibling indent:\n%s", got)
	}
	got, err = UpsertJSONField([]byte(`{"options":{}}`), []string{"options"}, "k", []byte(`"v"`))
	if err != nil || string(got) != "{\"options\":{\n  \"k\": \"v\"\n}}" {
		t.Fatalf("empty object insert = %q, %v", got, err)
	}
}

func TestUpsertJSONFieldRefusesNonObjects(t *testing.T) {
	for _, tc := range []struct{ data, want string }{
		{`not json`, "invalid JSON"},
		{`[]`, "manifest root must be a JSON object"},
		{`{"options": []}`, "manifest options must be a JSON object"},
		{`{"options": {"@putnami/cloud": "x"}}`, "manifest options.@putnami/cloud must be a JSON object"},
	} {
		if _, err := UpsertJSONField([]byte(tc.data), []string{"options", "@putnami/cloud"}, "workspace", []byte("{}")); err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v, want %q", tc.data, err, tc.want)
		}
	}
}

func TestManifestPathPrefersTheWorkspaceManifest(t *testing.T) {
	root := t.TempDir()
	if got := ManifestPath(root); got != "" {
		t.Fatalf("empty root = %q", got)
	}
	project := filepath.Join(root, "putnami.json")
	if err := os.WriteFile(project, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ManifestPath(root); got != project {
		t.Fatalf("project manifest = %q", got)
	}
	workspace := filepath.Join(root, "putnami.workspace.json")
	if err := os.WriteFile(workspace, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ManifestPath(root); got != workspace {
		t.Fatalf("workspace manifest = %q", got)
	}
}
