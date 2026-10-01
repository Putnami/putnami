package toolchain

import (
	"strings"

	extensionproto "go.putnami.dev/protocol/extension"
)

// OfflineDependencies reports whether env carries the signal of a hosted run
// (extensionproto.OfflineDependenciesEnv set to "1"): the workspace-fetch job
// downloaded every module before any repository-controlled process started, so
// no go command of this job may download one. The comparison is exact, like
// registrycred.OfflineDependencies.
func OfflineDependencies(env []string) bool {
	return envValue(env, extensionproto.OfflineDependenciesEnv) == "1"
}

// OfflineGoOverrides returns, as "KEY=value" entries, the variables that forbid
// every module download to a go command whose inherited GOFLAGS is goflags. It
// is the one offline policy: GoCommandEnv applies it to every task, and the
// workspace lifecycle jobs apply it to theirs.
//
//   - GOPROXY=off refuses every proxy and checksum lookup.
//   - GONOPROXY=none closes the direct route. A module that GONOPROXY matches
//     skips GOPROXY and is fetched from its origin, and Go falls back to
//     GOPRIVATE, from the environment or from Go's own env file, whenever
//     GONOPROXY is empty. "none" matches nothing and overrides both.
//   - -mod=readonly in GOFLAGS keeps go from updating go.mod or go.sum
//     (OfflineGoFlags).
func OfflineGoOverrides(goflags string) []string {
	return []string{
		"GOPROXY=off",
		"GONOPROXY=none",
		"GOFLAGS=" + OfflineGoFlags(goflags),
	}
}

// OfflineGoFlags returns goflags with -mod=readonly added, unless goflags
// already sets -mod. A -mod the caller set is kept: Go applies the last -mod
// it reads, so adding one would silently replace a -mod=vendor build with one
// that ignores the vendor directory, and no -mod value downloads a module once
// GOPROXY is off.
func OfflineGoFlags(goflags string) string {
	for _, field := range goFlagFields(goflags) {
		if isModFlag(field) {
			return goflags
		}
	}
	if trimmed := strings.TrimSpace(goflags); trimmed != "" {
		return trimmed + " -mod=readonly"
	}
	return "-mod=readonly"
}

// goFlagFields splits GOFLAGS the way the go command does: on white space,
// with a single or double quote honored only at the start of a field, where it
// runs to the matching quote.
func goFlagFields(goflags string) []string {
	var fields []string
	for {
		goflags = strings.TrimLeft(goflags, " \t\n\r")
		if goflags == "" {
			return fields
		}
		if quote := goflags[0]; quote == '"' || quote == '\'' {
			end := strings.IndexByte(goflags[1:], quote)
			if end < 0 {
				// Go rejects an unterminated quote; the rest is one field.
				return append(fields, goflags[1:])
			}
			fields = append(fields, goflags[1:1+end])
			goflags = goflags[2+end:]
			continue
		}
		end := strings.IndexAny(goflags, " \t\n\r")
		if end < 0 {
			return append(fields, goflags)
		}
		fields = append(fields, goflags[:end])
		goflags = goflags[end:]
	}
}

// isModFlag reports whether one GOFLAGS field sets -mod: "-mod=..." or
// "--mod=...", as the flag package reads a flag.
func isModFlag(field string) bool {
	name, ok := strings.CutPrefix(field, "-")
	if !ok {
		return false
	}
	name = strings.TrimPrefix(name, "-")
	if strings.HasPrefix(name, "-") {
		return false
	}
	name, _, _ = strings.Cut(name, "=")
	return name == "mod"
}

// withOfflineDependencies applies OfflineGoOverrides to env when env carries
// the offline signal, and returns env unchanged otherwise.
func withOfflineDependencies(env []string) []string {
	if !OfflineDependencies(env) {
		return env
	}
	for _, entry := range OfflineGoOverrides(envValue(env, "GOFLAGS")) {
		name, value, _ := strings.Cut(entry, "=")
		env = setEnvValue(env, name, value)
	}
	return env
}
