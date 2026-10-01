package extension

import (
	"fmt"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	// RuntimeToolchainCandidatePath resolves a bare executable name from PATH.
	RuntimeToolchainCandidatePath = "path"
	// RuntimeToolchainCandidateEnvironment resolves below a declared environment root.
	RuntimeToolchainCandidateEnvironment = "environment"
	// RuntimeToolchainCandidateHome resolves below the user's home directory.
	RuntimeToolchainCandidateHome = "home"
	// RuntimeToolchainCandidatePutnamiHome resolves below Putnami's managed home.
	RuntimeToolchainCandidatePutnamiHome = "putnami-home"

	// RuntimeToolchainEnvironmentExecutable publishes the exact resolved executable.
	RuntimeToolchainEnvironmentExecutable = "executable"
	// RuntimeToolchainEnvironmentAncestor publishes an ancestor of the resolved executable.
	RuntimeToolchainEnvironmentAncestor = "ancestor"
	// RuntimeToolchainEnvironmentLiteral publishes a declared literal value.
	RuntimeToolchainEnvironmentLiteral = "literal"

	// RuntimeToolchainVersionToken expands to the workspace lock's exact version.
	RuntimeToolchainVersionToken = "{version}"
)

// RuntimeToolchain is a provider-neutral recipe for resolving one executable
// at the exact version pinned by the workspace lock. The declaration owns all
// executable, probe, and environment vocabulary; the CLI only interprets this
// closed shape.
type RuntimeToolchain struct {
	Lock        string                                 `json:"lock"`
	Optional    bool                                   `json:"optional,omitempty"`
	Candidates  []RuntimeToolchainCandidate            `json:"candidates"`
	Probe       RuntimeToolchainProbe                  `json:"probe"`
	Environment map[string]RuntimeToolchainEnvironment `json:"environment,omitempty"`
	PrependPath bool                                   `json:"prependPath,omitempty"`
}

// RuntimeToolchainCandidate declares one ordered location for the executable.
type RuntimeToolchainCandidate struct {
	From        string `json:"from"`
	Path        string `json:"path"`
	Environment string `json:"environment,omitempty"`
}

// RuntimeToolchainProbe must match the trimmed combined output exactly after
// replacing {version} with the lock version. Exact matching prevents adjacent
// versions such as 1.2.30 and 1.2.3 from sharing an identity.
type RuntimeToolchainProbe struct {
	Args        []string          `json:"args,omitempty"`
	Expect      string            `json:"expect"`
	Unset       []string          `json:"unset,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
}

// RuntimeToolchainEnvironment derives one child environment value from the
// resolved executable or an authored literal. Ancestor levels start at the
// executable itself: level 1 is its directory, level 2 its parent.
type RuntimeToolchainEnvironment struct {
	From   string `json:"from"`
	Levels int    `json:"levels,omitempty"`
	Value  string `json:"value,omitempty"`
}

func validateRuntimeToolchains(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}
	if m.Runtime == nil {
		for _, taskName := range sortedTaskNames(m.Tasks) {
			if len(m.Tasks[taskName].Toolchains) != 0 {
				return []diag.Diagnostic{diag.Errorf("runtime-not-declared", "tasks."+taskName+".toolchains",
					"task toolchains require a runtime declaration")}
			}
		}
		return nil
	}

	var diags []diag.Diagnostic
	aliases := make([]string, 0, len(m.Runtime.Toolchains))
	for alias := range m.Runtime.Toolchains {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		base := "runtime.toolchains." + alias
		if !validRuntimeIdentifier(alias) {
			diags = append(diags, diag.Errorf("invalid-value", base, "toolchain alias %q is invalid", alias))
		}
		diags = append(diags, validateRuntimeToolchain(base, m.Runtime.Toolchains[alias])...)
	}
	diags = append(diags, validateRuntimeToolchainRefs("runtime.runToolchains", m.Runtime.RunToolchains, m.Runtime.Toolchains)...)
	if m.Runtime.Prepare != nil {
		diags = append(diags, validateRuntimeToolchainRefs("runtime.prepare.toolchains", m.Runtime.Prepare.Toolchains, m.Runtime.Toolchains)...)
	}
	for _, taskName := range sortedTaskNames(m.Tasks) {
		diags = append(diags, validateRuntimeToolchainRefs("tasks."+taskName+".toolchains", m.Tasks[taskName].Toolchains, m.Runtime.Toolchains)...)
	}
	return diags
}

func validateRuntimeToolchain(base string, requirement RuntimeToolchain) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if !validRuntimeIdentifier(requirement.Lock) {
		diags = append(diags, diag.Errorf("invalid-value", base+".lock", "lock identity %q is invalid", requirement.Lock))
	}
	if len(requirement.Candidates) == 0 {
		diags = append(diags, diag.Errorf("required-field", base+".candidates", "at least one executable candidate is required"))
	}
	if len(requirement.Candidates) > 16 {
		diags = append(diags, diag.Errorf("limit-exceeded", base+".candidates", "at most 16 executable candidates are allowed"))
	}
	if len(requirement.Environment) > 32 {
		diags = append(diags, diag.Errorf("limit-exceeded", base+".environment", "at most 32 environment bindings are allowed"))
	}
	for i, candidate := range requirement.Candidates {
		field := fmt.Sprintf("%s.candidates[%d]", base, i)
		diags = append(diags, validateRuntimeToolchainCandidate(field, candidate)...)
	}
	if !singleRuntimeVersionTemplate(requirement.Probe.Expect) || len(requirement.Probe.Expect) > 256 {
		diags = append(diags, diag.Errorf("invalid-value", base+".probe.expect",
			"probe expectation must contain exactly one %s token", RuntimeToolchainVersionToken))
	}
	if len(requirement.Probe.Args) > 16 || len(requirement.Probe.Unset) > 16 || len(requirement.Probe.Environment) > 16 {
		diags = append(diags, diag.Errorf("limit-exceeded", base+".probe", "probe arguments and environment are bounded to 16 entries each"))
	}
	for i, name := range requirement.Probe.Unset {
		if !validEnvironmentName(name) {
			diags = append(diags, diag.Errorf("invalid-value", fmt.Sprintf("%s.probe.unset[%d]", base, i), "invalid environment name %q", name))
		}
	}
	if duplicate := firstDuplicate(requirement.Probe.Unset); duplicate != "" {
		diags = append(diags, diag.Errorf("duplicate-value", base+".probe.unset", "environment name %q is declared more than once", duplicate))
	}
	for _, name := range sortedStringMapKeys(requirement.Probe.Environment) {
		if !validEnvironmentName(name) {
			diags = append(diags, diag.Errorf("invalid-value", base+".probe.environment."+name, "invalid environment name %q", name))
		}
		if !validRuntimeTemplate(requirement.Probe.Environment[name]) || len(requirement.Probe.Environment[name]) > 512 {
			diags = append(diags, diag.Errorf("invalid-value", base+".probe.environment."+name, "probe environment value contains an unknown token or exceeds 512 bytes"))
		}
	}
	for _, name := range sortedToolchainEnvironmentKeys(requirement.Environment) {
		field := base + ".environment." + name
		if !validEnvironmentName(name) {
			diags = append(diags, diag.Errorf("invalid-value", field, "invalid environment name %q", name))
		}
		binding := requirement.Environment[name]
		switch binding.From {
		case RuntimeToolchainEnvironmentExecutable:
			if binding.Levels != 0 || binding.Value != "" {
				diags = append(diags, diag.Errorf("invalid-value", field, "executable binding accepts neither levels nor value"))
			}
		case RuntimeToolchainEnvironmentAncestor:
			if binding.Levels < 1 || binding.Levels > 16 || binding.Value != "" {
				diags = append(diags, diag.Errorf("invalid-value", field, "ancestor binding requires levels from 1 to 16 and no value"))
			}
		case RuntimeToolchainEnvironmentLiteral:
			if binding.Levels != 0 {
				diags = append(diags, diag.Errorf("invalid-value", field, "literal binding accepts no levels"))
			}
			if !validRuntimeTemplate(binding.Value) || len(binding.Value) > 512 {
				diags = append(diags, diag.Errorf("invalid-value", field+".value", "literal contains an unknown token or exceeds 512 bytes"))
			}
		default:
			diags = append(diags, diag.Errorf("invalid-value", field+".from", "unknown environment binding source %q", binding.From))
		}
	}
	return diags
}

func validateRuntimeToolchainCandidate(field string, candidate RuntimeToolchainCandidate) []diag.Diagnostic {
	if strings.TrimSpace(candidate.Path) == "" || len(candidate.Path) > 512 {
		return []diag.Diagnostic{diag.Errorf("required-field", field+".path", "candidate path is required")}
	}
	switch candidate.From {
	case RuntimeToolchainCandidatePath:
		if candidate.Environment != "" || strings.ContainsAny(candidate.Path, `/\\`) || strings.Contains(candidate.Path, RuntimeToolchainVersionToken) {
			return []diag.Diagnostic{diag.Errorf("invalid-value", field, "PATH candidate must be a bare executable name")}
		}
	case RuntimeToolchainCandidateEnvironment:
		if !validEnvironmentName(candidate.Environment) {
			return []diag.Diagnostic{diag.Errorf("invalid-value", field+".environment", "environment-root candidate requires a valid environment name")}
		}
		if err := validateRuntimeCandidateRelativePath(candidate.Path); err != nil {
			return []diag.Diagnostic{diag.Errorf("invalid-runtime-path", field+".path", "%v", err)}
		}
	case RuntimeToolchainCandidateHome, RuntimeToolchainCandidatePutnamiHome:
		if candidate.Environment != "" {
			return []diag.Diagnostic{diag.Errorf("invalid-value", field+".environment", "candidate source %q accepts no environment name", candidate.From)}
		}
		if err := validateRuntimeCandidateRelativePath(candidate.Path); err != nil {
			return []diag.Diagnostic{diag.Errorf("invalid-runtime-path", field+".path", "%v", err)}
		}
	default:
		return []diag.Diagnostic{diag.Errorf("invalid-value", field+".from", "unknown candidate source %q", candidate.From)}
	}
	return nil
}

func validateRuntimeCandidateRelativePath(value string) error {
	expanded := strings.ReplaceAll(value, RuntimeToolchainVersionToken, "1.0.0")
	if strings.Contains(expanded, "{") || strings.Contains(expanded, "}") {
		return fmt.Errorf("candidate path contains an unknown template token")
	}
	if _, err := NormalizeRelativePath(expanded); err != nil {
		return fmt.Errorf("invalid candidate path %q: %w", value, err)
	}
	return nil
}

func validateRuntimeToolchainRefs(field string, refs []string, declarations map[string]RuntimeToolchain) []diag.Diagnostic {
	var diags []diag.Diagnostic
	seen := map[string]bool{}
	for i, ref := range refs {
		item := fmt.Sprintf("%s[%d]", field, i)
		if seen[ref] {
			diags = append(diags, diag.Errorf("duplicate-value", item, "toolchain reference %q is declared more than once", ref))
		}
		seen[ref] = true
		if !validRuntimeIdentifier(ref) {
			diags = append(diags, diag.Errorf("invalid-value", item, "invalid toolchain reference %q", ref))
		}
		if _, ok := declarations[ref]; !ok {
			diags = append(diags, diag.Errorf("unknown-reference", item, "toolchain %q is not declared by runtime.toolchains", ref))
		}
	}
	return diags
}

func validRuntimeIdentifier(value string) bool {
	for i, r := range value {
		if isASCIIAlpha(r) || r == '_' || (i > 0 && (isASCIIDigit(r) || r == '-' || r == '.')) {
			continue
		}
		return false
	}
	return value != ""
}

func validEnvironmentName(value string) bool {
	for i, r := range value {
		if r == '_' || isASCIIAlpha(r) || (i > 0 && isASCIIDigit(r)) {
			continue
		}
		return false
	}
	return value != ""
}

func isASCIIAlpha(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

func validRuntimeTemplate(value string) bool {
	return !strings.Contains(strings.ReplaceAll(value, RuntimeToolchainVersionToken, ""), "{") &&
		!strings.Contains(strings.ReplaceAll(value, RuntimeToolchainVersionToken, ""), "}")
}

func singleRuntimeVersionTemplate(value string) bool {
	return strings.Count(value, RuntimeToolchainVersionToken) == 1 && validRuntimeTemplate(value)
}

func firstDuplicate(values []string) string {
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] {
			return value
		}
		seen[value] = true
	}
	return ""
}

func sortedTaskNames(tasks map[string]TaskDefinition) []string {
	names := make([]string, 0, len(tasks))
	for name := range tasks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedStringMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedToolchainEnvironmentKeys(values map[string]RuntimeToolchainEnvironment) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
