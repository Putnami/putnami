package jobs

import (
	"fmt"
	"sort"
	"strings"
)

// Named resource budgets are the scheduler's SECOND admission dimension.
//
// --max-parallel bounds how MANY dispatches run at once, and the resource pool
// bounds how much of the MACHINE they hold. Neither can say that a subset of
// tasks shares one scarce external thing — a database's connection ceiling, a
// port range, a remote-cache pipe. Without that, the only way to keep the
// heaviest claimant safe is to lower the global count, which throttles every
// task that never touches the resource: a large consumer workspace sized its
// whole fan-out at 4 because one test task opens ~230 PostgreSQL connections,
// while the lint and build tasks that make up most of a run open none.
//
// A budget closes that gap with two halves that meet by NAME and nothing else:
//
//   - a task declares what it consumes (manifest `resources: {name: units}`),
//   - a run declares what exists (`--resource <name>=<units>`, repeatable).
//
// Names are opaque here. The CLI knows nothing about databases; PostgreSQL
// connections are only the first claimant.
//
// Three rules keep the primitive from ever making a run WORSE than it is
// today, and each is enforced in exactly one place:
//
//  1. A resource with no budget on the run is UNLIMITED — normalizeClaims
//     drops it — so every existing run, which declares none, admits exactly as
//     it did before.
//  2. A task that claims nothing is never gated by anyone else's claim: it
//     carries an empty claim set through admission untouched.
//  3. A single claim larger than the whole budget is refused at PLAN time
//     (ValidateResourceBudgets) instead of waiting forever for a budget that
//     can never free enough. normalizeClaims still clamps as a second belt, so
//     a scheduler assembled outside the planner degrades to "runs alone"
//     rather than deadlocking.

// jobResourceClaims returns one job's declared claims, or nil when it declares
// none. Non-positive units are dropped: the manifest validator already rejects
// them, and a zero-unit claim that survived would occupy a name in the ledger
// while gating nothing.
func jobResourceClaims(job *ScheduledJob) map[string]int {
	if job == nil || job.JobDef == nil || len(job.JobDef.Resources) == 0 {
		return nil
	}
	claims := make(map[string]int, len(job.JobDef.Resources))
	for name, units := range job.JobDef.Resources {
		if units > 0 {
			claims[name] = units
		}
	}
	if len(claims) == 0 {
		return nil
	}
	return claims
}

// groupResourceClaims aggregates the claims of one dispatch group.
//
// It SUMS, where groupResourceRequest takes the maximum for CPU and one shared
// envelope for memory. Those two are shared because a batch is one subprocess
// with one thread pool and one RSS; an external resource is not. A batched
// `go test` runs its members' packages in PARALLEL inside that single process,
// so their connections are open at the same time, and a named budget stands for
// a hard ceiling where exceeding the real limit fails the workload rather than
// merely slowing it.
//
// Group FORMATION is what keeps the sum inside the budget — see
// batchClaimAdmitter, which refuses a peer that would push the group past it.
func groupResourceClaims(jobs []*ScheduledJob) map[string]int {
	var claims map[string]int
	for _, job := range jobs {
		for name, units := range jobResourceClaims(job) {
			if claims == nil {
				claims = make(map[string]int)
			}
			claims[name] += units
		}
	}
	return claims
}

// batchClaimAdmitter returns the group-formation predicate for a batch led by
// first: whether adding one more peer keeps the group's SUMMED named claims
// inside the run's budgets.
//
// The check belongs at formation rather than at admission because the two have
// different remedies. A group refused at admission would either wait — for a
// release that can never be large enough — or be clamped, and a clamped group
// still runs, holding more of the external resource than the run says exists.
// Leaving the peer in the pending queue costs it only its own turn.
//
// The returned closure carries the group's running total, so it is stateful and
// must be called at most once per accepted candidate — which is why it sits
// last in takePendingGroup's short-circuiting condition.
func (s *Scheduler) batchClaimAdmitter(first *ScheduledJob) func(*ScheduledJob) bool {
	claims := jobResourceClaims(first)
	return func(candidate *ScheduledJob) bool {
		peer := jobResourceClaims(candidate)
		if len(peer) == 0 {
			return true
		}
		combined := make(map[string]int, len(claims)+len(peer))
		for name, units := range claims {
			combined[name] = units
		}
		for name, units := range peer {
			combined[name] += units
		}
		if !s.resources.claimsFitBudgets(combined) {
			return false
		}
		claims = combined
		return true
	}
}

// ValidateResourceBudgets refuses a plan whose single task can never be
// admitted: one execution claims more units of a resource than the entire
// budget the run declared for it, so no amount of waiting frees enough. It is
// the plan-time counterpart of admission — a message naming the resource and
// both amounts, rather than a scheduler that stalls with idle workers.
//
// Claims against resources the run did not budget are unlimited and reported as
// nothing. Group aggregation is deliberately NOT validated here: a batch is a
// dispatch-time grouping the planner cannot predict, and batchClaimAdmitter
// already keeps a forming group inside the budget. Together the two mean every
// dispatch the coordinator ever offers the pool is one it can eventually admit.
func ValidateResourceBudgets(planned []*ScheduledJob, budgets map[string]int) error {
	if len(budgets) == 0 {
		return nil
	}
	var lines []string
	for _, job := range planned {
		claims := jobResourceClaims(job)
		names := make([]string, 0, len(claims))
		for name := range claims {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			budget, declared := budgets[name]
			if !declared || claims[name] <= budget {
				continue
			}
			lines = append(lines, fmt.Sprintf("  %s claims %d units of %q, run budget is %d",
				job.Key(), claims[name], name, budget))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("resource budget too small for a single task:\n%s\nraise it with --resource <name>=<units> or lower the task's declared claim",
		strings.Join(lines, "\n"))
}
