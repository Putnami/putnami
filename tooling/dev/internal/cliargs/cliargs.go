// Package cliargs reads the arguments putnami forwards to an interactive
// subcommand of this runtime: the user's positionals and command flags, in
// the order the user wrote them. putnami has already consumed its global
// flags and split every inline value it recognized, but it passes an
// undeclared flag through, so a command still refuses a flag it does not
// accept.
package cliargs

import (
	"strings"
	"unicode"

	protocolcli "go.putnami.dev/protocol/cli"
)

// Flag is one flag a command accepts.
type Flag struct {
	// Long is the flag's spelling, such as --port.
	Long string
	// ValueName names the flag's value in a usage message, such as <n>. A
	// flag without one is a switch.
	ValueName string
}

// Values are the arguments of one invocation.
type Values struct {
	// Positionals are the arguments that are not flags or flag values, in
	// order, followed by every argument after a `--` separator.
	Positionals []string
	present     map[string]bool
	values      map[string]string
}

// Has reports whether the invocation names the flag spelled long.
func (v Values) Has(long string) bool {
	return v.present[long]
}

// Value returns the value of the flag spelled long, or "" when the
// invocation does not name it.
func (v Values) Value(long string) string {
	return v.values[long]
}

// Parse reads args as the arguments of `putnami <command>`, which accepts
// flags. A flag putnami reads itself is not among args. An unknown flag, a
// value given to a switch and a value flag without its value are usage
// errors.
func Parse(command string, args []string, flags []Flag) (Values, error) {
	parsed := Values{present: map[string]bool{}, values: map[string]string{}}
	words, rest := args, []string(nil)
	for i, arg := range args {
		if arg == "--" {
			words, rest = args[:i], args[i+1:]
			break
		}
	}
	for len(words) > 0 {
		arg := words[0]
		words = words[1:]
		if !isFlagToken(arg) {
			parsed.Positionals = append(parsed.Positionals, arg)
			continue
		}
		spelling, inline, hasInline := strings.Cut(arg, "=")
		flag, ok := lookup(flags, spelling)
		if !ok {
			return parsed, protocolcli.Usagef("unknown flag %s for `putnami %s`%s\n  Run `putnami help %s` for the accepted flags.",
				spelling, command, acceptedHint(flags), command)
		}
		if flag.ValueName == "" {
			if hasInline {
				return parsed, protocolcli.Usagef("flag %s is a switch: write %s, not %s", spelling, spelling, arg)
			}
			parsed.present[spelling] = true
			continue
		}
		value := inline
		if !hasInline {
			if len(words) == 0 {
				return parsed, protocolcli.Usagef("flag %s requires a value: %s %s", spelling, flag.Long, flag.ValueName)
			}
			value, words = words[0], words[1:]
		}
		parsed.present[spelling] = true
		parsed.values[spelling] = value
	}
	parsed.Positionals = append(parsed.Positionals, rest...)
	return parsed, nil
}

// isFlagToken reports whether arg is a flag: one or more dashes followed by a
// letter or an underscore. A negative number such as -5s is not a flag.
func isFlagToken(arg string) bool {
	trimmed := strings.TrimLeft(arg, "-")
	if trimmed == "" || trimmed == arg {
		return false
	}
	first := rune(trimmed[0])
	return first == '_' || unicode.IsLetter(first)
}

func lookup(flags []Flag, spelling string) (Flag, bool) {
	for _, flag := range flags {
		if flag.Long == spelling {
			return flag, true
		}
	}
	return Flag{}, false
}

func acceptedHint(flags []Flag) string {
	if len(flags) == 0 {
		return ""
	}
	names := make([]string, 0, len(flags))
	for _, flag := range flags {
		names = append(names, flag.Long)
	}
	return "\n  Accepted: " + strings.Join(names, ", ")
}
