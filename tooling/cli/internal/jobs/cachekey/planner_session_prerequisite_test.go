package cachekey

import (
	"reflect"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestSessionPrerequisiteParamProjectionMatchesDispatch(t *testing.T) {
	t.Parallel()
	for _, order := range [][]string{
		{"foo-bar", "fooBar", "foo--bar"},
		{"foo--bar", "fooBar", "foo-bar"},
	} {
		exact := make(extension.ParamMap)
		for _, name := range order {
			switch name {
			case "foo-bar":
				exact[name] = "kebab"
			case "foo--bar":
				exact[name] = "double-kebab"
			case "fooBar":
				exact[name] = "camel"
			}
		}
		projected, sources := ProjectParamAliases(exact)
		want := extension.ParamMap{
			"foo-bar":  "kebab",
			"foo--bar": "double-kebab",
			"fooBar":   "camel",
		}
		if !reflect.DeepEqual(projected, want) {
			t.Fatalf("projection for insertion order %v = %#v, want every exact key plus camel alias %#v", order, projected, want)
		}
		for name := range want {
			if sources[name] != name {
				t.Fatalf("projection source for exact %q = %q, want exact spelling", name, sources[name])
			}
		}
	}

	for _, order := range [][]string{
		{"foo-bar", "foo--bar"},
		{"foo--bar", "foo-bar"},
	} {
		exact := make(extension.ParamMap)
		for _, name := range order {
			exact[name] = name
		}
		projected, sources := ProjectParamAliases(exact)
		if got := projected["fooBar"]; got != "foo--bar" {
			t.Fatalf("collapsed alias for insertion order %v = %v, want deterministic sorted source foo--bar", order, got)
		}
		if sources["fooBar"] != "foo--bar" || projected["foo-bar"] != "foo-bar" || projected["foo--bar"] != "foo--bar" {
			t.Fatalf("multi-spelling projection for insertion order %v = %#v sources=%#v", order, projected, sources)
		}
	}

	job := &jobmodel.ScheduledJob{
		Project:   &workspace.Project{ID: "/A", Name: "A", Path: "A", Config: &wsproto.ProjectConfig{}},
		Extension: &extension.ExtensionDescription{Name: "@test/publisher"},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@test/publisher", Name: "publish",
			BoundParams: extension.ParamMap{"foo-bar": "bound-kebab"},
		},
	}
	params := ResolvedJobParams(job, extension.ParamMap{"fooBar": "exact-camel"}, nil)
	if params["foo-bar"] != "bound-kebab" || params["fooBar"] != "bound-kebab" {
		t.Fatalf("dispatch projection = %#v, want higher-precedence bound layer to expose both spellings", params)
	}
}
