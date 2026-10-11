package job

import (
	"encoding/json"
	"slices"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestValidate_V2ProjectTags pins the form of a present `project.tags`: a
// non-null list of non-empty tags with no surrounding white space, sorted in
// byte order and free of duplicates.
func TestValidate_V2ProjectTags(t *testing.T) {
	cases := []struct {
		name    string
		tags    []string
		present bool
		field   string
	}{
		{name: "absent is accepted"},
		{name: "an empty list is accepted", tags: []string{}},
		{name: "one tag is accepted", tags: []string{"go"}},
		{name: "a sorted list is accepted", tags: []string{"go", "protocol"}},
		{name: "byte order, not letter order", tags: []string{"Zeta", "alpha"}},
		{name: "present null is rejected", present: true, field: "project.tags"},
		{name: "an unsorted list is rejected", tags: []string{"protocol", "go"}, field: "project.tags"},
		{name: "a duplicate is rejected", tags: []string{"go", "go"}, field: "project.tags"},
		{name: "an empty tag is rejected", tags: []string{"", "go"}, field: "project.tags[0]"},
		{name: "a blank tag is rejected", tags: []string{" ", "go"}, field: "project.tags[0]"},
		{name: "a leading space is rejected", tags: []string{" go"}, field: "project.tags[0]"},
		{name: "a trailing space is rejected", tags: []string{"go", "protocol "}, field: "project.tags[1]"},
		{name: "a trailing tab is rejected", tags: []string{"go\t"}, field: "project.tags[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := validV2Context()
			ctx.Project.Tags = tc.tags
			ctx.presence.projectTags = tc.present
			diags := Validate(ctx)
			if tc.field == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("valid project tags rejected: %v", diags)
				}
				return
			}
			assertDiagnostic(t, diags, ErrorCodeInvalidValue, tc.field)
		})
	}
}

// A list that breaks both rules reports each once: every malformed tag at its
// index, and the first pair that is out of order.
func TestValidate_V2ProjectTagsReportsEachRuleOnce(t *testing.T) {
	ctx := validV2Context()
	ctx.Project.Tags = []string{"protocol", " go", "go", "go"}
	got := Validate(ctx)
	want := []struct{ code, field string }{
		{ErrorCodeInvalidValue, "project.tags[1]"},
		{ErrorCodeInvalidValue, "project.tags"},
	}
	if len(got) != len(want) {
		t.Fatalf("diagnostics = %v, want exactly %d", got, len(want))
	}
	for i, w := range want {
		if got[i].Code != w.code || got[i].Field != w.field {
			t.Errorf("diagnostic[%d] = (%s, %s), want (%s, %s)", i, got[i].Code, got[i].Field, w.code, w.field)
		}
	}
}

// TestValidate_V1RejectsProjectTags pins the version boundary. A version 1
// document carries no tags in any spelling: a list, an empty list, or null.
func TestValidate_V1RejectsProjectTags(t *testing.T) {
	cases := []struct {
		name    string
		tags    []string
		present bool
	}{
		{name: "a list", tags: []string{"go", "protocol"}},
		{name: "an empty list", tags: []string{}},
		{name: "null", present: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := validV2Context()
			ctx.ProtocolVersion = 0 // v1
			ctx.Identity = nil
			ctx.Project.Tags = tc.tags
			ctx.presence.projectTags = tc.present
			assertDiagnostic(t, Validate(ctx), ErrorCodeUnexpectedField, "project.tags")
		})
	}

	ctx := validV2Context()
	ctx.ProtocolVersion = 0 // v1
	ctx.Identity = nil
	for _, d := range Validate(ctx) {
		if d.Field == "project.tags" {
			t.Errorf("a v1 document with no tags reports on project.tags: %v", d)
		}
	}
}

// TestV2ProjectTagsFixtures pins each fixture to the one diagnostic it exists
// for, so a fixture cannot stay red for an unrelated reason.
func TestV2ProjectTagsFixtures(t *testing.T) {
	invalid := []struct {
		fixture string
		code    string
		field   string
	}{
		{"project-tags-null.json", ErrorCodeInvalidValue, "project.tags"},
		{"project-tags-unsorted.json", ErrorCodeInvalidValue, "project.tags"},
		{"project-tags-duplicate.json", ErrorCodeInvalidValue, "project.tags"},
		{"project-tags-empty-tag.json", ErrorCodeInvalidValue, "project.tags[0]"},
		{"project-tags-padded-tag.json", ErrorCodeInvalidValue, "project.tags[1]"},
		{"v1-with-project-tags.json", ErrorCodeUnexpectedField, "project.tags"},
		{"v1-with-empty-project-tags.json", ErrorCodeUnexpectedField, "project.tags"},
	}
	for _, tc := range invalid {
		t.Run(tc.fixture, func(t *testing.T) {
			_, got := ParseAndValidate(readFixture(t, "fixtures", "v2", "invalid", tc.fixture))
			if len(got) != 1 {
				t.Fatalf("diagnostics = %v, want exactly one", got)
			}
			if got[0].Code != tc.code || got[0].Field != tc.field {
				t.Errorf("diagnostic = (%s, %s), want (%s, %s)", got[0].Code, got[0].Field, tc.code, tc.field)
			}
		})
	}

	valid := []struct {
		fixture string
		want    []string
	}{
		{"project-tags.json", []string{"go", "protocol"}},
		{"project-tags-empty.json", []string{}},
	}
	for _, tc := range valid {
		t.Run(tc.fixture, func(t *testing.T) {
			ctx, diags := ParseAndValidate(readFixture(t, "fixtures", "v2", "valid", tc.fixture))
			if diag.HasErrors(diags) {
				t.Fatalf("valid fixture rejected: %v", diags)
			}
			if ctx.Project.Tags == nil {
				t.Fatal("project.tags = nil, want the list the fixture carries")
			}
			if !slices.Equal(ctx.Project.Tags, tc.want) {
				t.Errorf("project.tags = %q, want %q", ctx.Project.Tags, tc.want)
			}
		})
	}
}

// TestProjectTagsKeepAbsentAndEmptyApart pins the two answers on both entry
// points and across a round trip: an absent member decodes to a nil list and is
// omitted again, and an empty list decodes to an empty list and is written
// again as `[]`.
func TestProjectTagsKeepAbsentAndEmptyApart(t *testing.T) {
	projectMember := func(t *testing.T, ctx *Context) (json.RawMessage, bool) {
		t.Helper()
		encoded, err := json.Marshal(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Project map[string]json.RawMessage `json:"project"`
		}
		if err := json.Unmarshal(encoded, &document); err != nil {
			t.Fatal(err)
		}
		raw, ok := document.Project["tags"]
		return raw, ok
	}

	// The lenient entry point is the one a task reads through.
	empty, err := Parse(readFixture(t, "fixtures", "v2", "valid", "project-tags-empty.json"))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Project.Tags == nil || len(empty.Project.Tags) != 0 {
		t.Fatalf("project.tags = %#v, want an empty, non-nil list", empty.Project.Tags)
	}
	if raw, ok := projectMember(t, empty); !ok || string(raw) != "[]" {
		t.Errorf("an empty list encodes as %q (present=%v), want []", raw, ok)
	}

	absent, err := Parse(readFixture(t, "fixtures", "v2", "valid", "minimal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if absent.Project.Tags != nil {
		t.Fatalf("project.tags = %#v, want nil for a document without the member", absent.Project.Tags)
	}
	if raw, ok := projectMember(t, absent); ok {
		t.Errorf("an absent member encodes as %q, want it omitted", raw)
	}

	listed, err := Parse(readFixture(t, "fixtures", "v2", "valid", "project-tags.json"))
	if err != nil {
		t.Fatal(err)
	}
	if raw, ok := projectMember(t, listed); !ok || string(raw) != `["go","protocol"]` {
		t.Errorf("a tag list encodes as %q (present=%v), want [\"go\",\"protocol\"]", raw, ok)
	}
}
