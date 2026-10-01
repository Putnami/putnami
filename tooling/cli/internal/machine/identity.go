package machine

import (
	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
)

// Identity projects a plan node onto the typed v2 task identity.
//
// The derivation lives on the plan node itself
// (jobs.ScheduledJob.TypedIdentity, stamped once at planning): this package
// only NAMES it, so the machine documents and every other consumer read the
// same value by construction.
func Identity(job *jobs.ScheduledJob) protocolcli.TaskIdentity {
	return job.TypedIdentity()
}

// identityOfKey is the fallback for a result the plan does not name.
func identityOfKey(key string) protocolcli.TaskIdentity {
	return jobs.TaskIdentityOfKey(key)
}

// orUnknown fills a required member with the stated placeholder; the identity
// derivation in jobs uses the same rule.
func orUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
