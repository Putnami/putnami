package runcredential

import (
	"fmt"
	"sync"
)

// custody records whether this process started repository code, so that a
// hosted run hands its credential to no process started after that. A
// process that repository code started, or any process of the same user it
// left running, can rewrite the executable of a credential holder before the
// engine starts it: the store, the Putnami home and the temporary directory
// are writable by that user. A holder started before repository code runs the
// executable the store installed. A holder that reads more store files after
// it starts, such as a shell launcher or an interpreted entry, reads them after
// repository code may have rewritten them: the latch does not cover those
// reads, so a hosted run refuses a cache provider or credential-provider that
// is not its extension's native runtime (RequireNativeHolder, ADR 0055,
// part 4). A workspace-fetch job ends before repository code starts, and the
// image Exec starts is the CLI the store verified.
//
// A holder is a process that receives a credential from this one: a cache
// provider, a credential-provider, a workspace-fetch job, the image Exec
// starts. Repository code is every hook and every job other than a
// workspace-fetch job that receives the job credential. A toolchain probe, the
// help probe of a relaunch, a runtime-info handshake and the preparation of a
// store-installed extension start no repository code.
//
// Each held credential carries its own custody, so a process without a run
// credential has none and every call below does nothing there.
type custody struct {
	// mu is held for reading while a holder starts, and for writing while
	// repository code is recorded: repository code starts only once every
	// holder that started before it runs its own image.
	mu sync.RWMutex
	// reason names what ran repository code first, or is empty.
	reason string
}

// CustodyError reports a credential holder that would start after repository
// code ran.
type CustodyError struct {
	// Holder names the process that asked for the credential.
	Holder string
	// Reason names what ran repository code first.
	Reason string
}

func (e *CustodyError) Error() string {
	return fmt.Sprintf("%s: %s starts after %s ran repository code; a hosted run hands its credential to no process started after that",
		Flag, e.Holder, e.Reason)
}

// MarkRepositoryCodeStarted records that this process is about to start
// repository code that reason names, such as "hook hooks.commands.build.before".
// The caller calls it before it starts the process. It waits for a holder that
// is starting. The first reason stays. It does nothing without a run
// credential.
func MarkRepositoryCodeStarted(reason string) {
	c := held.Load()
	if c == nil {
		return
	}
	c.custody.mu.Lock()
	defer c.custody.mu.Unlock()
	if c.custody.reason == "" {
		c.custody.reason = reason
	}
}

// RequireCustody fails with a *CustodyError once this process recorded
// repository code, and returns nil otherwise or without a run credential.
// holder names what asks for the credential. It suits a handoff that prepares
// a holder before StartHolder starts it: the preparation then reads no
// credential in vain.
func RequireCustody(holder string) error {
	c := held.Load()
	if c == nil {
		return nil
	}
	c.custody.mu.RLock()
	defer c.custody.mu.RUnlock()
	return c.custody.check(holder)
}

// StartHolder runs start, which starts the process holder names and returns
// once that process runs its own image, unless this process recorded
// repository code: it then fails with a *CustodyError and does not run start.
// No repository code starts while start runs. Without a run credential it
// runs start and returns what start returns.
//
// start must neither start repository code nor call StartHolder.
func StartHolder(holder string, start func() error) error {
	c := held.Load()
	if c == nil {
		return start()
	}
	c.custody.mu.RLock()
	defer c.custody.mu.RUnlock()
	if err := c.custody.check(holder); err != nil {
		return err
	}
	return start()
}

// check is RequireCustody with the lock held.
func (c *custody) check(holder string) error {
	if c.reason == "" {
		return nil
	}
	return &CustodyError{Holder: holder, Reason: c.reason}
}
