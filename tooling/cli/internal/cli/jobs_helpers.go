package cli

import (
	"fmt"
	"strings"

	"go.putnami.dev/protocol/doctor"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/env"
)

// buildPlanSnapshot and buildSessionStats used to live here: the CLI shell's
// names for the two v1 session-file projections, kept beside the frozen wire
// golden that asserted the persisted plan.json and session.json bytes through
// them. B1b deleted both with the v1 session writer — a recorded
// session is the versioned cli.SessionFile now, projected by internal/machine
// from the same canonical reduction every other machine surface reads.

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
// every job cache key and run marker hashes. Before the passthrough separator a
// flag takes the next token as its value under takesNextAsValue, so a multi-word
// value binds even when it begins with a hyphen. Past the separator, only a next
// token that does not begin with a hyphen is a value.
func buildCommandParams(rawArgs []string) map[string]any {
	params := make(map[string]any)
	flagArgs := rawArgs[:passthroughCut(rawArgs)]
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
		if takesNextAsValue(flagArgs, i) || (i+1 < len(rawArgs) && !strings.HasPrefix(rawArgs[i+1], "-")) {
			params[name] = rawArgs[i+1]
			i++
		} else {
			params[name] = true
		}
	}
	return params
}
