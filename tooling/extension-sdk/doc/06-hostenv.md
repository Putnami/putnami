# Host Platform Identity

The `hostenv` package owns the single list of environment variables a managed runtime injects into a process to describe **the deployment it is running**, plus the scrub every language extension applies to that list before spawning a test process.

## The problem

A `putnami test` run executed on a managed runtime — a CI worker that is itself a Cloud Run service, an App Engine job, a Lambda — inherits that runtime's identity block. `K_SERVICE` and friends reach the test subprocess, where application code reads them as "I am the deployed production workload" and changes behaviour:

```ts
const allowEphemeral = config.allowEphemeralSigningKey && !process.env.K_SERVICE;
```

The guard is correct; the signal is not. `K_SERVICE=ci-worker` describes the **harness host**, not the code under test, so the guard fails closed inside a test and the suite dies with an error no local run can reproduce. The failure is deterministic on the hosted worker and invisible everywhere else.

Scrubbing also makes the test task's declared cache key honest: none of these variables is a declared `{"from": "env"}` input of any test task, so before the scrub a cache hit and a fresh run could legitimately disagree.

## Admission criteria

A variable belongs in the list when **both** hold:

1. A managed runtime sets it automatically to describe the deployment it injected it into — nobody types it, and it means nothing off-platform.
2. It carries no capability: not a credential, not a service endpoint, and not a region/project selector a client SDK needs to address an API.

Criterion 2 is what keeps integration tests working. Deployment **identity** is scrubbed; **credentials** are not.

| Kept on purpose | Why |
|-----------------|-----|
| `GOOGLE_APPLICATION_CREDENTIALS`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN` | Credentials — an integration test may legitimately need them |
| `AWS_REGION`, `KUBERNETES_SERVICE_HOST` | Endpoints/selectors a client needs to reach an API |
| `GOOGLE_CLOUD_PROJECT`, `GCLOUD_PROJECT` | Both a platform signal **and** a capability: every Google client library and the framework's own workload-identity token source read it to choose which project to address. Criterion 2 rejects it |
| `PORT` | A configuration knob a developer sets locally on purpose, not an identity |
| `CI`, `APP_ENV`, `DATABASE_TEST_BINDINGS`, `FORCE_COLOR`, `PUTNAMI_*` | Harness contract, not host identity |

## What is scrubbed

Whole injected blocks, not just the one variable a bug report named — an application guard may key on any member of a platform's block, and leaving a partial block behind just moves the bug.

| Platform | Variables |
|----------|-----------|
| Knative / Cloud Run services | `K_SERVICE`, `K_REVISION`, `K_CONFIGURATION` |
| Cloud Run jobs | `CLOUD_RUN_JOB`, `CLOUD_RUN_EXECUTION`, `CLOUD_RUN_TASK_INDEX`, `CLOUD_RUN_TASK_ATTEMPT`, `CLOUD_RUN_TASK_COUNT` |
| Cloud Functions (gen2 / functions-framework) | `FUNCTION_TARGET`, `FUNCTION_SIGNATURE_TYPE` |
| App Engine | `GAE_APPLICATION`, `GAE_DEPLOYMENT_ID`, `GAE_ENV`, `GAE_INSTANCE`, `GAE_MEMORY_MB`, `GAE_RUNTIME`, `GAE_SERVICE`, `GAE_VERSION` |
| AWS Lambda | `AWS_EXECUTION_ENV`, `AWS_LAMBDA_FUNCTION_NAME`, `AWS_LAMBDA_FUNCTION_VERSION`, `AWS_LAMBDA_FUNCTION_MEMORY_SIZE`, `AWS_LAMBDA_INITIALIZATION_TYPE`, `AWS_LAMBDA_LOG_GROUP_NAME`, `AWS_LAMBDA_LOG_STREAM_NAME`, `AWS_LAMBDA_RUNTIME_API`, `LAMBDA_TASK_ROOT`, `LAMBDA_RUNTIME_DIR`, `_HANDLER` |

Azure, Vercel, Fly and Heroku are deliberately absent: no Putnami surface detects them today, and each ships a set that mixes identity with build provenance and endpoints that would need the criteria applied variable by variable. Adding one later is a list edit plus a test.

## Stability contract

The list only ever grows, and growth is additive-safe — removing a variable from a test process cannot make a correct test fail, because a correct test does not assert on the identity of the machine that launched it. The list is sorted and duplicate-free (pinned by a test) so a diff to it is reviewable. Matching is exact and case-sensitive, matching POSIX environment semantics on the container platforms this describes.

## Job variables of a hosted run

A hosted run describes each job to the extension that runs it in two variables: `PUTNAMI_OFFLINE_DEPENDENCIES` and `PUTNAMI_JOB_CREDENTIAL_FD`. They are addressed to the extension, not to the repository's tests. A test process that inherits them takes the paths of a job of a hosted run: it refuses to refresh a credential and treats every dependency as already downloaded. Tests written for a developer machine then fail on a hosted run and nowhere else.

`JobVars` lists the two names and `ScrubJobVars` removes them. A test launcher applies its own offline policy first and scrubs afterwards, so the settings that forbid a dependency download (`GOPROXY=off` for Go) stay in the test process. Only the two names go.

## API

```go
import "go.putnami.dev/sdk/extension/hostenv"

// The names, sorted; a fresh copy on every call.
names := hostenv.PlatformIdentityVars()

// Exact, case-sensitive membership test.
ok := hostenv.IsPlatformIdentity("K_SERVICE") // true

// A copy of a KEY=VALUE environment with the block removed.
env := hostenv.ScrubPlatformIdentity(os.Environ())

// The two job variables of a hosted run, and a copy of env without them.
jobNames := hostenv.JobVars()
env = hostenv.ScrubJobVars(env)
```

`ScrubPlatformIdentity` covers the **inherited** environment only: anything the harness sets deliberately afterwards is appended after the scrub and still wins. [`exec.UnsetEnv`](./04-exec.md) gives the same ordering for callers that go through `exec.Run` instead of building an env slice themselves, so all three language extensions resolve a conflict identically.

## Where it is applied

| Extension | Seam |
|-----------|------|
| `@putnami/typescript` | `testjob.RunTests` passes `exec.UnsetEnv(hostenv.PlatformIdentityVars()...)` to the `bun test` spawn |
| `@putnami/go` | `buildTestEnv` scrubs the base env before `go test` |
| `@putnami/python` | `MakeTestEnv` (the test-only sibling of `MakeEnv`) scrubs before `pytest` |

Each seam removes the job variables of a hosted run in the same place.

Only **test** subprocesses are scrubbed. `run` and `serve` start a real application, and a locally served app is a deployment that has every right to see where it is running. The CLI's own process keeps the block too — it selects the `cloud-logging` renderer from `K_SERVICE`.
