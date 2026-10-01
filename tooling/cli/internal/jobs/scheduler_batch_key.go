package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// readyBatchKey returns the content identity used to group already-ready jobs.
// An empty key means the job keeps the historical singleton dispatch path.
func (s *Scheduler) readyBatchKey(job *ScheduledJob) string {
	key, _ := s.readyBatch(job)
	return key
}

// readyBatch returns the job's batch key together with the project cap that
// key folds (0 is unbounded). Returning both from one computation is what makes
// the cap a group is filled to the cap its key was computed with: a group only
// admits peers sharing the leader's key, so the leader's cap is the group's.
func (s *Scheduler) readyBatch(job *ScheduledJob) (string, int) {
	if s.cfg.CacheVerification {
		return "", 0
	}
	if job == nil || job.JobDef == nil || job.Extension == nil ||
		job.Project == nil || job.JobDef.Batchable == nil || !job.JobDef.Cache ||
		strings.TrimSpace(job.JobDef.Batchable.Tool) == "" {
		return "", 0
	}
	// A member of a plan-level shared node never batches. A batch opens
	// every member's task BEFORE running any of them, so a
	// follower and its leader landing in one dispatch group would park the
	// follower on a leader that cannot start until the follower's own open
	// returns. Two members of one shared node share a project and would already
	// be rejected by batchProjectOverlaps; this states the rule where the
	// grouping decision is made instead of relying on that coincidence.
	if job.SharedExecutionID != "" {
		return "", 0
	}
	if maxWorkers := job.JobDef.Batchable.MaxWorkers; maxWorkers > 0 &&
		s.workerCount > maxWorkers {
		return "", 0
	}
	if len(job.JobDef.FilePatterns) > 0 {
		hasInputs, err := store.HasMatchingFiles(
			filepath.Join(s.ws.Root, job.Project.Path),
			job.JobDef.FilePatterns,
		)
		if err != nil || !hasInputs {
			return "", 0
		}
	}

	jobCtx := BuildJobContext(s.ws, job, s.taskParams, jobConfigDefaults(s.ws, job), s.cfg.VersionInfo)
	params, err := json.Marshal(jobCtx.Params)
	if err != nil {
		return "", 0
	}
	// A workspace may replace the manifest's static cap through the parameter
	// the policy names. The key folds the resolved cap in the static cap's
	// place, so an unset option keeps today's key byte for byte and two caps
	// never share a batch. A cap of 1, or a value the plan refuses, keeps
	// singleton dispatch. validateBatchProjectLimits reads the same bag
	// (deliveredJobParams, which BuildJobContext assigns to jobCtx.Params).
	maxProjects, batches, err := batchProjectLimit(job.JobDef.Batchable, jobCtx.Params)
	if err != nil || !batches {
		return "", 0
	}

	configDigest, ok := s.batchConfigDigest(job)
	if !ok {
		// Batchability is tolerant: an unreadable declared config must never
		// make a previously valid singleton task fail.
		return "", 0
	}

	h := sha256.New()
	for _, field := range []string{
		job.Extension.Name,
		job.Extension.Version,
		cacheTaskName(job),
		job.JobDef.Traits.SideEffects,
		job.JobDef.Batchable.Tool,
		strconv.Itoa(maxProjects),
		strconv.Itoa(job.JobDef.Batchable.MaxWorkers),
		job.JobDef.Command,
		stringSliceKey(job.JobDef.Args),
		job.JobDef.Cwd,
		stringMapKey(job.JobDef.Env),
		invocationBatchKey(job),
		cacheToolchainVersion(job),
		configDigest,
		string(params),
	} {
		_, _ = h.Write([]byte(field))
		_, _ = h.Write([]byte{0})
	}
	key := hex.EncodeToString(h.Sum(nil))
	if s.cfg.Debug {
		slog.Debug("scheduler batch candidate", "job", job.Key(), "batchKey", key, "maxProjects", maxProjects)
	}
	return key, maxProjects
}

func (s *Scheduler) batchConfigDigest(job *ScheduledJob) (string, bool) {
	policy := job.JobDef.Batchable
	if policy == nil {
		return "", false
	}

	projRoot := filepath.Join(s.ws.Root, job.Project.Path)
	vars := extension.BuildTemplateVars(
		s.ws.Root,
		projRoot,
		jobExtensionRoot(s.ws, job),
		filepath.Join(s.ws.Root, ".putnami", "out", job.Project.Path, jobCommandName(job)),
	)
	for _, candidate := range policy.ConfigFiles {
		path := extension.ExpandTemplateVars(candidate, vars)
		if !filepath.IsAbs(path) {
			path = filepath.Join(projRoot, path)
		}
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", false
		}
		// Qualify identical bytes by their resolved source. Relative imports and
		// other config-local paths can make two copied files behave differently;
		// the common optimization target is projects sharing this exact root
		// config file.
		h := sha256.New()
		_, _ = h.Write([]byte(filepath.Clean(path)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		return hex.EncodeToString(h.Sum(nil)), true
	}

	// A task may intentionally have no config candidates. Keep that a stable
	// identity rather than disabling batching.
	return "none", true
}

func stringSliceKey(values []string) string {
	data, _ := json.Marshal(values)
	return string(data)
}

func stringMapKey(values map[string]string) string {
	data, _ := json.Marshal(values)
	return string(data)
}

// takePendingGroup preserves the pending queue's longest-first order before
// considering complete batches. If its first job has compatible peers that are
// already ready, they may share one invocation. A lone batch candidate can
// still yield to ordinary work that may unblock a peer, but a later batch must
// never jump ahead of an earlier long pole or dependency-unblocking job.
func (s *Scheduler) takePendingGroup(pending []*ScheduledJob) (jobGroup, []*ScheduledJob) {
	if len(pending) == 0 {
		return jobGroup{}, pending
	}

	first := pending[0]
	// Every candidate must share the leader's key, which folds the resolved
	// cap, so the cap returned with that key is the group's cap.
	key, maxProjects := s.readyBatch(first)
	if key == "" {
		return jobGroup{jobs: []*ScheduledJob{first}}, pending[1:]
	}

	group := jobGroup{jobs: []*ScheduledJob{first}}
	selected := make([]bool, len(pending))
	selected[0] = true
	admitsClaim := s.batchClaimAdmitter(first)
	if maxProjects > 0 {
		// Pending is longest-first. For bounded groups, pair a long pole with
		// the shortest compatible peers so several long tasks do not become
		// one straggling subprocess.
		for j := len(pending) - 1; j > 0 && len(group.jobs) < maxProjects; j-- {
			candidate := pending[j]
			if s.readyBatchKey(candidate) != key || batchProjectOverlaps(candidate, group.jobs) || !admitsClaim(candidate) {
				continue
			}
			group.jobs = append(group.jobs, candidate)
			selected[j] = true
		}
	} else {
		for j := 1; j < len(pending); j++ {
			candidate := pending[j]
			if s.readyBatchKey(candidate) != key || batchProjectOverlaps(candidate, group.jobs) || !admitsClaim(candidate) {
				continue
			}
			group.jobs = append(group.jobs, candidate)
			selected[j] = true
		}
	}

	if len(group.jobs) > 1 {
		return group, pendingWithout(pending, selected)
	}

	// Avoid launching a batch candidate alone while useful ordinary work is
	// ready. That work may be the last prerequisite for a compatible peer. This
	// is not a coalescing wait: a worker still receives work immediately.
	//
	// The candidate does NOT yield to work carrying a strictly shorter
	// critical path. Yielding bets that a future batch beats dispatching the
	// candidate alone, and that bet cannot pay off when the candidate's own
	// chain bounds the session: every peer the wait could recruit finishes
	// inside a shorter chain, so the wait only delays the session's long pole.
	// Measured on this repo's l,t,b --all gate, the CLI test suite — ready at
	// t=0, longest chain of the whole plan — sat queued for ~30s while every
	// short-chain micro-job drained ahead of it through this loop. With a cold
	// stats store every CriticalPathMs is zero and the historical yield-to-any
	// behavior is unchanged.
	for i := 1; i < len(pending); i++ {
		if s.readyBatchKey(pending[i]) != "" {
			continue
		}
		if pending[i].CriticalPathMs < first.CriticalPathMs {
			continue
		}
		return jobGroup{jobs: []*ScheduledJob{pending[i]}}, removePendingAt(pending, i)
	}

	// There is no idle coalescing window when only batch candidates remain.
	return group, pending[1:]
}

func pendingWithout(pending []*ScheduledJob, selected []bool) []*ScheduledJob {
	rest := make([]*ScheduledJob, 0, len(pending))
	for i, job := range pending {
		if i >= len(selected) || !selected[i] {
			rest = append(rest, job)
		}
	}
	return rest
}

func removePendingAt(pending []*ScheduledJob, index int) []*ScheduledJob {
	rest := make([]*ScheduledJob, 0, len(pending)-1)
	rest = append(rest, pending[:index]...)
	return append(rest, pending[index+1:]...)
}

// batchProjectOverlaps keeps parent/child project roots out of the same tool
// invocation. Passing both roots would process child files twice and make
// per-project diagnostic/cache attribution ambiguous.
func batchProjectOverlaps(candidate *ScheduledJob, group []*ScheduledJob) bool {
	if candidate == nil || candidate.Project == nil {
		return true
	}
	candidatePath := filepath.Clean(candidate.Project.Path)
	for _, existing := range group {
		if existing == nil || existing.Project == nil {
			return true
		}
		if projectPathsOverlap(candidatePath, filepath.Clean(existing.Project.Path)) {
			return true
		}
	}
	return false
}

func projectPathsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err == nil && rel != ".." && !filepath.IsAbs(rel) &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// invocationBatchKey isolates a consumer of an invocation-scoped resource from
// every job that is not the SAME relation's consumer.
//
// A batch is one subprocess with one job context, so the whole group receives
// the leader's `invocation` member and, through it, the leader's private
// artifact root. Two jobs may only share that when they would have received it
// identically — which, since a relation belongs to exactly one project's
// pipeline, means never sharing at all. Batching a provisioned consumer with an
// unprovisioned peer would either hand the peer a database it never declared or
// silently run the provisioned project's tests against nothing.
//
// The producer's plan key is the discriminator because it is stamped at PLAN
// time (resolveInvocationRelations), long before the locator exists: a batch key
// computed at dispatch must not depend on whether the producer has already run.
//
// This is also what the deleted extraEnv term used to do by accident — the
// synthesized DATABASE_TEST_BINDINGS differed per project, so a provisioned test
// job could never share a key with anything.
func invocationBatchKey(job *ScheduledJob) string {
	if job == nil || job.InvocationProducer == nil {
		return ""
	}
	return job.InvocationProducer.Key()
}
