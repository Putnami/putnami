package scheduler

import (
	"math"
	"os"
	"time"

	"go.putnami.dev/tooling/cli/internal/jobs/procdeadline"
)

// elapsesNever is a deadline or bound no test outlives.
const elapsesNever = time.Duration(math.MaxInt64)

// runningAtStart is a procdeadline.ProcessProgress under which a process
// counts as running as soon as exec.Cmd.Start returned, and its wait for a CPU
// is not measured.
func runningAtStart(*os.Process) func() procdeadline.ProcessSample { return nil }

// neverRunning is a procdeadline.ProcessProgress under which a process is
// never seen running, as if the host held it before its first instruction.
func neverRunning(*os.Process) func() procdeadline.ProcessSample {
	return func() procdeadline.ProcessSample { return procdeadline.ProcessSample{Held: true} }
}

// neverWaitingForCPU is a procdeadline.ProcessProgress under which a process
// runs at once and never waits for a CPU, as one blocked in its own work does.
func neverWaitingForCPU(*os.Process) func() procdeadline.ProcessSample {
	return func() procdeadline.ProcessSample { return procdeadline.ProcessSample{Measured: true} }
}

// alwaysWaitingForCPU is a procdeadline.ProcessProgress under which a process
// runs at once and then waits for a CPU all the time. Each sample reads a wait
// an hour longer than the last, which the deadline keeps to the time elapsed.
func alwaysWaitingForCPU(*os.Process) func() procdeadline.ProcessSample {
	var waited time.Duration
	return func() procdeadline.ProcessSample {
		waited += time.Hour
		return procdeadline.ProcessSample{WaitedForCPU: waited, Measured: true}
	}
}
