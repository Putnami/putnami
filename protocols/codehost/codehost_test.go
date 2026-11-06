package codehost

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const feature = "codehost/code-host-vocabulary"

// document is vocabulary.json.
type document struct {
	Schema          string       `json:"$schema,omitempty"`
	ProtocolVersion int          `json:"protocolVersion"`
	NoteMarker      string       `json:"noteMarker"`
	Hosts           []Vocabulary `json:"hosts"`
}

// expectations is fixtures/expectations.json, the corpus the TypeScript
// binding reads too.
type expectations struct {
	Lookups []struct {
		Slug     string `json:"slug"`
		Valid    bool   `json:"valid"`
		Displays string `json:"displays"`
	} `json:"lookups"`
	Labels []struct {
		Slug   string `json:"slug"`
		Number int    `json:"number"`
		Label  string `json:"label"`
	} `json:"labels"`
	Notes []struct {
		Body    string `json:"body"`
		Putnami bool   `json:"putnami"`
	} `json:"notes"`
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func readDocument(t *testing.T) document {
	t.Helper()
	var doc document
	readJSON(t, "vocabulary.json", &doc)
	return doc
}

func readExpectations(t *testing.T) expectations {
	t.Helper()
	var exp expectations
	readJSON(t, filepath.Join("fixtures", "expectations.json"), &exp)
	return exp
}

func TestSlugs_AreGitHubGitLabGeneric(t *testing.T) {
	spectest.Proves(t, feature, "a-closed-slug-set", "the-slugs-are-github-gitlab-generic")
	if got, want := Slugs(), []string{"github", "gitlab", "generic"}; !slices.Equal(got, want) {
		t.Fatalf("Slugs() = %v, want %v", got, want)
	}
	// Slugs hands out a copy: a caller cannot change the set.
	Slugs()[0] = "mutated"
	if Slugs()[0] != GitHub {
		t.Fatal("Slugs() shares its backing array")
	}
	for _, tc := range readExpectations(t).Lookups {
		if got := Valid(tc.Slug); got != tc.Valid {
			t.Errorf("Valid(%q) = %v, want %v", tc.Slug, got, tc.Valid)
		}
		if _, ok := Lookup(tc.Slug); ok != tc.Valid {
			t.Errorf("Lookup(%q) ok = %v, want %v", tc.Slug, ok, tc.Valid)
		}
		if got := For(tc.Slug).Slug; got != tc.Displays {
			t.Errorf("For(%q).Slug = %q, want %q", tc.Slug, got, tc.Displays)
		}
	}
}

type schemaNode struct {
	Properties map[string]*schemaNode `json:"properties"`
	Items      *schemaNode            `json:"items"`
	Defs       map[string]*schemaNode `json:"$defs"`
	Ref        string                 `json:"$ref"`
	Enum       []string               `json:"enum"`
	Required   []string               `json:"required"`
}

func (root *schemaNode) resolve(node *schemaNode) *schemaNode {
	if node == nil || !strings.HasPrefix(node.Ref, "#/$defs/") {
		return node
	}
	return root.Defs[strings.TrimPrefix(node.Ref, "#/$defs/")]
}

func propertyNames(node *schemaNode) []string {
	names := make([]string, 0, len(node.Properties))
	for name := range node.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// jsonFields returns the member names of a struct, and the ones without
// omitempty, which a schema requires.
func jsonFields(value any) (all, required []string) {
	typ := reflect.TypeOf(value)
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		parts := strings.Split(tag, ",")
		all = append(all, parts[0])
		if !slices.Contains(parts[1:], "omitempty") {
			required = append(required, parts[0])
		}
	}
	sort.Strings(all)
	sort.Strings(required)
	return all, required
}

func TestSchema_DeclaresTheGoMembersAndSlugs(t *testing.T) {
	spectest.Proves(t, feature, "one-vocabulary-document", "the-schema-declares-the-go-members-and-slugs")
	data, err := os.ReadFile(filepath.Join("schemas", "vocabulary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root schemaNode
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	host := root.resolve(root.Properties["hosts"].Items)
	cases := []struct {
		name   string
		node   *schemaNode
		goType any
	}{
		{"document", &root, document{}},
		{"host", host, Vocabulary{}},
	}
	for _, tc := range cases {
		all, required := jsonFields(tc.goType)
		if got := propertyNames(tc.node); !slices.Equal(got, all) {
			t.Errorf("%s: schema members %v, Go members %v", tc.name, got, all)
		}
		got := append([]string(nil), tc.node.Required...)
		sort.Strings(got)
		if !slices.Equal(got, required) {
			t.Errorf("%s: schema requires %v, Go requires %v", tc.name, got, required)
		}
	}
	if got := root.resolve(host.Properties["slug"]).Enum; !slices.Equal(got, Slugs()) {
		t.Errorf("slug enum %v, want %v", got, Slugs())
	}
}

func TestTable_IsTheDocument(t *testing.T) {
	spectest.Proves(t, feature, "one-vocabulary-document", "the-go-table-is-the-document")
	doc := readDocument(t)
	if doc.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d", doc.ProtocolVersion, ProtocolVersion)
	}
	if doc.NoteMarker != NoteMarker {
		t.Errorf("noteMarker = %q, want %q", doc.NoteMarker, NoteMarker)
	}
	if !reflect.DeepEqual(doc.Hosts, Vocabularies()) {
		t.Errorf("hosts = %+v, want %+v", doc.Hosts, Vocabularies())
	}
	var slugs []string
	for _, host := range doc.Hosts {
		slugs = append(slugs, host.Slug)
	}
	if !slices.Equal(slugs, Slugs()) {
		t.Errorf("hosts list %v, want one per slug in order %v", slugs, Slugs())
	}
	// Vocabularies hands out a copy: a caller cannot change the table.
	Vocabularies()[0].Name = "mutated"
	if For(GitHub).Name != "GitHub" {
		t.Error("Vocabularies() shares its backing array")
	}
}

func TestLabels_MatchTheFixtures(t *testing.T) {
	spectest.Proves(t, feature, "each-host-names-its-change-request", "every-label-fixture-gets-its-label")
	exp := readExpectations(t)
	if len(exp.Labels) == 0 {
		t.Fatal("the corpus lists no label")
	}
	for _, tc := range exp.Labels {
		if got := ChangeRequestLabel(tc.Slug, tc.Number); got != tc.Label {
			t.Errorf("ChangeRequestLabel(%q, %d) = %q, want %q", tc.Slug, tc.Number, got, tc.Label)
		}
	}
	diffTabs := map[string]string{GitHub: "Files changed", GitLab: "Changes", Generic: "Changes"}
	for slug, want := range diffTabs {
		if got := For(slug).DiffTab; got != want {
			t.Errorf("For(%q).DiffTab = %q, want %q", slug, got, want)
		}
	}
}

func TestDefaults_AreThePublicHosts(t *testing.T) {
	spectest.Proves(t, feature, "hosted-slugs-name-their-public-host", "the-defaults-are-the-public-hosts")
	cases := []struct{ slug, host, api string }{
		{GitHub, "github.com", "https://api.github.com"},
		{GitLab, "gitlab.com", "https://gitlab.com/api/v4"},
		{Generic, "", ""},
	}
	for _, tc := range cases {
		v := For(tc.slug)
		if v.DefaultHost != tc.host || v.DefaultAPIBase != tc.api {
			t.Errorf("%s defaults = (%q, %q), want (%q, %q)", tc.slug, v.DefaultHost, v.DefaultAPIBase, tc.host, tc.api)
		}
	}
}

func TestNotes_AreRecognizedAsListed(t *testing.T) {
	spectest.Proves(t, feature, "the-putnami-note-is-marked", "every-note-fixture-is-recognized-as-listed")
	exp := readExpectations(t)
	if len(exp.Notes) == 0 {
		t.Fatal("the corpus lists no note")
	}
	for _, tc := range exp.Notes {
		if got := IsPutnamiNote(tc.Body); got != tc.Putnami {
			t.Errorf("IsPutnamiNote(%q) = %v, want %v", tc.Body, got, tc.Putnami)
		}
	}
}
