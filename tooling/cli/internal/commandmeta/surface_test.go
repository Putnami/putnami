package commandmeta

import (
	"slices"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

func surfaceEntry(t *testing.T, surface protocolcli.CommandSurface, path string) protocolcli.CommandSurfaceCommand {
	t.Helper()
	for _, command := range surface.Commands {
		if command.Path == path {
			return command
		}
	}
	t.Fatalf("the command surface has no command %q", path)
	return protocolcli.CommandSurfaceCommand{}
}

// Every spelling a user can type is a command of the surface: each catalog
// path, the alternate spellings and the job commands included, and each
// built-in alias.
func TestSurface_ListsEveryCommandUsersType(t *testing.T) {
	t.Parallel()
	surface, err := Surface()
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	paths := make([]string, 0, len(surface.Commands))
	for _, command := range surface.Commands {
		paths = append(paths, command.Path)
	}
	for _, command := range catalog {
		if !slices.Contains(paths, command.Path) {
			t.Errorf("catalog path %q is missing from the surface", command.Path)
		}
	}
	for _, alias := range builtinAliases {
		if !slices.Contains(paths, alias.Name) {
			t.Errorf("built-in alias %q is missing from the surface", alias.Name)
		}
	}
	if want := len(catalog) + len(builtinAliases); len(paths) != want {
		t.Errorf("the surface lists %d commands, want %d", len(paths), want)
	}
	if len(surface.GlobalFlags) != len(globalFlags) {
		t.Errorf("the surface lists %d global flags, want %d", len(surface.GlobalFlags), len(globalFlags))
	}
}

// An alternate spelling accepts what its canonical path accepts, and an alias
// runs its command.
func TestSurface_AnAlternateSpellingSharesItsCommandsSurface(t *testing.T) {
	t.Parallel()
	surface, err := Surface()
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	for _, command := range catalog {
		if command.DetailFrom == "" {
			continue
		}
		alternate, canonical := surfaceEntry(t, surface, command.Path), surfaceEntry(t, surface, command.DetailFrom)
		if !slices.EqualFunc(alternate.Flags, canonical.Flags, flagsEqual) || !slices.Equal(alternate.Positionals, canonical.Positionals) {
			t.Errorf("%q does not carry the flags and positionals of %q", command.Path, command.DetailFrom)
		}
	}
	for _, alias := range builtinAliases {
		short, full := surfaceEntry(t, surface, alias.Name), surfaceEntry(t, surface, alias.Command)
		if !slices.EqualFunc(short.Flags, full.Flags, flagsEqual) || !slices.Equal(short.Positionals, full.Positionals) {
			t.Errorf("alias %q does not carry the surface of %q", alias.Name, alias.Command)
		}
	}
}

func flagsEqual(a, b protocolcli.CommandSurfaceFlag) bool {
	return a.Long == b.Long && a.Short == b.Short && a.Type == b.Type && slices.Equal(a.Values, b.Values)
}

// A closed list is what the parser enforces: a command flag's Values, never a
// global flag's completion candidates.
func TestSurface_ValuesAreTheEnforcedOnes(t *testing.T) {
	t.Parallel()
	surface, err := Surface()
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	for _, flag := range surface.GlobalFlags {
		if flag.Values != nil {
			t.Errorf("global flag %s carries values %v; completion candidates are not a closed list", flag.Long, flag.Values)
		}
	}
	for _, command := range catalog {
		declared, _ := ResolveFlags(command.Path)
		entry := surfaceEntry(t, surface, command.Path)
		for _, flag := range declared {
			index := slices.IndexFunc(entry.Flags, func(f protocolcli.CommandSurfaceFlag) bool { return f.Long == flag.Long })
			if index < 0 {
				t.Errorf("%q: flag %s is missing from the surface", command.Path, flag.Long)
				continue
			}
			want := slices.Sorted(slices.Values(flag.Values))
			if got := entry.Flags[index].Values; !slices.Equal(got, want) || (got == nil) != (len(flag.Values) == 0) {
				t.Errorf("%q: flag %s values = %v, want %v", command.Path, flag.Long, got, want)
			}
		}
	}
}
