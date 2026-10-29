// Providers: the optional top-level `providers` member of an extension
// manifest.
//
// A collaboration provider implements the operations of one collaboration
// contract (tasks, proposals, memory, missions) in an extension subprocess.
// The manifest declares one entry per contract, keyed by the contract name: the
// version it implements, the subprocess every operation of it runs, and the
// operations themselves. The CLI writes one provider request to the
// subprocess's standard input and reads one collaboration response from its
// standard output (go.putnami.dev/protocol/collaboration owns both documents).
//
// This package checks the declaration's shape. The collaboration catalog owns
// the contract names, the operations, their access and their precondition
// rules, and collaboration.ReadProviderOffer checks a declaration against it.
// Declaring the member is additive vocabulary: the manifest must carry
// cliContract ProviderContract, which a reader that predates the member
// refuses instead of loading the extension with no provider.

package extension

import (
	"regexp"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ProviderDeclaration is one entry of the manifest's `providers` member: what
// every operation of one contract shares, and the operations.
type ProviderDeclaration struct {
	// Version is the contract version the provider implements.
	Version int `json:"version"`
	// Command, Args, Cwd, Env and TimeoutMs follow TaskDefinition's subprocess
	// conventions, the {extensionRuntime} placeholder included. Every
	// operation of the contract runs this command. A provider gets no host RPC
	// channel.
	Command   string            `json:"command"`
	Args      []string          `json:"args,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	TimeoutMs int               `json:"timeoutMs,omitempty"`
	// Operations are the contract operations the provider implements, keyed by
	// operation name.
	Operations map[string]ProviderOperation `json:"operations"`
}

// ProviderOperation is one operation a provider declares. The catalog owns
// the operation's access, its destructiveness and its request and result
// documents; the declaration states only what the provider decides.
type ProviderOperation struct {
	// Description states the provider-specific behavior and limits of the
	// operation.
	Description string `json:"description"`
	// OpenWorld states whether the operation reaches a system outside this
	// machine. It is required: a provider says so either way.
	OpenWorld *bool `json:"openWorld"`
	// Preconditions states how the provider enforces a revision precondition:
	// one of ProviderPreconditions. The catalog says which operations take
	// one.
	Preconditions string `json:"preconditions,omitempty"`
	// WorkspaceSelection declares that the operation answers about the
	// workspace, and asks the orchestrator to resolve the workspace view it
	// needs: the complete resolved membership and the projection the canonical
	// `projects`, `impacted` and `baseline` arguments resolve to.
	//
	// It exists because an extension has no workspace loader and must never
	// grow one. Resolving `impacted` means diffing against a baseline ref,
	// mapping changed paths to owners and walking the dependent graph; resolving
	// `projects` means matching selectors against the workspace's own
	// identities. An extension that re-derived either would be a SECOND
	// definition of what the workspace contains. So the orchestrator, which
	// already holds the answer, publishes it, exactly as it does for a job
	// subprocess (`selection` and `workspaceProjects` in protocols/job).
	WorkspaceSelection bool `json:"workspaceSelection,omitempty"`
}

// Provider precondition rules: how a provider enforces a revision
// precondition.
const (
	// ProviderPreconditionsAtomic compares and writes in one step the backend
	// guarantees.
	ProviderPreconditionsAtomic = "atomic"
	// ProviderPreconditionsChecked compares, then writes; a concurrent writer
	// can interleave.
	ProviderPreconditionsChecked = "checked"
	// ProviderPreconditionsNone does not compare.
	ProviderPreconditionsNone = "none"
)

// ProviderPreconditions is the closed precondition vocabulary in canonical
// (sorted) order.
var ProviderPreconditions = []string{
	ProviderPreconditionsAtomic,
	ProviderPreconditionsChecked,
	ProviderPreconditionsNone,
}

// providerNameFormat is the grammar of a contract and of an operation name.
var providerNameFormat = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// DeclaresProviders reports whether the manifest declares at least one
// provider, the vocabulary of protocolcli.ProviderContract.
func DeclaresProviders(m *Manifest) bool {
	return m != nil && len(m.Providers) > 0
}

// ValidateProviders checks the shape of the manifest's providers, in sorted
// order for deterministic diagnostics. It is inert for a manifest that
// declares none. A malformed declaration fails package-time validation instead
// of becoming a provider that every call refuses at runtime.
func ValidateProviders(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}
	var diags []diag.Diagnostic
	for _, contract := range sortedKeys(m.Providers) {
		diags = append(diags, validateProviderDeclaration(contract, m.Providers[contract])...)
	}
	return diags
}

func validateProviderDeclaration(contract string, provider ProviderDeclaration) []diag.Diagnostic {
	field := "providers." + contract
	var diags []diag.Diagnostic
	if !providerNameFormat.MatchString(contract) {
		diags = append(diags, diag.Errorf("invalid-provider-name", field,
			"provider contract %q must be a contract name (for example, tasks)", contract))
	}
	if provider.Version < 1 {
		diags = append(diags, diag.Errorf("invalid-value", field+".version",
			"provider version must name the contract version, 1 or above"))
	}
	if strings.TrimSpace(provider.Command) == "" {
		diags = append(diags, diag.Errorf("required-field", field+".command", "provider command is required"))
	}
	if provider.TimeoutMs < 0 {
		diags = append(diags, diag.Errorf("invalid-value", field+".timeoutMs", "provider timeoutMs cannot be negative"))
	}
	if len(provider.Operations) == 0 {
		diags = append(diags, diag.Errorf("required-field", field+".operations",
			"a provider declares at least one operation"))
	}
	for _, name := range sortedKeys(provider.Operations) {
		diags = append(diags, validateProviderOperation(field+".operations."+name, name, provider.Operations[name])...)
	}
	return diags
}

func validateProviderOperation(field, name string, operation ProviderOperation) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if !providerNameFormat.MatchString(name) {
		diags = append(diags, diag.Errorf("invalid-provider-name", field,
			"operation name %q must be an operation of the contract (for example, find)", name))
	}
	if strings.TrimSpace(operation.Description) == "" {
		diags = append(diags, diag.Errorf("required-field", field+".description", "operation description is required"))
	}
	if operation.OpenWorld == nil {
		diags = append(diags, diag.Errorf("required-field", field+".openWorld",
			"operation openWorld is required: whether the operation reaches outside the machine"))
	}
	if operation.Preconditions != "" && !slices.Contains(ProviderPreconditions, operation.Preconditions) {
		diags = append(diags, diag.Errorf("invalid-enum", field+".preconditions",
			"operation preconditions must be one of %s", strings.Join(ProviderPreconditions, ", ")))
	}
	return diags
}
