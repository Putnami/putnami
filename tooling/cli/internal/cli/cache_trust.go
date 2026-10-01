package cli

import (
	"fmt"
	"os"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/env"
	"go.putnami.dev/tooling/cli/internal/store"
)

const cacheTrustOption = "cache-trust"

// resolveCacheTrust applies one explicit precedence chain exactly once on a
// command path: CLI flag > PUTNAMI_CACHE_TRUST > per-command workspace option
// > environment default (ci in CI, any locally). Publish and tag paths raise
// the result to at least ci so an explicit/configured "any" can never let a
// developer hint gate a release; "none" remains stricter and stays disabled.
func resolveCacheTrust(g *GlobalFlags, cfg *wsproto.Config, commands []string, subcommand string) error {
	if g == nil {
		return nil
	}

	var (
		trust  store.CacheTrust
		source string
	)
	if strings.TrimSpace(g.CacheTrust) != "" {
		trust = store.CacheTrust(strings.ToLower(strings.TrimSpace(g.CacheTrust)))
		source = "--cache-trust"
	} else if value := strings.TrimSpace(env.String("CACHE_TRUST")); value != "" {
		trust = store.CacheTrust(strings.ToLower(value))
		source = "PUTNAMI_CACHE_TRUST"
	} else {
		configured, configuredSource, ok, err := configuredCacheTrust(cfg, commands)
		if err != nil {
			return err
		}
		if ok {
			trust, source = configured, configuredSource
		} else if strings.TrimSpace(os.Getenv("CI")) != "" {
			trust, source = store.CacheTrustCI, "CI default"
		} else {
			trust, source = store.CacheTrustAny, "local default"
		}
	}

	if !trust.Valid() {
		return fmt.Errorf("invalid %s value %q: must be one of ci, any, none", source, trust)
	}
	if releaseCacheTrustRequired(commands, subcommand) && trust == store.CacheTrustAny {
		trust = store.CacheTrustCI
	}
	g.CacheTrust = string(trust)
	return nil
}

// configuredCacheTrust resolves options.* and options.<command> for every
// command in a multi-command run. The remote cache is run-scoped, so differing
// command defaults combine to the strictest policy (none > ci > any).
func configuredCacheTrust(cfg *wsproto.Config, commands []string) (store.CacheTrust, string, bool, error) {
	if cfg == nil {
		return "", "", false, nil
	}
	var (
		resolved store.CacheTrust
		source   string
		found    bool
	)
	for _, command := range commands {
		defaults := cfg.GetCommandDefaults(command, "")
		raw, ok := defaults[cacheTrustOption]
		if !ok {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return "", "", false, fmt.Errorf("invalid options.%s.%s value: must be a string (ci, any, or none)", command, cacheTrustOption)
		}
		trust := store.CacheTrust(strings.ToLower(strings.TrimSpace(value)))
		if !trust.Valid() {
			return "", "", false, fmt.Errorf("invalid options.%s.%s value %q: must be one of ci, any, none", command, cacheTrustOption, value)
		}
		if !found || stricterCacheTrust(trust, resolved) {
			resolved = trust
			source = "options." + command + "." + cacheTrustOption
			found = true
		}
	}
	return resolved, source, found, nil
}

func stricterCacheTrust(left, right store.CacheTrust) bool {
	rank := func(trust store.CacheTrust) int {
		switch trust {
		case store.CacheTrustNone:
			return 3
		case store.CacheTrustCI:
			return 2
		case store.CacheTrustAny:
			return 1
		default:
			return 0
		}
	}
	return rank(left) > rank(right)
}

func releaseCacheTrustRequired(commands []string, subcommand string) bool {
	for _, command := range commands {
		switch command {
		case "publish", "tag":
			return true
		case "version":
			if subcommand == "tag" {
				return true
			}
		}
	}
	return false
}
