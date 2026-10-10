package clicore

import (
	"fmt"
	"strings"
)

// booleanFlags is the set of flag names that stand alone (take no following
// value). The CLI registers its flags once at startup via RegisterBooleanFlags;
// ParseFlags and FirstPositional then know which flags consume the next argument
// and which don't — without this package hardcoding any command's flag names.
var booleanFlags = map[string]struct{}{}

// RegisterBooleanFlags marks names as value-less boolean flags. It is additive
// and safe to call repeatedly (e.g. from a consumer's package init).
func RegisterBooleanFlags(names ...string) {
	for _, name := range names {
		booleanFlags[name] = struct{}{}
	}
}

// IsBooleanFlag reports whether name was registered as a boolean flag.
func IsBooleanFlag(name string) bool {
	_, ok := booleanFlags[name]
	return ok
}

// ParseFlags turns a --flag / --flag=value / --no-flag argv into a params map.
// It skips the parent CLI's --putnamiContext token and treats registered
// boolean flags as standalone (a non-boolean flag consumes the next argument
// unless that argument is itself a flag).
func ParseFlags(argv []string) map[string]any {
	flags := map[string]any{}
	for i := 0; i < len(argv); i++ {
		raw := argv[i]
		if raw == "--putnamiContext" {
			i++
			continue
		}
		if !strings.HasPrefix(raw, "--") {
			continue
		}
		name := strings.TrimPrefix(raw, "--")
		if strings.HasPrefix(name, "no-") {
			flags[strings.TrimPrefix(name, "no-")] = false
			continue
		}
		if idx := strings.IndexByte(name, '='); idx >= 0 {
			flags[name[:idx]] = name[idx+1:]
			continue
		}
		if IsBooleanFlag(name) {
			flags[name] = true
			continue
		}
		if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
			flags[name] = argv[i+1]
			i++
			continue
		}
		flags[name] = true
	}
	return flags
}

// RejectAppFlag returns a usage error when argv carries --app/--appName for a
// command that takes its app positionally.
func RejectAppFlag(argv []string, command string) error {
	for _, raw := range argv {
		if raw == "--app" || raw == "--appName" ||
			strings.HasPrefix(raw, "--app=") || strings.HasPrefix(raw, "--appName=") {
			return NewError(fmt.Sprintf("%s targets apps positionally; remove --app and pass the app as the command positional", command), ExitUsage)
		}
	}
	return nil
}

// FirstPositional returns the first non-flag argument, skipping the parent CLI's
// --putnamiContext token and the values consumed by registered string flags.
// Using the registered boolean set (rather than a literal args[0]) is what lets
// `putnami cloud <verb> <app>` find its positional even when the parent CLI
// injects --putnamiContext into the args slice.
func FirstPositional(argv []string) string {
	skipNext := false
	for i, raw := range argv {
		if skipNext {
			skipNext = false
			continue
		}
		if raw == "--putnamiContext" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(raw, "--") {
			name := strings.TrimPrefix(raw, "--")
			if strings.Contains(name, "=") || strings.HasPrefix(name, "no-") || IsBooleanFlag(name) {
				continue
			}
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
				skipNext = true
			}
			continue
		}
		return raw
	}
	return ""
}

// AdoptPositionalApp sets params["app"] from the leading positional when neither
// --app nor --appName is already set, matching the native CLI's
// `putnami <verb> <project>` ergonomics. Mutates params in place.
func AdoptPositionalApp(params map[string]any, args []string) {
	if _, set := params["app"]; set {
		return
	}
	if _, set := params["appName"]; set {
		return
	}
	if app := FirstPositional(args); app != "" {
		params["app"] = app
	}
}

// MergeParams overlays sources left-to-right into a new map, ignoring nil and
// empty-string values so a later source doesn't blank an earlier one.
func MergeParams(sources ...map[string]any) map[string]any {
	merged := map[string]any{}
	for _, source := range sources {
		for k, v := range source {
			if v != nil && StringValue(v) != "" {
				merged[k] = v
			}
		}
	}
	return merged
}
