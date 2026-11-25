package toolchain

import (
	"maps"

	"go.putnami.dev/sdk/extension/cpubudget"
)

// BunThreadVars are the variables that bound the threads of a bun process,
// tsc included, which runs on bun: JavaScriptCore's parallel garbage
// collector starts one marker thread per core unless
// BUN_JSC_numberOfGCMarkers says otherwise.
var BunThreadVars = []string{"BUN_JSC_numberOfGCMarkers"}

// BunEnv returns env with the job's CPU budget bound to BunThreadVars, for a
// bun process the job starts: each variable the operator did not set takes the
// budget. Without a budget, env is returned as it is. env is not modified.
func BunEnv(env map[string]string) map[string]string {
	vars := cpubudget.Vars(BunThreadVars...)
	if len(vars) == 0 {
		return env
	}
	out := maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	for name, value := range vars {
		if _, set := out[name]; !set {
			out[name] = value
		}
	}
	return out
}
