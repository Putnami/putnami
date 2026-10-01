package envkeys

import (
	"runtime"
	"slices"
	"testing"
)

// The Windows system block spells the variable "Path". A folding match reads,
// replaces and removes it under any spelling, and never mistakes PATHEXT for
// PATH.
func TestFoldMatchesEverySpellingOfOneVariable(t *testing.T) {
	keys := Keys{Fold: true}
	block := []string{"ComSpec=cmd", "Path=system", "PATHEXT=.COM;.EXE", "path=later"}

	if got := keys.Last(block, "PATH"); got != "later" {
		t.Fatalf("Last(PATH) = %q, want the last spelling's value", got)
	}
	if value, ok := keys.Value("Path=system", "PATH"); !ok || value != "system" {
		t.Fatalf("Value(Path=system, PATH) = %q, %v", value, ok)
	}
	if _, ok := keys.Value("PATHEXT=.COM", "PATH"); ok {
		t.Fatal("PATHEXT matched PATH")
	}
	want := []string{"ComSpec=cmd", "PATHEXT=.COM;.EXE", "PATH=mine"}
	if got := keys.Set(block, "PATH", "mine"); !slices.Equal(got, want) {
		t.Fatalf("Set = %q, want %q", got, want)
	}
	if got := keys.Remove(block, "pathext"); slices.Contains(got, "PATHEXT=.COM;.EXE") || len(got) != 3 {
		t.Fatalf("Remove(pathext) = %q", got)
	}
	if got := keys.Canonical("Path"); got != "PATH" {
		t.Fatalf("Canonical(Path) = %q, want PATH", got)
	}
}

// On Unix names are case-sensitive: "Path" is another variable.
func TestExactTreatsOtherSpellingsAsOtherVariables(t *testing.T) {
	keys := Keys{}
	env := []string{"Path=/usr/bin", "HOME=/home/dev"}

	if keys.Has(env, "PATH") {
		t.Fatal("exact matching found PATH in a Path entry")
	}
	if got := keys.Last(env, "PATH"); got != "" {
		t.Fatalf("Last(PATH) = %q, want empty", got)
	}
	want := []string{"Path=/usr/bin", "HOME=/home/dev", "PATH=/opt/bin"}
	if got := keys.Set(env, "PATH", "/opt/bin"); !slices.Equal(got, want) {
		t.Fatalf("Set = %q, want %q", got, want)
	}
	if got := keys.Canonical("Path"); got != "Path" {
		t.Fatalf("Canonical(Path) = %q, want Path", got)
	}
}

func TestRemoveReturnsTheSameEnvironmentWhenNothingMatches(t *testing.T) {
	env := []string{"HOME=/home/dev"}
	if got := (Keys{}).Remove(env, "PATH"); &got[0] != &env[0] {
		t.Fatal("removing an absent key copied the environment")
	}
	removed := (Keys{}).Remove([]string{"A=1", "B=2", "A=3"}, "A")
	if !slices.Equal(removed, []string{"B=2"}) {
		t.Fatalf("Remove(A) = %q, want [B=2]", removed)
	}
}

func TestSetDoesNotModifyItsInput(t *testing.T) {
	env := make([]string, 2, 8)
	env[0], env[1] = "A=1", "B=2"
	got := (Keys{}).Set(env, "A", "9")
	if !slices.Equal(env, []string{"A=1", "B=2"}) || !slices.Equal(got, []string{"B=2", "A=9"}) {
		t.Fatalf("env = %q, Set = %q", env, got)
	}
	if &got[0] == &env[0] {
		t.Fatal("Set shares the backing array of its input")
	}
}

// An entry without "=" assigns nothing, and an empty value is a value.
func TestValueNeedsAnAssignment(t *testing.T) {
	if _, ok := (Keys{}).Value("PATH", "PATH"); ok {
		t.Fatal("an entry without = assigned PATH")
	}
	if value, ok := (Keys{}).Value("PATH=", "PATH"); !ok || value != "" {
		t.Fatalf("Value(PATH=) = %q, %v, want empty and true", value, ok)
	}
	if value, ok := (Keys{}).Value("A=b=c", "A"); !ok || value != "b=c" {
		t.Fatalf("Value(A=b=c) = %q, %v", value, ok)
	}
}

func TestHostFoldsOnlyOnWindows(t *testing.T) {
	if Host.Fold != (runtime.GOOS == "windows") {
		t.Fatalf("Host.Fold = %v on %s", Host.Fold, runtime.GOOS)
	}
}
