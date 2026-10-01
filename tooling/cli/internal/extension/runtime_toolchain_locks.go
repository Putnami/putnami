package extension

import "sort"

// RuntimeToolchainLocks returns the sorted workspace-lock identities the
// runtime of ext resolves: the toolchains of every task and job, the runtime's
// runToolchains, and, for a mutable local source, the toolchains its prepare
// step builds with. A published runtime never prepares, so its prepare
// toolchains are not counted. An optional declaration counts: the runtime uses
// the pin whenever the lock carries one.
//
// The result is a superset of every identity a command of ext can resolve, so
// a lock refresh that keeps these pins never breaks the next command.
func RuntimeToolchainLocks(ext *ExtensionDescription) []string {
	if ext == nil || ext.Runtime == nil || len(ext.Runtime.Toolchains) == 0 {
		return nil
	}
	refs := make(map[string]bool)
	add := func(aliases []string) {
		for _, alias := range aliases {
			refs[alias] = true
		}
	}
	add(ext.Runtime.RunToolchains)
	for _, task := range ext.Tasks {
		add(task.Toolchains)
	}
	for _, def := range ext.Jobs {
		if def != nil {
			add(def.Toolchains)
		}
	}
	if ext.LocalSource && ext.Runtime.Prepare != nil {
		add(ext.Runtime.Prepare.Toolchains)
	}
	seen := make(map[string]bool, len(refs))
	locks := make([]string, 0, len(refs))
	for alias := range refs {
		declaration, declared := ext.Runtime.Toolchains[alias]
		if !declared || declaration.Lock == "" || seen[declaration.Lock] {
			continue
		}
		seen[declaration.Lock] = true
		locks = append(locks, declaration.Lock)
	}
	sort.Strings(locks)
	return locks
}
