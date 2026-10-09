/**
 * TypeScript binding of the Putnami CLI machine result contract, version 2.
 *
 * The canonical source is `protocols/cli/schemas/result-v2.json` (`$id`
 * `https://putnami.dev/schemas/putnami-cli-result-v2.json`), implemented in Go as
 * `go.putnami.dev/protocol/cli`. Version 2 stamps `protocolVersion` on all four
 * machine surfaces — the `--output=json` envelope, the `--output=jsonl` run
 * stream or live-only `plan:end`, the recorded session event log, the MCP
 * `run_jobs`/`plan_jobs` results, and the session files — so a consumer reads the contract off the
 * document instead of guessing from the CLI version. A document WITHOUT the
 * field is version 1.
 *
 * {@link validateDocument} is a line-by-line mirror of the Go validator
 * (`protocols/cli/result_v2_rules.go`): same codes, same paths, same order.
 * `protocols/cli/conformance/manifest.json` is the single corpus both runtimes
 * run, and it pins the exact violation set of every fixture — so a divergence
 * between the two implementations fails this package's tests.
 *
 * A contract change lands as ONE commit touching three places: the corpus, the
 * Go validator, and this file.
 */

/** The version stamped on every v2 machine document. */
export const RESULT_PROTOCOL_VERSION = 2 as const;

/** The `$id` of the canonical v2 JSON schema. */
export const RESULT_V2_SCHEMA_ID = 'https://putnami.dev/schemas/putnami-cli-result-v2.json' as const;

/** The v2 machine documents, one discriminator per surface. */
export const DOCUMENT_KIND = {
  resultEnvelope: 'resultEnvelope',
  sessionStreamRecord: 'sessionStreamRecord',
  mcpResult: 'mcpResult',
  sessionFile: 'sessionFile',
  sessionPlanFile: 'sessionPlanFile',
  reportFile: 'reportFile',
} as const;

/** One of the six v2 machine documents. */
export type DocumentKind = (typeof DOCUMENT_KIND)[keyof typeof DOCUMENT_KIND];

/** The closed violation vocabulary. Codes are part of the contract. */
export const VIOLATION_CODE = {
  invalidJson: 'cli.result.invalid_json',
  unknownField: 'cli.result.unknown_field',
  missingField: 'cli.result.missing_field',
  unexpectedField: 'cli.result.unexpected_field',
  invalidType: 'cli.result.invalid_type',
  invalidEnum: 'cli.result.invalid_enum',
  invalidValue: 'cli.result.invalid_value',
  invalidProtocolVersion: 'cli.result.invalid_protocol_version',
  invalidKey: 'cli.result.invalid_key',
  countMismatch: 'cli.result.count_mismatch',
  outcomeMismatch: 'cli.result.outcome_mismatch',
  exitCodeMismatch: 'cli.result.exit_code_mismatch',
  errorMismatch: 'cli.result.error_mismatch',
  budgetExceeded: 'cli.result.budget_exceeded',
  elisionMismatch: 'cli.result.elision_mismatch',
  unsanitized: 'cli.result.unsanitized',
} as const;

/** A stable violation code. */
export type ViolationCode = (typeof VIOLATION_CODE)[keyof typeof VIOLATION_CODE];

/** One breach of the v2 contract: a stable code and the JSON path it sits at. */
export interface Violation {
  code: string;
  path: string;
}

/** The `run.outcome` and `Result.status` verdict. Precedence: aborted > failure > success. */
export const RUN_OUTCOME = { success: 'success', failure: 'failure', aborted: 'aborted' } as const;

/** The three-valued run verdict. */
export type RunOutcome = (typeof RUN_OUTCOME)[keyof typeof RUN_OUTCOME];

/** `Result.status` values in version 2. Version 1 has no `aborted`. */
export const RESULT_STATUS_V2 = RUN_OUTCOME;

/** A v2 envelope status. */
export type ResultStatusV2 = RunOutcome;

/** Abort sources. */
export const ABORTED_BY = { user: 'user', signal: 'signal' } as const;

/** One task's execution verdict, carried through reuse unchanged. */
export const TASK_STATUS = {
  success: 'success',
  failed: 'failed',
  canceled: 'canceled',
  skipped: 'skipped',
} as const;

/** A task verdict. */
export type TaskStatus = (typeof TASK_STATUS)[keyof typeof TASK_STATUS];

/** How a task's result was obtained without executing it. */
export const TASK_REUSE = {
  none: 'none',
  localCache: 'local-cache',
  remoteCache: 'remote-cache',
  coalesced: 'coalesced',
} as const;

/** A task's reuse provenance. */
export type TaskReuse = (typeof TASK_REUSE)[keyof typeof TASK_REUSE];

/**
 * Where `run.cpu.allocatedMillicores` came from. The two are different physics,
 * not two spellings: a quota THROTTLES a run that exceeds it, while a core count
 * is merely the point past which there is nothing left to run on.
 */
export const CPU_ALLOCATION = { cgroupQuota: 'cgroup-quota', logicalCpus: 'logical-cpus' } as const;

/** How a run's CPU allocation was determined. */
export type CPUAllocationSource = (typeof CPU_ALLOCATION)[keyof typeof CPU_ALLOCATION];

/** Stable memory-capacity provenance. */
export const MEMORY_CAPACITY_SOURCE = { physical: 'physical', cgroupLimit: 'cgroup-limit' } as const;

/** Memory-controller layouts that can supply exact facts. */
export const CGROUP_MEMORY_SOURCE = { v1: 'cgroup-v1', v2: 'cgroup-v2' } as const;

/** PSI populations. Host and cgroup pressure have the same units but different scope. */
export const MEMORY_PRESSURE_SCOPE = { host: 'host', cgroup: 'cgroup' } as const;

/** Task identity scopes. */
export const TASK_SCOPE = { project: 'project', workspace: 'workspace' } as const;

/**
 * Session-stream record discriminators. `test:case` is additive: a reader that
 * does not know a record value skips the line.
 */
export const STREAM_RECORD = {
  taskStart: 'task:start',
  taskEvent: 'task:event',
  taskEnd: 'task:end',
  testCase: 'test:case',
  planEnd: 'plan:end',
  sessionEnd: 'session:end',
} as const;

/** The closed vocabulary of {@link TestCase.status}, in the words of the runtime `testSummary` counters. */
export const TEST_CASE_STATUS = { passed: 'passed', failed: 'failed', skipped: 'skipped' } as const;

/** A test case's verdict. */
export type TestCaseStatus = (typeof TEST_CASE_STATUS)[keyof typeof TEST_CASE_STATUS];

/**
 * The most `test:case` records one task carries. A producer past it keeps
 * failed cases first, then skipped, then passed, and counts the rest in
 * {@link TaskRecord.testCasesDropped}. The bound spans records, so no
 * per-record check enforces it.
 *
 * The test-case bounds are the numbers the Go constants carry, and both are
 * pinned against `result-v2.json` by the drift tests.
 */
export const TEST_CASE_MAX_PER_TASK = 1000 as const;

/**
 * The most bytes of test cases one task carries: the sum of the compact JSON
 * encoding of each kept {@link TestCase}. A producer keeps cases in the
 * {@link TEST_CASE_MAX_PER_TASK} priority order while they fit.
 */
export const TEST_CASE_MAX_BYTES_PER_TASK = 1_048_576 as const;

/**
 * The most bytes of test cases all the members of one batched run carry
 * together. Each member gets an equal share, never more than
 * {@link TEST_CASE_MAX_BYTES_PER_TASK}.
 */
export const TEST_CASE_MAX_BYTES_PER_BATCH = 8_388_608 as const;

/** The longest {@link TestCase.output}, in UTF-8 BYTES. */
export const TEST_CASE_MAX_OUTPUT_BYTES = 4096 as const;

/** The longest {@link TestCase.name}, {@link TestCase.suite} and {@link TestCase.file}, in UTF-8 BYTES. */
export const TEST_CASE_MAX_TEXT_BYTES = 1024 as const;

/** Normal hides debug detail; verbose admits it under a larger cap, without changing semantics. */
export const MACHINE_OUTPUT_MODE = { normal: 'normal', verbose: 'verbose' } as const;

/** The live machine-output detail policy and fixed budget selector. */
export type MachineOutputMode = (typeof MACHINE_OUTPUT_MODE)[keyof typeof MACHINE_OUTPUT_MODE];

/** The deterministic sanitizer applied before persistence, measurement and emission. */
export const MACHINE_OUTPUT_SANITIZATION = 'terminal-safe-redacted-v1' as const;

/** The full-detail artifact path, relative to its retained session directory. */
export const MACHINE_OUTPUT_ARTIFACT_PATH = 'events.jsonl' as const;

/** The artifact is retained and pruned with its owning session. */
export const MACHINE_OUTPUT_ARTIFACT_RETENTION = 'session' as const;

/** One final record and 16 KiB are reserved inside every total live cap. */
export const MACHINE_OUTPUT_FINAL_RESERVE_BYTES = 16 * 1024;
export const MACHINE_OUTPUT_FINAL_RESERVE_RECORDS = 1;

/**
 * Fixed live JSONL budgets. Bytes include compact UTF-8 JSON plus one LF for
 * every record, including the mandatory final `session:end` record. Ordinary
 * detail cannot spend the failure reserve, and unused failure/final capacity is
 * never reclaimed.
 */
export const MACHINE_OUTPUT_BUDGETS = {
  normal: {
    maxBytes: 1 * 1024 * 1024,
    maxRecords: 1024,
    failureReserveBytes: 256 * 1024,
    failureReserveRecords: 256,
    finalReserveBytes: MACHINE_OUTPUT_FINAL_RESERVE_BYTES,
    finalReserveRecords: MACHINE_OUTPUT_FINAL_RESERVE_RECORDS,
  },
  verbose: {
    maxBytes: 8 * 1024 * 1024,
    maxRecords: 8192,
    failureReserveBytes: 2 * 1024 * 1024,
    failureReserveRecords: 2048,
    finalReserveBytes: MACHINE_OUTPUT_FINAL_RESERVE_BYTES,
    finalReserveRecords: MACHINE_OUTPUT_FINAL_RESERVE_RECORDS,
  },
} as const;

/** MCP tools that return a v2 result document. */
export const MCP_TOOL = { runJobs: 'run_jobs', planJobs: 'plan_jobs' } as const;

/**
 * Which surface drove the run a report describes. Not cosmetic: an MCP run is an
 * agent's, and a consumer aggregating build economics over time has to keep
 * agent traffic apart from a human's or CI's rather than average them together.
 */
export const REPORT_ORIGIN = { cli: 'cli', mcp: 'mcp' } as const;

/** A report's origin. */
export type ReportOrigin = (typeof REPORT_ORIGIN)[keyof typeof REPORT_ORIGIN];

/**
 * What a coverage percentage counts. It travels WITH the number because
 * languages measure differently (Go counts statements, TypeScript lines), so
 * averaging across granularities is the reader's decision to make knowingly.
 */
export const COVERAGE_GRANULARITY = {
  statements: 'statements',
  lines: 'lines',
  functions: 'functions',
  branches: 'branches',
} as const;

/** A coverage granularity. */
export type CoverageGranularity = (typeof COVERAGE_GRANULARITY)[keyof typeof COVERAGE_GRANULARITY];

/**
 * The report's bounds. They are CONTRACT clauses, not producer preferences: the
 * report is a bounded synthesis a consumer picks up WHOLE, so its worst case is
 * stated rather than discovered in production.
 *
 * These are the same numbers the Go constants carry, and both are pinned against
 * `result-v2.json`'s own `maxItems`/`maxLength` by the drift tests — so the
 * producer, this mirror and the schema cannot disagree about what "bounded"
 * means.
 */
export const REPORT_MAX_JOBS = 64 as const;

/** The most diagnostics one job may carry; past it the producer truncates. */
export const REPORT_MAX_JOB_DIAGNOSTICS = 16 as const;

/** The longest diagnostic message, in UTF-8 BYTES. */
export const REPORT_MAX_MESSAGE_BYTES = 1024 as const;

/** Accounting diagnostic emitted when a test producer omits causal failure details. */
export const TEST_FAILURE_DETAILS_TRUNCATED_CODE = 'TEST_FAILURE_DETAILS_TRUNCATED' as const;

/** Runtime-log context key attributing an event to one batch project. */
export const BATCH_PROJECT_LOG_CONTEXT_KEY = 'batchProjectId' as const;

/** Runtime-log context key attributing one shared event to several batch projects. */
export const BATCH_PROJECT_LOGS_CONTEXT_KEY = 'batchProjectIds' as const;

/** The owning project, identified structurally rather than by a display name. */
export interface ProjectIdentity {
  id: string;
  name: string;
}

/** The task within its project, identified structurally. */
export interface TaskRef {
  /** Canonical plan name (the scheduler/DAG name), never a display name. */
  name: string;
  command: string;
  step?: string;
  kind: string;
}

/** The extension that provides the task. */
export interface ProviderIdentity {
  extension: string;
  version?: string;
}

/** The immutable typed identity of one planned task. */
export interface TaskIdentity {
  /** A DERIVED view: exactly `project.id + ':' + task.name`. */
  key: string;
  scope: (typeof TASK_SCOPE)[keyof typeof TASK_SCOPE];
  project: ProjectIdentity;
  task: TaskRef;
  provider: ProviderIdentity;
}

/** One diagnostic attributed to a task. */
export interface DiagnosticV2 {
  severity: 'error' | 'warning' | 'info';
  message: string;
  code?: string;
  file?: string;
  line?: number;
  column?: number;
}

/** The error member of a failed or aborted document; identical to version 1. */
export interface ResultErrorV2 {
  code: 'usage' | 'auth' | 'api' | 'signal' | 'failure';
  message: string;
  next?: string;
}

/**
 * One PHYSICAL execution — one subprocess the CLI spawned — and the resources
 * that process tree consumed.
 *
 * The task list is LOGICAL and fans out: a batched dispatch runs n projects in
 * ONE subprocess, so n task records describe work that was measured once.
 * Summing those records double-counts cost; these records are each listed once,
 * so a physical roll-up sums them instead, while per-project attribution keeps
 * reading the task list. Reuse contributes no execution at all.
 *
 * The list is the run's COMPLETE spawn ledger, so it also carries executions no
 * task record references — a superseded retry attempt, or a batch leader whose
 * members all re-executed solo. `tasks` is 0 for those, and the work is real:
 * omitting it would under-state what the machine paid.
 *
 * `maxRssBytes` is normalized to BYTES by the producer, because the underlying
 * rusage member is bytes on darwin and kilobytes on linux. Any counter the
 * platform does not expose is omitted rather than reported as a measured zero.
 */
export interface ExecutionRecord {
  id: string;
  wallMs: number;
  userCpuMs: number;
  systemCpuMs: number;
  maxRssBytes?: number;
  ioInBlocks?: number;
  ioOutBlocks?: number;
  concurrency?: number;
  tasks: number;
}

/** One task's terminal result. */
export interface TaskRecord {
  identity: TaskIdentity;
  /**
   * The physical execution that produced this record; every logical record of a
   * batch carries the SAME id, and it must name an execution the same session
   * file declares. Absent for a task that spawned nothing in this run.
   */
  executionId?: string;
  /**
   * The cache key the scheduler computed for this task, spelled `sha256:` and 64
   * lowercase hex characters. It is the same value the cache is addressed by, so
   * it moves exactly when the key does. Absent on a skipped record and on a task
   * that has no cache identity.
   */
  inputDigest?: string;
  status: TaskStatus;
  reuse: TaskReuse;
  exitCode: number;
  durationMs: number;
  taskWallMs?: number;
  spawnToFirstEventMs?: number;
  error?: ResultErrorV2;
  diagnostics?: DiagnosticV2[];
  /**
   * How many test cases this task ran that have no `test:case` record. Absent
   * when every reported case has its record.
   */
  testCasesDropped?: number;
}

/** One test case a test task ran: the payload of a `test:case` record. */
export interface TestCase {
  /** The full test name as the runner reports it, at most {@link TEST_CASE_MAX_TEXT_BYTES}. */
  name: string;
  /** The Go package import path, or the workspace-relative test file for TypeScript and Python. */
  suite: string;
  status: TestCaseStatus;
  /** The case's wall time as the runner measured it. */
  durationMs: number;
  /** What a failed or skipped case printed, at most {@link TEST_CASE_MAX_OUTPUT_BYTES}. Never on a passed case. */
  output?: string;
  /** The output lost its middle to the bound. Only beside `output`. */
  outputTruncated?: boolean;
  /**
   * The workspace-relative file the runner ties to the case: where it is
   * declared (TypeScript, Python), or the first test-file location its output
   * names (Go).
   */
  file?: string;
  /** The 1-based line in `file`. Only beside `file`. */
  line?: number;
}

/** One failed task in a run summary, carrying the full typed identity. */
export interface TaskFailure {
  identity: TaskIdentity;
  error: ResultErrorV2;
  diagnostics?: DiagnosticV2[];
}

/** The verdict histogram over EVERY selected task, reuse included. */
export interface RunCounts {
  total: number;
  succeeded: number;
  failed: number;
  canceled: number;
  skipped: number;
}

/** The provenance histogram over the same tasks {@link RunCounts} spans. */
export interface RunReuse {
  localCache: number;
  remoteCache: number;
  coalesced: number;
}

/** Measured per-workload Docker publication phases. */
export interface DockerPublishTimings {
  buildMs: number;
  cacheLookupMs: number;
  cacheTransferMs: number;
  registryPushMs: number;
  referencePublishMs: number;
  digestResolveMs: number;
}

/** Configured versus actually observed Docker registry publication concurrency. */
export interface DockerPublishConcurrency {
  configuredCap: number;
  effective: number;
}

/** A Docker publication with verified immutable provenance. */
export interface DockerPublication {
  identity: TaskIdentity;
  /** The resolved release identity shared by this publish session. */
  session: string;
  registry: string;
  image: string;
  /** Exact verified `image@sha256:...` reference. */
  immutableRef: string;
  tags?: string[];
  /** Always `sha256:<64 lowercase hex chars>`, never a mutable tag. */
  imageDigest: string;
  contentStatus: 'pushed' | 'retagged' | 'reused';
  cacheOutcome: 'hit' | 'miss';
  digestReused: boolean;
  timings: DockerPublishTimings;
  concurrency: DockerPublishConcurrency;
}

/**
 * The run's CPU balance sheet: what the runner was ENTITLED to over this run's
 * wall, against what the run actually burned.
 *
 * `actualMs` is summed from the PHYSICAL execution ledger, so each subprocess
 * contributes exactly once however many logical task records it produced —
 * summing `tasks[].durationMs` instead multiplies a batch's cost by its fan-out.
 * `allocatedMs` is `durationMs * allocatedMillicores / 1000` (truncating), so it
 * is always stated against the same wall this summary reports.
 *
 * `actualMs` may exceed `allocatedMs`: a cgroup quota is enforced per period,
 * not per run, and clamping the measurement would hide the oversubscription this
 * block exists to show. The block is absent when the run spawned nothing, and
 * absent when no runner environment was captured.
 */
export interface RunCPU {
  allocatedMillicores: number;
  allocatedSource: CPUAllocationSource;
  allocatedMs: number;
  actualMs: number;
  executions: number;
}

/**
 * Measured work performed by the CLI's local cache leg outside task spans.
 * `servedMs` is the interval union; phase fields partition it once with stable
 * bindings/keys/restore-verify priority and may trail it by at most 2 ms after
 * independent integer-millisecond truncation.
 */
export interface RunLocalCache {
  hits: number;
  misses: number;
  servedMs: number;
  keysMs: number;
  bindingsMs: number;
  restoreVerifyMs: number;
  spawnedProcesses: number;
}

/** Typed cache attribution carried by every run surface. */
export interface RunCache {
  local?: RunLocalCache;
}

/**
 * The canonical verdict of one run, identical on all four surfaces.
 *
 * Unified success is strict: `outcome` is `success` only when the run was not
 * aborted and `counts.failed` is 0 over every selected task, reuse included.
 */
export interface RunSummary {
  outcome: RunOutcome;
  abortedBy?: (typeof ABORTED_BY)[keyof typeof ABORTED_BY];
  exitCode: number;
  counts: RunCounts;
  reuse: RunReuse;
  durationMs: number;
  failures?: TaskFailure[];
  publications?: DockerPublication[];
  cpu?: RunCPU;
  cache?: RunCache;
}

/**
 * The bounded terminal verdict carried by `session:end`. Unbounded `failures`
 * and `publications` are impossible on this wire type: `counts.failed` remains
 * exact and the complete detail is retained in the named session artifact.
 */
export interface StreamRunSummary {
  outcome: RunOutcome;
  abortedBy?: (typeof ABORTED_BY)[keyof typeof ABORTED_BY];
  exitCode: number;
  counts: RunCounts;
  reuse: RunReuse;
  durationMs: number;
  cpu?: RunCPU;
}

/** The shape of a plan. */
export interface PlanMetrics {
  tasks: number;
  edges: number;
  projects: number;
  byCommand?: Record<string, number>;
}

/** One task in a plan; edges reference other tasks by their derived key. */
export interface PlannedTask {
  identity: TaskIdentity;
  dependsOn?: string[];
  after?: string[];
  cache: boolean;
}

/** The plan a dry run or `plan_jobs` reports. */
export interface PlanSummary {
  dryRun: boolean;
  metrics: PlanMetrics;
  tasks: PlannedTask[];
}

/** The `--output=json` document. */
export interface ResultV2 {
  protocolVersion: typeof RESULT_PROTOCOL_VERSION;
  command: string;
  status: ResultStatusV2;
  exitCode: number;
  data?: unknown;
  run?: RunSummary;
  /** A successful plan-only preview; exclusive with `run`, `data`, and `error`. */
  plan?: PlanSummary;
  error?: ResultErrorV2;
}

/** One `--output=jsonl` line; `plan:end` is live-only, not a session event. */
export interface SessionStreamRecord {
  protocolVersion: typeof RESULT_PROTOCOL_VERSION;
  record: (typeof STREAM_RECORD)[keyof typeof STREAM_RECORD];
  time: string;
  identity?: TaskIdentity;
  /** The subprocess runtime event, versioned by `protocols/runtime`. */
  event?: Record<string, unknown>;
  task?: TaskRecord;
  /** Required on `test:case` and forbidden on every other record. */
  testCase?: TestCase;
  run?: RunSummary;
  /** Required on `plan:end` and forbidden on every other record. */
  plan?: PlanSummary;
  machineOutput?: MachineOutputSummary;
}

/** Opt-in bounded terminal type used by the whole-sequence profile. */
export interface BoundedSessionEndRecord {
  protocolVersion: typeof RESULT_PROTOCOL_VERSION;
  record: typeof STREAM_RECORD.sessionEnd;
  time: string;
  run: StreamRunSummary;
  machineOutput: MachineOutputSummary;
}

/** The fixed budget selected by a machine-output mode. */
export interface MachineOutputBudget {
  maxBytes: number;
  maxRecords: number;
  failureReserveBytes: number;
  failureReserveRecords: number;
  finalReserveBytes: number;
  finalReserveRecords: number;
}

/** Whole sanitized records and compact JSON-plus-LF bytes omitted live. */
export interface MachineOutputElision {
  records: number;
  bytes: number;
}

/** Ordinary detail and failure-priority evidence are accounted separately. */
export interface MachineOutputElisions {
  ordinary: MachineOutputElision;
  failure: MachineOutputElision;
}

/** The complete sanitized stream retained beside `session.json`. */
export interface MachineOutputArtifact {
  sessionId: string;
  path: typeof MACHINE_OUTPUT_ARTIFACT_PATH;
  retention: typeof MACHINE_OUTPUT_ARTIFACT_RETENTION;
}

/** Required accounting on the opt-in bounded profile's final `session:end`. */
export interface MachineOutputSummary {
  mode: MachineOutputMode;
  sanitization: typeof MACHINE_OUTPUT_SANITIZATION;
  budget: MachineOutputBudget;
  elided: MachineOutputElisions;
  artifact: MachineOutputArtifact;
}

/** Returns the one contract budget for a machine-output mode. */
export function machineOutputBudgetFor(mode: MachineOutputMode): MachineOutputBudget {
  return { ...MACHINE_OUTPUT_BUDGETS[mode] };
}

/** The document an MCP tool call returns. */
export interface MCPResult {
  protocolVersion: typeof RESULT_PROTOCOL_VERSION;
  tool: (typeof MCP_TOOL)[keyof typeof MCP_TOOL];
  commands: string[];
  run?: RunSummary;
  plan?: PlanSummary;
}

/** The git state recorded with a session. */
export interface SessionGit {
  branch?: string;
  baseline?: string;
}

/**
 * The worktree a session ran against, identified by CONTENT.
 *
 * It exists because "the same files are dirty" is not "the same bytes are on
 * disk". {@link SessionGit} records a branch and a baseline and {@link ReportGit}
 * records a commit; neither says anything about the uncommitted state, so a
 * consumer that wants to know WHICH tree a recorded run measured has to compute
 * a digest itself, against a worktree that may have moved since or may not exist
 * any more. The session states it instead, and every later reader compares
 * strings.
 *
 * The digest is defined once, in `protocols/cli/doc/02-result-v2.md` § Gated
 * tree fingerprint, and computed once, by `putnami tree fingerprint`: two
 * parties comparing fingerprints produced by two implementations compare
 * nothing.
 */
export interface SessionTree {
  /**
   * Lowercase hex sha256 of the canonical tree byte stream — always 64
   * characters, whatever hash the repository uses for its own objects.
   */
  fingerprint: string;
  /**
   * Whether the worktree differed from `headSHA`. It is a plain boolean, not the
   * optional one {@link ReportGit.dirty} uses, because the two carry "unknown"
   * differently: a report exists whether or not its producer could inspect the
   * tree, while this whole object is written only when the fingerprint was
   * computed — and computing it IS the measurement that decides dirtiness.
   */
  dirty: boolean;
  /**
   * The FULL object id of the commit the worktree sat on. It is folded into
   * `fingerprint` as well, so a moved HEAD reads as a different tree instead of
   * canceling out against a coincidentally identical diff.
   */
  headSHA: string;
}

/**
 * The closed vocabulary of how a run chose the projects it planned over, in
 * canonical (sorted) order.
 */
export const SESSION_SELECTION_MODES = ['all', 'impacted', 'projects'] as const;

/** How a run chose the projects it planned over. */
export interface SessionSelection {
  /** How the projection was chosen: one of SESSION_SELECTION_MODES. */
  mode: (typeof SESSION_SELECTION_MODES)[number];
  /**
   * Distinguishes a narrowed run from the whole-workspace default. A session
   * may only claim to have covered the workspace when this is false.
   */
  scoped: boolean;
  /**
   * The selected projects' canonical ids, sorted. Absent when the run selected
   * nothing — an `--impacted` run over an unchanged tree is the ordinary case.
   */
  projects?: string[];
  /**
   * The canonical ids, sorted, of the projects whose publish and package steps
   * this session's release-set plan owned. Absent when the session coordinated
   * no release set, and when its plan selected no member. Always a subset of
   * `projects`.
   *
   * A session that names publish beside other commands does two things at once:
   * it publishes what the channel head decided, and it verifies what the
   * caller's own selection asked for. `projects` is the union — what ran — so
   * this member is what lets a consumer attribute each half.
   */
  releaseSetProjects?: string[];
}

/**
 * cgroup v2 CPU bandwidth accounting, WINDOWED to the session: each member is
 * the delta between the sample taken at session start and the one at session
 * end.
 *
 * It is a nested object rather than three optional members because a ZERO here
 * is a measurement, and the important one — "the quota never throttled us" is
 * what separates contention from quota starvation, and an omitted-when-zero
 * member could not say it. The object is present exactly when `cpu.stat` exposed
 * these counters.
 */
export interface CgroupThrottle {
  periods: number;
  throttledPeriods: number;
  throttledUs: number;
}

/**
 * The cgroup v2 CPU controller as the session saw it. Absent entirely when no
 * `cpu.max` is readable — a developer laptop, a cgroup v1 host, or a platform
 * with no cgroups.
 *
 * `quotaUs` is absent when `cpu.max` says `max`: there is no quota, and
 * reporting one would invent a ceiling. `usageUs` is the WHOLE cgroup's CPU over
 * the window, so it is wider than `run.cpu.actualMs`, which counts only the
 * subprocesses.
 */
export interface CgroupCPU {
  periodUs: number;
  quotaUs?: number;
  usageUs?: number;
  throttle?: CgroupThrottle;
}

/**
 * Linux PSI for CPU (`/proc/pressure/cpu`), windowed to the session. It answers
 * the shared-host half of the question: a runner whose own quota is never
 * exceeded can still be slow because the HOST is oversubscribed.
 *
 * Only the `some` line's cumulative total is reported, delta'd over the session.
 * The avg10/avg60/avg300 columns are decaying averages over windows that are not
 * this session's, so nothing in this document could reconcile them.
 */
export interface CPUPressure {
  someStalledUs: number;
}

/**
 * The `/proc/stat` aggregate, windowed to the session — the hypervisor's view:
 * steal is time the host gave to somebody else while this vCPU was runnable,
 * iowait is time it had nothing to run because a device had not answered.
 *
 * Counters are in USER_HZ TICKS exactly as the kernel states them, because
 * converting to milliseconds needs a USER_HZ this process cannot read.
 * `totalTicks` is every column summed over the same window, so the ratios — the
 * only reading this block is for — are unit-free, and neither column may exceed
 * it.
 */
export interface HostCPUTime {
  stealTicks: number;
  ioWaitTicks: number;
  totalTicks: number;
}

/** A finite memory-controller limit and the controller layout that supplied it. */
export interface CgroupMemoryLimit {
  bytes: number;
  source: (typeof CGROUP_MEMORY_SOURCE)[keyof typeof CGROUP_MEMORY_SOURCE];
}

/** Stable physical/cgroup/effective capacity and explicit provenance. */
export interface MemoryCapacity {
  /** Host physical RAM, absent when it could not be measured. */
  physicalBytes?: number;
  /** Absent for an unlimited or unreadable memory controller. */
  cgroupLimit?: CgroupMemoryLimit;
  /** The smaller available stable bound. */
  effectiveBytes: number;
  effectiveSource: (typeof MEMORY_CAPACITY_SOURCE)[keyof typeof MEMORY_CAPACITY_SOURCE];
}

/**
 * Closing cgroup v2 memory.stat composition. `fileBytes` INCLUDES
 * `shmemBytes`; consumers MUST NOT sum them. The block is absent on v1, whose
 * similarly named counters do not have these exact semantics.
 */
export interface CgroupMemoryComposition {
  anonBytes: number;
  fileBytes: number;
  shmemBytes: number;
}

/** A peak over the CGROUP LIFETIME, sampled at session close. */
export interface CgroupMemoryLifetimePeak {
  bytes: number;
}

/** Closing memory-controller gauges; object presence preserves measured zero. */
export interface CgroupMemoryClosing {
  currentBytes: number;
  composition?: CgroupMemoryComposition;
  lifetimePeak?: CgroupMemoryLifetimePeak;
}

/** Standard cgroup v2 memory.events counters, delta'd over environment.windowMs. */
export interface CgroupMemoryEvents {
  low: number;
  high: number;
  max: number;
  oom: number;
  oomKill: number;
}

/** The session cgroup's memory controller: closing gauges plus v2-only event deltas. */
export interface CgroupMemory {
  source: (typeof CGROUP_MEMORY_SOURCE)[keyof typeof CGROUP_MEMORY_SOURCE];
  closing: CgroupMemoryClosing;
  events?: CgroupMemoryEvents;
}

/** Linux memory PSI delta, with its host or cgroup population stated explicitly. */
export interface MemoryPressure {
  scope: (typeof MEMORY_PRESSURE_SCOPE)[keyof typeof MEMORY_PRESSURE_SCOPE];
  someStalledUs: number;
  fullStalledUs: number;
}

/**
 * The RUNNER the session executed on, and how much of it the session got.
 *
 * A per-task duration is uninterpretable alone: CI runners are CPU-quota-limited
 * and share a host, so a task that took twice as long may have hit its cgroup
 * quota, been starved by a neighbor, or simply had more work — three problems
 * with three fixes, indistinguishable from timings.
 *
 * EVERY MEMBER IS MEASURED. A file the platform does not have yields an ABSENT
 * member, never a zero: macOS has no cgroups and no PSI, and a zero there would
 * claim "measured no throttling" for a machine that cannot throttle. Blocks
 * whose zeros ARE meaningful are nested objects, so the object's presence is
 * what says "this was read".
 *
 * Cumulative counters are reported as SESSION DELTAS between a sample at
 * session start and one at session end. Closing memory gauges are named
 * separately and are not delta'd. `windowMs` is the wall every delta is read
 * against.
 */
export interface SessionEnvironment {
  os: string;
  arch: string;
  logicalCpus: number;
  cpuModel?: string;
  windowMs: number;
  cgroupCpu?: CgroupCPU;
  cpuPressure?: CPUPressure;
  hostCpu?: HostCPUTime;
  memoryCapacity?: MemoryCapacity;
  cgroupMemory?: CgroupMemory;
  memoryPressure?: MemoryPressure;
}

/** The CLOSED ownership vocabulary the dependency-preparation stage is decomposed into. */
export const PREPARATION_PHASE = {
  network: 'network',
  resolution: 'resolution',
  verification: 'verification',
  generation: 'generation',
  mutation: 'mutation',
} as const;

export type PreparationPhase = (typeof PREPARATION_PHASE)[keyof typeof PREPARATION_PHASE];

/**
 * One OWNERSHIP CLASS's contribution to the dependency-preparation stage.
 *
 * `wallMs` is a SUM OF SPANS, not an interval. When `preparation.parallelism` is
 * above 1 the spans overlapped, so the phases can add up to more than
 * `preparation.wallMs` — deliberately: the phase total is the WORK the class
 * represents, and the gap is what the parallelism bought. Only at parallelism 1
 * do the phases partition the stage.
 *
 * `cpuMs` is MEASURED child CPU for the spans that spawned a subprocess, and is
 * absent for a phase that ran entirely in the CLI process — an in-process phase
 * reports no child CPU rather than a fabricated one.
 */
export interface PreparationPhaseRecord {
  phase: PreparationPhase;
  wallMs: number;
  steps: number;
  cpuMs?: number;
}

/**
 * The dependency-preparation stage — resolving, fetching, building and
 * publishing the artifacts a run needs BEFORE it can plan anything — decomposed
 * into ownership phases.
 *
 * Its separation from `executions` is a correctness property rather than a
 * layout choice: an `ExecutionRecord` is one SUBPROCESS, joined to by
 * `TaskRecord.executionId` and summed into `run.cpu.actualMs`, while most of
 * preparation is in-process work no subprocess performed and no task record
 * could reference. Nothing here is in `executions`, and nothing in `executions`
 * is here.
 */
export interface SessionPreparation {
  wallMs: number;
  parallelism: number;
  phases: PreparationPhaseRecord[];
}

/**
 * Requested and actual execution locations. A remote request may execute
 * locally when no provider is available. Observational metadata only; it does
 * not contribute to task identity, cache keys or the gated tree fingerprint.
 */
export interface SessionPlacement {
  requested: 'local' | 'remote';
  actual: 'local' | 'remote';
  /**
   * The bound execution request a remote session ran for, stated by the
   * executing engine. Present exactly when a bound request executed; absent
   * for a local run, a local fallback and every older producer.
   */
  provenance?: SessionProvenance;
}

/**
 * The identity of the bound execution request whose execution a session
 * records, stated by the EXECUTING engine from the request it was handed. It
 * lets the submitting side prove an imported session is the one it submitted
 * without trusting anything the transport asserts about itself. It names no
 * provider, attempt or machine: those are transport observations and live
 * beside the session. Like {@link SessionTree}, a present block always knows
 * all three members; absence is the only unknown.
 */
export interface SessionProvenance {
  /** Canonical source-manifest digest of the executed snapshot: `sha256:` and 64 lowercase hex. */
  sourceDigest: string;
  /** Execution-input digest of the bound request, same spelling. Applicability identity, never a task cache key. */
  inputDigest: string;
  /** The request's idempotency key, 32 lowercase hex characters: the one submission this session answers. */
  submission: string;
}

/** The recorded session metadata document (`session.json`). */
export interface SessionFile {
  protocolVersion: typeof RESULT_PROTOCOL_VERSION;
  sessionId: string;
  /**
   * The session of the run that SPAWNED this one; absent for a top-level run.
   *
   * A task may invoke the CLI again, and the nested run records its own session
   * beside its parent's. Without this member the only discriminator is time
   * containment, which is not one — concurrent worktrees share a store root,
   * and two ordinary runs in one worktree overlap — so an accounting consumer
   * would count a nested run as a gate and double-count its CPU.
   */
  parentSessionId?: string;
  startTime: string;
  endTime?: string;
  commands: string[];
  /** How the run chose the projects it planned over, absent when unrecorded. */
  selection?: SessionSelection;
  git?: SessionGit;
  /**
   * The worktree this session ran against, as it stood when the session opened.
   * Absent when the producer could not compute it — outside a git worktree, in a
   * repository with no commit, or when git failed — because a session that
   * cannot say which tree it measured says nothing rather than claiming a clean
   * one.
   */
  tree?: SessionTree;
  /** Absent when the producer did not record placement, including older sessions. */
  placement?: SessionPlacement;
  run: RunSummary;
  tasks?: TaskRecord[];
  /**
   * The session's complete physical spawn ledger, each execution listed once
   * however many task records it produced — including those every record
   * superseded. Absent when nothing executed.
   */
  executions?: ExecutionRecord[];
  /**
   * The runner this session ran on and how much of it the session got. Absent
   * when the producer captured none.
   */
  environment?: SessionEnvironment;
  /**
   * The dependency-preparation stage decomposed into ownership phases. Absent
   * when the session prepared nothing.
   */
  preparation?: SessionPreparation;
  scheduler?: unknown;
  /** Detailed producer-owned cache snapshot; portable attribution is in run.cache. */
  cache?: unknown;
}

/** The recorded plan snapshot document (`plan.json`). */
export interface SessionPlanFile {
  protocolVersion: typeof RESULT_PROTOCOL_VERSION;
  sessionId: string;
  commands: string[];
  tasks: PlannedTask[];
}

/**
 * The repository state a report was produced against.
 *
 * Report-owned rather than a reuse of {@link SessionGit}: the session's git
 * block records what SELECTION was computed from, while a report is a durable
 * fact a consumer files against a commit, so `sha` is required here — a full
 * object id, because an abbreviation or a symbolic name can move.
 */
export interface ReportGit {
  branch?: string;
  sha: string;
  /** Absent when the producer did not determine it — never a claimed-clean tree. */
  dirty?: boolean;
  baseline?: string;
}

/**
 * The run's verdict as the report states it.
 *
 * Deliberately NOT {@link RunSummary}: that shape carries `failures[]`, whose
 * length equals `counts.failed` and whose members each embed a full typed
 * identity, an error and that task's diagnostics — an UNBOUNDED member, and the
 * report's premise is that its worst case is stated up front. The failed tasks
 * are not lost: they sort first into `jobs[]`, so a report elides successes
 * before failures.
 */
export interface ReportRun {
  outcome: RunOutcome;
  exitCode: number;
  counts: RunCounts;
  reuse: RunReuse;
  durationMs: number;
  cpu?: RunCPU;
}

/** One command's test outcome, mirroring `protocols/runtime`'s `TestSummary`. */
export interface ReportTests {
  /** Equals `passed + failed + skipped`. */
  total: number;
  passed: number;
  failed: number;
  skipped: number;
  /** Causal failure diagnostics producers omitted before report reduction. */
  failureDetailsTruncated?: number;
}

/**
 * A coverage measurement, mirroring `protocols/runtime`'s `CoverageSummary`.
 * Present only where coverage was actually COLLECTED: the report displays what
 * the run measured and never forces instrumentation, so an uninstrumented
 * command omits the block rather than reporting 0%.
 */
export interface ReportCoverage {
  /** The covered share in [0,100] at the declared granularity. */
  percentage: number;
  granularity: CoverageGranularity;
  covered?: number;
  total?: number;
  /** Whether a threshold was in force — "could have failed the run", not "was measured". */
  enforced: boolean;
}

/**
 * One root command's synthesis. The vocabulary is COMMANDS, not phases: a task
 * names the root command it belongs to, so this aggregation is a projection of
 * facts the run already carries. Each command appears at most once, and the
 * per-command totals sum to `run.counts.total`.
 */
export interface ReportCommand {
  command: string;
  counts: RunCounts;
  reuse: RunReuse;
  /** Summed wall of the tasks that actually EXECUTED; reused tasks contribute nothing. */
  freshWallMs: number;
  /** Summed CPU of the fresh tasks' shares. Over all commands it never exceeds `run.cpu.actualMs`. */
  cpuMs?: number;
  tests?: ReportTests;
  coverage?: ReportCoverage;
  /** Every real reported error (including report drops), plus producer-omitted failure details. */
  errors: number;
  warnings: number;
}

/**
 * One selected task's line in the report. The identity is FLATTENED to the three
 * strings a consumer joins on plus the root command; the full
 * {@link TaskIdentity} stays in the session, because repeating it 64 times would
 * spend the report's whole budget on identity a reader can look up.
 */
export interface ReportJob {
  /** Exactly `project + ':' + task`, the key every other v2 surface joins on. */
  key: string;
  project: string;
  task: string;
  command: string;
  outcome: TaskStatus;
  reuse: TaskReuse;
  durationMs: number;
  /** This task's share of its execution's measured CPU; absent when nothing was measured. */
  cpuMs?: number;
  coverage?: ReportCoverage;
  /** At most {@link REPORT_MAX_JOB_DIAGNOSTICS}, each message at most {@link REPORT_MAX_MESSAGE_BYTES}. */
  diagnostics?: DiagnosticV2[];
  /** The producer-side portion of {@link truncatedCount}. */
  failureDetailsTruncated?: number;
  /** Producer-side omissions plus report-side drops; report drops require a full list. */
  truncatedCount?: number;
}

/**
 * The run's remote-cache economics: a TYPED subset of the scheduler's own cache
 * snapshot. The session file's free-form `cache` is exactly what a contracted
 * document must not repeat. ABSENT when no remote cache participated — an
 * all-zero block would claim "the cache was asked and answered nothing".
 */
export interface ReportCache {
  hits: number;
  misses: number;
  /** Never exceeds `hits`: a key that was not held cannot have been restored. */
  restored: number;
  uploads: number;
  timeSavedMs: number;
  bytesFetched: number;
  /** Byte traffic is stated once, at run level: any split of a deduplicated transfer is a fiction. */
  bytesUploaded: number;
}

/** The two scheduler facts a budget consumer reads. Absent when none was recorded. */
export interface ReportScheduler {
  parallelism: number;
  /** The makespan floor no worker count can beat; 0 on a fully cached plan is a real measurement. */
  criticalPathMs: number;
}

/**
 * The recorded synthesis document (`report.json`) — the run's contracted bilan.
 *
 * Three rules govern its content:
 *
 * - ABSENT IS NOT ZERO. A fact the run could not measure is OMITTED; a zero
 *   means measured-zero.
 * - IT IS A PROJECTION, NEVER AN INPUT. Derived after the run settled: nothing
 *   in it influences pass/fail, and nothing in it enters a cache key.
 * - IT IS BOUNDED BY CONTRACT. Jobs, per-job diagnostics and message length have
 *   stated ceilings, and what the ceilings drop is COUNTED (`elidedJobs`,
 *   `truncatedCount`, the per-command error/warning totals) rather than lost.
 */
export interface ReportFile {
  protocolVersion: typeof RESULT_PROTOCOL_VERSION;
  /** The session this report synthesizes, so full detail is one directory away. */
  sessionId: string;
  startTime: string;
  /** Required: a report is written from a SETTLED run. */
  endTime: string;
  origin: ReportOrigin;
  /**
   * Whether a configured coverage-threshold could fail this run. Defaults to
   * true; --no-enforce-coverage still measures, so this marks whether a green
   * verdict actually means the thresholds held.
   */
  enforceCoverage: boolean;
  /**
   * The run's explicit --fix value: false for --fix=false or --no-fix, true for
   * --fix or --fix=true. Absent when the run was not given the flag or gave it
   * any other value. False states that lint reported findings instead of
   * rewriting files.
   */
  fix?: boolean;
  git?: ReportGit;
  run: ReportRun;
  commands: ReportCommand[];
  /** At most {@link REPORT_MAX_JOBS}: failed first, then diagnostic-bearing, then coverage-bearing, then longest wall. */
  jobs: ReportJob[];
  /** `jobs.length + elidedJobs` equals `run.counts.total`; positive only when `jobs` is full. */
  elidedJobs: number;
  cache?: ReportCache;
  scheduler?: ReportScheduler;
}

/** Reports the strict unified verdict: not aborted and nothing failed. */
export function runSucceeded(summary: RunSummary): boolean {
  return summary.outcome !== RUN_OUTCOME.aborted && summary.counts.failed === 0;
}

/** Returns the key a typed identity's structured fields imply. */
export function derivedKey(identity: Pick<TaskIdentity, 'project' | 'task'>): string {
  return `${identity.project.id}:${identity.task.name}`;
}

type JsonObject = Record<string, unknown>;
type Check = (v: Validator, path: string, value: unknown) => void;

interface Field {
  name: string;
  required: boolean;
  check: Check;
}

class Validator {
  readonly violations: Violation[] = [];

  add(code: string, path: string): void {
    this.violations.push({ code, path });
  }

  /**
   * Returns the violations in the contract's deterministic order: by path, then
   * by code, compared by UTF-16 code unit. Code-unit order matches Go's byte
   * order on the ASCII the contract's paths use, and unlike a locale-aware
   * comparison it cannot vary with the host's ICU data — both runtimes emit one
   * order for one document.
   */
  sorted(): Violation[] {
    const byCodeUnit = (a: string, b: string): number => (a < b ? -1 : a > b ? 1 : 0);
    return [...this.violations].sort((a, b) =>
      a.path === b.path ? byCodeUnit(a.code, b.code) : byCodeUnit(a.path, b.path),
    );
  }
}

const req = (name: string, check: Check): Field => ({ name, required: true, check });
const opt = (name: string, check: Check): Field => ({ name, required: false, check });

const join = (parent: string, name: string): string => (parent === '' ? name : `${parent}.${name}`);

function isObject(value: unknown): value is JsonObject {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/**
 * Checks that value is an object with exactly the declared members: unknown
 * members and missing required members are violations, and every present member
 * is checked. Returns the object so the cross-field rules can read the values
 * structural validation already accepted.
 */
function object(v: Validator, path: string, value: unknown, fields: Field[]): JsonObject | undefined {
  if (!isObject(value)) {
    v.add(VIOLATION_CODE.invalidType, path);
    return undefined;
  }
  const declared = new Set(fields.map((f) => f.name));
  for (const name of Object.keys(value).sort()) {
    if (!declared.has(name)) {
      v.add(VIOLATION_CODE.unknownField, join(path, name));
    }
  }
  for (const f of fields) {
    if (!(f.name in value)) {
      if (f.required) {
        v.add(VIOLATION_CODE.missingField, join(path, f.name));
      }
      continue;
    }
    f.check(v, join(path, f.name), value[f.name]);
  }
  return value;
}

function nonEmptyString(v: Validator, path: string, value: unknown): void {
  if (typeof value !== 'string') {
    v.add(VIOLATION_CODE.invalidType, path);
    return;
  }
  if (value === '') {
    v.add(VIOLATION_CODE.invalidValue, path);
  }
}

const UTF8 = new TextEncoder();

function nonEmptyStringBytesAtMost(maximum: number): Check {
  return (v, path, value) => {
    if (typeof value !== 'string') {
      v.add(VIOLATION_CODE.invalidType, path);
      return;
    }
    if (value === '' || UTF8.encode(value).length > maximum) {
      v.add(VIOLATION_CODE.invalidValue, path);
    }
  };
}

function immutableSha256(v: Validator, path: string, value: unknown): void {
  if (typeof value !== 'string') {
    v.add(VIOLATION_CODE.invalidType, path);
    return;
  }
  if (!/^sha256:[0-9a-f]{64}$/.test(value)) {
    v.add(VIOLATION_CODE.invalidValue, path);
  }
}

function enumOf(...allowed: string[]): Check {
  return (v, path, value) => {
    if (typeof value !== 'string') {
      v.add(VIOLATION_CODE.invalidType, path);
      return;
    }
    if (!allowed.includes(value)) {
      v.add(VIOLATION_CODE.invalidEnum, path);
    }
  };
}

/**
 * An integer member is a JSON number carrying an integral value in IEEE-754's
 * safe range — the rule JSON Schema's `integer` implies (`1.0` and `1e2`
 * conform) and the exact set the Go validator accepts, so the two runtimes
 * agree on every wire encoding.
 */
function isContractInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value);
}

function intAtLeast(minimum: number): Check {
  return (v, path, value) => {
    if (!isContractInteger(value)) {
      v.add(VIOLATION_CODE.invalidType, path);
      return;
    }
    if (value < minimum) {
      v.add(VIOLATION_CODE.invalidValue, path);
    }
  };
}

function protocolVersion(v: Validator, path: string, value: unknown): void {
  if (!isContractInteger(value)) {
    v.add(VIOLATION_CODE.invalidType, path);
    return;
  }
  if (value !== RESULT_PROTOCOL_VERSION) {
    v.add(VIOLATION_CODE.invalidProtocolVersion, path);
  }
}

function boolean(v: Validator, path: string, value: unknown): void {
  if (typeof value !== 'boolean') {
    v.add(VIOLATION_CODE.invalidType, path);
  }
}

/** Accepts whatever a command chose to put in a free-form member. */
function anyValue(): void {
  // Free-form by contract: nothing to check.
}

/** Accepts any object without inspecting it — a payload another protocol owns. */
function openObject(v: Validator, path: string, value: unknown): void {
  if (!isObject(value)) {
    v.add(VIOLATION_CODE.invalidType, path);
  }
}

function arrayOf(item: Check): Check {
  return (v, path, value) => {
    if (!Array.isArray(value)) {
      v.add(VIOLATION_CODE.invalidType, path);
      return;
    }
    for (const [i, element] of value.entries()) {
      item(v, `${path}[${i}]`, element);
    }
  };
}

function intMapValues(v: Validator, path: string, value: unknown): void {
  if (!isObject(value)) {
    v.add(VIOLATION_CODE.invalidType, path);
    return;
  }
  const check = intAtLeast(0);
  for (const name of Object.keys(value).sort()) {
    check(v, join(path, name), value[name]);
  }
}

function childValue(obj: JsonObject | undefined, name: string): { value: unknown; present: boolean } {
  if (obj === undefined || !(name in obj)) {
    return { value: undefined, present: false };
  }
  return { value: obj[name], present: true };
}

function childObject(obj: JsonObject | undefined, name: string): JsonObject | undefined {
  const { value } = childValue(obj, name);
  return isObject(value) ? value : undefined;
}

function childString(obj: JsonObject | undefined, name: string): string | undefined {
  const { value } = childValue(obj, name);
  return typeof value === 'string' ? value : undefined;
}

function childBool(obj: JsonObject | undefined, name: string): boolean | undefined {
  const { value } = childValue(obj, name);
  return typeof value === 'boolean' ? value : undefined;
}

function childInt(obj: JsonObject | undefined, name: string): number | undefined {
  const { value } = childValue(obj, name);
  return isContractInteger(value) ? value : undefined;
}

function childArray(obj: JsonObject | undefined, name: string): unknown[] | undefined {
  const { value } = childValue(obj, name);
  return Array.isArray(value) ? value : undefined;
}

const projectIdentityFields: Field[] = [req('id', nonEmptyString), req('name', nonEmptyString)];

const taskRefFields: Field[] = [
  req('name', nonEmptyString),
  req('command', nonEmptyString),
  opt('step', nonEmptyString),
  req('kind', nonEmptyString),
];

const providerIdentityFields: Field[] = [req('extension', nonEmptyString), opt('version', nonEmptyString)];

const projectIdentity: Check = (v, path, value) => object(v, path, value, projectIdentityFields);
const taskRef: Check = (v, path, value) => object(v, path, value, taskRefFields);
const providerIdentity: Check = (v, path, value) => object(v, path, value, providerIdentityFields);

const taskIdentityFields: Field[] = [
  req('key', nonEmptyString),
  req('scope', enumOf(TASK_SCOPE.project, TASK_SCOPE.workspace)),
  req('project', projectIdentity),
  req('task', taskRef),
  req('provider', providerIdentity),
];

/**
 * Validates a typed task identity and enforces that `key` stays a DERIVED view
 * of the structured fields — a key that disagrees is a violation, not an
 * alternative spelling.
 */
const identity: Check = (v, path, value) => {
  const obj = object(v, path, value, taskIdentityFields);
  const key = childString(obj, 'key');
  const projectId = childString(childObject(obj, 'project'), 'id');
  const taskName = childString(childObject(obj, 'task'), 'name');
  if (key === undefined || projectId === undefined || taskName === undefined) {
    return;
  }
  if (key !== `${projectId}:${taskName}`) {
    v.add(VIOLATION_CODE.invalidKey, join(path, 'key'));
  }
};

const resultErrorFields: Field[] = [
  req('code', enumOf('usage', 'auth', 'api', 'signal', 'failure')),
  req('message', nonEmptyString),
  opt('next', nonEmptyString),
];

const diagnosticFields: Field[] = [
  req('severity', enumOf('error', 'warning', 'info')),
  req('message', nonEmptyString),
  opt('code', nonEmptyString),
  opt('file', nonEmptyString),
  opt('line', intAtLeast(1)),
  opt('column', intAtLeast(1)),
];

const resultError: Check = (v, path, value) => object(v, path, value, resultErrorFields);
const diagnostic: Check = (v, path, value) => object(v, path, value, diagnosticFields);

const executionRecordFields: Field[] = [
  req('id', nonEmptyString),
  req('wallMs', intAtLeast(0)),
  req('userCpuMs', intAtLeast(0)),
  req('systemCpuMs', intAtLeast(0)),
  opt('maxRssBytes', intAtLeast(0)),
  opt('ioInBlocks', intAtLeast(0)),
  opt('ioOutBlocks', intAtLeast(0)),
  opt('concurrency', intAtLeast(1)),
  req('tasks', intAtLeast(0)),
];

const executionRecord: Check = (v, path, value) => object(v, path, value, executionRecordFields);

const taskRecordFields: Field[] = [
  req('identity', identity),
  opt('executionId', nonEmptyString),
  opt('inputDigest', prefixedSha256Digest),
  req('status', enumOf(TASK_STATUS.success, TASK_STATUS.failed, TASK_STATUS.canceled, TASK_STATUS.skipped)),
  req('reuse', enumOf(TASK_REUSE.none, TASK_REUSE.localCache, TASK_REUSE.remoteCache, TASK_REUSE.coalesced)),
  req('exitCode', intAtLeast(0)),
  req('durationMs', intAtLeast(0)),
  opt('taskWallMs', intAtLeast(0)),
  opt('spawnToFirstEventMs', intAtLeast(0)),
  opt('error', resultError),
  opt('diagnostics', arrayOf(diagnostic)),
  opt('testCasesDropped', intAtLeast(1)),
];

/**
 * Validates one task's terminal result. Status and reuse are orthogonal: reuse
 * never rewrites the verdict, so a reused failure stays `failed` and keeps its
 * error.
 */
const taskRecord: Check = (v, path, value) => {
  const obj = object(v, path, value, taskRecordFields);
  const status = childString(obj, 'status');
  if (status === TASK_STATUS.success && childValue(obj, 'error').present) {
    v.add(VIOLATION_CODE.errorMismatch, join(path, 'error'));
  }
  // A skipped task never looked its key up, so a digest beside it names inputs
  // nothing was keyed on.
  if (status === TASK_STATUS.skipped && childValue(obj, 'inputDigest').present) {
    v.add(VIOLATION_CODE.invalidValue, join(path, 'inputDigest'));
  }
};

const testCaseFields: Field[] = [
  req('name', nonEmptyStringBytesAtMost(TEST_CASE_MAX_TEXT_BYTES)),
  req('suite', nonEmptyStringBytesAtMost(TEST_CASE_MAX_TEXT_BYTES)),
  req('status', enumOf(TEST_CASE_STATUS.passed, TEST_CASE_STATUS.failed, TEST_CASE_STATUS.skipped)),
  req('durationMs', intAtLeast(0)),
  opt('output', nonEmptyStringBytesAtMost(TEST_CASE_MAX_OUTPUT_BYTES)),
  opt('outputTruncated', boolean),
  opt('file', nonEmptyStringBytesAtMost(TEST_CASE_MAX_TEXT_BYTES)),
  opt('line', intAtLeast(1)),
];

/**
 * Validates the payload of a `test:case` record. Output belongs to a case that
 * did not pass, `outputTruncated` describes an output that is present, and
 * `line` locates a case inside a file that is named. A member outside those
 * pairings is forbidden rather than ignored.
 */
const testCase: Check = (v, path, value) => {
  const obj = object(v, path, value, testCaseFields);
  const hasOutput = childValue(obj, 'output').present;
  if (childString(obj, 'status') === TEST_CASE_STATUS.passed && hasOutput) {
    v.add(VIOLATION_CODE.unexpectedField, join(path, 'output'));
  }
  if (childValue(obj, 'outputTruncated').present && !hasOutput) {
    v.add(VIOLATION_CODE.unexpectedField, join(path, 'outputTruncated'));
  }
  if (childValue(obj, 'line').present && !childValue(obj, 'file').present) {
    v.add(VIOLATION_CODE.unexpectedField, join(path, 'line'));
  }
};

const taskFailureFields: Field[] = [
  req('identity', identity),
  req('error', resultError),
  opt('diagnostics', arrayOf(diagnostic)),
];

const runCountsFields: Field[] = [
  req('total', intAtLeast(0)),
  req('succeeded', intAtLeast(0)),
  req('failed', intAtLeast(0)),
  req('canceled', intAtLeast(0)),
  req('skipped', intAtLeast(0)),
];

const runReuseFields: Field[] = [
  req('localCache', intAtLeast(0)),
  req('remoteCache', intAtLeast(0)),
  req('coalesced', intAtLeast(0)),
];

const taskFailure: Check = (v, path, value) => object(v, path, value, taskFailureFields);
const runCounts: Check = (v, path, value) => object(v, path, value, runCountsFields);
const runReuse: Check = (v, path, value) => object(v, path, value, runReuseFields);

const dockerPublishTimingsFields: Field[] = [
  req('buildMs', intAtLeast(0)),
  req('cacheLookupMs', intAtLeast(0)),
  req('cacheTransferMs', intAtLeast(0)),
  req('registryPushMs', intAtLeast(0)),
  req('referencePublishMs', intAtLeast(0)),
  req('digestResolveMs', intAtLeast(0)),
];

const dockerPublishConcurrencyFields: Field[] = [req('configuredCap', intAtLeast(1)), req('effective', intAtLeast(0))];

const dockerPublishTimings: Check = (v, path, value) => object(v, path, value, dockerPublishTimingsFields);
const dockerPublishConcurrency: Check = (v, path, value) => {
  const obj = object(v, path, value, dockerPublishConcurrencyFields);
  const cap = childInt(obj, 'configuredCap');
  const effective = childInt(obj, 'effective');
  if (cap !== undefined && effective !== undefined && effective > cap) {
    v.add(VIOLATION_CODE.invalidValue, join(path, 'effective'));
  }
};

const dockerPublicationFields: Field[] = [
  req('identity', identity),
  req('session', nonEmptyString),
  req('registry', nonEmptyString),
  req('image', nonEmptyString),
  req('immutableRef', nonEmptyString),
  opt('tags', arrayOf(nonEmptyString)),
  req('imageDigest', immutableSha256),
  req('contentStatus', enumOf('pushed', 'retagged', 'reused')),
  req('cacheOutcome', enumOf('hit', 'miss')),
  req('digestReused', boolean),
  req('timings', dockerPublishTimings),
  req('concurrency', dockerPublishConcurrency),
];
const dockerPublication: Check = (v, path, value) => {
  const obj = object(v, path, value, dockerPublicationFields);
  const image = childString(obj, 'image');
  const immutableRef = childString(obj, 'immutableRef');
  const digest = childString(obj, 'imageDigest');
  const status = childString(obj, 'contentStatus');
  const cacheOutcome = childString(obj, 'cacheOutcome');
  const digestReused = childBool(obj, 'digestReused');
  if (
    image !== undefined &&
    immutableRef !== undefined &&
    digest !== undefined &&
    /^sha256:[0-9a-f]{64}$/.test(digest) &&
    immutableRef !== `${image}@${digest}`
  ) {
    v.add(VIOLATION_CODE.invalidValue, join(path, 'immutableRef'));
  }
  if (status !== undefined && cacheOutcome !== undefined && digestReused !== undefined) {
    const valid =
      (cacheOutcome === 'miss' && status === 'pushed' && digestReused === false) ||
      (cacheOutcome === 'hit' && (status === 'retagged' || status === 'reused') && digestReused === true);
    if (!valid) v.add(VIOLATION_CODE.invalidValue, join(path, 'contentStatus'));
  }
};

const runCPUFields: Field[] = [
  req('allocatedMillicores', intAtLeast(1)),
  req('allocatedSource', enumOf(CPU_ALLOCATION.cgroupQuota, CPU_ALLOCATION.logicalCpus)),
  req('allocatedMs', intAtLeast(0)),
  req('actualMs', intAtLeast(0)),
  req('executions', intAtLeast(1)),
];

const runCPU: Check = (v, path, value) => object(v, path, value, runCPUFields);

const runLocalCacheFields: Field[] = [
  req('hits', intAtLeast(0)),
  req('misses', intAtLeast(0)),
  req('servedMs', intAtLeast(0)),
  req('keysMs', intAtLeast(0)),
  req('bindingsMs', intAtLeast(0)),
  req('restoreVerifyMs', intAtLeast(0)),
  req('spawnedProcesses', intAtLeast(0)),
];

const runLocalCache: Check = (v, path, value) => object(v, path, value, runLocalCacheFields);

const runCacheFields: Field[] = [opt('local', runLocalCache)];

const runCache: Check = (v, path, value) => object(v, path, value, runCacheFields);

const runSummaryFields: Field[] = [
  req('outcome', enumOf(RUN_OUTCOME.success, RUN_OUTCOME.failure, RUN_OUTCOME.aborted)),
  opt('abortedBy', enumOf(ABORTED_BY.user, ABORTED_BY.signal)),
  req('exitCode', intAtLeast(0)),
  req('counts', runCounts),
  req('reuse', runReuse),
  req('durationMs', intAtLeast(0)),
  opt('failures', arrayOf(taskFailure)),
  opt('publications', arrayOf(dockerPublication)),
  opt('cpu', runCPU),
  opt('cache', runCache),
];

const streamRunSummaryFields: Field[] = [
  req('outcome', enumOf(RUN_OUTCOME.success, RUN_OUTCOME.failure, RUN_OUTCOME.aborted)),
  opt('abortedBy', enumOf(ABORTED_BY.user, ABORTED_BY.signal)),
  req('exitCode', intAtLeast(0)),
  req('counts', runCounts),
  req('reuse', runReuse),
  req('durationMs', intAtLeast(0)),
  opt('cpu', runCPU),
];

const streamRunSummary: Check = (v, path, value) => {
  const obj = object(v, path, value, streamRunSummaryFields);
  checkRunArithmetic(v, path, obj);
  checkRunVerdict(v, path, obj);
  checkRunCPUBudget(v, path, obj);
};

/** Exit codes from the taxonomy in `protocols/cli/doc/01-contract.md`. */
const EXIT_SUCCESS = 0;
const EXIT_SIGNAL = 130;

/**
 * Enforces that the two histograms describe the same task set: counts spans
 * every selected task and must add up, and reuse — the provenance histogram over
 * those same tasks — can never exceed it. Keeping reuse OUT of the verdict
 * histogram is what removes version 1's strict/lenient fork.
 */
function checkRunArithmetic(v: Validator, path: string, obj: JsonObject | undefined): void {
  const counts = childObject(obj, 'counts');
  const total = childInt(counts, 'total');
  const succeeded = childInt(counts, 'succeeded');
  const failed = childInt(counts, 'failed');
  const canceled = childInt(counts, 'canceled');
  const skipped = childInt(counts, 'skipped');
  if (
    total !== undefined &&
    succeeded !== undefined &&
    failed !== undefined &&
    canceled !== undefined &&
    skipped !== undefined &&
    total !== succeeded + failed + canceled + skipped
  ) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'counts.total'));
  }

  const reuse = childObject(obj, 'reuse');
  const local = childInt(reuse, 'localCache');
  const remote = childInt(reuse, 'remoteCache');
  const coalesced = childInt(reuse, 'coalesced');
  if (
    total !== undefined &&
    local !== undefined &&
    remote !== undefined &&
    coalesced !== undefined &&
    local + remote + coalesced > total
  ) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'reuse'));
  }

  // A listed failure set must be complete: a reused failure that the run counted
  // cannot then be left out of the list an agent reads.
  const failures = childArray(obj, 'failures');
  if (failures !== undefined && failed !== undefined && failures.length !== failed) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'failures'));
  }
}

/**
 * Pins the exit code an outcome must carry. It is the same derivation on every
 * surface, which is what makes "the envelope and the process agree" checkable
 * rather than aspirational.
 */
function checkExitCodeForOutcome(v: Validator, path: string, outcome: string, obj: JsonObject | undefined): void {
  const code = childInt(obj, 'exitCode');
  if (code === undefined) {
    return;
  }
  if (outcome === RUN_OUTCOME.success && code !== EXIT_SUCCESS) {
    v.add(VIOLATION_CODE.exitCodeMismatch, path);
  }
  if (outcome === RUN_OUTCOME.aborted && code !== EXIT_SIGNAL) {
    v.add(VIOLATION_CODE.exitCodeMismatch, path);
  }
  if (outcome === RUN_OUTCOME.failure && (code === EXIT_SUCCESS || code === EXIT_SIGNAL)) {
    v.add(VIOLATION_CODE.exitCodeMismatch, path);
  }
}

/**
 * Enforces the settled verdict rules: strict unified success, the
 * aborted > failure > success precedence, and exit-code agreement.
 */
function checkRunVerdict(v: Validator, path: string, obj: JsonObject | undefined): void {
  const outcome = childString(obj, 'outcome');
  if (outcome === undefined) {
    return;
  }
  const hasAbortedBy = childValue(obj, 'abortedBy').present;
  const failed = childInt(childObject(obj, 'counts'), 'failed');

  if (outcome === RUN_OUTCOME.aborted && !hasAbortedBy) {
    v.add(VIOLATION_CODE.missingField, join(path, 'abortedBy'));
  } else if (outcome !== RUN_OUTCOME.aborted && hasAbortedBy) {
    // An abort source without the aborted outcome is exactly version 1's JSON
    // envelope ordering, where an interrupted run that also had failures was
    // reported as a plain failure.
    v.add(VIOLATION_CODE.outcomeMismatch, join(path, 'outcome'));
  } else if (outcome === RUN_OUTCOME.success && failed !== undefined && failed > 0) {
    v.add(VIOLATION_CODE.outcomeMismatch, join(path, 'outcome'));
  } else if (outcome === RUN_OUTCOME.failure && failed === 0) {
    v.add(VIOLATION_CODE.outcomeMismatch, join(path, 'outcome'));
  }
  checkExitCodeForOutcome(v, join(path, 'exitCode'), outcome, obj);
}

/**
 * Enforces the one arithmetic tie the CPU balance carries: `allocatedMs` is
 * `allocatedMillicores` applied over the SAME `durationMs` this summary states,
 * so the two cannot be published against different walls. The relation is
 * truncating integer division, exactly how a producer computes it.
 *
 * `actualMs` is deliberately unconstrained against `allocatedMs`: a run CAN
 * exceed its allocation for a while — a cgroup quota is enforced per period, not
 * per run — and clamping the measurement to the budget would hide precisely the
 * oversubscription this block exists to show.
 */
function checkRunCPUBudget(v: Validator, path: string, obj: JsonObject | undefined): void {
  const cpu = childObject(obj, 'cpu');
  if (cpu === undefined) {
    return;
  }
  const durationMs = childInt(obj, 'durationMs');
  const millicores = childInt(cpu, 'allocatedMillicores');
  const allocatedMs = childInt(cpu, 'allocatedMs');
  if (durationMs === undefined || millicores === undefined || allocatedMs === undefined) {
    return;
  }
  if (allocatedMs !== Math.trunc((durationMs * millicores) / 1000)) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'cpu.allocatedMs'));
  }
}

const runSummary: Check = (v, path, value) => {
  const obj = object(v, path, value, runSummaryFields);
  checkRunArithmetic(v, path, obj);
  checkRunVerdict(v, path, obj);
  checkRunCPUBudget(v, path, obj);
  checkRunCache(v, path, obj);
};

function checkRunCache(v: Validator, path: string, obj: JsonObject | undefined): void {
  const local = childObject(childObject(obj, 'cache'), 'local');
  if (local === undefined) {
    return;
  }
  const reuseLocal = childInt(childObject(obj, 'reuse'), 'localCache');
  const hits = childInt(local, 'hits');
  if (reuseLocal !== undefined && hits !== undefined && reuseLocal !== hits) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'cache.local.hits'));
  }
  const served = childInt(local, 'servedMs');
  const duration = childInt(obj, 'durationMs');
  if (served !== undefined && duration !== undefined && served > duration) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'cache.local.servedMs'));
  }
  let phaseTotal = 0;
  let hasAllPhases = true;
  for (const member of ['keysMs', 'bindingsMs', 'restoreVerifyMs']) {
    const phase = childInt(local, member);
    if (phase === undefined) {
      hasAllPhases = false;
      continue;
    }
    phaseTotal += phase;
  }
  // Producers partition the exact served timeline, then truncate each of its
  // three phase totals independently to milliseconds. Their sum is therefore
  // at most servedMs and can trail it by no more than two milliseconds.
  if (served !== undefined && hasAllPhases && (phaseTotal > served || served - phaseTotal > 2)) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'cache.local.servedMs'));
  }
}

const planMetricsFields: Field[] = [
  req('tasks', intAtLeast(0)),
  req('edges', intAtLeast(0)),
  req('projects', intAtLeast(0)),
  opt('byCommand', intMapValues),
];

const planMetrics: Check = (v, path, value) => object(v, path, value, planMetricsFields);

const plannedTaskFields: Field[] = [
  req('identity', identity),
  opt('dependsOn', arrayOf(nonEmptyString)),
  opt('after', arrayOf(nonEmptyString)),
  req('cache', boolean),
];

const plannedTask: Check = (v, path, value) => object(v, path, value, plannedTaskFields);

const planSummaryFields: Field[] = [
  req('dryRun', boolean),
  req('metrics', planMetrics),
  req('tasks', arrayOf(plannedTask)),
];

const planSummary: Check = (v, path, value) => {
  const obj = object(v, path, value, planSummaryFields);
  const taskCount = childInt(childObject(obj, 'metrics'), 'tasks');
  const tasks = childArray(obj, 'tasks');
  if (taskCount !== undefined && tasks !== undefined && taskCount !== tasks.length) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'metrics.tasks'));
  }
};

const sessionGitFields: Field[] = [opt('branch', nonEmptyString), opt('baseline', nonEmptyString)];
const sessionGit: Check = (v, path, value) => object(v, path, value, sessionGitFields);

// Every member is required: the block is written only when the fingerprint was
// computed, and that one computation decides all three at once. A partial tree
// block would be a claim nobody measured.
const sessionTreeFields: Field[] = [
  req('fingerprint', sha256Digest),
  req('dirty', boolean),
  req('headSHA', gitObjectId),
];
const sessionTree: Check = (v, path, value) => object(v, path, value, sessionTreeFields);

// Every member is required: the executing engine states all three from the
// one bound request it was handed, so a partial block would be a claim nobody
// executed.
const sessionProvenanceFields: Field[] = [
  req('sourceDigest', prefixedSha256Digest),
  req('inputDigest', prefixedSha256Digest),
  req('submission', hexOfLength(32)),
];
const sessionProvenance: Check = (v, path, value) => object(v, path, value, sessionProvenanceFields);

const sessionPlacementFields: Field[] = [
  req('requested', enumOf('local', 'remote')),
  req('actual', enumOf('local', 'remote')),
  opt('provenance', sessionProvenance),
];
// Provenance is the executing engine's statement about a bound request, and a
// bound request executes only as the remote placement, so a block beside a
// local execution is a claim nothing executed.
const sessionPlacement: Check = (v, path, value) => {
  const obj = object(v, path, value, sessionPlacementFields);
  if (obj !== undefined && 'provenance' in obj && obj['actual'] !== 'remote') {
    v.add(VIOLATION_CODE.invalidValue, join(path, 'provenance'));
  }
};

const sessionSelectionFields: Field[] = [
  req('mode', enumOf(...SESSION_SELECTION_MODES)),
  req('scoped', boolean),
  opt('projects', arrayOf(nonEmptyString)),
  opt('releaseSetProjects', arrayOf(nonEmptyString)),
];
const sessionSelection: Check = (v, path, value) => object(v, path, value, sessionSelectionFields);

const cgroupThrottleFields: Field[] = [
  req('periods', intAtLeast(0)),
  req('throttledPeriods', intAtLeast(0)),
  req('throttledUs', intAtLeast(0)),
];

const cgroupThrottle: Check = (v, path, value) => object(v, path, value, cgroupThrottleFields);

const cgroupCPUFields: Field[] = [
  req('periodUs', intAtLeast(1)),
  opt('quotaUs', intAtLeast(1)),
  opt('usageUs', intAtLeast(0)),
  opt('throttle', cgroupThrottle),
];

const cgroupCPU: Check = (v, path, value) => object(v, path, value, cgroupCPUFields);

const cpuPressureFields: Field[] = [req('someStalledUs', intAtLeast(0))];

const cpuPressure: Check = (v, path, value) => object(v, path, value, cpuPressureFields);

const hostCPUTimeFields: Field[] = [
  req('stealTicks', intAtLeast(0)),
  req('ioWaitTicks', intAtLeast(0)),
  req('totalTicks', intAtLeast(0)),
];

/**
 * Keeps the `/proc/stat` window internally consistent: steal and iowait are
 * COLUMNS of the same total, so neither can exceed it. A producer that mixed two
 * sample windows, or subtracted them in the wrong order, lands here rather than
 * publishing a steal share above 100%.
 */
const hostCPUTime: Check = (v, path, value) => {
  const obj = object(v, path, value, hostCPUTimeFields);
  const total = childInt(obj, 'totalTicks');
  if (total === undefined) {
    return;
  }
  for (const name of ['ioWaitTicks', 'stealTicks']) {
    const ticks = childInt(obj, name);
    if (ticks !== undefined && ticks > total) {
      v.add(VIOLATION_CODE.countMismatch, join(path, name));
    }
  }
};

const cgroupMemoryLimitFields: Field[] = [
  req('bytes', intAtLeast(1)),
  req('source', enumOf(CGROUP_MEMORY_SOURCE.v1, CGROUP_MEMORY_SOURCE.v2)),
];

const cgroupMemoryLimit: Check = (v, path, value) => object(v, path, value, cgroupMemoryLimitFields);

const memoryCapacityFields: Field[] = [
  opt('physicalBytes', intAtLeast(1)),
  opt('cgroupLimit', cgroupMemoryLimit),
  req('effectiveBytes', intAtLeast(1)),
  req('effectiveSource', enumOf(MEMORY_CAPACITY_SOURCE.physical, MEMORY_CAPACITY_SOURCE.cgroupLimit)),
];

const memoryCapacity: Check = (v, path, value) => {
  const obj = object(v, path, value, memoryCapacityFields);
  const physical = childInt(obj, 'physicalBytes');
  const cgroup = childInt(childObject(obj, 'cgroupLimit'), 'bytes');
  const effective = childInt(obj, 'effectiveBytes');
  const source = childString(obj, 'effectiveSource');

  // Effective capacity is a projection of measured stable bounds, never an
  // independent estimate. With both inputs it is their minimum.
  if (physical === undefined && cgroup === undefined) {
    v.add(VIOLATION_CODE.invalidValue, join(path, 'effectiveSource'));
    return;
  }
  if (effective !== undefined) {
    const want = physical === undefined ? cgroup : cgroup === undefined ? physical : Math.min(physical, cgroup);
    if (effective !== want) {
      v.add(VIOLATION_CODE.countMismatch, join(path, 'effectiveBytes'));
    }
  }
  if (source === undefined || effective === undefined) {
    return;
  }
  if (source === MEMORY_CAPACITY_SOURCE.physical && (physical === undefined || effective !== physical)) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'effectiveSource'));
  } else if (source === MEMORY_CAPACITY_SOURCE.cgroupLimit && (cgroup === undefined || effective !== cgroup)) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'effectiveSource'));
  }
};

const cgroupMemoryCompositionFields: Field[] = [
  req('anonBytes', intAtLeast(0)),
  req('fileBytes', intAtLeast(0)),
  req('shmemBytes', intAtLeast(0)),
];

const cgroupMemoryComposition: Check = (v, path, value) => {
  const obj = object(v, path, value, cgroupMemoryCompositionFields);
  const file = childInt(obj, 'fileBytes');
  const shmem = childInt(obj, 'shmemBytes');
  if (file !== undefined && shmem !== undefined && shmem > file) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'shmemBytes'));
  }
};

const cgroupMemoryLifetimePeakFields: Field[] = [req('bytes', intAtLeast(0))];

const cgroupMemoryLifetimePeak: Check = (v, path, value) => object(v, path, value, cgroupMemoryLifetimePeakFields);

const cgroupMemoryClosingFields: Field[] = [
  req('currentBytes', intAtLeast(0)),
  opt('composition', cgroupMemoryComposition),
  opt('lifetimePeak', cgroupMemoryLifetimePeak),
];

const cgroupMemoryClosing: Check = (v, path, value) => object(v, path, value, cgroupMemoryClosingFields);

const cgroupMemoryEventsFields: Field[] = [
  req('low', intAtLeast(0)),
  req('high', intAtLeast(0)),
  req('max', intAtLeast(0)),
  req('oom', intAtLeast(0)),
  req('oomKill', intAtLeast(0)),
];

const cgroupMemoryEvents: Check = (v, path, value) => object(v, path, value, cgroupMemoryEventsFields);

const cgroupMemoryFields: Field[] = [
  req('source', enumOf(CGROUP_MEMORY_SOURCE.v1, CGROUP_MEMORY_SOURCE.v2)),
  req('closing', cgroupMemoryClosing),
  opt('events', cgroupMemoryEvents),
];

const cgroupMemory: Check = (v, path, value) => {
  const obj = object(v, path, value, cgroupMemoryFields);
  if (childString(obj, 'source') !== CGROUP_MEMORY_SOURCE.v1) {
    return;
  }
  if (childValue(obj, 'events').present) {
    v.add(VIOLATION_CODE.unexpectedField, join(path, 'events'));
  }
  if (childValue(childObject(obj, 'closing'), 'composition').present) {
    v.add(VIOLATION_CODE.unexpectedField, join(path, 'closing.composition'));
  }
};

const memoryPressureFields: Field[] = [
  req('scope', enumOf(MEMORY_PRESSURE_SCOPE.host, MEMORY_PRESSURE_SCOPE.cgroup)),
  req('someStalledUs', intAtLeast(0)),
  req('fullStalledUs', intAtLeast(0)),
];

const memoryPressure: Check = (v, path, value) => {
  const obj = object(v, path, value, memoryPressureFields);
  const some = childInt(obj, 'someStalledUs');
  const full = childInt(obj, 'fullStalledUs');
  if (some !== undefined && full !== undefined && full > some) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'fullStalledUs'));
  }
};

const sessionEnvironmentFields: Field[] = [
  req('os', nonEmptyString),
  req('arch', nonEmptyString),
  req('logicalCpus', intAtLeast(1)),
  opt('cpuModel', nonEmptyString),
  req('windowMs', intAtLeast(0)),
  opt('cgroupCpu', cgroupCPU),
  opt('cpuPressure', cpuPressure),
  opt('hostCpu', hostCPUTime),
  opt('memoryCapacity', memoryCapacity),
  opt('cgroupMemory', cgroupMemory),
  opt('memoryPressure', memoryPressure),
];

const sessionEnvironment: Check = (v, path, value) => {
  const obj = object(v, path, value, sessionEnvironmentFields);
  const cgroupSource = childString(childObject(obj, 'cgroupMemory'), 'source');
  const pressureScope = childString(childObject(obj, 'memoryPressure'), 'scope');
  if (pressureScope === MEMORY_PRESSURE_SCOPE.cgroup && cgroupSource !== CGROUP_MEMORY_SOURCE.v2) {
    v.add(VIOLATION_CODE.invalidValue, join(path, 'memoryPressure.scope'));
  }
  const limitSource = childString(childObject(childObject(obj, 'memoryCapacity'), 'cgroupLimit'), 'source');
  if (limitSource !== undefined && cgroupSource !== undefined && limitSource !== cgroupSource) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'cgroupMemory.source'));
  }
};

const preparationPhaseRecordFields: Field[] = [
  req(
    'phase',
    enumOf(
      PREPARATION_PHASE.network,
      PREPARATION_PHASE.resolution,
      PREPARATION_PHASE.verification,
      PREPARATION_PHASE.generation,
      PREPARATION_PHASE.mutation,
    ),
  ),
  req('wallMs', intAtLeast(0)),
  req('steps', intAtLeast(1)),
  opt('cpuMs', intAtLeast(0)),
];

const preparationPhaseRecord: Check = (v, path, value) => object(v, path, value, preparationPhaseRecordFields);

const sessionPreparationFields: Field[] = [
  req('wallMs', intAtLeast(0)),
  req('parallelism', intAtLeast(1)),
  req('phases', arrayOf(preparationPhaseRecord)),
];

const machineOutputBudgetFields: Field[] = [
  req('maxBytes', intAtLeast(1)),
  req('maxRecords', intAtLeast(1)),
  req('failureReserveBytes', intAtLeast(1)),
  req('failureReserveRecords', intAtLeast(1)),
  req('finalReserveBytes', intAtLeast(1)),
  req('finalReserveRecords', intAtLeast(1)),
];

const machineOutputBudget: Check = (v, path, value) => object(v, path, value, machineOutputBudgetFields);

const machineOutputElisionFields: Field[] = [req('records', intAtLeast(0)), req('bytes', intAtLeast(0))];

const machineOutputElision: Check = (v, path, value) => {
  const obj = object(v, path, value, machineOutputElisionFields);
  const records = childInt(obj, 'records');
  const bytes = childInt(obj, 'bytes');
  if (records !== undefined && bytes !== undefined && (records === 0) !== (bytes === 0)) {
    v.add(VIOLATION_CODE.countMismatch, path);
  }
};

const machineOutputElisionsFields: Field[] = [
  req('ordinary', machineOutputElision),
  req('failure', machineOutputElision),
];
const machineOutputElisions: Check = (v, path, value) => object(v, path, value, machineOutputElisionsFields);

const machineOutputArtifactFields: Field[] = [
  req('sessionId', nonEmptyStringBytesAtMost(64)),
  req('path', enumOf(MACHINE_OUTPUT_ARTIFACT_PATH)),
  req('retention', enumOf(MACHINE_OUTPUT_ARTIFACT_RETENTION)),
];
const machineOutputArtifact: Check = (v, path, value) => object(v, path, value, machineOutputArtifactFields);

const machineOutputSummaryFields: Field[] = [
  req('mode', enumOf(MACHINE_OUTPUT_MODE.normal, MACHINE_OUTPUT_MODE.verbose)),
  req('sanitization', enumOf(MACHINE_OUTPUT_SANITIZATION)),
  req('budget', machineOutputBudget),
  req('elided', machineOutputElisions),
  req('artifact', machineOutputArtifact),
];

const machineOutputSummary: Check = (v, path, value) => {
  const obj = object(v, path, value, machineOutputSummaryFields);
  const mode = childString(obj, 'mode');
  const budget = childObject(obj, 'budget');
  if ((mode !== MACHINE_OUTPUT_MODE.normal && mode !== MACHINE_OUTPUT_MODE.verbose) || budget === undefined) {
    return;
  }
  const expected = machineOutputBudgetFor(mode);
  for (const [name, want] of Object.entries(expected)) {
    const got = childInt(budget, name);
    if (got !== undefined && got !== want) {
      v.add(VIOLATION_CODE.invalidValue, join(join(path, 'budget'), name));
    }
  }
};

/**
 * Keeps the decomposition from becoming a list of unrelated numbers. Two rules,
 * both locally checkable:
 *
 * - A phase appears AT MOST ONCE. The block is a HISTOGRAM over ownership
 *   classes, so a repeated class means the producer emitted per-step rows under
 *   a summary's contract, and every ratio a reader computes from it would be
 *   wrong with nothing else in the document to reveal it.
 * - At parallelism 1 the phase walls cannot overlap, so they must fit inside the
 *   stage's own wall. Above 1 they may overlap and the check is skipped — which
 *   is why parallelism is required rather than a nice-to-have.
 */
const checkPreparationPhases = (v: Validator, path: string, obj: JsonObject | undefined): void => {
  const phases = childArray(obj, 'phases');
  if (phases === undefined) {
    return;
  }
  const seen = new Set<string>();
  let summed = 0;
  phases.forEach((entry, index) => {
    if (!isObject(entry)) {
      return;
    }
    const name = childString(entry, 'phase');
    if (name !== undefined) {
      if (seen.has(name)) {
        v.add(VIOLATION_CODE.countMismatch, join(path, `phases[${index}].phase`));
      }
      seen.add(name);
    }
    summed += childInt(entry, 'wallMs') ?? 0;
  });
  const parallelism = childInt(obj, 'parallelism');
  const stageWall = childInt(obj, 'wallMs');
  if (parallelism === 1 && stageWall !== undefined && summed > stageWall) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'wallMs'));
  }
};

const sessionPreparation: Check = (v, path, value) => {
  checkPreparationPhases(v, path, object(v, path, value, sessionPreparationFields));
};

const resultEnvelopeFields: Field[] = [
  req('protocolVersion', protocolVersion),
  req('command', nonEmptyString),
  req('status', enumOf(RUN_OUTCOME.success, RUN_OUTCOME.failure, RUN_OUTCOME.aborted)),
  req('exitCode', intAtLeast(0)),
  opt('data', anyValue),
  opt('run', runSummary),
  opt('plan', planSummary),
  opt('error', resultError),
];

const sessionStreamRecordFields: Field[] = [
  req('protocolVersion', protocolVersion),
  req(
    'record',
    enumOf(
      STREAM_RECORD.taskStart,
      STREAM_RECORD.taskEvent,
      STREAM_RECORD.taskEnd,
      STREAM_RECORD.testCase,
      STREAM_RECORD.planEnd,
      STREAM_RECORD.sessionEnd,
    ),
  ),
  req('time', nonEmptyString),
  opt('identity', identity),
  opt('event', openObject),
  opt('task', taskRecord),
  opt('testCase', testCase),
  opt('run', anyValue),
  opt('plan', planSummary),
  opt('machineOutput', machineOutputSummary),
];

const boundedSessionEndRecordFields: Field[] = [
  req('protocolVersion', protocolVersion),
  req('record', enumOf(STREAM_RECORD.sessionEnd)),
  req('time', nonEmptyStringBytesAtMost(64)),
  req('run', streamRunSummary),
  req('machineOutput', machineOutputSummary),
];

const mcpResultFields: Field[] = [
  req('protocolVersion', protocolVersion),
  req('tool', enumOf(MCP_TOOL.runJobs, MCP_TOOL.planJobs)),
  req('commands', arrayOf(nonEmptyString)),
  opt('run', runSummary),
  opt('plan', planSummary),
];

const sessionFileFields: Field[] = [
  req('protocolVersion', protocolVersion),
  req('sessionId', nonEmptyString),
  opt('parentSessionId', nonEmptyString),
  req('startTime', nonEmptyString),
  opt('endTime', nonEmptyString),
  req('commands', arrayOf(nonEmptyString)),
  opt('selection', sessionSelection),
  opt('git', sessionGit),
  opt('tree', sessionTree),
  opt('placement', sessionPlacement),
  req('run', runSummary),
  opt('tasks', arrayOf(taskRecord)),
  opt('executions', arrayOf(executionRecord)),
  opt('environment', sessionEnvironment),
  opt('preparation', sessionPreparation),
  opt('scheduler', anyValue),
  opt('cache', anyValue),
];

const sessionPlanFileFields: Field[] = [
  req('protocolVersion', protocolVersion),
  req('sessionId', nonEmptyString),
  req('commands', arrayOf(nonEmptyString)),
  req('tasks', arrayOf(plannedTask)),
];

/**
 * Requires a JSON number in [0,100]. It is the contract's only non-integer
 * member: a coverage share is a measurement, not a count, and rounding it would
 * erase the difference between 89.4% and 89.6% against a 90% threshold.
 *
 * A number outside the range — including the `Infinity` a too-large literal
 * parses to, which is exactly the Go mirror's float64 range error — is an
 * invalid VALUE rather than an invalid type: the wire form was a number, it just
 * was not a percentage.
 */
function percentage(v: Validator, path: string, value: unknown): void {
  if (typeof value !== 'number') {
    v.add(VIOLATION_CODE.invalidType, path);
    return;
  }
  if (!Number.isFinite(value) || value < 0 || value > 100) {
    v.add(VIOLATION_CODE.invalidValue, path);
  }
}

/**
 * Accepts a full git object id — 40 lowercase hex for a sha1 repository, 64 for
 * a sha256 one. An abbreviation, a symbolic name (`HEAD`) or a branch is
 * rejected: the sha is the join key a longitudinal consumer files the report
 * under, and a reference that can move is not one.
 */
function gitObjectId(v: Validator, path: string, value: unknown): void {
  if (typeof value !== 'string') {
    v.add(VIOLATION_CODE.invalidType, path);
    return;
  }
  if (!/^([0-9a-f]{40}|[0-9a-f]{64})$/.test(value)) {
    v.add(VIOLATION_CODE.invalidValue, path);
  }
}

/**
 * Accepts a full lowercase hex sha256. Deliberately stricter than
 * {@link gitObjectId}: a tree fingerprint is a join key two parties compare as
 * strings, so an uppercase or abbreviated spelling of the same digest would
 * silently read as a different tree.
 */
function sha256Digest(v: Validator, path: string, value: unknown): void {
  if (typeof value !== 'string') {
    v.add(VIOLATION_CODE.invalidType, path);
    return;
  }
  if (!/^[0-9a-f]{64}$/.test(value)) {
    v.add(VIOLATION_CODE.invalidValue, path);
  }
}

/**
 * Accepts the runner contract's digest spelling — `sha256:` and 64 lowercase
 * hex characters — so a session's provenance compares with a request identity
 * as one string.
 */
function prefixedSha256Digest(v: Validator, path: string, value: unknown): void {
  if (typeof value !== 'string') {
    v.add(VIOLATION_CODE.invalidType, path);
    return;
  }
  if (!/^sha256:[0-9a-f]{64}$/.test(value)) {
    v.add(VIOLATION_CODE.invalidValue, path);
  }
}

/** Accepts exactly `length` lowercase hex characters. */
function hexOfLength(length: number): Check {
  return (v, path, value) => {
    if (typeof value !== 'string') {
      v.add(VIOLATION_CODE.invalidType, path);
      return;
    }
    if (value.length !== length || !/^[0-9a-f]*$/.test(value)) {
      v.add(VIOLATION_CODE.invalidValue, path);
    }
  };
}

const reportGitFields: Field[] = [
  opt('branch', nonEmptyString),
  req('sha', gitObjectId),
  opt('dirty', boolean),
  opt('baseline', nonEmptyString),
];

const reportGit: Check = (v, path, value) => object(v, path, value, reportGitFields);

const reportRunFields: Field[] = [
  req('outcome', enumOf(RUN_OUTCOME.success, RUN_OUTCOME.failure, RUN_OUTCOME.aborted)),
  req('exitCode', intAtLeast(0)),
  req('counts', runCounts),
  req('reuse', runReuse),
  req('durationMs', intAtLeast(0)),
  opt('cpu', runCPU),
];

/**
 * The report's verdict rules. It has no `abortedBy`, because an abort source is
 * a run-ledger fact and the report keeps only the verdict — everything else is
 * the run summary's rules verbatim, on deliberately identical member names, so
 * the two documents cannot start counting differently.
 */
function checkReportRunVerdict(v: Validator, path: string, obj: JsonObject | undefined): void {
  const outcome = childString(obj, 'outcome');
  if (outcome === undefined) {
    return;
  }
  const failed = childInt(childObject(obj, 'counts'), 'failed');
  if (outcome === RUN_OUTCOME.success && failed !== undefined && failed > 0) {
    v.add(VIOLATION_CODE.outcomeMismatch, join(path, 'outcome'));
  } else if (outcome === RUN_OUTCOME.failure && failed === 0) {
    v.add(VIOLATION_CODE.outcomeMismatch, join(path, 'outcome'));
  }
  checkExitCodeForOutcome(v, join(path, 'exitCode'), outcome, obj);
}

const reportRun: Check = (v, path, value) => {
  const obj = object(v, path, value, reportRunFields);
  checkRunArithmetic(v, path, obj);
  checkReportRunVerdict(v, path, obj);
  checkRunCPUBudget(v, path, obj);
};

const reportTestsFields: Field[] = [
  req('total', intAtLeast(0)),
  req('passed', intAtLeast(0)),
  req('failed', intAtLeast(0)),
  req('skipped', intAtLeast(0)),
  opt('failureDetailsTruncated', intAtLeast(0)),
];

/** Keeps the outcome counters one measurement; omission accounting is orthogonal. */
const reportTests: Check = (v, path, value) => {
  const obj = object(v, path, value, reportTestsFields);
  const total = childInt(obj, 'total');
  const passed = childInt(obj, 'passed');
  const failed = childInt(obj, 'failed');
  const skipped = childInt(obj, 'skipped');
  if (
    total !== undefined &&
    passed !== undefined &&
    failed !== undefined &&
    skipped !== undefined &&
    total !== passed + failed + skipped
  ) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'total'));
  }
};

const reportCoverageFields: Field[] = [
  req('percentage', percentage),
  req(
    'granularity',
    enumOf(
      COVERAGE_GRANULARITY.statements,
      COVERAGE_GRANULARITY.lines,
      COVERAGE_GRANULARITY.functions,
      COVERAGE_GRANULARITY.branches,
    ),
  ),
  opt('covered', intAtLeast(0)),
  opt('total', intAtLeast(0)),
  req('enforced', boolean),
];

/**
 * A producer that states `covered` and `total` has stated the same measurement
 * twice, and the counts cannot describe more covered units than there are units.
 */
const reportCoverage: Check = (v, path, value) => {
  const obj = object(v, path, value, reportCoverageFields);
  const covered = childInt(obj, 'covered');
  const total = childInt(obj, 'total');
  if (covered !== undefined && total !== undefined && covered > total) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'covered'));
  }
};

const reportCommandFields: Field[] = [
  req('command', nonEmptyString),
  req('counts', runCounts),
  req('reuse', runReuse),
  req('freshWallMs', intAtLeast(0)),
  opt('cpuMs', intAtLeast(0)),
  opt('tests', reportTests),
  opt('coverage', reportCoverage),
  req('errors', intAtLeast(0)),
  req('warnings', intAtLeast(0)),
];

/** A command's counts span its own selected tasks and add up the run's way. */
const reportCommand: Check = (v, path, value) => {
  checkRunArithmetic(v, path, object(v, path, value, reportCommandFields));
};

/**
 * The shared diagnostic shape plus the report's message bound. The bound is in
 * UTF-8 BYTES, which is what a size budget is actually spent in; the schema's
 * `maxLength` counts code points and is therefore the weaker of the two, so a
 * document this validator accepts always satisfies it.
 */
const reportDiagnostic: Check = (v, path, value) => {
  const obj = object(v, path, value, diagnosticFields);
  const message = childString(obj, 'message');
  if (message !== undefined && UTF8.encode(message).length > REPORT_MAX_MESSAGE_BYTES) {
    v.add(VIOLATION_CODE.invalidValue, join(path, 'message'));
  }
};

/** A job's diagnostics under both report bounds: how many, and how long each. */
const reportDiagnostics: Check = (v, path, value) => {
  if (!Array.isArray(value)) {
    v.add(VIOLATION_CODE.invalidType, path);
    return;
  }
  if (value.length > REPORT_MAX_JOB_DIAGNOSTICS) {
    v.add(VIOLATION_CODE.invalidValue, path);
  }
  for (const [i, element] of value.entries()) {
    reportDiagnostic(v, `${path}[${i}]`, element);
  }
};

const reportJobFields: Field[] = [
  req('key', nonEmptyString),
  req('project', nonEmptyString),
  req('task', nonEmptyString),
  req('command', nonEmptyString),
  req('outcome', enumOf(TASK_STATUS.success, TASK_STATUS.failed, TASK_STATUS.canceled, TASK_STATUS.skipped)),
  req('reuse', enumOf(TASK_REUSE.none, TASK_REUSE.localCache, TASK_REUSE.remoteCache, TASK_REUSE.coalesced)),
  req('durationMs', intAtLeast(0)),
  opt('cpuMs', intAtLeast(0)),
  opt('coverage', reportCoverage),
  opt('diagnostics', reportDiagnostics),
  opt('failureDetailsTruncated', intAtLeast(1)),
  opt('truncatedCount', intAtLeast(1)),
];

/**
 * One job line. The key stays a DERIVED view of project and task, the same rule
 * {@link TaskIdentity} carries — the report flattens the identity, it does not
 * invent a second spelling of it. `failureDetailsTruncated` identifies the
 * producer-side part of `truncatedCount`; any remainder was dropped by report
 * reduction and therefore requires a full diagnostics list.
 */
const reportJob: Check = (v, path, value) => {
  const obj = object(v, path, value, reportJobFields);
  const key = childString(obj, 'key');
  const project = childString(obj, 'project');
  const task = childString(obj, 'task');
  if (key !== undefined && project !== undefined && task !== undefined && key !== `${project}:${task}`) {
    v.add(VIOLATION_CODE.invalidKey, join(path, 'key'));
  }
  const truncated = childInt(obj, 'truncatedCount');
  const producerOmitted = childInt(obj, 'failureDetailsTruncated');
  // Field checks own non-positive values; avoid cascading arithmetic findings
  // from an operand that is already invalid.
  if ((truncated !== undefined && truncated <= 0) || (producerOmitted !== undefined && producerOmitted <= 0)) {
    return;
  }
  if (producerOmitted !== undefined && (truncated === undefined || producerOmitted > truncated)) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'truncatedCount'));
    return;
  }
  if (truncated === undefined || truncated <= (producerOmitted ?? 0)) {
    return;
  }
  const diagnostics = childArray(obj, 'diagnostics');
  if (diagnostics === undefined || diagnostics.length !== REPORT_MAX_JOB_DIAGNOSTICS) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'truncatedCount'));
  }
};

/**
 * The bounded job list. The bound is a CONTRACT clause, not a producer
 * preference: a consumer that accepts this document accepts a stated worst case,
 * so a list past the cap is rejected rather than quietly read.
 */
const reportJobs: Check = (v, path, value) => {
  if (!Array.isArray(value)) {
    v.add(VIOLATION_CODE.invalidType, path);
    return;
  }
  if (value.length > REPORT_MAX_JOBS) {
    v.add(VIOLATION_CODE.invalidValue, path);
  }
  for (const [i, element] of value.entries()) {
    reportJob(v, `${path}[${i}]`, element);
  }
};

const reportCacheFields: Field[] = [
  req('hits', intAtLeast(0)),
  req('misses', intAtLeast(0)),
  req('restored', intAtLeast(0)),
  req('uploads', intAtLeast(0)),
  req('timeSavedMs', intAtLeast(0)),
  req('bytesFetched', intAtLeast(0)),
  req('bytesUploaded', intAtLeast(0)),
];

/**
 * Restores are a subset of hits: a key that was not held cannot have been
 * materialized, so the reverse means the two counters came from different
 * accountings.
 */
const reportCache: Check = (v, path, value) => {
  const obj = object(v, path, value, reportCacheFields);
  const hits = childInt(obj, 'hits');
  const restored = childInt(obj, 'restored');
  if (hits !== undefined && restored !== undefined && restored > hits) {
    v.add(VIOLATION_CODE.countMismatch, join(path, 'restored'));
  }
};

const reportSchedulerFields: Field[] = [req('parallelism', intAtLeast(1)), req('criticalPathMs', intAtLeast(0))];

const reportScheduler: Check = (v, path, value) => object(v, path, value, reportSchedulerFields);

const reportFileFields: Field[] = [
  req('protocolVersion', protocolVersion),
  req('sessionId', nonEmptyString),
  req('startTime', nonEmptyString),
  req('endTime', nonEmptyString),
  req('origin', enumOf(REPORT_ORIGIN.cli, REPORT_ORIGIN.mcp)),
  req('enforceCoverage', boolean),
  opt('fix', boolean),
  opt('git', reportGit),
  req('run', reportRun),
  req('commands', arrayOf(reportCommand)),
  req('jobs', reportJobs),
  req('elidedJobs', intAtLeast(0)),
  opt('cache', reportCache),
  opt('scheduler', reportScheduler),
];

/** The run's task total, read through the report's own verdict block. */
function reportRunTotal(obj: JsonObject | undefined): number | undefined {
  return childInt(childObject(childObject(obj, 'run'), 'counts'), 'total');
}

/**
 * Ties the bounded job list back to the run it summarizes. Two rules, and
 * together they make the truncation honest:
 *
 * - ACCOUNTING: `jobs.length + elidedJobs` equals the run's own task total, so a
 *   reader can always tell a small run from a truncated one.
 * - EXHAUSTION: nothing is elided until the budget is FULL. Otherwise a producer
 *   could ship one job, call the other 744 elided, and satisfy the accounting
 *   rule while carrying none of the information the bound was sized to allow.
 */
function checkReportJobBudget(v: Validator, obj: JsonObject | undefined): void {
  const jobs = childArray(obj, 'jobs');
  const elided = childInt(obj, 'elidedJobs');
  if (jobs === undefined || elided === undefined) {
    return;
  }
  const total = reportRunTotal(obj);
  if (total !== undefined && jobs.length + elided !== total) {
    v.add(VIOLATION_CODE.countMismatch, 'elidedJobs');
  }
  if (elided > 0 && jobs.length !== REPORT_MAX_JOBS) {
    v.add(VIOLATION_CODE.countMismatch, 'jobs');
  }
}

/** What one pass over the per-command rows yields for the run-level checks. */
interface CommandRollup {
  totals: number;
  cpuMs: number;
  /** False when a row stated no total, so the sum is partial and cannot be compared. */
  countsComplete: boolean;
}

/**
 * Walks the per-command rows ONCE: it reports a repeated command — the list is a
 * histogram over root commands, not a per-task log — and returns the sums the
 * run-level rules are checked against.
 */
function rollUpReportCommands(v: Validator, commands: unknown[]): CommandRollup {
  const seen = new Set<string>();
  const rollup: CommandRollup = { totals: 0, cpuMs: 0, countsComplete: true };
  for (const [i, entry] of commands.entries()) {
    if (!isObject(entry)) {
      rollup.countsComplete = false;
      continue;
    }
    const name = childString(entry, 'command');
    if (name !== undefined) {
      if (seen.has(name)) {
        v.add(VIOLATION_CODE.countMismatch, join(`commands[${i}]`, 'command'));
      }
      seen.add(name);
    }
    const total = childInt(childObject(entry, 'counts'), 'total');
    if (total === undefined) {
      rollup.countsComplete = false;
    }
    rollup.totals += total ?? 0;
    rollup.cpuMs += childInt(entry, 'cpuMs') ?? 0;
  }
  return rollup;
}

/**
 * Keeps the per-command synthesis a partition of the run rather than a list of
 * unrelated rows: the per-command totals sum to the run's total (every selected
 * task belongs to exactly one root command), and the per-command CPU never
 * exceeds the run's ACTUAL CPU — superseded attempts belong to the run's ledger
 * and to no command, so the run's figure can be higher but never lower.
 */
function checkReportCommands(v: Validator, obj: JsonObject | undefined): void {
  const commands = childArray(obj, 'commands');
  if (commands === undefined) {
    return;
  }
  const rollup = rollUpReportCommands(v, commands);
  const runTotal = reportRunTotal(obj);
  if (rollup.countsComplete && runTotal !== undefined && rollup.totals !== runTotal) {
    v.add(VIOLATION_CODE.countMismatch, 'commands');
  }
  const actual = childInt(childObject(childObject(obj, 'run'), 'cpu'), 'actualMs');
  if (actual !== undefined && rollup.cpuMs > actual) {
    v.add(VIOLATION_CODE.invalidValue, 'commands');
  }
}

/**
 * The member list each document declares, keyed by the schema `$def` it mirrors.
 * Exported so the conformance test can pin it against `result-v2.json` exactly
 * as the Go drift test does.
 */
export const V2_FIELD_NAMES: Record<string, { members: string[]; required: string[] }> = Object.fromEntries(
  (
    [
      ['projectIdentity', projectIdentityFields],
      ['taskRef', taskRefFields],
      ['providerIdentity', providerIdentityFields],
      ['taskIdentity', taskIdentityFields],
      ['resultError', resultErrorFields],
      ['diagnostic', diagnosticFields],
      ['executionRecord', executionRecordFields],
      ['taskRecord', taskRecordFields],
      ['testCase', testCaseFields],
      ['taskFailure', taskFailureFields],
      ['runCounts', runCountsFields],
      ['runReuse', runReuseFields],
      ['dockerPublishTimings', dockerPublishTimingsFields],
      ['dockerPublishConcurrency', dockerPublishConcurrencyFields],
      ['dockerPublication', dockerPublicationFields],
      ['runCpu', runCPUFields],
      ['runLocalCache', runLocalCacheFields],
      ['runCache', runCacheFields],
      ['runSummary', runSummaryFields],
      ['streamRunSummary', streamRunSummaryFields],
      ['cgroupThrottle', cgroupThrottleFields],
      ['cgroupCpu', cgroupCPUFields],
      ['cpuPressure', cpuPressureFields],
      ['hostCpuTime', hostCPUTimeFields],
      ['cgroupMemoryLimit', cgroupMemoryLimitFields],
      ['memoryCapacity', memoryCapacityFields],
      ['cgroupMemoryComposition', cgroupMemoryCompositionFields],
      ['cgroupMemoryLifetimePeak', cgroupMemoryLifetimePeakFields],
      ['cgroupMemoryClosing', cgroupMemoryClosingFields],
      ['cgroupMemoryEvents', cgroupMemoryEventsFields],
      ['cgroupMemory', cgroupMemoryFields],
      ['memoryPressure', memoryPressureFields],
      ['sessionTree', sessionTreeFields],
      ['sessionPlacement', sessionPlacementFields],
      ['sessionProvenance', sessionProvenanceFields],
      ['sessionEnvironment', sessionEnvironmentFields],
      ['preparationPhaseRecord', preparationPhaseRecordFields],
      ['sessionPreparation', sessionPreparationFields],
      ['machineOutputBudget', machineOutputBudgetFields],
      ['machineOutputElision', machineOutputElisionFields],
      ['machineOutputElisions', machineOutputElisionsFields],
      ['machineOutputArtifact', machineOutputArtifactFields],
      ['machineOutputSummary', machineOutputSummaryFields],
      ['planMetrics', planMetricsFields],
      ['plannedTask', plannedTaskFields],
      ['planSummary', planSummaryFields],
      ['resultEnvelope', resultEnvelopeFields],
      ['sessionStreamRecord', sessionStreamRecordFields],
      ['boundedSessionEndRecord', boundedSessionEndRecordFields],
      ['mcpResult', mcpResultFields],
      ['sessionFile', sessionFileFields],
      ['sessionPlanFile', sessionPlanFileFields],
      ['reportGit', reportGitFields],
      ['reportRun', reportRunFields],
      ['reportTests', reportTestsFields],
      ['reportCoverage', reportCoverageFields],
      ['reportCommand', reportCommandFields],
      ['reportJob', reportJobFields],
      ['reportCache', reportCacheFields],
      ['reportScheduler', reportSchedulerFields],
      ['reportFile', reportFileFields],
    ] as [string, Field[]][]
  ).map(([name, fields]) => [
    name,
    {
      members: fields.map((f) => f.name).sort(),
      required: fields
        .filter((f) => f.required)
        .map((f) => f.name)
        .sort(),
    },
  ]),
);

/** The members any document variant may carry. */
const VARIANT_MEMBERS = ['identity', 'event', 'task', 'testCase', 'run', 'plan'];

/**
 * Enforces a variant's member set: everything in `required` must be present, and
 * every other variant member must be absent.
 */
function requireMembers(v: Validator, obj: JsonObject | undefined, required: string[]): void {
  const need = new Set(required);
  for (const name of required) {
    if (!childValue(obj, name).present) {
      v.add(VIOLATION_CODE.missingField, name);
    }
  }
  for (const name of VARIANT_MEMBERS) {
    if (!need.has(name) && childValue(obj, name).present) {
      v.add(VIOLATION_CODE.unexpectedField, name);
    }
  }
}

/** Maps the envelope status onto the run outcome vocabulary. */
function envelopeOutcome(status: string): string {
  if (status === RUN_OUTCOME.success) {
    return RUN_OUTCOME.success;
  }
  if (status === RUN_OUTCOME.aborted) {
    return RUN_OUTCOME.aborted;
  }
  return RUN_OUTCOME.failure;
}

/** Only an aborted document carries the `signal` class, and it carries no other. */
function checkErrorClass(v: Validator, status: string, errObj: JsonObject): void {
  const code = childString(errObj, 'code');
  if (code === undefined) {
    return;
  }
  if ((status === RUN_OUTCOME.aborted) !== (code === 'signal')) {
    v.add(VIOLATION_CODE.errorMismatch, 'error.code');
  }
}

/**
 * Ties status, error and exitCode together. Version 2 reports an abort
 * DISTINCTLY (status `aborted`, class `signal`, code 130) instead of folding it
 * into `failure`, so the precedence costs the reader nothing: the failure counts
 * stay in `run.counts.failed` and `run.failures`.
 */
function checkEnvelopeVerdict(v: Validator, obj: JsonObject | undefined): void {
  const status = childString(obj, 'status');
  if (status === undefined) {
    return;
  }
  const { value: errValue, present: hasError } = childValue(obj, 'error');
  if (status === RUN_OUTCOME.success) {
    if (hasError) {
      v.add(VIOLATION_CODE.errorMismatch, 'error');
    }
  } else if (!hasError) {
    v.add(VIOLATION_CODE.missingField, 'error');
  }
  if (isObject(errValue)) {
    checkErrorClass(v, status, errValue);
  }
  checkExitCodeForOutcome(v, 'exitCode', envelopeOutcome(status), obj);
}

/** A job run's envelope and its run summary are one verdict, not two. */
function checkEnvelopeRunAgreement(v: Validator, obj: JsonObject | undefined): void {
  const run = childObject(obj, 'run');
  if (run === undefined) {
    return;
  }
  const status = childString(obj, 'status');
  const outcome = childString(run, 'outcome');
  if (status !== undefined && outcome !== undefined && outcome !== status) {
    v.add(VIOLATION_CODE.outcomeMismatch, 'run.outcome');
  }
  const envelopeCode = childInt(obj, 'exitCode');
  const runCode = childInt(run, 'exitCode');
  if (envelopeCode !== undefined && runCode !== undefined && envelopeCode !== runCode) {
    v.add(VIOLATION_CODE.exitCodeMismatch, 'run.exitCode');
  }
}

/** The members a stream record variant must carry. */
function streamMembersFor(record: string): string[] {
  if (record === STREAM_RECORD.planEnd) {
    return ['plan'];
  }
  if (record === STREAM_RECORD.sessionEnd) {
    return ['run'];
  }
  if (record === STREAM_RECORD.taskEvent) {
    return ['identity', 'event'];
  }
  if (record === STREAM_RECORD.taskEnd) {
    return ['identity', 'task'];
  }
  if (record === STREAM_RECORD.testCase) {
    return ['identity', 'testCase'];
  }
  return ['identity'];
}

/**
 * Enforces the one cross-member rule the physical ledger needs: a task's
 * `executionId` must name an execution the same document declares. A dangling
 * reference would silently drop a task's cost from the physical roll-up while
 * the record still looks attributed.
 *
 * The converse is deliberately NOT a violation: an execution nothing references
 * is real work (a superseded retry attempt), and dropping it would understate
 * the machine's cost.
 */
function checkExecutionReferences(v: Validator, obj: JsonObject | undefined): void {
  const tasks = childArray(obj, 'tasks');
  if (tasks === undefined || tasks.length === 0) {
    return;
  }
  const declared = new Set<string>();
  for (const item of childArray(obj, 'executions') ?? []) {
    if (!isObject(item)) {
      continue;
    }
    const id = childString(item, 'id');
    if (id !== undefined) {
      declared.add(id);
    }
  }
  for (const [i, item] of tasks.entries()) {
    if (!isObject(item)) {
      continue;
    }
    const id = childString(item, 'executionId');
    if (id === undefined || id === '' || declared.has(id)) {
      continue;
    }
    v.add(VIOLATION_CODE.invalidKey, join(`tasks[${i}]`, 'executionId'));
  }
}

/**
 * Refuses a record that names itself as its own parent. Self-parenting is never
 * a real nesting relation: it is the shape of a producer that inherited the
 * ambient parent id and then stamped it beside its own, and a consumer trusting
 * it would drop the top-level run out of its gate ledger entirely.
 */
function checkParentSessionReference(v: Validator, obj: JsonObject | undefined): void {
  const parent = childString(obj, 'parentSessionId');
  if (parent === undefined || parent === '') {
    return;
  }
  if (childString(obj, 'sessionId') === parent) {
    v.add(VIOLATION_CODE.invalidKey, 'parentSessionId');
  }
}

function validateResultEnvelope(v: Validator, root: JsonObject): void {
  const obj = object(v, '', root, resultEnvelopeFields);
  checkEnvelopeVerdict(v, obj);
  checkEnvelopeRunAgreement(v, obj);
  if (childValue(obj, 'plan').present) {
    for (const name of ['data', 'run', 'error']) {
      if (childValue(obj, name).present) {
        v.add(VIOLATION_CODE.unexpectedField, name);
      }
    }
    const status = childString(obj, 'status');
    if (status !== undefined && status !== RUN_OUTCOME.success) {
      v.add(VIOLATION_CODE.invalidValue, 'status');
    }
    const exitCode = childInt(obj, 'exitCode');
    if (exitCode !== undefined && exitCode !== 0) {
      v.add(VIOLATION_CODE.exitCodeMismatch, 'exitCode');
    }
  }
}

function validateSessionStreamRecord(v: Validator, root: JsonObject): void {
  const obj = object(v, '', root, sessionStreamRecordFields);
  const hasMachineOutput = childValue(obj, 'machineOutput').present;
  const run = childValue(obj, 'run');
  if (run.present) {
    (hasMachineOutput ? streamRunSummary : runSummary)(v, 'run', run.value);
  }
  const record = childString(obj, 'record');
  if (record === undefined) {
    return;
  }
  requireMembers(v, obj, streamMembersFor(record));
  if (record !== STREAM_RECORD.sessionEnd && hasMachineOutput) {
    v.add(VIOLATION_CODE.unexpectedField, 'machineOutput');
  }
  if (record === STREAM_RECORD.sessionEnd && hasMachineOutput) {
    nonEmptyStringBytesAtMost(64)(v, 'time', obj?.['time']);
  }
}

function validateMCPResult(v: Validator, root: JsonObject): void {
  const obj = object(v, '', root, mcpResultFields);
  const tool = childString(obj, 'tool');
  if (tool === undefined) {
    return;
  }
  requireMembers(v, obj, tool === MCP_TOOL.runJobs ? ['run'] : ['plan']);
}

const DOCUMENT_VALIDATORS: Record<DocumentKind, (v: Validator, root: JsonObject) => void> = {
  resultEnvelope: validateResultEnvelope,
  sessionStreamRecord: validateSessionStreamRecord,
  mcpResult: validateMCPResult,
  sessionFile: (v, root) => {
    const obj = object(v, '', root, sessionFileFields);
    checkExecutionReferences(v, obj);
    checkParentSessionReference(v, obj);
  },
  sessionPlanFile: (v, root) => {
    object(v, '', root, sessionPlanFileFields);
  },
  reportFile: (v, root) => {
    const obj = object(v, '', root, reportFileFields);
    checkReportJobBudget(v, obj);
    checkReportCommands(v, obj);
  },
};

/**
 * Validates one v2 machine document of the given kind and returns every
 * violation found, sorted by path then code. An empty array means the document
 * conforms.
 *
 * `document` is the raw JSON text, so a malformed line is reported rather than
 * thrown.
 */
export function validateDocument(kind: string, document: string): Violation[] {
  const v = new Validator();
  const validate = DOCUMENT_VALIDATORS[kind as DocumentKind];
  if (validate === undefined) {
    v.add(VIOLATION_CODE.invalidJson, '');
    return v.sorted();
  }
  let root: unknown;
  try {
    root = JSON.parse(document);
  } catch {
    v.add(VIOLATION_CODE.invalidJson, '');
    return v.sorted();
  }
  if (!isObject(root)) {
    v.add(VIOLATION_CODE.invalidJson, '');
    return v.sorted();
  }
  validate(v, root);
  const boundedSessionEnd = kind === DOCUMENT_KIND.sessionStreamRecord && childValue(root, 'machineOutput').present;
  if (boundedSessionEnd) {
    if (hasUnpairedJsonSurrogate(document)) {
      v.add(VIOLATION_CODE.unsanitized, '');
    } else {
      checkMachineOutputSanitized(v, '', root);
    }
  }
  if (
    boundedSessionEnd &&
    childString(root, 'record') === STREAM_RECORD.sessionEnd &&
    UTF8.encode(`${JSON.stringify(root)}\n`).length > MACHINE_OUTPUT_FINAL_RESERVE_BYTES
  ) {
    v.add(VIOLATION_CODE.budgetExceeded, 'machineOutput');
  }
  return v.sorted();
}

const MACHINE_OUTPUT_REDACTED = '[REDACTED]';
const MACHINE_OUTPUT_SENSITIVE_MEMBERS = new Set([
  'authorization',
  'proxyauthorization',
  'cookie',
  'setcookie',
  'token',
  'accesstoken',
  'refreshtoken',
  'password',
  'secret',
  'apikey',
  'clientsecret',
  'privatekey',
  'secretkey',
  'credential',
  'credentials',
]);

const MACHINE_OUTPUT_PRIVATE_KEY_HEADER = /-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----/;
const MACHINE_OUTPUT_TOKEN_PATTERNS = [
  /(?:AKIA|ASIA)[0-9A-Z]{16}/g,
  /gh[pousr]_[0-9A-Za-z]{36,255}/g,
  /github_pat_[0-9A-Za-z_]{20,255}/g,
  /AIza[0-9A-Za-z_-]{35}/g,
  /xox[baprs]-[0-9A-Za-z-]{10,}/g,
  /sk_(?:live|test)_[0-9A-Za-z]{16,}/g,
];

function normalizeSensitiveMember(name: string): string {
  return name.replace(/[-_.]/g, '').replace(/[A-Z]/g, (letter) => letter.toLowerCase());
}

/** Applies the string portion of machine-output sanitization v1. */
export function sanitizeMachineOutputString(value: string): string {
  let out = '';
  for (let i = 0; i < value.length; ) {
    const code = value.charCodeAt(i);
    if (code === 0x1b) {
      i = skipTerminalEscape(value, i);
      continue;
    }
    const point = value.codePointAt(i) ?? 0xff_fd;
    const width = point > 0xff_ff ? 2 : 1;
    if (point === 0x9b) {
      i = skipTerminalCsi(value, i + width);
      continue;
    }
    if ([0x90, 0x98, 0x9d, 0x9e, 0x9f].includes(point)) {
      i = skipTerminalString(value, i + width);
      continue;
    }
    if (
      (point >= 0xd8_00 && point <= 0xdf_ff) ||
      ((point < 0x20 || (point >= 0x7f && point <= 0x9f)) && point !== 0x09 && point !== 0x0a && point !== 0x0d)
    ) {
      out += '\uFFFD';
    } else {
      out += String.fromCodePoint(point);
    }
    i += width;
  }
  if (MACHINE_OUTPUT_PRIVATE_KEY_HEADER.test(out)) {
    return MACHINE_OUTPUT_REDACTED;
  }
  return MACHINE_OUTPUT_TOKEN_PATTERNS.reduce(
    (sanitized, pattern) => sanitized.replace(pattern, MACHINE_OUTPUT_REDACTED),
    out,
  );
}

function skipTerminalEscape(value: string, start: number): number {
  if (start + 1 >= value.length) {
    return value.length;
  }
  const next = value.charCodeAt(start + 1);
  if (next === 0x5b) {
    return skipTerminalCsi(value, start + 2);
  }
  if ([0x5d, 0x50, 0x58, 0x5e, 0x5f].includes(next)) {
    return skipTerminalString(value, start + 2);
  }
  return next >= 0x40 && next <= 0x5f ? start + 2 : start + 1;
}

function skipTerminalCsi(value: string, start: number): number {
  for (let i = start; i < value.length; i++) {
    const code = value.charCodeAt(i);
    if (code >= 0x40 && code <= 0x7e) {
      return i + 1;
    }
  }
  return value.length;
}

function skipTerminalString(value: string, start: number): number {
  for (let i = start; i < value.length; i++) {
    const code = value.charCodeAt(i);
    if (code === 0x07 || code === 0x9c) {
      return i + 1;
    }
    if (code === 0x1b && value.charCodeAt(i + 1) === 0x5c) {
      return i + 2;
    }
  }
  return value.length;
}

/**
 * Returns a recursively sanitized copy of decoded JSON string values. Object
 * member names are retained and normalized only for sensitive-member lookup.
 */
export function sanitizeMachineOutputValue(value: unknown): unknown {
  if (Array.isArray(value)) {
    return value.map(sanitizeMachineOutputValue);
  }
  if (isObject(value)) {
    return Object.fromEntries(
      Object.entries(value).map(([name, child]) => [
        name,
        MACHINE_OUTPUT_SENSITIVE_MEMBERS.has(normalizeSensitiveMember(name))
          ? MACHINE_OUTPUT_REDACTED
          : sanitizeMachineOutputValue(child),
      ]),
    );
  }
  return typeof value === 'string' ? sanitizeMachineOutputString(value) : value;
}

function checkMachineOutputSanitized(v: Validator, path: string, value: unknown): void {
  if (Array.isArray(value)) {
    for (const [index, child] of value.entries()) {
      checkMachineOutputSanitized(v, `${path}[${index}]`, child);
    }
    return;
  }
  if (isObject(value)) {
    for (const name of Object.keys(value).sort()) {
      const childPath = join(path, name);
      if (MACHINE_OUTPUT_SENSITIVE_MEMBERS.has(normalizeSensitiveMember(name))) {
        if (value[name] !== MACHINE_OUTPUT_REDACTED) {
          v.add(VIOLATION_CODE.unsanitized, childPath);
        }
      } else {
        checkMachineOutputSanitized(v, childPath, value[name]);
      }
    }
    return;
  }
  if (typeof value === 'string' && sanitizeMachineOutputString(value) !== value) {
    v.add(VIOLATION_CODE.unsanitized, path);
  }
}

/**
 * Reports whether a record consumes the protected failure/control-evidence
 * reserve. A `test:case` record never does: the failed task's `task:end` already
 * is.
 */
export function isMachineOutputFailurePriority(record: SessionStreamRecord): boolean {
  if (record.record === STREAM_RECORD.taskEnd) {
    return record.task?.status === TASK_STATUS.failed || record.task?.status === TASK_STATUS.canceled;
  }
  return record.record === STREAM_RECORD.taskEvent && failurePriorityEvent(record.event);
}

/**
 * Reports whether normal live output suppresses this debug-level task detail: a
 * `task:event` whose level is debug, or any `test:case`.
 */
export function isMachineOutputDebugDetail(record: SessionStreamRecord): boolean {
  if (record.record === STREAM_RECORD.testCase) {
    return true;
  }
  return record.record === STREAM_RECORD.taskEvent && childString(record.event, 'level') === 'debug';
}

function failurePriorityRecord(root: JsonObject): boolean {
  const record = childString(root, 'record');
  if (record === STREAM_RECORD.taskEnd) {
    const status = childString(childObject(root, 'task'), 'status');
    return status === TASK_STATUS.failed || status === TASK_STATUS.canceled;
  }
  return record === STREAM_RECORD.taskEvent && failurePriorityEvent(childObject(root, 'event'));
}

/**
 * {@link isMachineOutputDebugDetail} over a decoded record. The two must agree,
 * since whole-stream validation recomputes the producer's selection with this
 * one.
 */
function debugDetailRecord(root: JsonObject): boolean {
  const record = childString(root, 'record');
  if (record === STREAM_RECORD.testCase) {
    return true;
  }
  return record === STREAM_RECORD.taskEvent && childString(childObject(root, 'event'), 'level') === 'debug';
}

function failurePriorityEvent(event: JsonObject | undefined): boolean {
  if (event === undefined) {
    return false;
  }
  if (childString(event, 'level') === 'error') {
    return true;
  }
  switch (childString(event, 'type')) {
    case 'diagnostic':
      return childString(event, 'severity') === 'error';
    case 'phase':
      return childString(event, 'status') === 'failed';
    case 'result': {
      const result = childObject(event, 'data');
      const status = childString(result, 'status') ?? childString(event, 'status');
      if (status === 'FAILED') {
        return true;
      }
      const releaseSet = childObject(childObject(result, 'data'), 'releaseSet');
      return status === 'OK' && releaseSet !== undefined;
    }
    default:
      return false;
  }
}

interface MachineOutputLine {
  raw: string;
  root: JsonObject;
  bytes: number;
}

/**
 * Validates a bounded live JSONL sequence against the complete sanitized
 * `events.jsonl` session artifact, including total caps and exact elision.
 */
export function validateSessionStream(live: string, artifact: string): Violation[] {
  const v = new Validator();
  const liveLines = parseMachineOutputSequence(v, 'live', live);
  const artifactLines = parseMachineOutputSequence(v, 'artifact', artifact);
  const liveFinal = terminalMachineOutputLine(v, 'live', liveLines);
  const artifactFinal = terminalMachineOutputLine(v, 'artifact', artifactLines);
  if (liveFinal === undefined || artifactFinal === undefined) {
    return v.sorted();
  }
  if (liveFinal.raw !== artifactFinal.raw) {
    v.add(VIOLATION_CODE.elisionMismatch, 'stream.final');
  }
  const summary = childObject(liveFinal.root, 'machineOutput');
  const mode = childString(summary, 'mode');
  if (mode !== MACHINE_OUTPUT_MODE.normal && mode !== MACHINE_OUTPUT_MODE.verbose) {
    return v.sorted();
  }
  const budget = machineOutputBudgetFor(mode);
  if (UTF8.encode(live).length > budget.maxBytes) {
    v.add(VIOLATION_CODE.budgetExceeded, 'stream.bytes');
  }
  if (liveLines.length > budget.maxRecords) {
    v.add(VIOLATION_CODE.budgetExceeded, 'stream.records');
  }
  if (liveFinal.bytes > budget.finalReserveBytes) {
    v.add(VIOLATION_CODE.budgetExceeded, 'stream.final.bytes');
  }
  const selected = selectMachineOutput(artifactLines.slice(0, -1), mode, budget);
  const expectedLive = [...selected.lines, artifactFinal];
  if (!sameMachineOutputLines(liveLines, expectedLive)) {
    v.add(VIOLATION_CODE.elisionMismatch, 'stream.selection');
  }
  checkElision(v, summary, 'ordinary', selected.ordinaryElided);
  checkElision(v, summary, 'failure', selected.failureElided);
  return v.sorted();
}

function parseMachineOutputSequence(v: Validator, prefix: string, text: string): MachineOutputLine[] {
  if (text === '') {
    v.add(VIOLATION_CODE.missingField, `${prefix}.session:end`);
    return [];
  }
  if (!text.endsWith('\n')) {
    v.add(VIOLATION_CODE.invalidValue, `${prefix}.framing`);
  }
  const rawLines = (text.endsWith('\n') ? text.slice(0, -1) : text).split('\n');
  const lines: MachineOutputLine[] = [];
  rawLines.forEach((raw, index) => {
    const path = `${prefix}[${index}]`;
    if (raw === '') {
      v.add(VIOLATION_CODE.invalidJson, path);
      return;
    }
    if (hasJsonWhitespaceOutsideStrings(raw)) {
      v.add(VIOLATION_CODE.invalidValue, path);
    }
    const hasUnpairedSurrogate = hasUnpairedJsonSurrogate(raw);
    if (hasUnpairedSurrogate) {
      // Go's encoding/json replaces this while JSON.parse preserves it. Reject
      // the raw spelling so both bindings expose one wire rule.
      v.add(VIOLATION_CODE.unsanitized, path);
    }
    let root: unknown;
    try {
      root = JSON.parse(raw);
    } catch {
      v.add(VIOLATION_CODE.invalidJson, path);
      return;
    }
    if (!isObject(root)) {
      v.add(VIOLATION_CODE.invalidJson, path);
      return;
    }
    const local = new Validator();
    validateSessionStreamRecord(local, root);
    if (!hasUnpairedSurrogate) {
      checkMachineOutputSanitized(local, '', root);
    }
    for (const violation of local.sorted()) {
      v.add(violation.code, violation.path === '' ? path : `${path}.${violation.path}`);
    }
    lines.push({ raw, root, bytes: UTF8.encode(`${raw}\n`).length });
  });
  return lines;
}

function hasUnpairedJsonSurrogate(line: string): boolean {
  let inString = false;
  for (let i = 0; i < line.length; ) {
    if (!inString) {
      if (line.charCodeAt(i) === 0x22) {
        inString = true;
      }
      i++;
      continue;
    }
    const code = line.charCodeAt(i);
    if (code === 0x22) {
      inString = false;
      i++;
      continue;
    }
    if (code !== 0x5c) {
      i++;
      continue;
    }
    if (line.charCodeAt(i + 1) !== 0x75) {
      i += 2;
      continue;
    }
    const high = jsonHexCodeUnit(line.slice(i + 2, i + 6));
    if (high === undefined) {
      i += 2;
      continue;
    }
    if (high >= 0xdc_00 && high <= 0xdf_ff) {
      return true;
    }
    if (high < 0xd8_00 || high > 0xdb_ff) {
      i += 6;
      continue;
    }
    if (line.charCodeAt(i + 6) !== 0x5c || line.charCodeAt(i + 7) !== 0x75) {
      return true;
    }
    const low = jsonHexCodeUnit(line.slice(i + 8, i + 12));
    if (low === undefined || low < 0xdc_00 || low > 0xdf_ff) {
      return true;
    }
    i += 12;
  }
  return false;
}

function jsonHexCodeUnit(raw: string): number | undefined {
  return /^[0-9A-Fa-f]{4}$/.test(raw) ? Number.parseInt(raw, 16) : undefined;
}

function terminalMachineOutputLine(
  v: Validator,
  prefix: string,
  lines: MachineOutputLine[],
): MachineOutputLine | undefined {
  let terminal = -1;
  lines.forEach((line, index) => {
    if (childString(line.root, 'record') === STREAM_RECORD.sessionEnd) {
      if (terminal >= 0) {
        v.add(VIOLATION_CODE.invalidValue, `${prefix}[${index}].record`);
      }
      terminal = index;
    }
  });
  if (terminal < 0) {
    v.add(VIOLATION_CODE.missingField, `${prefix}.session:end`);
    return undefined;
  }
  if (terminal !== lines.length - 1) {
    v.add(VIOLATION_CODE.invalidValue, `${prefix}[${terminal}].record`);
    return undefined;
  }
  const final = lines[terminal];
  if (!childValue(final?.root, 'machineOutput').present) {
    v.add(VIOLATION_CODE.missingField, `${prefix}[${terminal}].machineOutput`);
    return undefined;
  }
  return final;
}

function selectMachineOutput(
  candidates: MachineOutputLine[],
  mode: MachineOutputMode,
  budget: MachineOutputBudget,
): {
  lines: MachineOutputLine[];
  ordinaryElided: MachineOutputElision;
  failureElided: MachineOutputElision;
} {
  let ordinaryBytes = budget.maxBytes - budget.failureReserveBytes - budget.finalReserveBytes;
  let ordinaryRecords = budget.maxRecords - budget.failureReserveRecords - budget.finalReserveRecords;
  let failureBytes = budget.failureReserveBytes;
  let failureRecords = budget.failureReserveRecords;
  const lines: MachineOutputLine[] = [];
  const ordinaryElided = { records: 0, bytes: 0 };
  const failureElided = { records: 0, bytes: 0 };
  for (const candidate of candidates) {
    if (failurePriorityRecord(candidate.root)) {
      if (failureRecords > 0 && candidate.bytes <= failureBytes) {
        lines.push(candidate);
        failureRecords--;
        failureBytes -= candidate.bytes;
      } else {
        failureElided.records++;
        failureElided.bytes += candidate.bytes;
      }
    } else if (mode === MACHINE_OUTPUT_MODE.normal && debugDetailRecord(candidate.root)) {
      ordinaryElided.records++;
      ordinaryElided.bytes += candidate.bytes;
    } else if (ordinaryRecords > 0 && candidate.bytes <= ordinaryBytes) {
      lines.push(candidate);
      ordinaryRecords--;
      ordinaryBytes -= candidate.bytes;
    } else {
      ordinaryElided.records++;
      ordinaryElided.bytes += candidate.bytes;
    }
  }
  return { lines, ordinaryElided, failureElided };
}

function sameMachineOutputLines(got: MachineOutputLine[], want: MachineOutputLine[]): boolean {
  return got.length === want.length && got.every((line, index) => line.raw === want[index]?.raw);
}

function checkElision(
  v: Validator,
  summary: JsonObject | undefined,
  category: 'ordinary' | 'failure',
  want: MachineOutputElision,
): void {
  const elision = childObject(childObject(summary, 'elided'), category);
  const records = childInt(elision, 'records');
  if (records !== undefined && records !== want.records) {
    v.add(VIOLATION_CODE.elisionMismatch, `stream.final.machineOutput.elided.${category}.records`);
  }
  const bytes = childInt(elision, 'bytes');
  if (bytes !== undefined && bytes !== want.bytes) {
    v.add(VIOLATION_CODE.elisionMismatch, `stream.final.machineOutput.elided.${category}.bytes`);
  }
}

function hasJsonWhitespaceOutsideStrings(line: string): boolean {
  let inString = false;
  let escaped = false;
  for (const char of line) {
    if (inString) {
      if (escaped) {
        escaped = false;
      } else if (char === '\\') {
        escaped = true;
      } else if (char === '"') {
        inString = false;
      }
    } else if (char === '"') {
      inString = true;
    } else if (char === ' ' || char === '\t' || char === '\r' || char === '\n') {
      return true;
    }
  }
  return false;
}
