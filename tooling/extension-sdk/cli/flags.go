package cli

import (
	"strconv"
	"strings"
	"unicode"
)

// ParseFlags parses remaining args into a flag map.
// Supports: --flag value, --flag=value, --flag (bool true), --no-flag (bool false).
func ParseFlags(args []string) map[string]string {
	flags := make(map[string]string)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}

		// Strip leading dashes
		key := strings.TrimLeft(arg, "-")

		// Handle --key=value
		if idx := strings.IndexByte(key, '='); idx >= 0 {
			flags[key[:idx]] = key[idx+1:]
			continue
		}

		// Handle --no-X pattern
		if strings.HasPrefix(key, "no-") {
			flags[key[3:]] = "false"
			continue
		}

		// The next arg is a value when it does not begin with a hyphen, or when
		// it contains whitespace: no flag spelling does, so "--check --dry-run"
		// is a value. The CLI binds job params by the same rule.
		if i+1 < len(args) && (!strings.HasPrefix(args[i+1], "-") || strings.ContainsFunc(args[i+1], unicode.IsSpace)) {
			flags[key] = args[i+1]
			i++
		} else {
			flags[key] = "true"
		}
	}
	return flags
}

// FlagString returns the flag value, or defaultVal if not set.
func FlagString(flags map[string]string, key, defaultVal string) string {
	if v, ok := flags[key]; ok {
		return v
	}
	return defaultVal
}

// FlagBool returns the flag boolean value, or defaultVal if not set.
func FlagBool(flags map[string]string, key string, defaultVal bool) bool {
	v, ok := flags[key]
	if !ok {
		return defaultVal
	}
	switch v {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	return defaultVal
}

// FlagInt returns the flag integer value, or defaultVal if not set/invalid.
func FlagInt(flags map[string]string, key string, defaultVal int) int {
	v, ok := flags[key]
	if !ok {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}
