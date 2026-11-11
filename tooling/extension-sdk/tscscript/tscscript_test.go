package tscscript

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		script string
		ok     bool
		want   Command
	}{
		{"tsc", true, Command{Mode: ModeDefault}},
		{"  tsc  ", true, Command{Mode: ModeDefault}},
		{"tsc -b", true, Command{Mode: ModeBuild}},
		{"tsc --build", true, Command{Mode: ModeBuild}},
		{"tsc -b tsconfig.lib.json", true, Command{Mode: ModeBuild, Config: "tsconfig.lib.json"}},
		{"tsc --build --verbose", true, Command{Mode: ModeBuild, Flags: []string{"--verbose"}}},
		{"tsc -p tsconfig.build.json", true, Command{Mode: ModeProject, Config: "tsconfig.build.json"}},
		{"tsc --project ./config/tsconfig.json --pretty", true, Command{Mode: ModeProject, Config: "config/tsconfig.json", Flags: []string{"--pretty"}}},
		{"tsc -p .", true, Command{Mode: ModeProject, Config: "."}},
		{"tsc -p", false, Command{}},
		{"tsc -b a.json b.json", false, Command{}},
		{"tsc -b -p tsconfig.json", false, Command{}},
		{"tsc -w", false, Command{}},
		{"tsc --watch", false, Command{}},
		{"tsc --noEmit", false, Command{}},
		{"tsc -b --force", false, Command{}},
		{"tsc -b --clean", false, Command{}},
		{"tsc --outDir dist", false, Command{}},
		{"tsc src/index.ts", false, Command{}},
		{"tsc -p ../tsconfig.json", false, Command{}},
		{"tsc -p /abs/tsconfig.json", false, Command{}},
		{"tsc && cp README.md dist", false, Command{}},
		{"tsc; echo done", false, Command{}},
		{"tsc | tee log", false, Command{}},
		{"tsc > log", false, Command{}},
		{"NODE_ENV=production tsc", false, Command{}},
		{"rimraf dist && tsc", false, Command{}},
		{"tsc -p 'tsconfig.json'", false, Command{}},
		{"tsc -p $CONFIG", false, Command{}},
		{"tsup", false, Command{}},
		{"bun build src/index.ts", false, Command{}},
		{"npx tsc", false, Command{}},
		{"", false, Command{}},
	}
	for _, c := range cases {
		got, ok := Parse(c.script)
		if ok != c.ok {
			t.Errorf("Parse(%q) ok = %v, want %v", c.script, ok, c.ok)
			continue
		}
		if ok && !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q) = %+v, want %+v", c.script, got, c.want)
		}
		if Runs(c.script) != c.ok {
			t.Errorf("Runs(%q) disagrees with Parse", c.script)
		}
	}
}

func TestArgs(t *testing.T) {
	cases := []struct {
		script string
		want   []string
	}{
		{"tsc", nil},
		{"tsc -b", []string{"-b"}},
		{"tsc --build tsconfig.lib.json --verbose", []string{"-b", "tsconfig.lib.json", "--verbose"}},
		{"tsc --project tsconfig.json --pretty", []string{"-p", "tsconfig.json", "--pretty"}},
	}
	for _, c := range cases {
		cmd, ok := Parse(c.script)
		if !ok {
			t.Fatalf("Parse(%q) refused", c.script)
		}
		if got := cmd.Args(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q).Args() = %q, want %q", c.script, got, c.want)
		}
	}
}
