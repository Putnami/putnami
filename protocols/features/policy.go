package features

import (
	"fmt"
	"sort"
)

// The verification policy vocabulary of the SDD vertical. One
// domain-keyed object under the open command-options surface —
// options.sdd.verification — serves specs, features, and architecture with one
// value set and one resolver family, so a sibling domain can never grow a
// second vocabulary. Project-scoped verification resolves project > workspace
// > default; workspace-wide verification resolves workspace > default.
//
// The resolver lives beside the wire types deliberately: core (which
// sanctions) and the SDD extension (which renders) both import this package
// already, and two resolvers reading one committed policy is exactly the
// drift this file exists to prevent.

// VerificationMode is the closed tri-state verification policy vocabulary.
// The values are verbs, not on/info/off aliases, because each names what the
// gate does after a selected test run.
type VerificationMode string

const (
	// VerificationModeEnforce evaluates every selected requirement and lets an
	// unresolved or failed verification add a failed synthetic result.
	VerificationModeEnforce VerificationMode = "enforce"
	// VerificationModeReport evaluates and renders every selected requirement
	// without ever changing an otherwise successful exit.
	VerificationModeReport VerificationMode = "report"
	// VerificationModeOff collects and evaluates nothing automatically.
	VerificationModeOff VerificationMode = "off"
)

// DefaultVerificationMode is the built-in mode a workspace inherits when no
// committed policy names one.
const DefaultVerificationMode = VerificationModeReport

// VerificationDomain selects which SDD vertical a policy key governs.
type VerificationDomain string

const (
	// VerificationDomainSpecs gates executable textual spec requirements.
	VerificationDomainSpecs VerificationDomain = "specs"
	// VerificationDomainFeatures is reserved for features-maturity enforcement.
	VerificationDomainFeatures VerificationDomain = "features"
	// VerificationDomainArchitecture governs automatic workspace architecture
	// validation. Interactive architecture commands remain explicitly requested.
	VerificationDomainArchitecture VerificationDomain = "architecture"
)

// VerificationModeSource says which committed configuration produced an
// effective mode, so every surface can report provenance instead of a bare
// value.
type VerificationModeSource string

const (
	// VerificationModeSourceProject is the spec-hosting project's own override.
	VerificationModeSourceProject VerificationModeSource = "project"
	// VerificationModeSourceWorkspace is the workspace-level policy.
	VerificationModeSourceWorkspace VerificationModeSource = "workspace"
	// VerificationModeSourceDefault is the built-in default.
	VerificationModeSourceDefault VerificationModeSource = "default"
)

const (
	// VerificationOptionsBlock is the options key the SDD vertical owns.
	VerificationOptionsBlock = "sdd"
	// VerificationOptionsMember is the domain-keyed policy object inside it.
	VerificationOptionsMember = "verification"
)

var validVerificationModes = map[VerificationMode]bool{
	VerificationModeEnforce: true, VerificationModeReport: true, VerificationModeOff: true,
}

var validVerificationDomains = map[VerificationDomain]bool{
	VerificationDomainSpecs: true, VerificationDomainFeatures: true, VerificationDomainArchitecture: true,
}

// VerificationPolicy is one strictly decoded options.sdd.verification object.
// A missing block decodes to an empty map: absence inherits, and only presence
// can be wrong.
type VerificationPolicy map[VerificationDomain]VerificationMode

// DecodeVerificationPolicy extracts and strictly validates the
// options.sdd.verification member of one decoded options map. scope names the
// configuration being read ("workspace" or a project identity) so an error
// points at the exact committed file to fix.
//
// A wrong type, an unknown domain key, or an unknown value is an error, never
// a silent fallback: this policy decides whether a run may fail, so the only
// safe reading of an unreadable one is to stop before jobs execute.
func DecodeVerificationPolicy(scope string, options map[string]map[string]any) (VerificationPolicy, error) {
	policy := VerificationPolicy{}
	if options == nil {
		return policy, nil
	}
	block, exists := options[VerificationOptionsBlock]
	if !exists || block == nil {
		return policy, nil
	}
	raw, exists := block[VerificationOptionsMember]
	if !exists {
		return policy, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s options.%s.%s must be an object keyed by domain, got %T",
			scope, VerificationOptionsBlock, VerificationOptionsMember, raw)
	}
	domains := make([]string, 0, len(object))
	for domain := range object {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	for _, domain := range domains {
		if !validVerificationDomains[VerificationDomain(domain)] {
			return nil, fmt.Errorf("%s options.%s.%s names unknown domain %q (want %q, %q, or %q)",
				scope, VerificationOptionsBlock, VerificationOptionsMember, domain,
				VerificationDomainSpecs, VerificationDomainFeatures, VerificationDomainArchitecture)
		}
		value, ok := object[domain].(string)
		if !ok {
			return nil, fmt.Errorf("%s options.%s.%s.%s must be a string, got %T",
				scope, VerificationOptionsBlock, VerificationOptionsMember, domain, object[domain])
		}
		mode := VerificationMode(value)
		if !validVerificationModes[mode] {
			return nil, fmt.Errorf("%s options.%s.%s.%s is %q (want %q, %q, or %q)",
				scope, VerificationOptionsBlock, VerificationOptionsMember, domain, value,
				VerificationModeEnforce, VerificationModeReport, VerificationModeOff)
		}
		policy[VerificationDomain(domain)] = mode
	}
	return policy, nil
}

// ResolveVerificationMode resolves one domain's effective mode
// deterministically: the spec-hosting project's committed policy, then the
// workspace's, then the built-in default. Both inputs must already have
// decoded cleanly; the error path exists so a caller that skipped validation
// still cannot receive a fallback for an unreadable policy.
func ResolveVerificationMode(domain VerificationDomain, workspaceScope string, workspaceOptions map[string]map[string]any,
	projectScope string, projectOptions map[string]map[string]any) (VerificationMode, VerificationModeSource, error) {
	if !validVerificationDomains[domain] {
		return "", "", fmt.Errorf("unknown verification domain %q", domain)
	}
	project, err := DecodeVerificationPolicy(projectScope, projectOptions)
	if err != nil {
		return "", "", err
	}
	if mode, exists := project[domain]; exists {
		return mode, VerificationModeSourceProject, nil
	}
	return ResolveWorkspaceVerificationMode(domain, workspaceScope, workspaceOptions)
}

// ResolveWorkspaceVerificationMode resolves policy for a workspace-wide
// subject. No project override participates: choosing an arbitrary project to
// govern one workspace graph would make the verdict depend on which task
// happened to ask the question. The committed workspace policy wins, then the
// built-in default. An unreadable policy fails closed.
func ResolveWorkspaceVerificationMode(domain VerificationDomain, workspaceScope string,
	workspaceOptions map[string]map[string]any) (VerificationMode, VerificationModeSource, error) {
	if !validVerificationDomains[domain] {
		return "", "", fmt.Errorf("unknown verification domain %q", domain)
	}
	workspace, err := DecodeVerificationPolicy(workspaceScope, workspaceOptions)
	if err != nil {
		return "", "", err
	}
	if mode, exists := workspace[domain]; exists {
		return mode, VerificationModeSourceWorkspace, nil
	}
	return DefaultVerificationMode, VerificationModeSourceDefault, nil
}
