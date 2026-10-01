package clientcontract

import "testing"

func TestRPCName(t *testing.T) {
	cases := []struct {
		method, path string
		want         string
	}{
		{"GET", "/users", "ListUsers"},
		{"GET", "/users/{id}", "GetUsers"},
		{"POST", "/users", "CreateUsers"},
		{"PUT", "/users/{id}", "UpdateUsers"},
		{"PATCH", "/users/{id}", "UpdateUsers"},
		{"DELETE", "/users/{id}", "DeleteUsers"},
		{"GET", "/admin/projects/{projectId}", "GetAdminProjects"},
		{"get", "/api-keys/{id}/rotate_now", "GetApiKeysRotateNow"},
		{"POST", "/", "CreateResource"},
		{"GET", "/{id}", "GetResource"},
		{"STREAM", "/events", "StreamEvents"},
	}
	for _, c := range cases {
		if got := RPCName(c.method, c.path); got != c.want {
			t.Errorf("RPCName(%q, %q) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

func TestUpperCamel(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"users":          "Users",
		"api-keys":       "ApiKeys",
		"a.b_c d":        "ABCD",
		"--x--":          "X",
		"alreadyCamel":   "AlreadyCamel",
		"projectId...":   "ProjectId",
		"release_set.v2": "ReleaseSetV2",
	}
	for input, want := range cases {
		if got := UpperCamel(input); got != want {
			t.Errorf("UpperCamel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestEnumMember(t *testing.T) {
	cases := []struct {
		enum, value, want string
	}{
		{"WidgetState", "WIDGET_STATE_ACTIVE", "active"},
		{"WidgetState", "WIDGET_STATE_ON_HOLD", "on_hold"},
		{"WidgetState", "ACTIVE", "active"},
		{"State", "STATE_UNSPECIFIED", "unspecified"},
	}
	for _, c := range cases {
		if got := EnumMember(c.enum, c.value); got != c.want {
			t.Errorf("EnumMember(%q, %q) = %q, want %q", c.enum, c.value, got, c.want)
		}
	}
	if got := ScreamingSnake("WidgetState"); got != "WIDGET_STATE" {
		t.Errorf("ScreamingSnake(WidgetState) = %q, want WIDGET_STATE", got)
	}
}
