package cli

import (
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// The reserved global-flag registry in go.putnami.dev/protocol/cli is the
// single source of truth for the flags that are part of the cross-surface CLI
// contract. These tests bind the framework CLI's own reserved-flag handling to
// that registry so the two cannot drift: every reserved global must be consumed
// by the parser and surfaced in the help catalog.

// TestFrameworkParserConsumesReservedGlobals asserts the typed global-flag
// parser recognizes every reserved global flag (long form, short form, and the
// --no- form for negatable flags) rather than leaking it to job args.
func TestFrameworkParserConsumesReservedGlobals(t *testing.T) {
	t.Parallel()
	for _, f := range protocolcli.ReservedGlobalFlags() {
		long := []string{"--" + f.Name}
		if f.TakesValue {
			long = append(long, "value")
		}
		if _, remaining, err := parseFlags(long); err != nil || len(remaining) != 0 {
			t.Errorf("parser does not consume reserved global --%s (remaining=%v, err=%v)", f.Name, remaining, err)
		}

		if f.Short != "" {
			if _, remaining, err := parseFlags([]string{"-" + f.Short}); err != nil || len(remaining) != 0 {
				t.Errorf("parser does not consume short form -%s of --%s (remaining=%v, err=%v)", f.Short, f.Name, remaining, err)
			}
		}

		if f.Negatable {
			if _, remaining, err := parseFlags([]string{"--no-" + f.Name}); err != nil || len(remaining) != 0 {
				t.Errorf("parser does not consume --no-%s (remaining=%v, err=%v)", f.Name, remaining, err)
			}
		}
	}
}

// TestHelpCatalogCoversReservedGlobals asserts every reserved global flag is
// documented in the help flag catalog, so help never omits a contract flag.
// --version is the sole exception: it is printed by the version path, not the
// flag catalog.
func TestHelpCatalogCoversReservedGlobals(t *testing.T) {
	t.Parallel()
	inCatalog := map[string]bool{}
	for _, cat := range allFlagCategories {
		for _, fi := range cat.Flags {
			inCatalog[strings.TrimPrefix(fi.Long, "--")] = true
		}
	}
	for _, f := range protocolcli.ReservedGlobalFlags() {
		if f.Name == "version" {
			continue
		}
		if !inCatalog[f.Name] {
			t.Errorf("help flag catalog is missing reserved global --%s", f.Name)
		}
	}
}
