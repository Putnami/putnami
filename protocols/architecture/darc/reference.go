package darc

import (
	"fmt"
	"sort"

	archproto "go.putnami.dev/protocol/architecture"
)

// ErrFactNotImported reports a read of a fact the import does not minimize.
var ErrFactNotImported = fmt.Errorf("darc: the import does not name that fact")

// Reference is a stable handle on facts another domain owns.
//
// It is the thinnest of the five modes and the most common: nothing is copied,
// so there is no freshness bound, no ordering, and no local model. The consumer
// keeps an identity and resolves it at the owner.
//
// What that leaves to enforce is MINIMIZATION. An import names the exact facts
// it consumes, and the reason it does is that a fact nobody uses is a permission
// nobody needed. [Reference.Fact] refuses a name the contract does not carry, so
// reaching past the declared surface fails where it happens rather than being
// discovered later by a reviewer comparing code with a manifest.
//
// A Reference is immutable after construction and safe for concurrent use.
type Reference struct {
	contract archproto.Import
	facts    map[string]bool
}

// NewReference builds a reference from its declared contract. The contract is
// validated by the protocol, and the import must be ACTIVE: a planned reference
// is a target, and resolving one would make the claim in code that the protocol
// forbids a manifest from making.
func NewReference(contract archproto.Import) (*Reference, error) {
	if err := validateContract(contract, archproto.ModeReference); err != nil {
		return nil, err
	}
	if err := requireActive(contract, contract.Transport); err != nil {
		return nil, err
	}
	facts := make(map[string]bool, len(contract.Facts))
	for _, fact := range contract.Facts {
		facts[fact] = true
	}
	return &Reference{contract: contract, facts: facts}, nil
}

// Contract returns the declaration this reference enforces.
func (r *Reference) Contract() archproto.Import { return r.contract }

// Fact reports whether the import names the fact, and returns its provenance —
// the producer export that stays authoritative for it. A name the contract does
// not carry returns ErrFactNotImported.
func (r *Reference) Fact(name string) (provenance string, err error) {
	if !r.facts[name] {
		return "", fmt.Errorf("%w: import %s names %v, not %q",
			ErrFactNotImported, r.contract.ID, r.Facts(), name)
	}
	return r.contract.From.Export, nil
}

// Facts returns the minimized fact list the import declares, sorted.
func (r *Reference) Facts() []string {
	facts := make([]string, 0, len(r.facts))
	for fact := range r.facts {
		facts = append(facts, fact)
	}
	sort.Strings(facts)
	return facts
}
