package shared

import (
	"errors"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
)

// ParseProjectFlag extracts the value of a --project flag from args, accepting
// both "--project value" and "--project=value". It returns:
//   - ("", nil) when the flag is absent (callers apply their default target);
//   - (value, nil) when a non-empty value is given;
//   - ("", usage error) when --project is present but its value is MISSING —
//     trailing, followed by another flag, or an empty "--project=". A missing
//     value must never silently collapse to the flag-absent default (e.g.
//     "context pack --project --check" would otherwise target every project),
//     so it is a usage error, not "".
func ParseProjectFlag(args []string) (string, error) {
	missing := func() (string, error) {
		return "", protocolcli.Classify(
			errors.New("--project requires a value: pass --project <id|name|path>"),
			protocolcli.ErrUsage)
	}
	for i := range len(args) {
		arg := args[i]
		if arg == "--project" {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1], nil
			}
			return missing()
		}
		if v, ok := strings.CutPrefix(arg, "--project="); ok {
			if strings.TrimSpace(v) == "" {
				return missing()
			}
			return v, nil
		}
	}
	return "", nil
}
