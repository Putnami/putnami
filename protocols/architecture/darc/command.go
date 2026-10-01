package darc

import (
	"context"
	"fmt"
	"sync"

	archproto "go.putnami.dev/protocol/architecture"
)

// SendFunc delivers one command payload over the declared transport. It is the
// consumer's carrier — an HTTP call, an event publish, a queue write — because
// the protocol deliberately does not rank transports.
type SendFunc[T any] func(ctx context.Context, payload T) error

// Command is the right to ask another domain to do something.
//
// The producer stays the authority over whether it happens; the consumer
// declares only that it may ask, and over which carrier. What this type enforces
// is the part of that declaration a running process can get wrong: it refuses to
// send at all while the import or its transport is still planned, so a target
// design cannot quietly become traffic.
//
// The two send shapes are the call site's choice, not the contract's — the
// protocol has no fire-and-forget flag — so they are two methods rather than a
// mode. [Command.Send] surfaces the delivery error; [Command.Emit] does not,
// which is what an ingest whose failure must never fail the caller needs.
//
// A Command is safe for concurrent use.
type Command[T any] struct {
	contract archproto.Import
	send     SendFunc[T]

	mu       sync.Mutex
	observer func(error)
	sent     int
	failed   int
}

// CommandOption configures a command at construction.
type CommandOption[T any] func(*Command[T])

// WithFailureObserver receives the error from every [Command.Emit] that failed.
// Without one, a fire-and-forget failure is silent, which is what
// fire-and-forget means and almost never what an operator wants.
func WithFailureObserver[T any](observe func(error)) CommandOption[T] {
	return func(c *Command[T]) { c.observer = observe }
}

// NewCommand builds a command from its declared contract.
//
// The contract must be ACTIVE, and so must its transport. A planned import is a
// target: the protocol already forbids it from claiming a current project
// binding, and a component that sent over it would be making the same claim in
// code, where no gate reads it.
func NewCommand[T any](contract archproto.Import, send SendFunc[T], opts ...CommandOption[T]) (*Command[T], error) {
	if err := validateContract(contract, archproto.ModeCommand); err != nil {
		return nil, err
	}
	if send == nil {
		return nil, &ContractError{
			Import: contract.ID,
			Reason: "no carrier was supplied; a command needs the transport its contract declares",
		}
	}
	if err := requireActive(contract, contract.Transport); err != nil {
		return nil, err
	}
	command := &Command[T]{contract: contract, send: send}
	for _, opt := range opts {
		opt(command)
	}
	return command, nil
}

// Contract returns the declaration this command enforces.
func (c *Command[T]) Contract() archproto.Import { return c.contract }

// Send delivers the payload and waits for the carrier's answer. Use it when the
// caller's own outcome depends on the request being accepted.
func (c *Command[T]) Send(ctx context.Context, payload T) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := c.send(ctx, payload)
	c.record(err)
	if err != nil {
		return fmt.Errorf("send command %s over %s: %w", c.contract.ID, c.contract.Transport.Contract, err)
	}
	return nil
}

// Emit delivers the payload and does not report a delivery failure to the
// caller. It is the shape a contract like "a refused or unreachable ingest never
// fails a run" needs: the failure reaches the observer, and the caller carries on.
func (c *Command[T]) Emit(ctx context.Context, payload T) {
	if err := ctx.Err(); err != nil {
		c.record(err)
		c.notify(err)
		return
	}
	err := c.send(ctx, payload)
	c.record(err)
	c.notify(err)
}

func (c *Command[T]) record(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent++
	if err != nil {
		c.failed++
	}
}

func (c *Command[T]) notify(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	observer := c.observer
	c.mu.Unlock()
	if observer != nil {
		observer(fmt.Errorf("emit command %s over %s: %w", c.contract.ID, c.contract.Transport.Contract, err))
	}
}

// Stats reports how many sends this command attempted and how many failed. It
// exists because a fire-and-forget contract is otherwise unobservable from the
// caller's side, and "we sent nothing all day" and "everything failed" must not
// look alike.
func (c *Command[T]) Stats() (attempted, failed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent, c.failed
}
