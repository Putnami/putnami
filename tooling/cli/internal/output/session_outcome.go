package output

import "go.putnami.dev/cli/model/jobs"

// abortDescription phrases why a run stopped short, in a form that reads the
// same across every renderer. Naming the source matters: "interrupted by user"
// tells the reader the missing results are their own doing, while an
// unattributed abort points at the environment instead.
func abortDescription(outcome jobs.SessionOutcome) string {
	switch outcome.AbortedBy {
	case jobs.AbortUser:
		return "interrupted by user"
	case jobs.AbortSignal:
		return "interrupted by signal"
	default:
		return "interrupted"
	}
}
