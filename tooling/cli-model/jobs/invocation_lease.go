package jobs

import (
	protocoljob "go.putnami.dev/protocol/job"
)

// InvocationLocator returns the non-secret invocation locator this node
// received, or nil when the node participates in no finalizes relation.
func (s *ScheduledJob) InvocationLocator() *protocoljob.Invocation {
	if s == nil {
		return nil
	}
	return s.Invocation
}
