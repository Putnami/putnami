// Package cpubudget reads the CPU budget the scheduler grants a job and binds
// it to the controls the tools a job starts honor.
//
// The CLI exports the budget to every job as PUTNAMI_CPU_BUDGET, a positive
// integer of cores, and as GOMAXPROCS for the jobs of @putnami/go. A job
// passes it on to each tool through the tool's own control: a variable, such
// as RAYON_NUM_THREADS or CARGO_BUILD_JOBS, or a flag, such as swift's
// --jobs. A variable the operator already set wins over the budget. Without a
// budget, as in a run by hand, a job sets no variable and a flag takes
// GOMAXPROCS, the CPUs Go grants the process, its cgroup quota included.
package cpubudget

import (
	"os"
	"runtime"
	"strconv"
	"strings"

	"go.putnami.dev/sdk/extension/envkeys"
)

// Env is the variable that carries the budget to a job.
const Env = "PUTNAMI_CPU_BUDGET"

// Parse reads a budget value: a positive integer, else 0.
func Parse(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// Get is the budget of the running job, or 0 when it has none.
func Get() int {
	return Parse(os.Getenv(Env))
}

// In is the budget an environment entry list carries, or 0 when it carries
// none.
func In(env []string) int {
	return Parse(envkeys.Host.Last(env, Env))
}

// Threads is the thread count a tool takes from the job's budget: the budget,
// else GOMAXPROCS.
func Threads() int {
	if n := Get(); n > 0 {
		return n
	}
	return runtime.GOMAXPROCS(0)
}

// ThreadsIn is Threads for an environment entry list.
func ThreadsIn(env []string) int {
	if n := In(env); n > 0 {
		return n
	}
	return runtime.GOMAXPROCS(0)
}

// Bind returns env with NAME=<budget> appended for each name env does not
// assign, where budget is the one env carries. env is returned unchanged when
// it carries no budget or assigns every name. env is not modified.
func Bind(env []string, names ...string) []string {
	budget := In(env)
	if budget == 0 {
		return env
	}
	value := strconv.Itoa(budget)
	var out []string
	for _, name := range names {
		if envkeys.Host.Has(env, name) || envkeys.Host.Has(out, name) {
			continue
		}
		if out == nil {
			out = append(make([]string, 0, len(env)+len(names)), env...)
		}
		out = append(out, name+"="+value)
	}
	if out == nil {
		return env
	}
	return out
}

// Vars answers NAME=<budget> for each name the running process's environment
// does not set, from the job's budget; none without a budget. A job that
// starts a tool with its own environment plus a few entries passes these.
func Vars(names ...string) map[string]string {
	budget := Get()
	if budget == 0 {
		return nil
	}
	vars := map[string]string{}
	for _, name := range names {
		if _, set := os.LookupEnv(name); !set {
			vars[name] = strconv.Itoa(budget)
		}
	}
	if len(vars) == 0 {
		return nil
	}
	return vars
}
