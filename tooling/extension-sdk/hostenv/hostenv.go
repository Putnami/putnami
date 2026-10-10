// Package hostenv owns the single list of environment variables a managed
// runtime injects into a process to describe THE DEPLOYMENT IT IS RUNNING, and
// the scrub every language extension applies to that list before it spawns a
// test process.
//
// THE PROBLEM. A `putnami test` run executed on a managed runtime — a
// CI worker that is itself a Cloud Run service, an App Engine job, a Lambda —
// inherits that runtime's identity block. `K_SERVICE` and friends then reach
// the test subprocess, where application code reads them as "I am the deployed
// production workload" and changes behavior:
//
//	allowEphemeral = config.allowEphemeralSigningKey && !process.env.K_SERVICE
//
// The guard is correct; the signal is not. `K_SERVICE=ci-worker` describes the
// HARNESS HOST, not the code under test, so the guard fails closed inside a
// test and the suite dies with an error no local run can reproduce
// ("NoSigningKeyError: no signing key configured and allowEphemeral is false").
// The failure is deterministic on the hosted worker and invisible everywhere
// else, which is the worst shape a test-environment bug can have.
//
// THE RULE. A test process must not be able to tell which managed runtime, if
// any, launched its harness. Scrubbing these variables also makes the test
// task's declared cache key HONEST: none of them is a declared `{"from":"env"}`
// input of any test task, so before the scrub a cache hit and a fresh run could
// legitimately disagree. After it, they cannot.
//
// ADMISSION CRITERIA. A variable belongs here when BOTH hold:
//
//  1. A managed runtime sets it automatically to describe the deployment it
//     injected it into — nobody types it, and it means nothing off-platform.
//  2. It carries no capability. It is not a credential, not a service
//     endpoint, and not a region/project selector a client SDK needs to
//     address an API.
//
// Criterion 2 is what keeps integration tests working. Deployment IDENTITY is
// scrubbed; CREDENTIALS are not. `GOOGLE_APPLICATION_CREDENTIALS`,
// `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_SESSION_TOKEN`,
// `AWS_REGION`, `KUBERNETES_SERVICE_HOST` (an API-server endpoint) and
// `DATABASE_TEST_BINDINGS` all stay: a test that talks to a real backend needs
// them, and none of them is what an in-app production guard keys on.
//
// Two deliberate exclusions worth naming, because they look like candidates:
//
//   - `GOOGLE_CLOUD_PROJECT` (and `GCLOUD_PROJECT`). It is BOTH a platform
//     signal and a capability: every Google client library reads it to choose
//     which project to address, and the framework's JSON log sinks read it to
//     qualify trace IDs.
//     Criterion 2 rejects it, so it is preserved.
//   - `PORT`. Cloud Run and App Engine set it, but it is a configuration knob a
//     developer sets locally on purpose, not an identity. Criterion 1 rejects
//     it.
//
// SCOPE. Whole injected blocks are scrubbed, not just the one variable a bug
// report named: an application guard may key on any member of a platform's
// block (`GAE_ENV` reads exactly as production-ish as `GAE_SERVICE`), and
// leaving a partial block behind just moves the bug.
//
// Platforms are admitted when the repository has a reason to care about them:
// Google Cloud is the evidenced case, and AWS Lambda is named as a target
// adapter by the framework's own API server. Azure, Vercel, Fly and Heroku are
// deliberately absent — no putnami surface detects them today, and each ships
// a set that mixes identity with build provenance and endpoints that would need
// the criteria applied variable by variable. Adding one later is a list edit
// plus a test; the criteria above are what a reviewer checks it against.
//
// STABILITY CONTRACT. The list only ever grows, and growth is additive-safe:
// removing a variable from a test process cannot make a correct test fail,
// because a correct test does not assert on the identity of the machine that
// launched it. The list is sorted and duplicate-free (pinned by a test) so a
// diff to it is reviewable. Matching is exact and case-sensitive, matching
// POSIX environment semantics on the container platforms this describes.
//
// JOB VARIABLES. A hosted run also describes each job to the extension that
// runs it, in two variables of go.putnami.dev/protocol/extension:
// OfflineDependenciesEnv and JobCredentialFDEnv. They are addressed to the
// extension, not to the repository's tests. A test process that inherits them
// takes the paths of a job of a hosted run (it refuses to refresh a credential,
// it treats every dependency as already downloaded) and fails there and nowhere
// else. JobVars lists them and ScrubJobVars removes them. The extension applies
// its offline policy first: the scrub removes the two names only, so the
// package manager settings that forbid a download stay in the test process.
package hostenv

import (
	"slices"
	"strings"

	extensionproto "go.putnami.dev/protocol/extension"
)

// platformIdentityVars is the sorted, duplicate-free set of host deployment
// identity variables. Grouped by the platform that injects them; kept sorted
// within the file as one flat list so the invariant test can check it directly.
var platformIdentityVars = []string{
	// AWS Lambda. `_HANDLER`, `LAMBDA_TASK_ROOT` and `LAMBDA_RUNTIME_DIR` are
	// the unprefixed members of the same injected block.
	"AWS_EXECUTION_ENV",
	"AWS_LAMBDA_FUNCTION_MEMORY_SIZE",
	"AWS_LAMBDA_FUNCTION_NAME",
	"AWS_LAMBDA_FUNCTION_VERSION",
	"AWS_LAMBDA_INITIALIZATION_TYPE",
	"AWS_LAMBDA_LOG_GROUP_NAME",
	"AWS_LAMBDA_LOG_STREAM_NAME",
	// The Lambda runtime API is an endpoint, but one that exists only inside a
	// live Lambda sandbox and can serve nothing to a test process, so criterion
	// 2 admits it as part of the identity block.
	"AWS_LAMBDA_RUNTIME_API",
	// Cloud Run jobs (as opposed to services, which use the K_* block).
	"CLOUD_RUN_EXECUTION",
	"CLOUD_RUN_JOB",
	"CLOUD_RUN_TASK_ATTEMPT",
	"CLOUD_RUN_TASK_COUNT",
	"CLOUD_RUN_TASK_INDEX",
	// Cloud Functions / functions-framework (gen2 also sets the K_* block).
	// The gen1 `FUNCTION_NAME`/`ENTRY_POINT` names are deliberately omitted:
	// they are generic enough to collide with a variable someone set on
	// purpose, and gen1 is retired.
	"FUNCTION_SIGNATURE_TYPE",
	"FUNCTION_TARGET",
	// App Engine standard and flexible.
	"GAE_APPLICATION",
	"GAE_DEPLOYMENT_ID",
	"GAE_ENV",
	"GAE_INSTANCE",
	"GAE_MEMORY_MB",
	"GAE_RUNTIME",
	"GAE_SERVICE",
	"GAE_VERSION",
	// Knative / Cloud Run services. K_SERVICE is the variable this was
	// reported against and the one the framework itself treats as the
	// production signal (typescript/framework/runtime config + logger,
	// go/framework logging sink selection).
	"K_CONFIGURATION",
	"K_REVISION",
	"K_SERVICE",
	"LAMBDA_RUNTIME_DIR",
	"LAMBDA_TASK_ROOT",
	"_HANDLER",
}

var platformIdentitySet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(platformIdentityVars))
	for _, name := range platformIdentityVars {
		set[name] = struct{}{}
	}
	return set
}()

// PlatformIdentityVars returns the host deployment identity variable names, in
// sorted order. The returned slice is a fresh copy, so a caller may sort,
// filter or extend it without affecting anyone else's view of the list.
func PlatformIdentityVars() []string {
	return append([]string(nil), platformIdentityVars...)
}

// IsPlatformIdentity reports whether name is a host deployment identity
// variable. The match is exact and case-sensitive.
func IsPlatformIdentity(name string) bool {
	_, ok := platformIdentitySet[name]
	return ok
}

// ScrubPlatformIdentity returns a copy of env — `KEY=VALUE` entries, as
// produced by os.Environ and consumed by exec.Cmd.Env — with every host
// deployment identity variable removed.
//
// It scrubs the INHERITED environment only. A harness that sets one of these
// names deliberately afterwards still wins, because its entry is appended after
// the scrub and the last entry for a name is the one a process sees. That is
// the same ordering exec.UnsetEnv gives the TypeScript path, so all three
// language extensions resolve a conflict identically.
//
// An entry with no "=" is not a variable assignment and is passed through
// untouched. Passing a nil or empty env returns nil.
func ScrubPlatformIdentity(env []string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok && IsPlatformIdentity(name) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// jobVars is the sorted set of variables the engine sets to describe a job of
// a hosted run to the extension that runs it. Neither holds a secret.
var jobVars = []string{
	extensionproto.JobCredentialFDEnv,
	extensionproto.OfflineDependenciesEnv,
}

// JobVars returns the names of the variables that describe a job of a hosted
// run, in sorted order. The returned slice is a fresh copy.
func JobVars() []string {
	return append([]string(nil), jobVars...)
}

// ScrubJobVars returns a copy of env, `KEY=VALUE` entries, with every job
// variable removed (JobVars). A test launcher calls it after it applied its own
// offline policy to env, so the test process keeps the settings that forbid a
// dependency download and loses only the two names.
//
// An entry with no "=" is passed through untouched. Passing a nil or empty env
// returns nil.
func ScrubJobVars(env []string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok && slices.Contains(jobVars, name) {
			continue
		}
		out = append(out, entry)
	}
	return out
}
