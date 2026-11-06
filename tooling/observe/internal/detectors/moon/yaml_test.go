package moon

import (
	"reflect"
	"testing"
)

func TestTopLevelReadsTheValueOfOneKey(t *testing.T) {
	cases := map[string]struct {
		text string
		key  string
		want any
	}{
		"a block list beside other keys": {
			text: "$schema: 'x'\nprojects:\n  - 'apps/*'   # apps\n  - \"libs/*\"\nvcs:\n  manager: git\n",
			key:  "projects",
			want: []any{"apps/*", "libs/*"},
		},
		"a list at the key's indentation": {
			text: "projects:\n- apps/*\n- libs/*\nnext: 1\n",
			key:  "projects",
			want: []any{"apps/*", "libs/*"},
		},
		"a flow list across lines": {
			text: "projects: [\n  'apps/*',\n  'libs/*',\n]\n",
			key:  "projects",
			want: []any{"apps/*", "libs/*"},
		},
		"a nested mapping": {
			text: "projects:\n  globs: ['apps/*']\n  sources:\n    'web': apps/web\n",
			key:  "projects",
			want: map[string]any{"globs": []any{"apps/*"}, "sources": map[string]any{"web": "apps/web"}},
		},
		"compact mappings and flow mappings in a list": {
			text: "dependsOn:\n  - id: 'a'\n    scope: 'build'\n  - { id: \"b\", scope: peer }\n  - c\n",
			key:  "dependsOn",
			want: []any{map[string]any{"id": "a", "scope": "build"}, map[string]any{"id": "b", "scope": "peer"}, "c"},
		},
		"block scalars and anchors elsewhere are not read": {
			text: "description: |\n  dependsOn: [x]\n  it's # not a comment\ntasks:\n  build: &build\n    command: a\n  test: *build\ndependsOn: ['core'] # edge\n",
			key:  "dependsOn",
			want: []any{"core"},
		},
		"quotes and a plain apostrophe": {
			text: "id: don't # comment\n",
			key:  "id",
			want: "don't",
		},
		"an empty value": {
			text: "dependsOn:\nid: x\n",
			key:  "dependsOn",
			want: nil,
		},
		"a multi-line quoted scalar": {
			text: "tasks:\n  build:\n    command: 'cargo build\n      --release'\n    deps: ['core:build']\n",
			key:  "tasks",
			want: map[string]any{"build": map[string]any{"command": "cargo build --release", "deps": []any{"core:build"}}},
		},
		"a directive before the document start": {
			text: "%YAML 1.2\n---\nid: a\n",
			key:  "id",
			want: "a",
		},
		"a document start": {
			text: "---\nid: 'a''b'\n",
			key:  "id",
			want: "a'b",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, found, err := topLevel(c.text, c.key)
			if err != nil || !found {
				t.Fatalf("found=%v err=%v", found, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v, want %#v", got, c.want)
			}
		})
	}
	if _, found, err := topLevel("tasks:\n  dependsOn: [x]\n", "dependsOn"); found || err != nil {
		t.Fatalf("a nested key is read as a top-level one: %v %v", found, err)
	}
	if _, found, err := topLevel("id: a\n---\ndependsOn: [b]\n", "dependsOn"); found || err != nil {
		t.Fatalf("a key of the second document is read: %v %v", found, err)
	}
	for name, text := range map[string]string{
		"an alias":            "dependsOn: *deps\n",
		"an unclosed list":    "dependsOn: [a, b\n",
		"a duplicated key":    "dependsOn:\n  a: 1\n  a: 2\n",
		"an unterminated":     "dependsOn: 'a\n",
		"a stray line":        "dependsOn:\n    - a\n  - b\n",
		"garbage after flow":  "dependsOn: [a] b\n",
		"a tag":               "dependsOn: !!seq [a]\n",
		"a bad double quoted": "dependsOn: \"a\\q\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := topLevel(text, "dependsOn"); err == nil {
				t.Fatal("no error")
			}
		})
	}
}
