package mcp

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// collabResolution is one contract's binding resolved against the extensions an
// entry path discovered.
type collabResolution struct {
	Contract string
	Status   collab.BindingStatus
	// Binding is the declared binding, when it parsed.
	Binding *collab.Binding
	// Spec is the bound contract version, when this orchestrator implements it.
	Spec *collab.ContractSpec
	// Provider is the bound extension, when exactly one available extension
	// carries the bound name.
	Provider *extension.ExtensionDescription
	// Operations are the bound provider's operations of the bound version,
	// present only when Status is bound.
	Operations map[string]collab.ProvidedOperation
	Issues     []collab.CapabilityIssue
	// Candidates are the available extensions that declare the contract, bound
	// or not. They are reported and never selected.
	Candidates []collab.ProviderIdentity
}

// resolveCollab resolves one contract. unavailable names extensions that were
// discovered but must not serve: disabled by the workspace, or not prepared
// from the exact lock. A binding is honored only when it names exactly one
// available extension that implements every required operation of the bound
// version and every operation the binding requires; nothing else is ever
// substituted for it.
func resolveCollab(document collabDocument, contract string, extensions []*extension.ExtensionDescription, unavailable map[string]bool) collabResolution {
	resolution := collabResolution{Contract: contract, Status: collab.BindingStatusUnbound}
	resolution.Candidates = candidates(contract, extensions, unavailable)
	blockIssues := document.Issues[""]
	if len(blockIssues) > 0 {
		resolution.Status = collab.BindingStatusInvalid
		resolution.Issues = append(resolution.Issues, blockIssues...)
	}
	if !document.bound(contract) {
		if resolution.Status != collab.BindingStatusInvalid {
			resolution.Issues = append(resolution.Issues, document.Unknown...)
		}
		return resolution
	}
	if issues := document.Issues[contract]; len(issues) > 0 {
		resolution.Status = collab.BindingStatusInvalid
		resolution.Issues = append(resolution.Issues, issues...)
	}
	binding, parsed := document.Bindings[contract]
	if !parsed {
		if resolution.Status != collab.BindingStatusInvalid {
			resolution.Status = collab.BindingStatusInvalid
			resolution.Issues = append(resolution.Issues, collab.CapabilityIssue{
				Reason: collab.ReasonBindingInvalid, Message: "options.collaboration." + contract + " cannot be read",
			})
		}
		return resolution
	}
	resolution.Binding = &binding
	if spec, ok := collab.Lookup(contract, binding.Version); ok {
		resolution.Spec = &spec
	}
	if resolution.Status == collab.BindingStatusInvalid {
		return resolution
	}
	invalid := func(reason, format string, args ...any) collabResolution {
		resolution.Status = collab.BindingStatusInvalid
		resolution.Issues = append(resolution.Issues, collab.CapabilityIssue{Reason: reason, Message: fmt.Sprintf(format, args...)})
		return resolution
	}

	var matches []*extension.ExtensionDescription
	for _, ext := range extensions {
		if ext != nil && ext.Name == binding.Provider {
			matches = append(matches, ext)
		}
	}
	switch {
	case len(matches) == 0 && unavailable[binding.Provider]:
		return invalid(collab.ReasonBindingProviderMissing,
			"%s is bound to %s, which is disabled or was not prepared from the workspace lock", contract, binding.Provider)
	case len(matches) == 0:
		return invalid(collab.ReasonBindingProviderMissing,
			"%s is bound to %s, which is not an installed extension of this workspace: declare it under extensions and run `putnami install`",
			contract, binding.Provider)
	case len(matches) > 1:
		return invalid(collab.ReasonBindingAmbiguous,
			"%s is bound to %s, and %d discovered extensions carry that name", contract, binding.Provider, len(matches))
	case unavailable[binding.Provider]:
		return invalid(collab.ReasonBindingProviderMissing,
			"%s is bound to %s, which is disabled or was not prepared from the workspace lock", contract, binding.Provider)
	}
	provider := matches[0]
	resolution.Provider = provider

	offer, diags := collab.ReadProviderOffer(provider.Tools)
	operations, offered := offer.Operations(contract, binding.Version)
	if !offered {
		if problems := contractDiagnostics(diags, contract); problems != "" {
			return invalid(collab.ReasonBindingIncomplete,
				"%s does not implement %s version %d: %s", provider.Name, contract, binding.Version, problems)
		}
		if versions := offer.Versions(contract); len(versions) > 0 {
			return invalid(collab.ReasonBindingVersion,
				"%s implements %s versions %v, not the bound version %d", provider.Name, contract, versions, binding.Version)
		}
		return invalid(collab.ReasonBindingIncomplete, "%s does not implement the %s contract", provider.Name, contract)
	}
	for _, name := range binding.Require {
		if _, ok := operations[name]; !ok {
			return invalid(collab.ReasonBindingIncomplete,
				"the workspace requires %s.%s, which %s does not offer", contract, name, provider.Name)
		}
	}
	resolution.Status = collab.BindingStatusBound
	resolution.Operations = operations
	resolution.Issues = append(resolution.Issues, document.Unknown...)
	return resolution
}

// contractDiagnostics renders the declaration errors that concern one
// contract's tools.
func contractDiagnostics(diags []diag.Diagnostic, contract string) string {
	var parts []string
	for _, d := range diag.Errors(diags) {
		if strings.Contains(d.Message, contract) || strings.Contains(d.Field, contract) {
			parts = append(parts, d.Message)
		}
	}
	return strings.Join(parts, "; ")
}

// candidates lists the available extensions declaring any version of contract.
func candidates(contract string, extensions []*extension.ExtensionDescription, unavailable map[string]bool) []collab.ProviderIdentity {
	var found []collab.ProviderIdentity
	for _, ext := range extensions {
		if ext == nil || unavailable[ext.Name] {
			continue
		}
		offer, _ := collab.ReadProviderOffer(ext.Tools)
		if len(offer.Versions(contract)) > 0 {
			found = append(found, collab.ProviderIdentity{Name: ext.Name, Version: ext.Version})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found
}

// Capabilities renders a resolution as the capabilities document.
func capabilitiesOf(resolution collabResolution) collab.Capabilities {
	capabilities := collab.Capabilities{
		Contract:          resolution.Contract,
		SupportedVersions: collab.SupportedVersions(resolution.Contract),
		Status:            resolution.Status,
		Binding:           resolution.Binding,
		Operations:        []collab.OperationCapability{},
		Issues:            resolution.Issues,
		Candidates:        resolution.Candidates,
	}
	if resolution.Provider != nil {
		capabilities.Provider = &collab.ProviderIdentity{Name: resolution.Provider.Name, Version: resolution.Provider.Version}
	}
	if resolution.Spec == nil {
		return capabilities
	}
	for _, op := range resolution.Spec.Operations {
		entry := collab.OperationCapability{Name: op.Name, Access: op.Access, Required: op.Required}
		if provided, ok := resolution.Operations[op.Name]; ok {
			entry.Supported = true
			entry.Preconditions = provided.Declaration.Preconditions
			entry.Description = provided.Definition.Description
			if annotations := provided.Definition.Annotations; annotations != nil && annotations.OpenWorldHint != nil {
				openWorld := *annotations.OpenWorldHint
				entry.OpenWorld = &openWorld
			}
		}
		capabilities.Operations = append(capabilities.Operations, entry)
	}
	return capabilities
}

// routedOperations lists the operations an entry path exposes for a declared
// contract: every operation the bound provider offers when the binding holds,
// otherwise the operations the binding promised (the required ones and the
// ones it requires), each of which answers with the binding's failure. An
// undeclared contract exposes none.
func routedOperations(resolution collabResolution) []collab.OperationSpec {
	if resolution.Status == collab.BindingStatusBound {
		ops := make([]collab.OperationSpec, 0, len(resolution.Operations))
		for _, op := range resolution.Spec.Operations {
			if _, ok := resolution.Operations[op.Name]; ok {
				ops = append(ops, op)
			}
		}
		return ops
	}
	if resolution.Binding == nil {
		return nil
	}
	spec := resolution.Spec
	if spec == nil {
		versions := collab.SupportedVersions(resolution.Contract)
		if len(versions) == 0 {
			return nil
		}
		latest, _ := collab.Lookup(resolution.Contract, versions[len(versions)-1])
		spec = &latest
	}
	var ops []collab.OperationSpec
	for _, op := range spec.Operations {
		if op.Required || slices.Contains(resolution.Binding.Require, op.Name) {
			ops = append(ops, op)
		}
	}
	return ops
}
