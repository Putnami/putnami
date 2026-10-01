package cli

import (
	"fmt"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/env"
)

const cpuPolicyOption = "cpu-policy"

// Both policies are correct answers to "how much of the machine may this task
// use"; which one is right depends on the repository's shape, so neither is
// hard-coded. critical-path suits a plan whose makespan is one long chain,
// measured suits a plan of many comparable tasks or a shared machine.
const (
	cpuPolicyCriticalPath = "critical-path"
	cpuPolicyMeasured     = "measured"
)

// resolveCPUPolicy applies one explicit precedence chain exactly once on a
// command path: CLI flag > PUTNAMI_CPU_POLICY > workspace option > default.
// It mirrors resolveCacheTrust deliberately: a run-shaping policy that a repo
// wants to pin should be settable where the repo is described, not only on a
// command line an operator has to remember.
//
// An unrecognized value is an error rather than a silent fallback: a
// user who asked for a specific policy must not quietly get the other one, and
// this policy is exactly the variable a measurement campaign is controlling.
func resolveCPUPolicy(g *GlobalFlags, cfg *wsproto.Config, commands []string) error {
	if g == nil {
		return nil
	}

	value, source := strings.TrimSpace(g.CPUBudgetPolicy), "--cpu-policy"
	if value == "" {
		value, source = strings.TrimSpace(env.String("CPU_POLICY")), "PUTNAMI_CPU_POLICY"
	}
	if value == "" {
		configured, configuredSource, ok, err := configuredCPUPolicy(cfg, commands)
		if err != nil {
			return err
		}
		if !ok {
			g.CPUBudgetPolicy = ""
			return nil
		}
		value, source = configured, configuredSource
	}

	policy := strings.ToLower(value)
	if !validCPUPolicy(policy) {
		return usageErrorf("invalid %s value %q: expected one of %s",
			source, value, strings.Join(commandmeta.GlobalFlagValues("--cpu-policy"), ", "))
	}
	g.CPUBudgetPolicy = policy
	return nil
}

// configuredCPUPolicy resolves options.* and options.<command>. The ceiling
// policy is run-scoped — one allocator serves the whole DAG — so two commands
// in a multi-command run cannot disagree: a conflict is an error rather than a
// silent winner.
func configuredCPUPolicy(cfg *wsproto.Config, commands []string) (string, string, bool, error) {
	if cfg == nil {
		return "", "", false, nil
	}
	var (
		resolved string
		source   string
		found    bool
	)
	for _, command := range commands {
		raw, ok := cfg.GetCommandDefaults(command, "")[cpuPolicyOption]
		if !ok {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return "", "", false, fmt.Errorf("invalid options.%s.%s value: must be a string (%s or %s)",
				command, cpuPolicyOption, cpuPolicyCriticalPath, cpuPolicyMeasured)
		}
		policy := strings.ToLower(strings.TrimSpace(value))
		if found && policy != resolved {
			return "", "", false, fmt.Errorf(
				"conflicting options.<command>.%s values in one run (%q and %q): the CPU ceiling policy is run-scoped",
				cpuPolicyOption, resolved, policy)
		}
		resolved, source, found = policy, "options."+command+"."+cpuPolicyOption, true
	}
	return resolved, source, found, nil
}

func validCPUPolicy(policy string) bool {
	return policy == cpuPolicyCriticalPath || policy == cpuPolicyMeasured
}
