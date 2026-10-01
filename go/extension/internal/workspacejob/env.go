package workspacejob

import (
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/envkeys"
)

// Env is the environment a job exports to the commands it starts.
//
// The script exported variables into its own process and every command it ran
// inherited them. Env keeps that model without touching the environment of the
// extension process: a job reads and sets variables here, and each command
// starts with Environ. The order of the entries is kept, so a command sees the
// variables in a stable order.
type Env struct {
	keys   []string
	values map[string]envEntry
}

type envEntry struct {
	name  string
	value string
}

// NewEnv returns an environment holding the "KEY=value" entries of environ.
// A later entry replaces an earlier one with the same key.
func NewEnv(environ []string) *Env {
	env := &Env{values: make(map[string]envEntry, len(environ))}
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			continue
		}
		env.Set(name, value)
	}
	return env
}

// Get returns the value of key, or "" when it is unset. Like the script's
// "${KEY:-}", it does not tell an unset variable from an empty one.
func (e *Env) Get(key string) string {
	return e.values[envkeys.Host.Canonical(key)].value
}

// Set exports key with value.
func (e *Env) Set(key, value string) {
	folded := envkeys.Host.Canonical(key)
	if existing, ok := e.values[folded]; ok {
		existing.value = value
		e.values[folded] = existing
		return
	}
	e.keys = append(e.keys, folded)
	e.values[folded] = envEntry{name: key, value: value}
}

// Clone returns an independent copy of the environment.
func (e *Env) Clone() *Env {
	clone := &Env{keys: append([]string(nil), e.keys...), values: make(map[string]envEntry, len(e.values))}
	for key, entry := range e.values {
		clone.values[key] = entry
	}
	return clone
}

// Environ returns the environment as "KEY=value" entries, with overrides
// ("KEY=value" entries that apply to one command) replacing the exported
// values.
func (e *Env) Environ(overrides ...string) []string {
	env := e
	if len(overrides) > 0 {
		env = e.Clone()
		for _, entry := range overrides {
			if name, value, ok := strings.Cut(entry, "="); ok && name != "" {
				env.Set(name, value)
			}
		}
	}
	out := make([]string, 0, len(env.keys))
	for _, key := range env.keys {
		entry := env.values[key]
		out = append(out, entry.name+"="+entry.value)
	}
	return out
}

// PrependPath puts dir first on PATH, as the script's
// PATH="$dir:$PATH" did: an existing entry for dir is kept.
func (e *Env) PrependPath(dir string) {
	current := e.Get("PATH")
	if current == "" {
		e.Set("PATH", dir)
		return
	}
	e.Set(pathEnvName(e), dir+string(filepath.ListSeparator)+current)
}

// pathEnvName is the spelling of PATH this environment already uses, so a
// prepend replaces the variable rather than adding a second spelling.
func pathEnvName(e *Env) string {
	if entry, ok := e.values[envkeys.Host.Canonical("PATH")]; ok {
		return entry.name
	}
	return "PATH"
}

// appendCommaValue appends value to a comma-separated list, as the script's
// "${LIST:+${LIST},}${value}" did: nothing is removed or deduplicated.
func appendCommaValue(list, value string) string {
	if list == "" {
		return value
	}
	return list + "," + value
}

// removeCommaValue drops every entry of a comma-separated list that is empty
// or equal to value, keeping the others in order.
func removeCommaValue(list, value string) string {
	kept := make([]string, 0, strings.Count(list, ",")+1)
	for _, entry := range strings.Split(list, ",") {
		if entry == "" || entry == value {
			continue
		}
		kept = append(kept, entry)
	}
	return strings.Join(kept, ",")
}
