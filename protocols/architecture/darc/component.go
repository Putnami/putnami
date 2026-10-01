package darc

import archproto "go.putnami.dev/protocol/architecture"

// Component is one runtime-enforced Domain Access & Replication Contract. The
// constructors of this package return one; the interface is closed so a value
// that is not one of them cannot claim to implement a contract.
//
// The interface lives with the components, but what a Component is FOR is the
// evidence channel: `go.putnami.dev/app/darc` walks a plugin's components and
// projects each one onto the `domainAccess` capability row describe emits.
type Component interface {
	// Contract returns the declaration the component enforces.
	Contract() archproto.Import
	// darcComponent keeps the interface closed to this package.
	darcComponent()
}

func (r *Reference) darcComponent()     {}
func (p *Projection[T]) darcComponent() {}
func (s *Snapshot[T]) darcComponent()   {}
func (c *Command[T]) darcComponent()    {}
