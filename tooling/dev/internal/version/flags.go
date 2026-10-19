package version

import (
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
)

// flags are the flags of `version get` and `version tag`. A version is
// derived from git per line, so the only selector is the line.
type flags struct {
	Scope  string
	Push   bool
	DryRun bool
	Yes    bool
}

// parseFlags reads the flags of args and returns the other arguments in
// order. --scope takes the next argument or an inline value, without its
// leading and trailing slashes; a missing value is a usage error.
func parseFlags(args []string) (flags, []string, error) {
	var parsed flags
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--scope":
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				return parsed, nil, protocolcli.Usagef("--scope requires a value")
			}
			parsed.Scope = strings.Trim(args[i], "/")
		case strings.HasPrefix(arg, "--scope="):
			value := strings.TrimPrefix(arg, "--scope=")
			if value == "" || strings.HasPrefix(value, "-") {
				return parsed, nil, protocolcli.Usagef("--scope requires a value")
			}
			parsed.Scope = strings.Trim(value, "/")
		case arg == "--push":
			parsed.Push = true
		case arg == "--dry-run":
			parsed.DryRun = true
		case arg == "--yes":
			parsed.Yes = true
		default:
			rest = append(rest, arg)
		}
	}
	return parsed, rest, nil
}
