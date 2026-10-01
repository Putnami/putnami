// Package envkeys matches process environment entries ("NAME=value") by
// variable name, the one rule the CLI and the extensions share.
//
// Names compare exactly on Unix. On Windows they compare without regard to
// case: the system block spells PATH as "Path", and a child process sees one
// variable for every spelling. A caller that builds a child environment with
// exact matching there keeps the system "Path" beside its own "PATH", and the
// child runs with whichever entry os/exec keeps.
package envkeys

import (
	"runtime"
	"strings"
)

// Keys matches environment entries by variable name. With Fold set, names
// compare by their upper case.
type Keys struct{ Fold bool }

// Host matches names the way this platform's process environment does:
// exactly on Unix, and without regard to case on Windows.
var Host = Keys{Fold: runtime.GOOS == "windows"}

// Canonical is the form under which name is stored when every spelling that
// matches it is one variable: name itself, or its upper case when k folds.
func (k Keys) Canonical(name string) string {
	if k.Fold {
		return strings.ToUpper(name)
	}
	return name
}

// Value returns the value entry assigns and whether entry assigns key.
func (k Keys) Value(entry, key string) (string, bool) {
	name, value, ok := strings.Cut(entry, "=")
	if !ok || name != key && (!k.Fold || k.Canonical(name) != k.Canonical(key)) {
		return "", false
	}
	return value, true
}

// Has reports whether any entry assigns key.
func (k Keys) Has(env []string, key string) bool {
	for _, entry := range env {
		if _, ok := k.Value(entry, key); ok {
			return true
		}
	}
	return false
}

// Last returns the value of the last entry that assigns key, the one os/exec
// keeps, or "" when none does.
func (k Keys) Last(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := k.Value(env[i], key); ok {
			return value
		}
	}
	return ""
}

// Remove drops every entry that assigns key. env is returned unchanged when no
// entry does.
func (k Keys) Remove(env []string, key string) []string {
	if !k.Has(env, key) {
		return env
	}
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		if _, ok := k.Value(entry, key); !ok {
			kept = append(kept, entry)
		}
	}
	return kept
}

// Set returns a new environment in which one trailing KEY=value entry replaces
// every entry that assigns key. env is not modified.
func (k Keys) Set(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if _, ok := k.Value(entry, key); ok {
			continue
		}
		out = append(out, entry)
	}
	return append(out, key+"="+value)
}
