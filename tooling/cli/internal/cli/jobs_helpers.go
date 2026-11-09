package cli

import (
	"fmt"
	"maps"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/protocol/doctor"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/env"
)

// resolveProfile resolves the deployment profile onto g.EnvProfile with
// precedence --profile flag > PUTNAMI_PROFILE env > workspace config > dev, and
// validates the result against the frozen protocols/doctor enum. It returns a
// usage error when a source yields a value outside dev|test|production — most
// often a stale `--profile <trace-path>` invocation, whose Chrome-trace output
// now belongs on --trace-profile. Resolution runs once, before command
// dispatch, so job commands (parsed.Global) and structured commands
// (CommandEnv.Global) read the same resolved value.
func resolveProfile(g *GlobalFlags, cfg *wsproto.Config) error {
	// The flag wins outright; a bad flag value is almost always stale trace-path
	// usage, so its error points back at --trace-profile.
	if g.EnvProfile != "" {
		if !doctor.ValidProfiles[doctor.Profile(g.EnvProfile)] {
			return profileUsageError(g.EnvProfile, true)
		}
		return nil
	}
	profile := env.String("PROFILE")
	if profile == "" && cfg != nil {
		profile = cfg.Profile
	}
	if profile == "" {
		g.EnvProfile = string(doctor.ProfileDev)
		return nil
	}
	if !doctor.ValidProfiles[doctor.Profile(profile)] {
		return profileUsageError(profile, false)
	}
	g.EnvProfile = profile
	return nil
}

// profileUsageError builds the invalid-profile usage error, listing the valid
// profiles in strictness order. When the value came from the --profile flag it
// also names --trace-profile, since a path-shaped value is the tell-tale sign of
// a pre-rename `--profile <trace.json>` invocation.
func profileUsageError(value string, fromFlag bool) error {
	valid := make([]string, len(doctor.ProfileValues))
	for i, p := range doctor.ProfileValues {
		valid[i] = string(p)
	}
	if fromFlag {
		return fmt.Errorf("invalid --profile value %q: must be one of %s (the Chrome-trace output flag is now --trace-profile)",
			value, strings.Join(valid, ", "))
	}
	return fmt.Errorf("invalid profile %q: must be one of %s", value, strings.Join(valid, ", "))
}

// buildCommandParams turns the job args into the params every task receives and
// every job cache key and run marker hashes. A flag takes the next token as its
// value under takesNextAsValue, so a multi-word value binds even when it begins
// with a hyphen, as the extension SDK's ParseFlags reads the same tokens.
func buildCommandParams(rawArgs []string) map[string]any {
	params := make(map[string]any)
	for i := 0; i < len(rawArgs); i++ {
		arg := rawArgs[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}

		name := strings.TrimLeft(arg, "-")

		// Handle --flag=value
		if idx := strings.Index(name, "="); idx >= 0 {
			params[name[:idx]] = name[idx+1:]
			continue
		}

		// Handle --no-flag
		if strings.HasPrefix(name, "no-") {
			params[name[3:]] = false
			continue
		}

		// Check if next arg is a value
		if i+1 < len(rawArgs) && takesNextAsValue(rawArgs, i) {
			params[name] = rawArgs[i+1]
			i++
		} else {
			params[name] = true
		}
	}
	return params
}

// shortAliasTargets maps each short alias the declared flags give to the long
// name it stands for, without dashes: manifests spell an alias "-t" or "t". A
// short name that is itself a declared long name keeps its own meaning and is
// left out, as is an alias two declarations give to different long names:
// neither can be renamed without guessing.
func shortAliasTargets(declared map[string]extension.FlagDefinition) map[string]string {
	targets := make(map[string]string)
	ambiguous := make(map[string]bool)
	for long, def := range declared {
		short := strings.TrimLeft(def.Short, "-")
		if short == "" || short == long {
			continue
		}
		if _, isLong := declared[short]; isLong {
			continue
		}
		if previous, seen := targets[short]; seen && previous != long {
			ambiguous[short] = true
			continue
		}
		targets[short] = long
	}
	for short := range ambiguous {
		delete(targets, short)
	}
	return targets
}

// shortAliasRenames returns, for each declared short alias the invocation
// spells before the passthrough separator ("-t x", "-t=x"), the long name its
// param takes. A token after the separator is the user's payload and is never
// resolved. It refuses an invocation that spells one flag both by its alias
// and by its long name, such as `-t a --test b`: both bind the same param, and
// which value wins is not the user's to guess.
func shortAliasRenames(rawArgs []string, declared map[string]extension.FlagDefinition) (map[string]string, error) {
	targets := shortAliasTargets(declared)
	if len(targets) == 0 {
		return nil, nil
	}
	renames := make(map[string]string)
	spelledLong := make(map[string]string)
	for _, token := range jobFlagTokens(rawArgs) {
		spelling := flagSpelling(token)
		if !strings.HasPrefix(spelling, "--") {
			if long, ok := targets[spelling[1:]]; ok {
				renames[spelling[1:]] = long
				continue
			}
		}
		spelledLong[flagBaseName(token)] = spelling
	}
	shorts := make([]string, 0, len(renames))
	for short := range renames {
		shorts = append(shorts, short)
	}
	sort.Strings(shorts)
	for _, short := range shorts {
		if spelling, both := spelledLong[renames[short]]; both {
			return nil, usageErrorf("flags -%s and %s name the same flag\n  Pass one of them.", short, spelling)
		}
	}
	return renames, nil
}

// renameParams returns params with each key renames names moved to the name
// it maps to, such as a short alias's "t" to its flag's "test", the rename the
// engine applies to a job command's params (Request.CommandParamRenames). A
// moved key replaces a value already under the new name. params itself is
// left unchanged; with nothing to move it is returned as is.
func renameParams[V any](params map[string]V, renames map[string]string) map[string]V {
	var renamed map[string]V
	for from, to := range renames {
		value, ok := params[from]
		if !ok {
			continue
		}
		if renamed == nil {
			renamed = maps.Clone(params)
		}
		delete(renamed, from)
		renamed[to] = value
	}
	if renamed == nil {
		return params
	}
	return renamed
}
