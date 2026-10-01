package darc

import (
	"fmt"
	"sort"

	"go.putnami.dev/app"
	archproto "go.putnami.dev/protocol/architecture"
)

// The evidence half: what a registered component tells the describe pass.
//
// A component enforces a contract at run time; describe records THAT IT DOES, as
// one machine row in the project's capability manifest. The row is evidence and
// never authority — `putnami architecture validate` reads it beside the declared
// imports and reports a declared active contract nothing implements, or an
// implemented one nobody declared. Emitting a row cannot create a cross-domain
// permission; only a reviewed manifest edit can (ADR 0001).
//
// Describe stays the sole committer of the sidecar: nothing here writes
// a file, invents an artifact kind, or opens a second channel. A component hands
// its already-validated contract to a plugin, the plugin hands it to describe,
// and describe emits.

// Evidence projects one enforced contract onto the row describe emits.
//
// Every value is carried verbatim from the contract the component was
// constructed with — and that contract already passed the protocol's own
// validation, so the row cannot claim a mode or a behavior the gate would
// reject.
func Evidence(component Component) app.DomainAccessContract {
	contract := component.Contract()
	row := app.DomainAccessContract{
		Import: contract.ID,
		Mode:   string(contract.Mode),
		Status: string(contract.Status),
	}
	for role, carrier := range map[string]*archproto.Transport{
		"transport": contract.Transport,
		"bootstrap": contract.Bootstrap,
		"updates":   contract.Updates,
	} {
		if carrier == nil {
			continue
		}
		row.Transports = append(row.Transports, app.DomainAccessTransport{
			Role:         role,
			Kind:         string(carrier.Kind),
			Contract:     carrier.Contract,
			Availability: string(carrier.Availability),
		})
	}
	sort.Slice(row.Transports, func(i, j int) bool { return row.Transports[i].Role < row.Transports[j].Role })
	if consistency := contract.Consistency; consistency != nil {
		row.Enforced.MaxStaleness = consistency.MaxStaleness
		row.Enforced.OnMissing = string(consistency.OnMissing)
		row.Enforced.OnStale = string(consistency.OnStale)
		row.Enforced.Ordering = string(consistency.Ordering)
		row.Enforced.LateEvents = string(consistency.LateEvents)
	}
	if deletion := contract.Deletion; deletion != nil {
		row.Enforced.Deletion = string(deletion.Strategy)
	}
	if model := contract.LocalModel; model != nil {
		row.Enforced.Writer = model.Writer
		row.Enforced.Rebuild = string(model.Rebuild)
	}
	return row
}

// Plugin carries a project's enforced contracts into the describe pass.
//
// It takes no part in the runtime lifecycle beyond existing: it configures
// nothing, starts nothing, and holds no state the components do not already
// hold. Registering it is how a workload says "these contracts are implemented
// here", and the describe pass is what turns that into a committed row.
//
//	a.Use(darc.NewPlugin("observability-contracts", projection, usageCommand))
//
// A component registered twice is registered once: the manifest identity is the
// import and its mode, and a duplicate would be the same statement twice.
type Plugin struct {
	name       string
	components []Component
}

// NewPlugin builds the describe-only carrier for a project's enforced contracts.
func NewPlugin(name string, components ...Component) *Plugin {
	plugin := &Plugin{name: name}
	return plugin.Register(components...)
}

// Name identifies the plugin in the module tree.
func (p *Plugin) Name() string { return p.name }

// Register adds components. A component whose (import, mode) pair is already
// registered is ignored rather than duplicated.
func (p *Plugin) Register(components ...Component) *Plugin {
	seen := make(map[string]bool, len(p.components))
	for _, existing := range p.components {
		seen[componentKey(existing)] = true
	}
	for _, component := range components {
		if component == nil || seen[componentKey(component)] {
			continue
		}
		seen[componentKey(component)] = true
		p.components = append(p.components, component)
	}
	return p
}

// DomainAccessContracts reports every registered contract, ordered by import and
// mode so the emitted rows are deterministic.
func (p *Plugin) DomainAccessContracts() []app.DomainAccessContract {
	contracts := make([]app.DomainAccessContract, 0, len(p.components))
	for _, component := range p.components {
		contracts = append(contracts, Evidence(component))
	}
	sort.SliceStable(contracts, func(i, j int) bool {
		if contracts[i].Import != contracts[j].Import {
			return contracts[i].Import < contracts[j].Import
		}
		return contracts[i].Mode < contracts[j].Mode
	})
	return contracts
}

func componentKey(component Component) string {
	contract := component.Contract()
	return fmt.Sprintf("%s\x00%s", contract.ID, contract.Mode)
}
