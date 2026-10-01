package doctor

// Production preflight gate for `putnami build --profile production`. This
// file WIRES the existing doctor engine
// together: the checks engine (doctor_checks*.go), profile grading (doctor.go's
// severityFor), and waiver evaluation (doctor_waivers.go). It adds NO new check
// logic and does not restructure the engine — DoctorPreflight is a thin
// composition over DoctorRun, the single evaluate+waiver invocation shared by
// the build gate and by downstream deploy tooling.

import (
	"time"

	"go.putnami.dev/cli/model/workspace"
	doctor "go.putnami.dev/protocol/doctor"
)

// DoctorPreflight evaluates the committed artifacts of the given projects under
// profile and returns the deterministic report plus, when an UNWAIVED high or
// critical finding remains (only reachable under the production profile) or the
// committed doctor.waivers.json is malformed, a blocking exit-2 error
// (protocolcli.ErrInvalidConfig, carrying the report via WithResultData for the
// structured failure envelope). A clean run, findings that are only info/warning,
// and blocking findings a live waiver suppressed all return a nil error.
//
// It is the same engine invocation the build gate uses (runProductionPreflight)
// and is exported so deploy tooling can gate a deployment on the identical
// contract without duplicating check or waiver logic — a thin composition over
// DoctorRun.
//
// Its signature IS engine.PreflightGate: since an earlier change,
// every adapter injects this function into engine.Request.Preflight rather than
// the engine importing this package, so the one build-gate implementation is
// still this one — it is now handed over instead of reached for.
//
// The clock is injected here at the single top-level seam the engine defines —
// DoctorRun's now parameter — exactly as DoctorCommand does: real time.Now()
// drives waiver expiry, so the report stays a pure function of the committed
// tree, the profile, and that clock. No other code path in the gate reads the
// wall clock.
func DoctorPreflight(wsRoot string, projects []*workspace.Project, profile doctor.Profile) (doctor.Report, error) {
	return DoctorRun(wsRoot, projects, profile, time.Now())
}
