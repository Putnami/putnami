// Package env reads the CLI's PUTNAMI_* environment overrides.
//
// The vocabulary is the global-flag vocabulary: PUTNAMI_OUTPUT, PUTNAMI_DEBUG,
// PUTNAMI_VERBOSE, PUTNAMI_QUIET, PUTNAMI_COLOR, PUTNAMI_NO_COLOR,
// PUTNAMI_PROFILE, PUTNAMI_CACHE_TRUST. The engine layers them onto the parsed
// flags (applyEnvOverrides) and two terminal-adapter helpers read one each, so
// the spelling rule — uppercase, dashes to underscores, one prefix — lives in
// exactly one place.
//
// It is a leaf: nothing here knows what a flag means, only how its name is
// spelled in the environment. It moved out of
// internal/config, which was otherwise a pure alias of the
// go.putnami.dev/protocol/workspace types and was deleted; carrying the config
// loader's name over a process-environment reader was the only thing that had
// ever tied the two together.
package env

import (
	"os"
	"strings"
)

// Prefix is the prefix for all Putnami environment variables.
const Prefix = "PUTNAMI_"

// String reads a PUTNAMI_* environment variable. The name is given in flag
// spelling ("cache-trust", "OUTPUT"); it is uppercased and its dashes become
// underscores.
func String(name string) string {
	return os.Getenv(Prefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_")))
}

// Bool reads a PUTNAMI_* boolean environment variable. The second result
// reports whether the variable was both set and recognized, so an unset
// variable and an unparseable one are distinguishable from an explicit false.
func Bool(name string) (bool, bool) {
	v := String(name)
	if v == "" {
		return false, false
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes":
		return true, true
	case "false", "0", "no":
		return false, true
	}
	return false, false
}
