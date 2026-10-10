package clicore

import (
	"slices"
	"testing"
)

func TestPositionalsSkipFlagValuesAndTheParentContext(t *testing.T) {
	RegisterBooleanFlags("argv-test-switch")
	argv := []string{
		"packages", "--putnamiContext", "/tmp/context.json", "--namespace", "@owner", "namespaces",
		"--argv-test-switch", "activate", "--output=json", "--no-cache", "last",
	}
	if got, want := Positionals(argv), []string{"packages", "namespaces", "activate", "last"}; !slices.Equal(got, want) {
		t.Fatalf("Positionals = %v, want %v", got, want)
	}
	if got := Positionals([]string{"--namespace", "--output=json"}); len(got) != 0 {
		t.Fatalf("Positionals of flags only = %v, want none", got)
	}
}

func TestDropFirstPositionalKeepsEveryFlagInPlace(t *testing.T) {
	argv := []string{"--namespace", "@owner", "packages", "namespaces", "--output=json"}
	got := DropFirstPositional(argv)
	if want := []string{"--namespace", "@owner", "namespaces", "--output=json"}; !slices.Equal(got, want) {
		t.Fatalf("DropFirstPositional = %v, want %v", got, want)
	}
	if argv[2] != "packages" {
		t.Fatal("DropFirstPositional changed its input")
	}
	flagsOnly := []string{"--output=json"}
	if got := DropFirstPositional(flagsOnly); !slices.Equal(got, flagsOnly) {
		t.Fatalf("DropFirstPositional without a positional = %v, want %v", got, flagsOnly)
	}
}

func TestReplaceFirstPositionalRewritesOrAppendsTheVerb(t *testing.T) {
	argv := []string{"--env", "prod", "show", "my-app"}
	if got, want := ReplaceFirstPositional(argv, "list"), []string{"--env", "prod", "list", "my-app"}; !slices.Equal(got, want) {
		t.Fatalf("ReplaceFirstPositional = %v, want %v", got, want)
	}
	if argv[2] != "show" {
		t.Fatal("ReplaceFirstPositional changed its input")
	}
	if got, want := ReplaceFirstPositional([]string{"--output=json"}, "resolve"), []string{"--output=json", "resolve"}; !slices.Equal(got, want) {
		t.Fatalf("ReplaceFirstPositional without a positional = %v, want %v", got, want)
	}
}
