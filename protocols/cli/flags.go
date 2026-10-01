package cli

import "strings"

// ReservedFlag describes a global flag that is part of the CLI contract and is
// therefore reserved on every surface (the framework CLI, the extension SDK,
// and cloud's cli-core). A reserved flag means the same thing wherever it
// appears, and an extension must not define a command flag whose name collides
// with one.
type ReservedFlag struct {
	// Name is the long form without leading dashes, e.g. "output".
	Name string
	// Short is the single-character alias without a dash, e.g. "v"; empty when
	// the flag has no short form.
	Short string
	// TakesValue reports whether the flag consumes a following value
	// (--output json) rather than being a boolean presence flag (--verbose).
	TakesValue bool
	// Negatable reports whether a --no-<name> form is also reserved
	// (e.g. --color / --no-color).
	Negatable bool
	// Usage is a one-line description used in help and validation messages.
	Usage string
}

// reservedGlobalFlags is the canonical, ordered set of reserved global flags —
// the single source of truth every CLI surface handles as global contract
// flags, and that no extension may redefine.
//
// Framework-specific orchestration flags (project selection, cache control,
// parallelism, watch, …) are intentionally NOT here: they are not part of the
// cross-surface contract and remain private to the framework CLI.
var reservedGlobalFlags = []ReservedFlag{
	{Name: "output", TakesValue: true, Usage: "output mode: text | json | jsonl | cloud-logging"},
	{Name: "json", Usage: "shorthand for --output=json"},
	{Name: "help", Short: "h", Usage: "show help"},
	{Name: "version", Short: "V", Usage: "print the CLI version"},
	{Name: "verbose", Short: "v", Usage: "verbose output"},
	{Name: "debug", Short: "d", Usage: "debug output (implies --verbose)"},
	{Name: "quiet", Usage: "suppress non-essential output"},
	{Name: "color", Negatable: true, Usage: "colored output (--color / --no-color)"},
}

// ReservedGlobalFlags returns a copy of the reserved global-flag registry in
// display order. Callers may sort or filter the copy without affecting the
// canonical set.
func ReservedGlobalFlags() []ReservedFlag {
	out := make([]ReservedFlag, len(reservedGlobalFlags))
	copy(out, reservedGlobalFlags)
	return out
}

// normalizeFlagToken strips a leading "--" or "-" and any "=value" suffix,
// yielding the bare flag name: "--output=json" -> "output", "-v" -> "v",
// "output" -> "output". Flag names are case-sensitive, so no case folding.
func normalizeFlagToken(token string) string {
	name := strings.TrimPrefix(token, "--")
	if name == token { // no "--" prefix; strip a single leading "-"
		name = strings.TrimPrefix(name, "-")
	}
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	return name
}

// LookupReservedGlobalFlag resolves a raw flag token — any of "--output",
// "--output=json", "output", "-v", or "--no-color" — to its reserved-flag
// descriptor. The bool is false when the token does not name a reserved global.
func LookupReservedGlobalFlag(token string) (ReservedFlag, bool) {
	name := normalizeFlagToken(token)
	if name == "" {
		return ReservedFlag{}, false
	}
	for _, f := range reservedGlobalFlags {
		if name == f.Name || (f.Short != "" && name == f.Short) {
			return f, true
		}
		if f.Negatable && name == "no-"+f.Name {
			return f, true
		}
	}
	return ReservedFlag{}, false
}

// IsReservedGlobalFlag reports whether the raw flag token names a reserved
// global flag. It accepts long, short, negated, and "=value" forms. Surfaces
// use it to route reserved globals through the contract layer instead of
// passing them to a command/job as an ordinary flag.
func IsReservedGlobalFlag(token string) bool {
	_, ok := LookupReservedGlobalFlag(token)
	return ok
}

// ValidateNoReservedShadow returns an ErrUsage-classified error if any of the
// given flag names collides with a reserved global flag. Names may be given
// with or without leading dashes. Extensions call this to reject a command
// flag that would shadow a global. A nil result means no collision.
func ValidateNoReservedShadow(flagNames ...string) error {
	for _, n := range flagNames {
		if f, ok := LookupReservedGlobalFlag(n); ok {
			return Usagef("flag %q is reserved as a global CLI flag (%s) and cannot be redefined", "--"+f.Name, f.Usage)
		}
	}
	return nil
}
