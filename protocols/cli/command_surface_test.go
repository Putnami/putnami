package cli

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The command-surface document has two representations that must move
// together: these Go types and schemas/command-surface.json. Go is its only
// runtime, so its corpus lives in testdata rather than in the cross-language
// conformance directory. Both files are embedded, so the test's cache key
// covers them.

//go:embed schemas/command-surface.json
var commandSurfaceSchema []byte

//go:embed testdata/command-surface/documents.json
var commandSurfaceCorpus []byte

func TestCommandSurfaceCorpus(t *testing.T) {
	t.Parallel()
	var corpus []struct {
		Name     string          `json:"name"`
		Valid    bool            `json:"valid"`
		Document json.RawMessage `json:"document"`
		Raw      string          `json:"raw"`
	}
	if err := json.Unmarshal(commandSurfaceCorpus, &corpus); err != nil {
		t.Fatal(err)
	}
	valid, invalid := 0, 0
	for _, c := range corpus {
		t.Run(c.Name, func(t *testing.T) {
			data := []byte(c.Raw)
			if c.Document != nil {
				data = c.Document
			}
			surface, err := ParseCommandSurface(data)
			if (err == nil) != c.Valid {
				t.Fatalf("valid=%v error=%v", c.Valid, err)
			}
			if err != nil {
				return
			}
			// A valid document is canonical: it renders back to the same JSON value.
			rendered, err := MarshalCommandSurface(*surface)
			if err != nil {
				t.Fatalf("render a parsed document: %v", err)
			}
			var want, got any
			if err := json.Unmarshal(data, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(rendered, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("rendered %s, want the parsed value back", rendered)
			}
		})
		if c.Valid {
			valid++
		} else {
			invalid++
		}
	}
	if valid == 0 || invalid == 0 {
		t.Fatalf("corpus must pin both verdicts: %d valid, %d invalid", valid, invalid)
	}
}

func TestCommandSurfaceSchemaDrift(t *testing.T) {
	t.Parallel()
	var schema struct {
		ID   string `json:"$id"`
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(commandSurfaceSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.ID != CommandSurfaceSchemaID {
		t.Fatalf("schema $id = %q, constant = %q", schema.ID, CommandSurfaceSchemaID)
	}
	for name, testCase := range map[string]struct {
		value    any
		optional []string
	}{
		"document":   {value: CommandSurface{}},
		"command":    {value: CommandSurfaceCommand{}},
		"flag":       {value: CommandSurfaceFlag{}, optional: []string{"short", "values"}},
		"positional": {value: CommandSurfacePositional{}},
	} {
		def, ok := schema.Defs[name]
		if !ok {
			t.Fatalf("schema has no $defs/%s", name)
		}
		members := jsonFieldNames(t, testCase.value)
		if got := keys(def.Properties); !reflect.DeepEqual(got, members) {
			t.Errorf("%s members: schema %v, Go %v", name, got, members)
		}
		var required []string
		for _, member := range members {
			if !slices.Contains(testCase.optional, member) {
				required = append(required, member)
			}
		}
		if got := sortedStrings(append([]string(nil), def.Required...)); !reflect.DeepEqual(got, required) {
			t.Errorf("%s required: schema %v, want %v", name, got, required)
		}
	}

	var bounds struct {
		Defs struct {
			Document struct {
				Properties struct {
					ProtocolVersion struct {
						Const int `json:"const"`
					} `json:"protocolVersion"`
				} `json:"properties"`
			} `json:"document"`
			Command struct {
				Properties struct {
					Path struct {
						Pattern string `json:"pattern"`
					} `json:"path"`
				} `json:"properties"`
			} `json:"command"`
			Flag struct {
				Properties struct {
					Long struct {
						Pattern string `json:"pattern"`
					} `json:"long"`
					Short struct {
						Pattern string `json:"pattern"`
					} `json:"short"`
					Type struct {
						Enum []string `json:"enum"`
					} `json:"type"`
					Values struct {
						Items struct {
							Pattern string `json:"pattern"`
						} `json:"items"`
					} `json:"values"`
				} `json:"properties"`
			} `json:"flag"`
			Positional struct {
				Properties struct {
					Name struct {
						Pattern string `json:"pattern"`
					} `json:"name"`
				} `json:"properties"`
			} `json:"positional"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(commandSurfaceSchema, &bounds); err != nil {
		t.Fatal(err)
	}
	if got := bounds.Defs.Document.Properties.ProtocolVersion.Const; got != CommandSurfaceVersion {
		t.Errorf("schema protocolVersion const = %d, CommandSurfaceVersion = %d", got, CommandSurfaceVersion)
	}
	if got, want := bounds.Defs.Flag.Properties.Type.Enum, []string{CommandFlagBool, CommandFlagValue}; !reflect.DeepEqual(got, want) {
		t.Errorf("schema flag type enum = %v, Go vocabulary = %v", got, want)
	}
	// The schema anchors the end of input with a lookahead, because "$" may
	// match before a final newline in other regular expression dialects.
	schemaPattern := func(goPattern string) string { return strings.TrimSuffix(goPattern, "$") + `(?![\s\S])` }
	for name, pair := range map[string][2]string{
		"command path":    {bounds.Defs.Command.Properties.Path.Pattern, commandPathPattern},
		"flag long":       {bounds.Defs.Flag.Properties.Long.Pattern, commandLongPattern},
		"flag short":      {bounds.Defs.Flag.Properties.Short.Pattern, commandShortPattern},
		"flag value":      {bounds.Defs.Flag.Properties.Values.Items.Pattern, commandTokenPattern},
		"positional name": {bounds.Defs.Positional.Properties.Name.Pattern, commandTokenPattern},
	} {
		if want := schemaPattern(pair[1]); pair[0] != want {
			t.Errorf("schema %s pattern = %q, want %q", name, pair[0], want)
		}
	}
}

func TestNewCommandSurfaceIsCanonical(t *testing.T) {
	t.Parallel()
	values := []string{"tag", "push"}
	commands := []CommandSurfaceCommand{
		{Path: "projects list", Flags: []CommandSurfaceFlag{
			{Long: "--tag", Type: CommandFlagValue},
			{Long: "--event", Type: CommandFlagValue, Values: values},
		}},
		{Path: "build"},
	}
	globals := []CommandSurfaceFlag{{Long: "--verbose", Short: "-v", Type: CommandFlagBool}, {Long: "--json", Type: CommandFlagBool}}

	surface, err := NewCommandSurface(globals, commands)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{surface.Commands[0].Path, surface.Commands[1].Path}; !slices.Equal(got, []string{"build", "projects list"}) {
		t.Fatalf("commands = %v, want them sorted by path", got)
	}
	if surface.GlobalFlags[0].Long != "--json" || surface.Commands[1].Flags[0].Long != "--event" {
		t.Fatalf("flags are not sorted by long name: %+v", surface)
	}
	if got := surface.Commands[1].Flags[0].Values; !slices.Equal(got, []string{"push", "tag"}) {
		t.Fatalf("values = %v, want them sorted", got)
	}
	if !slices.Equal(values, []string{"tag", "push"}) || commands[0].Path != "projects list" {
		t.Fatal("NewCommandSurface reordered its caller's slices")
	}
	if surface.Commands[0].Flags == nil || surface.Commands[0].Positionals == nil {
		t.Fatal("a command without flags or positionals must carry empty lists")
	}

	first, err := MarshalCommandSurface(surface)
	if err != nil {
		t.Fatal(err)
	}
	again, err := NewCommandSurface(globals, slices.Clone(commands))
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalCommandSurface(again)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || !bytes.HasSuffix(first, []byte("}\n")) {
		t.Fatalf("the same surface rendered different bytes:\n%s\n%s", first, second)
	}
	if !bytes.Contains(first, []byte(`"path": "build",`)) {
		t.Fatalf("rendering is not indented with two spaces:\n%s", first)
	}
	parsed, err := ParseCommandSurface(first)
	if err != nil {
		t.Fatalf("the rendered document does not parse: %v", err)
	}
	if !reflect.DeepEqual(*parsed, surface) {
		t.Fatalf("parsed %+v, want %+v", *parsed, surface)
	}
}

func TestNewCommandSurfaceRefusesADuplicate(t *testing.T) {
	t.Parallel()
	for name, commands := range map[string][]CommandSurfaceCommand{
		"command": {{Path: "build"}, {Path: "build"}},
		"flag":    {{Path: "build", Flags: []CommandSurfaceFlag{{Long: "--all", Type: CommandFlagBool}, {Long: "--all", Type: CommandFlagBool}}}},
		"short alias": {{Path: "build", Flags: []CommandSurfaceFlag{
			{Long: "--all", Short: "-a", Type: CommandFlagBool}, {Long: "--any", Short: "-a", Type: CommandFlagBool},
		}}},
		"value": {{Path: "build", Flags: []CommandSurfaceFlag{{Long: "--mode", Type: CommandFlagValue, Values: []string{"a", "a"}}}}},
	} {
		if _, err := NewCommandSurface(nil, commands); err == nil {
			t.Errorf("a duplicate %s was accepted", name)
		}
	}
	if _, err := MarshalCommandSurface(CommandSurface{ProtocolVersion: CommandSurfaceVersion}); err == nil {
		t.Error("a surface without its lists was rendered")
	}
}

func TestParseCommandSurfaceRefusesAnOversizedDocument(t *testing.T) {
	t.Parallel()
	document := []byte(`{"protocolVersion":1,"globalFlags":[],"commands":[]}`)
	padded := append(bytes.Repeat([]byte(" "), CommandSurfaceMaxBytes), document...)
	if _, err := ParseCommandSurface(padded); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want the size bound", err)
	}
}

// surfaceFixture is the released surface the diff cases change.
func surfaceFixture(t *testing.T) CommandSurface {
	t.Helper()
	surface, err := NewCommandSurface(
		[]CommandSurfaceFlag{
			{Long: "--output", Type: CommandFlagValue, Values: []string{"json", "text"}},
			{Long: "--verbose", Short: "-v", Type: CommandFlagBool},
		},
		[]CommandSurfaceCommand{
			{Path: "build"},
			{Path: "ci init", Flags: []CommandSurfaceFlag{
				{Long: "--force", Short: "-f", Type: CommandFlagBool},
				{Long: "--event", Type: CommandFlagValue, Values: []string{"pull_request", "push"}},
				{Long: "--tag", Type: CommandFlagValue},
			}},
			{Path: "projects tag", Positionals: []CommandSurfacePositional{{Name: "name", Required: true}, {Name: "tag"}}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return surface
}

func surfaceCommand(surface *CommandSurface, path string) *CommandSurfaceCommand {
	for i := range surface.Commands {
		if surface.Commands[i].Path == path {
			return &surface.Commands[i]
		}
	}
	return nil
}

func surfaceFlag(flags []CommandSurfaceFlag, long string) *CommandSurfaceFlag {
	for i := range flags {
		if flags[i].Long == long {
			return &flags[i]
		}
	}
	return nil
}

func TestIncompatibleCommandChanges(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		change func(*CommandSurface)
		want   []CommandChange
	}{
		{
			name: "a removed command",
			change: func(s *CommandSurface) {
				s.Commands = slices.DeleteFunc(s.Commands, func(c CommandSurfaceCommand) bool { return c.Path == "build" })
			},
			want: []CommandChange{{Command: "build", Message: `command "build" removed`}},
		},
		{
			name: "a removed flag",
			change: func(s *CommandSurface) {
				c := surfaceCommand(s, "ci init")
				c.Flags = slices.DeleteFunc(c.Flags, func(f CommandSurfaceFlag) bool { return f.Long == "--force" })
			},
			want: []CommandChange{{Command: "ci init", Flag: "--force", Message: `command "ci init": flag --force removed`}},
		},
		{
			name: "a removed global flag",
			change: func(s *CommandSurface) {
				s.GlobalFlags = slices.DeleteFunc(s.GlobalFlags, func(f CommandSurfaceFlag) bool { return f.Long == "--verbose" })
			},
			want: []CommandChange{{Flag: "--verbose", Message: "global flag --verbose removed"}},
		},
		{
			name:   "a removed short alias",
			change: func(s *CommandSurface) { surfaceFlag(surfaceCommand(s, "ci init").Flags, "--force").Short = "" },
			want:   []CommandChange{{Command: "ci init", Flag: "--force", Message: `command "ci init": flag --force lost its short alias -f`}},
		},
		{
			name:   "a changed short alias",
			change: func(s *CommandSurface) { surfaceFlag(s.GlobalFlags, "--verbose").Short = "-V" },
			want:   []CommandChange{{Flag: "--verbose", Message: "global flag --verbose lost its short alias -v"}},
		},
		{
			name: "a bool flag that takes a value",
			change: func(s *CommandSurface) {
				surfaceFlag(surfaceCommand(s, "ci init").Flags, "--force").Type = CommandFlagValue
			},
			want: []CommandChange{{Command: "ci init", Flag: "--force", Message: `command "ci init": flag --force now takes a value`}},
		},
		{
			name: "a value flag that takes none",
			change: func(s *CommandSurface) {
				f := surfaceFlag(surfaceCommand(s, "ci init").Flags, "--event")
				f.Type, f.Values = CommandFlagBool, nil
			},
			want: []CommandChange{{Command: "ci init", Flag: "--event", Message: `command "ci init": flag --event no longer takes a value`}},
		},
		{
			name:   "a removed value",
			change: func(s *CommandSurface) { surfaceFlag(s.GlobalFlags, "--output").Values = []string{"json"} },
			want:   []CommandChange{{Flag: "--output", Message: `global flag --output no longer accepts "text"`}},
		},
		{
			name: "a closed list on a flag that accepted any value",
			change: func(s *CommandSurface) {
				surfaceFlag(surfaceCommand(s, "ci init").Flags, "--tag").Values = []string{"a", "b"}
			},
			want: []CommandChange{{Command: "ci init", Flag: "--tag", Message: `command "ci init": flag --tag now accepts only a, b`}},
		},
		{
			name:   "an optional positional that becomes required",
			change: func(s *CommandSurface) { surfaceCommand(s, "projects tag").Positionals[1].Required = true },
			want:   []CommandChange{{Command: "projects tag", Message: `command "projects tag": positional "tag" (position 2) is now required`}},
		},
		{
			name: "a new required positional",
			change: func(s *CommandSurface) {
				c := surfaceCommand(s, "build")
				c.Positionals = append(c.Positionals, CommandSurfacePositional{Name: "target", Required: true})
			},
			want: []CommandChange{{Command: "build", Message: `command "build": new required positional "target" (position 1)`}},
		},
		{
			name:   "a removed positional",
			change: func(s *CommandSurface) { c := surfaceCommand(s, "projects tag"); c.Positionals = c.Positionals[:1] },
			want:   []CommandChange{{Command: "projects tag", Message: `command "projects tag": positional "tag" (position 2) removed`}},
		},
		{
			name: "additions",
			change: func(s *CommandSurface) {
				s.Commands = append(s.Commands, CommandSurfaceCommand{Path: "deploy", Flags: []CommandSurfaceFlag{}, Positionals: []CommandSurfacePositional{}})
				c := surfaceCommand(s, "ci init")
				c.Flags = append(c.Flags, CommandSurfaceFlag{Long: "--yes", Short: "-y", Type: CommandFlagBool})
				surfaceFlag(c.Flags, "--event").Values = []string{"pull_request", "push", "tag"}
				s.GlobalFlags = append(s.GlobalFlags, CommandSurfaceFlag{Long: "--watch", Type: CommandFlagBool})
				surfaceFlag(s.GlobalFlags, "--output").Short = "-o"
				b := surfaceCommand(s, "build")
				b.Positionals = append(b.Positionals, CommandSurfacePositional{Name: "target"})
			},
		},
		{
			name: "relaxations",
			change: func(s *CommandSurface) {
				surfaceFlag(surfaceCommand(s, "ci init").Flags, "--event").Values = nil
				positionals := surfaceCommand(s, "projects tag").Positionals
				positionals[0].Required = false
				positionals[0].Name = "project"
			},
		},
		{
			name:   "no change",
			change: func(*CommandSurface) {},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			previous := surfaceFixture(t)
			current := surfaceFixture(t)
			testCase.change(&current)
			if got := IncompatibleCommandChanges(previous, current); !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("changes = %#v\nwant      %#v", got, testCase.want)
			}
		})
	}
}

// Changes come back in one order whatever order the edits were made in, and a
// removed command or flag is one change, not one per member it held.
func TestIncompatibleCommandChangesAreSortedAndNotRepeated(t *testing.T) {
	t.Parallel()
	previous := surfaceFixture(t)
	current := surfaceFixture(t)
	current.Commands = slices.DeleteFunc(current.Commands, func(c CommandSurfaceCommand) bool { return c.Path == "projects tag" })
	initCommand := surfaceCommand(&current, "ci init")
	initCommand.Flags = slices.DeleteFunc(initCommand.Flags, func(f CommandSurfaceFlag) bool { return f.Long == "--event" })
	surfaceFlag(initCommand.Flags, "--force").Short = ""
	surfaceFlag(current.GlobalFlags, "--output").Values = []string{"yaml"}

	got := IncompatibleCommandChanges(previous, current)
	want := []CommandChange{
		{Flag: "--output", Message: `global flag --output no longer accepts "json"`},
		{Flag: "--output", Message: `global flag --output no longer accepts "text"`},
		{Command: "ci init", Flag: "--event", Message: `command "ci init": flag --event removed`},
		{Command: "ci init", Flag: "--force", Message: `command "ci init": flag --force lost its short alias -f`},
		{Command: "projects tag", Message: `command "projects tag" removed`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %#v\nwant      %#v", got, want)
	}
}
