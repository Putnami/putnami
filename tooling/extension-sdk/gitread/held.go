package gitread

import (
	"sync/atomic"
)

// Held is a source of answers a process kept for git calls whose answer is a
// function of files it can witness, such as the repository's refs and its
// work tree. A process that installs one (UseHeld) answers each call it holds
// without starting git; every other process, and every call it does not
// hold, runs git.
//
// Only calls in the process's environment are offered: a call with an
// environment of its own (RunCaptureEnv with a non-nil env) always runs git.
type Held interface {
	// Answer returns what git printed for args, with stdin on its standard
	// input and env as its environment, in dir, when the source holds an
	// answer that is still git's.
	Answer(dir, stdin string, env, args []string) (stdout, stderr string, ok bool)
	// Asking is called before git runs args in dir with env. When it returns
	// a function, that function is called with what git printed once git
	// exited 0, so the source can keep the answer.
	Asking(dir, stdin string, env, args []string) (keep func(stdout, stderr string))
}

var held atomic.Pointer[heldSource]

type heldSource struct{ Held }

// UseHeld installs h as the process's source of held answers and returns the
// function that restores the previous one, unless another source was
// installed since. A nil h installs none.
func UseHeld(h Held) (restore func()) {
	var next *heldSource
	if h != nil {
		next = &heldSource{h}
	}
	previous := held.Swap(next)
	return func() { held.CompareAndSwap(next, previous) }
}

// RunHeld answers a call of git with args in dir, with stdin on its standard
// input and env as its environment, from the process's held answers when they
// hold one, and runs it otherwise: run starts git and returns what it
// printed. A call run is offered to the held answers to keep once it
// succeeded.
func RunHeld(dir, stdin string, env, args []string, run func() (stdout, stderr string, err error)) (string, string, error) {
	source := held.Load()
	if source == nil {
		return run()
	}
	if stdout, stderr, ok := source.Answer(dir, stdin, env, args); ok {
		return stdout, stderr, nil
	}
	keep := source.Asking(dir, stdin, env, args)
	stdout, stderr, err := run()
	if err == nil && keep != nil {
		keep(stdout, stderr)
	}
	return stdout, stderr, err
}
