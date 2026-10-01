import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/spectest';
import {
  DOCUMENT_KIND,
  derivedKey,
  isMachineOutputDebugDetail,
  isMachineOutputFailurePriority,
  MACHINE_OUTPUT_BUDGETS,
  MACHINE_OUTPUT_MODE,
  PREPARATION_PHASE,
  REPORT_MAX_JOB_DIAGNOSTICS,
  REPORT_MAX_JOBS,
  REPORT_MAX_MESSAGE_BYTES,
  REPORT_ORIGIN,
  type ReportTests,
  RUN_OUTCOME,
  type RunSummary,
  runSucceeded,
  type SessionFile,
  type SessionPlacement,
  type SessionProvenance,
  type SessionStreamRecord,
  STREAM_RECORD,
  sanitizeMachineOutputString,
  sanitizeMachineOutputValue,
  TASK_REUSE,
  TASK_SCOPE,
  TASK_STATUS,
  TEST_CASE_MAX_OUTPUT_BYTES,
  TEST_CASE_MAX_TEXT_BYTES,
  TEST_CASE_STATUS,
  type TestCase,
  VIOLATION_CODE,
  type Violation,
  validateDocument,
} from '../src/index';

// The cross-language corpus owns the contract's behavior. These tests cover the
// TypeScript-only edges it cannot express: an unknown document kind, malformed
// text, and the derived helpers consumers use.

const identity = {
  key: '/tooling/cli:build~compile',
  scope: TASK_SCOPE.project,
  project: { id: '/tooling/cli', name: '@putnami/cli' },
  task: { name: 'build~compile', command: 'build', step: 'compile', kind: 'go-build' },
  provider: { extension: '@putnami/go' },
};

describe('bounded machine-output helpers', () => {
  specTest(
    'returns only the two fixed mode budgets',
    {
      feature: 'typescript/cli-machine-output',
      requirement: 'bounded-stream-policy',
      check: 'the-two-mode-budgets-are-fixed',
    },
    () => {
      expect(MACHINE_OUTPUT_BUDGETS[MACHINE_OUTPUT_MODE.normal]).toEqual({
        maxBytes: 1_048_576,
        maxRecords: 1024,
        failureReserveBytes: 262_144,
        failureReserveRecords: 256,
        finalReserveBytes: 16_384,
        finalReserveRecords: 1,
      });
      expect(MACHINE_OUTPUT_BUDGETS[MACHINE_OUTPUT_MODE.verbose]).toEqual({
        maxBytes: 8_388_608,
        maxRecords: 8192,
        failureReserveBytes: 2_097_152,
        failureReserveRecords: 2048,
        finalReserveBytes: 16_384,
        finalReserveRecords: 1,
      });
    },
  );

  specTest(
    'sanitizes controls and values while retaining member names',
    {
      feature: 'typescript/cli-machine-output',
      requirement: 'sanitization',
      check: 'controls-strip-and-member-names-remain',
    },
    () => {
      expect(
        sanitizeMachineOutputValue({
          'Access-Token': 'synthetic',
          'label\0': 'member name retained',
          nested: { client_secret: 'synthetic', message: '\u001b[31mred\u001b[0m\0ok\u0085\t\n\r' },
        }),
      ).toEqual({
        'Access-Token': '[REDACTED]',
        'label\0': 'member name retained',
        nested: { client_secret: '[REDACTED]', message: 'red\uFFFDok\uFFFD\t\n\r' },
      });
      expect(sanitizeMachineOutputString('\u001b]8;;https://example.invalid\u0007help\u001b]8;;\u0007')).toBe('help');
      expect(sanitizeMachineOutputString('\u009b31mred\u009b0m')).toBe('red');
      expect(sanitizeMachineOutputString('\u009dtitle\u009chelp')).toBe('help');
      expect(sanitizeMachineOutputString('\ud800')).toBe('\uFFFD');
    },
  );

  specTest(
    'redacts every high-confidence credential family',
    {
      feature: 'typescript/cli-machine-output',
      requirement: 'sanitization',
      check: 'every-credential-family-is-redacted',
    },
    () => {
      const syntheticValues = [
        `AK${'IA'}${'A'.repeat(16)}`,
        `AS${'IA'}${'B'.repeat(16)}`,
        `gh${'p_'}${'a'.repeat(36)}`,
        `github${'_pat_'}${'b'.repeat(20)}`,
        `AI${'za'}${'c'.repeat(35)}`,
        `xo${'xb-'}${'d'.repeat(10)}`,
        `sk_${'live_'}${'e'.repeat(16)}`,
      ];
      for (const synthetic of syntheticValues) {
        expect(sanitizeMachineOutputString(`before ${synthetic} after`)).toBe('before [REDACTED] after');
      }
      expect(sanitizeMachineOutputString(`-----BEGIN ${'PRIVATE KEY-----'}\nsynthetic`)).toBe('[REDACTED]');
    },
  );

  specTest(
    'classifies the closed failure-priority vocabulary',
    {
      feature: 'typescript/cli-machine-output',
      requirement: 'bounded-stream-policy',
      check: 'failure-priority-is-classified',
    },
    () => {
      const eventRecord = (event: Record<string, unknown>): SessionStreamRecord => ({
        protocolVersion: 2,
        record: STREAM_RECORD.taskEvent,
        time: 't',
        identity,
        event,
      });
      expect(isMachineOutputFailurePriority(eventRecord({ level: 'error' }))).toBe(true);
      expect(isMachineOutputFailurePriority(eventRecord({ type: 'diagnostic', severity: 'error' }))).toBe(true);
      expect(isMachineOutputFailurePriority(eventRecord({ type: 'phase', status: 'failed' }))).toBe(true);
      expect(isMachineOutputFailurePriority(eventRecord({ type: 'result', data: { status: 'FAILED' } }))).toBe(true);
      expect(isMachineOutputFailurePriority(eventRecord({ type: 'result', status: 'FAILED' }))).toBe(true);
      expect(
        isMachineOutputFailurePriority(
          eventRecord({ v: 1, type: 'result', data: { status: 'OK', data: { releaseSet: {} } } }),
        ),
      ).toBe(true);
      expect(isMachineOutputFailurePriority(eventRecord({ v: 1, type: 'result', data: { status: 'OK' } }))).toBe(false);
      expect(isMachineOutputFailurePriority(eventRecord({ type: 'diagnostic', severity: 'warning' }))).toBe(false);
      expect(isMachineOutputDebugDetail(eventRecord({ type: 'log', level: 'debug' }))).toBe(true);
      expect(isMachineOutputDebugDetail(eventRecord({ type: 'log', level: 'info' }))).toBe(false);
      expect(
        isMachineOutputFailurePriority(eventRecord({ type: 'diagnostic', level: 'debug', severity: 'error' })),
      ).toBe(true);
      expect(isMachineOutputDebugDetail(eventRecord({ type: 'diagnostic', level: 'debug', severity: 'error' }))).toBe(
        true,
      );

      const taskRecord = (status: (typeof TASK_STATUS)[keyof typeof TASK_STATUS]): SessionStreamRecord => ({
        protocolVersion: 2,
        record: STREAM_RECORD.taskEnd,
        time: 't',
        identity,
        task: {
          identity,
          status,
          reuse: TASK_REUSE.none,
          exitCode: status === TASK_STATUS.failed ? 1 : 0,
          durationMs: 1,
        },
      });
      expect(isMachineOutputFailurePriority(taskRecord(TASK_STATUS.failed))).toBe(true);
      expect(isMachineOutputFailurePriority(taskRecord(TASK_STATUS.canceled))).toBe(true);
      expect(isMachineOutputFailurePriority(taskRecord(TASK_STATUS.success))).toBe(false);

      // Every test case is debug detail, and none uses the failure reserve: the
      // failed task's task:end already does.
      const testCaseRecord = (status: TestCase['status']): SessionStreamRecord => ({
        protocolVersion: 2,
        record: STREAM_RECORD.testCase,
        time: 't',
        identity,
        testCase: { name: 'TestA', suite: 'example.com/a', status, durationMs: 1 },
      });
      expect(isMachineOutputDebugDetail(testCaseRecord(TEST_CASE_STATUS.passed))).toBe(true);
      expect(isMachineOutputDebugDetail(testCaseRecord(TEST_CASE_STATUS.skipped))).toBe(true);
      expect(isMachineOutputDebugDetail(testCaseRecord(TEST_CASE_STATUS.failed))).toBe(true);
      const { testCase: _, ...withoutPayload } = testCaseRecord(TEST_CASE_STATUS.failed);
      expect(isMachineOutputDebugDetail(withoutPayload)).toBe(true);
      expect(isMachineOutputFailurePriority(testCaseRecord(TEST_CASE_STATUS.failed))).toBe(false);
    },
  );
});

describe('validateDocument malformed input', () => {
  it('rejects an unknown document kind', () => {
    expect(validateDocument('planFile', '{}')).toEqual([{ code: VIOLATION_CODE.invalidJson, path: '' }]);
  });

  it.each(['{"protocolVersion":2', '2', 'null', '[]', '{"a":1} {}'])('rejects %p as a document', (text) => {
    expect(validateDocument(DOCUMENT_KIND.resultEnvelope, text)).toEqual([
      { code: VIOLATION_CODE.invalidJson, path: '' },
    ]);
  });
});

describe('validateDocument container types', () => {
  it('rejects a non-object identity', () => {
    const document = JSON.stringify({
      protocolVersion: 2,
      record: 'task:start',
      time: 't',
      identity: '/tooling/cli:build~compile',
    });
    expect(validateDocument(DOCUMENT_KIND.sessionStreamRecord, document)).toEqual([
      { code: VIOLATION_CODE.invalidType, path: 'identity' },
    ]);
  });

  it('rejects a non-object runtime event payload', () => {
    const document = JSON.stringify({ protocolVersion: 2, record: 'task:event', time: 't', identity, event: [] });
    expect(validateDocument(DOCUMENT_KIND.sessionStreamRecord, document)).toEqual([
      { code: VIOLATION_CODE.invalidType, path: 'event' },
    ]);
  });

  it('rejects a non-array command list and a non-object metric map', () => {
    const plan = { dryRun: true, metrics: { tasks: 0, edges: 0, projects: 0, byCommand: [] }, tasks: [] };
    const document = JSON.stringify({ protocolVersion: 2, tool: 'plan_jobs', commands: 'build', plan });
    expect(validateDocument(DOCUMENT_KIND.mcpResult, document)).toEqual([
      { code: VIOLATION_CODE.invalidType, path: 'commands' },
      { code: VIOLATION_CODE.invalidType, path: 'plan.metrics.byCommand' },
    ]);
  });

  it('rejects a non-integer metric value and a non-boolean cache flag', () => {
    const document = JSON.stringify({
      protocolVersion: 2,
      sessionId: 's',
      commands: ['build'],
      tasks: [{ identity, cache: 'yes' }],
    });
    expect(validateDocument(DOCUMENT_KIND.sessionPlanFile, document)).toEqual([
      { code: VIOLATION_CODE.invalidType, path: 'tasks[0].cache' },
    ]);
  });

  // The contract's integer rule: any JSON number carrying an integral value in
  // IEEE-754's safe range conforms; anything fractional or beyond it does not —
  // identically to the Go validator, whatever the wire encoding.
  it.each([
    ['1', true],
    ['1.0', true],
    ['1e2', true],
    ['9007199254740991', true], // 2^53 - 1, the largest safe integer
    ['1.5', false],
    ['9007199254740993', false], // above the safe range
    ['1e300', false],
  ] as [string, boolean][])('durationMs %s conforms: %p', (duration, valid) => {
    const document =
      '{"protocolVersion":2,"record":"session:end","time":"t","run":{"outcome":"success","exitCode":0,' +
      '"counts":{"total":0,"succeeded":0,"failed":0,"canceled":0,"skipped":0},' +
      `"reuse":{"localCache":0,"remoteCache":0,"coalesced":0},"durationMs":${duration}}}`;
    expect(validateDocument(DOCUMENT_KIND.sessionStreamRecord, document)).toEqual(
      valid ? [] : [{ code: VIOLATION_CODE.invalidType, path: 'run.durationMs' }],
    );
  });

  it('validates local cache attribution on session:end', () => {
    const run = {
      outcome: 'success',
      exitCode: 0,
      counts: { total: 1, succeeded: 1, failed: 0, canceled: 0, skipped: 0 },
      reuse: { localCache: 1, remoteCache: 0, coalesced: 0 },
      durationMs: 100,
      cache: {
        local: {
          hits: 1,
          misses: 0,
          servedMs: 90,
          keysMs: 30,
          bindingsMs: 40,
          restoreVerifyMs: 20,
          spawnedProcesses: 4,
        },
      },
    };
    const record = (value: unknown) =>
      JSON.stringify({ protocolVersion: 2, record: 'session:end', time: 't', run: value });
    expect(validateDocument(DOCUMENT_KIND.sessionStreamRecord, record(run))).toEqual([]);
    expect(
      validateDocument(
        DOCUMENT_KIND.sessionStreamRecord,
        record({
          ...run,
          cache: { local: { ...run.cache.local, hits: 0 } },
        }),
      ),
    ).toEqual([{ code: VIOLATION_CODE.countMismatch, path: 'run.cache.local.hits' }]);
    expect(
      validateDocument(
        DOCUMENT_KIND.sessionStreamRecord,
        record({
          ...run,
          cache: { local: { ...run.cache.local, servedMs: 101, keysMs: 39 } },
        }),
      ),
    ).toEqual([{ code: VIOLATION_CODE.countMismatch, path: 'run.cache.local.servedMs' }]);
    expect(
      validateDocument(
        DOCUMENT_KIND.sessionStreamRecord,
        record({
          ...run,
          cache: {
            local: {
              ...run.cache.local,
              keysMs: 30,
              bindingsMs: 50,
              restoreVerifyMs: 20,
            },
          },
        }),
      ),
    ).toEqual([{ code: VIOLATION_CODE.countMismatch, path: 'run.cache.local.servedMs' }]);
    expect(
      validateDocument(
        DOCUMENT_KIND.sessionStreamRecord,
        record({
          ...run,
          cache: {
            local: {
              ...run.cache.local,
              keysMs: 29,
              bindingsMs: 38,
              restoreVerifyMs: 20,
            },
          },
        }),
      ),
    ).toEqual([{ code: VIOLATION_CODE.countMismatch, path: 'run.cache.local.servedMs' }]);
    expect(
      validateDocument(
        DOCUMENT_KIND.sessionStreamRecord,
        record({
          ...run,
          cache: {
            local: {
              ...run.cache.local,
              keysMs: 29,
              bindingsMs: 39,
              restoreVerifyMs: 20,
            },
          },
        }),
      ),
    ).toEqual([]);
  });

  it('reports a non-object session git block', () => {
    const run = {
      outcome: 'success',
      exitCode: 0,
      counts: { total: 0, succeeded: 0, failed: 0, canceled: 0, skipped: 0 },
      reuse: { localCache: 0, remoteCache: 0, coalesced: 0 },
      durationMs: 0,
    };
    const document = JSON.stringify({
      protocolVersion: 2,
      sessionId: 's',
      startTime: 't',
      commands: ['build'],
      git: 'main',
      run,
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
      { code: VIOLATION_CODE.invalidType, path: 'git' },
    ]);
  });
});

describe('physical execution ledger', () => {
  const run = {
    outcome: RUN_OUTCOME.success,
    exitCode: 0,
    counts: { total: 1, succeeded: 1, failed: 0, canceled: 0, skipped: 0 },
    reuse: { localCache: 0, remoteCache: 0, coalesced: 0 },
    durationMs: 3000,
  };
  const task = { identity, status: 'success', reuse: 'none', exitCode: 0, durationMs: 1500 };
  const execution = { id: 'exec-000001', wallMs: 3000, userCpuMs: 4200, systemCpuMs: 610, tasks: 2 };

  const sessionFile = (overrides: Record<string, unknown>) =>
    JSON.stringify({ protocolVersion: 2, sessionId: 's', startTime: 't', commands: ['lint'], run, ...overrides });

  it('accepts a task with no execution — reuse spends nothing physical', () => {
    expect(validateDocument(DOCUMENT_KIND.sessionFile, sessionFile({ tasks: [task] }))).toEqual([]);
  });

  it('accepts several logical records sharing one physical execution', () => {
    const document = sessionFile({
      tasks: [
        { ...task, executionId: execution.id },
        { ...task, executionId: execution.id },
      ],
      executions: [execution],
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([]);
  });

  it('rejects a task referencing an execution the document does not declare', () => {
    const document = sessionFile({
      tasks: [{ ...task, executionId: 'exec-000009' }],
      executions: [execution],
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
      { code: VIOLATION_CODE.invalidKey, path: 'tasks[0].executionId' },
    ]);
  });
});

describe('gated tree fingerprint', () => {
  const run = {
    outcome: RUN_OUTCOME.success,
    exitCode: 0,
    counts: { total: 1, succeeded: 1, failed: 0, canceled: 0, skipped: 0 },
    reuse: { localCache: 0, remoteCache: 0, coalesced: 0 },
    durationMs: 3000,
  };
  const fingerprint = '4f3d0d6f0c1ba6c8c4b9a2e7d6c5b4a39281706f5e4d3c2b1a0f9e8d7c6b5a49';
  const headSHA = 'ad048a6b3c0d1e2f30415263748596a7b8c9d0e1';

  const sessionFile = (overrides: Record<string, unknown>) =>
    JSON.stringify({
      protocolVersion: 2,
      sessionId: 's',
      startTime: 't',
      commands: ['lint', 'test', 'build', 'validate'],
      run,
      ...overrides,
    });

  it('accepts a session that names the tree it ran against', () => {
    const document = sessionFile({ tree: { fingerprint, dirty: true, headSHA } });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([]);
  });

  // Additive by absence: every session already on disk predates the block.
  it('accepts a session that records no tree at all', () => {
    expect(validateDocument(DOCUMENT_KIND.sessionFile, sessionFile({}))).toEqual([]);
  });

  it('rejects an abbreviated or uppercase fingerprint', () => {
    for (const spelling of [fingerprint.slice(0, 16), fingerprint.toUpperCase()]) {
      const document = sessionFile({ tree: { fingerprint: spelling, dirty: false, headSHA } });
      expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
        { code: VIOLATION_CODE.invalidValue, path: 'tree.fingerprint' },
      ]);
    }
  });

  it('rejects a symbolic head', () => {
    const document = sessionFile({ tree: { fingerprint, dirty: false, headSHA: 'HEAD' } });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
      { code: VIOLATION_CODE.invalidValue, path: 'tree.headSHA' },
    ]);
  });

  // A present block always knows all three facts, because one computation
  // produces them together. "Unknown" is spelled by omitting the whole object.
  it('rejects a tree block that omits the dirty verdict', () => {
    const document = sessionFile({ tree: { fingerprint, headSHA } });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
      { code: VIOLATION_CODE.missingField, path: 'tree.dirty' },
    ]);
  });
});

describe('session execution placement', () => {
  specTest(
    'records requested and actual placement without changing the session tree',
    {
      feature: 'typescript/cli-machine-output',
      requirement: 'execution-placement',
      check: 'placement-is-optional-and-preserves-fallback',
    },
    () => {
      const session: SessionFile = {
        protocolVersion: 2,
        sessionId: 's',
        startTime: 't',
        commands: ['test'],
        tree: {
          fingerprint: '4f3d0d6f0c1ba6c8c4b9a2e7d6c5b4a39281706f5e4d3c2b1a0f9e8d7c6b5a49',
          headSHA: 'ad048a6b3c0d1e2f30415263748596a7b8c9d0e1',
          dirty: true,
        },
        run: {
          outcome: RUN_OUTCOME.success,
          exitCode: 0,
          counts: { total: 1, succeeded: 1, failed: 0, canceled: 0, skipped: 0 },
          reuse: { localCache: 0, remoteCache: 0, coalesced: 0 },
          durationMs: 3000,
        },
      };
      expect(validateDocument(DOCUMENT_KIND.sessionFile, JSON.stringify(session))).toEqual([]);
      expect(JSON.parse(JSON.stringify(session))).not.toHaveProperty('placement');
      const placement: SessionPlacement = { requested: 'remote', actual: 'local' };
      const document = JSON.stringify({ ...session, placement } satisfies SessionFile);
      expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([]);
      const parsed = JSON.parse(document) as SessionFile;
      expect(parsed.placement).toEqual(placement);
      expect(parsed.tree).toEqual(session.tree);
    },
  );

  // Provenance is the executing engine's statement of the bound request it
  // ran: complete or absent, spelled like the runner contract's digests, and
  // only ever beside a remote execution. The shared corpus pins the exact
  // verdicts; this keeps the typed shape honest.
  it('carries complete execution provenance only beside a remote execution', () => {
    const provenance: SessionProvenance = {
      sourceDigest: `sha256:${'ab'.repeat(32)}`,
      inputDigest: `sha256:${'cd'.repeat(32)}`,
      submission: 'ef'.repeat(16),
    };
    const base: SessionFile = {
      protocolVersion: 2,
      sessionId: 's',
      startTime: 't',
      commands: ['test'],
      run: {
        outcome: RUN_OUTCOME.success,
        exitCode: 0,
        counts: { total: 1, succeeded: 1, failed: 0, canceled: 0, skipped: 0 },
        reuse: { localCache: 0, remoteCache: 0, coalesced: 0 },
        durationMs: 3000,
      },
    };
    const remote: SessionFile = { ...base, placement: { requested: 'remote', actual: 'remote', provenance } };
    expect(validateDocument(DOCUMENT_KIND.sessionFile, JSON.stringify(remote))).toEqual([]);
    expect((JSON.parse(JSON.stringify(remote)) as SessionFile).placement?.provenance).toEqual(provenance);
    const fallback = { ...base, placement: { requested: 'remote', actual: 'local', provenance } };
    expect(validateDocument(DOCUMENT_KIND.sessionFile, JSON.stringify(fallback))).toEqual([
      { code: VIOLATION_CODE.invalidValue, path: 'placement.provenance' },
    ]);
    const partial = {
      ...base,
      placement: { requested: 'remote', actual: 'remote', provenance: { sourceDigest: provenance.sourceDigest } },
    };
    expect(validateDocument(DOCUMENT_KIND.sessionFile, JSON.stringify(partial))).toEqual([
      { code: VIOLATION_CODE.missingField, path: 'placement.provenance.inputDigest' },
      { code: VIOLATION_CODE.missingField, path: 'placement.provenance.submission' },
    ]);
    const bare = {
      ...base,
      placement: {
        requested: 'remote',
        actual: 'remote',
        provenance: { ...provenance, sourceDigest: 'ab'.repeat(32) },
      },
    };
    expect(validateDocument(DOCUMENT_KIND.sessionFile, JSON.stringify(bare))).toEqual([
      { code: VIOLATION_CODE.invalidValue, path: 'placement.provenance.sourceDigest' },
    ]);
  });
});

describe('runner environment and CPU balance', () => {
  const run = (overrides: Record<string, unknown> = {}) => ({
    outcome: RUN_OUTCOME.success,
    exitCode: 0,
    counts: { total: 1, succeeded: 1, failed: 0, canceled: 0, skipped: 0 },
    reuse: { localCache: 0, remoteCache: 0, coalesced: 0 },
    durationMs: 3500,
    ...overrides,
  });

  const sessionFile = (overrides: Record<string, unknown>) =>
    JSON.stringify({
      protocolVersion: 2,
      sessionId: 's',
      startTime: 't',
      commands: ['build'],
      run: run(),
      ...overrides,
    });

  it('accepts a session with no environment at all — the block is additive', () => {
    expect(validateDocument(DOCUMENT_KIND.sessionFile, sessionFile({}))).toEqual([]);
  });

  it('accepts a darwin capture that measured only what exists', () => {
    const document = sessionFile({
      environment: { os: 'darwin', arch: 'arm64', logicalCpus: 12, cpuModel: 'Apple M2 Max', windowMs: 3520 },
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([]);
  });

  it('accepts a throttle block whose counters are a measured zero', () => {
    const document = sessionFile({
      environment: {
        os: 'linux',
        arch: 'amd64',
        logicalCpus: 16,
        windowMs: 3520,
        cgroupCpu: {
          periodUs: 100_000,
          quotaUs: 800_000,
          usageUs: 22_140_000,
          throttle: { periods: 35, throttledPeriods: 0, throttledUs: 0 },
        },
        cpuPressure: { someStalledUs: 0 },
        hostCpu: { stealTicks: 0, ioWaitTicks: 0, totalTicks: 2816 },
      },
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([]);
  });

  it('accepts closing memory gauges and windowed deltas whose measured value is zero', () => {
    const document = sessionFile({
      environment: {
        os: 'linux',
        arch: 'amd64',
        logicalCpus: 16,
        windowMs: 3520,
        memoryCapacity: {
          physicalBytes: 32 * 1024 * 1024 * 1024,
          cgroupLimit: { bytes: 16 * 1024 * 1024 * 1024, source: 'cgroup-v2' },
          effectiveBytes: 16 * 1024 * 1024 * 1024,
          effectiveSource: 'cgroup-limit',
        },
        cgroupMemory: {
          source: 'cgroup-v2',
          closing: {
            currentBytes: 0,
            composition: { anonBytes: 0, fileBytes: 0, shmemBytes: 0 },
            lifetimePeak: { bytes: 0 },
          },
          events: { low: 0, high: 0, max: 0, oom: 0, oomKill: 0 },
        },
        memoryPressure: { scope: 'cgroup', someStalledUs: 0, fullStalledUs: 0 },
      },
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([]);
  });

  it('keeps cgroup v1 fallback exact instead of relabelling v2 facts', () => {
    const base = {
      os: 'linux',
      arch: 'amd64',
      logicalCpus: 16,
      windowMs: 3520,
      cgroupMemory: {
        source: 'cgroup-v1',
        closing: { currentBytes: 1024, lifetimePeak: { bytes: 2048 } },
      },
      memoryPressure: { scope: 'host', someStalledUs: 0, fullStalledUs: 0 },
    };
    expect(validateDocument(DOCUMENT_KIND.sessionFile, sessionFile({ environment: base }))).toEqual([]);

    const document = sessionFile({
      environment: {
        ...base,
        cgroupMemory: {
          source: 'cgroup-v1',
          closing: {
            currentBytes: 1024,
            composition: { anonBytes: 0, fileBytes: 0, shmemBytes: 0 },
          },
          events: { low: 0, high: 1, max: 0, oom: 0, oomKill: 0 },
        },
      },
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
      { code: VIOLATION_CODE.unexpectedField, path: 'environment.cgroupMemory.closing.composition' },
      { code: VIOLATION_CODE.unexpectedField, path: 'environment.cgroupMemory.events' },
    ]);
  });

  it('rejects an unsafe memory byte count before JavaScript can round it', () => {
    const document = sessionFile({
      environment: {
        os: 'linux',
        arch: 'amd64',
        logicalCpus: 16,
        windowMs: 3520,
        memoryCapacity: {
          cgroupLimit: { bytes: 1024, source: 'cgroup-v2' },
          effectiveBytes: Number.MAX_SAFE_INTEGER + 1,
          effectiveSource: 'cgroup-limit',
        },
      },
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
      { code: VIOLATION_CODE.invalidType, path: 'environment.memoryCapacity.effectiveBytes' },
    ]);
  });

  it('rejects memory subset counters that exceed their enclosing totals', () => {
    const document = sessionFile({
      environment: {
        os: 'linux',
        arch: 'amd64',
        logicalCpus: 16,
        windowMs: 3520,
        cgroupMemory: {
          source: 'cgroup-v2',
          closing: {
            currentBytes: 1024,
            composition: { anonBytes: 1024, fileBytes: 128, shmemBytes: 256 },
          },
        },
        memoryPressure: { scope: 'cgroup', someStalledUs: 10, fullStalledUs: 11 },
      },
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
      {
        code: VIOLATION_CODE.countMismatch,
        path: 'environment.cgroupMemory.closing.composition.shmemBytes',
      },
      { code: VIOLATION_CODE.countMismatch, path: 'environment.memoryPressure.fullStalledUs' },
    ]);
  });

  it('rejects a cgroup quota of zero — unlimited is spelled by omitting it', () => {
    const document = sessionFile({
      environment: {
        os: 'linux',
        arch: 'amd64',
        logicalCpus: 16,
        windowMs: 3520,
        cgroupCpu: { periodUs: 100_000, quotaUs: 0 },
      },
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
      { code: VIOLATION_CODE.invalidValue, path: 'environment.cgroupCpu.quotaUs' },
    ]);
  });

  it('rejects a /proc/stat column wider than the window it was sampled over', () => {
    const document = sessionFile({
      environment: {
        os: 'linux',
        arch: 'amd64',
        logicalCpus: 16,
        windowMs: 3520,
        hostCpu: { stealTicks: 900, ioWaitTicks: 500, totalTicks: 404 },
      },
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
      { code: VIOLATION_CODE.countMismatch, path: 'environment.hostCpu.ioWaitTicks' },
      { code: VIOLATION_CODE.countMismatch, path: 'environment.hostCpu.stealTicks' },
    ]);
  });

  it('ties allocatedMs to the run wall it is stated against', () => {
    const cpu = {
      allocatedMillicores: 8000,
      allocatedSource: 'cgroup-quota',
      allocatedMs: 28_001,
      actualMs: 18_000,
      executions: 1,
    };
    expect(validateDocument(DOCUMENT_KIND.sessionFile, sessionFile({ run: run({ cpu }) }))).toEqual([
      { code: VIOLATION_CODE.countMismatch, path: 'run.cpu.allocatedMs' },
    ]);
    const exact = { ...cpu, allocatedMs: 28_000 };
    expect(validateDocument(DOCUMENT_KIND.sessionFile, sessionFile({ run: run({ cpu: exact }) }))).toEqual([]);
  });

  it('accepts a run that burned more CPU than it was allocated', () => {
    const cpu = {
      allocatedMillicores: 8000,
      allocatedSource: 'cgroup-quota',
      allocatedMs: 28_000,
      actualMs: 33_000,
      executions: 4,
    };
    expect(validateDocument(DOCUMENT_KIND.sessionFile, sessionFile({ run: run({ cpu }) }))).toEqual([]);
  });
});

describe('dependency-preparation attribution', () => {
  const sessionFile = (overrides: Record<string, unknown>) =>
    JSON.stringify({
      protocolVersion: 2,
      sessionId: 's',
      startTime: 't',
      commands: ['build'],
      run: {
        outcome: RUN_OUTCOME.success,
        exitCode: 0,
        counts: { total: 1, succeeded: 1, failed: 0, canceled: 0, skipped: 0 },
        reuse: { localCache: 0, remoteCache: 0, coalesced: 0 },
        durationMs: 12_000,
      },
      ...overrides,
    });

  it('accepts a session with no preparation at all — the block is additive', () => {
    expect(validateDocument(DOCUMENT_KIND.sessionFile, sessionFile({}))).toEqual([]);
  });

  it('lets phase spans overlap when the stage ran them in parallel', () => {
    const document = sessionFile({
      preparation: {
        wallMs: 4200,
        parallelism: 4,
        phases: [
          { phase: PREPARATION_PHASE.resolution, wallMs: 640, steps: 8 },
          { phase: PREPARATION_PHASE.verification, wallMs: 310, steps: 9, cpuMs: 120 },
          { phase: PREPARATION_PHASE.generation, wallMs: 14_800, steps: 4, cpuMs: 51_200 },
          { phase: PREPARATION_PHASE.mutation, wallMs: 720, steps: 13 },
        ],
      },
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([]);
  });

  it('rejects serial phases that do not fit the stage wall they claim', () => {
    const document = sessionFile({
      preparation: {
        wallMs: 4200,
        parallelism: 1,
        phases: [
          { phase: PREPARATION_PHASE.resolution, wallMs: 640, steps: 8 },
          { phase: PREPARATION_PHASE.generation, wallMs: 14_800, steps: 4, cpuMs: 51_200 },
        ],
      },
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
      { code: VIOLATION_CODE.countMismatch, path: 'preparation.wallMs' },
    ]);
  });

  it('rejects a repeated ownership class — the list is a histogram, not a log', () => {
    const document = sessionFile({
      preparation: {
        wallMs: 4200,
        parallelism: 4,
        phases: [
          { phase: PREPARATION_PHASE.generation, wallMs: 3000, steps: 1, cpuMs: 9000 },
          { phase: PREPARATION_PHASE.generation, wallMs: 2800, steps: 1, cpuMs: 8400 },
        ],
      },
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
      { code: VIOLATION_CODE.countMismatch, path: 'preparation.phases[1].phase' },
    ]);
  });

  it('rejects a sixth ownership class', () => {
    const document = sessionFile({
      preparation: { wallMs: 4200, parallelism: 2, phases: [{ phase: 'bookkeeping', wallMs: 10, steps: 1 }] },
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
      { code: VIOLATION_CODE.invalidEnum, path: 'preparation.phases[0].phase' },
    ]);
  });

  it('rejects a zero-step phase — a class never entered is ABSENT, not zero', () => {
    const document = sessionFile({
      preparation: {
        wallMs: 4200,
        parallelism: 2,
        phases: [{ phase: PREPARATION_PHASE.network, wallMs: 0, steps: 0 }],
      },
    });
    expect(validateDocument(DOCUMENT_KIND.sessionFile, document)).toEqual([
      { code: VIOLATION_CODE.invalidValue, path: 'preparation.phases[0].steps' },
    ]);
  });
});

describe('validateDocument determinism', () => {
  specTest(
    'sorts violations by path, then code',
    {
      feature: 'typescript/cli-machine-output',
      requirement: 'deterministic-violations',
      check: 'violations-sort-by-path-then-code',
    },
    () => {
      expect(validateDocument(DOCUMENT_KIND.sessionPlanFile, '{"zzz":1,"protocolVersion":1,"aaa":2}')).toEqual([
        { code: VIOLATION_CODE.unknownField, path: 'aaa' },
        { code: VIOLATION_CODE.missingField, path: 'commands' },
        { code: VIOLATION_CODE.invalidProtocolVersion, path: 'protocolVersion' },
        { code: VIOLATION_CODE.missingField, path: 'sessionId' },
        { code: VIOLATION_CODE.missingField, path: 'tasks' },
        { code: VIOLATION_CODE.unknownField, path: 'zzz' },
      ]);
    },
  );

  specTest(
    'orders paths by code unit, never by locale',
    {
      feature: 'typescript/cli-machine-output',
      requirement: 'deterministic-violations',
      check: 'paths-compare-by-code-unit',
    },
    () => {
      const document = JSON.stringify({
        protocolVersion: 2,
        sessionId: 's',
        commands: ['build'],
        tasks: [],
        Zfield: 1,
        afield: 2,
      });
      expect(validateDocument(DOCUMENT_KIND.sessionPlanFile, document)).toEqual([
        { code: VIOLATION_CODE.unknownField, path: 'Zfield' },
        { code: VIOLATION_CODE.unknownField, path: 'afield' },
      ]);
    },
  );

  specTest(
    'indexes array elements above nine',
    {
      feature: 'typescript/cli-machine-output',
      requirement: 'deterministic-violations',
      check: 'array-indices-order-numerically',
    },
    () => {
      const commands = ['a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', ''];
      const document = JSON.stringify({ protocolVersion: 2, sessionId: 's', commands, tasks: [] });
      expect(validateDocument(DOCUMENT_KIND.sessionPlanFile, document)).toEqual([
        { code: VIOLATION_CODE.invalidValue, path: 'commands[11]' },
      ]);
    },
  );
});

describe('v2 derivations', () => {
  it('derives the identity key from the structured fields', () => {
    expect(derivedKey(identity)).toBe(identity.key);
  });

  specTest(
    'reports the strict unified verdict',
    {
      feature: 'typescript/cli-machine-output',
      requirement: 'strict-verdict',
      check: 'the-strict-verdict-gates-run-success',
    },
    () => {
      const summary: RunSummary = {
        outcome: RUN_OUTCOME.success,
        exitCode: 0,
        counts: { total: 4, succeeded: 4, failed: 0, canceled: 0, skipped: 0 },
        reuse: { localCache: 3, remoteCache: 1, coalesced: 0 },
        durationMs: 10,
      };
      expect(runSucceeded(summary)).toBe(true);

      // A reused failure sinks the run: the rule is strict, not lenient.
      expect(runSucceeded({ ...summary, counts: { ...summary.counts, succeeded: 3, failed: 1 } })).toBe(false);

      // And an abort is never green, whatever the counts say.
      expect(runSucceeded({ ...summary, outcome: RUN_OUTCOME.aborted, abortedBy: 'user' })).toBe(false);
    },
  );
});

// The report document's BOUNDS.
//
// The cross-language corpus owns the report's semantics — every cross-field rule
// it carries has an accept and a reject fixture in conformance/manifest.json,
// executed identically by this package and by Go. These tests cover the one
// class of clause a corpus cannot hold honestly: the caps themselves. Pinning
// "65 jobs is too many" as a fixture would put 65 near-identical rows in a file
// whose whole value is that a reviewer can read it, so the documents are BUILT
// here from the exported constants — exactly as the Go mirror
// (result_v2_report_test.go) builds them from its own, which the schema pins to
// these.

const reportJob = (index: number) => ({
  key: `/p:build~t${index}`,
  project: '/p',
  task: `build~t${index}`,
  command: 'build',
  outcome: 'success',
  reuse: 'none',
  durationMs: index,
});

const reportFile = (jobCount: number, elidedJobs: number, overrides: Record<string, unknown> = {}) => {
  const jobs = Array.from({ length: jobCount }, (_, index) => reportJob(index));
  const total = jobCount + elidedJobs;
  const counts = { total, succeeded: total, failed: 0, canceled: 0, skipped: 0 };
  const reuse = { localCache: 0, remoteCache: 0, coalesced: 0 };
  return JSON.stringify({
    protocolVersion: 2,
    sessionId: '20260807-091500-abc123',
    startTime: '2026-08-07T09:15:00.000Z',
    endTime: '2026-08-07T09:15:26.000Z',
    origin: REPORT_ORIGIN.cli,
    enforceCoverage: true,
    run: { outcome: RUN_OUTCOME.success, exitCode: 0, counts, reuse, durationMs: 26_000 },
    commands: [{ command: 'build', counts, reuse, freshWallMs: 0, errors: 0, warnings: 0 }],
    jobs,
    elidedJobs,
    ...overrides,
  });
};

describe('report bounds', () => {
  it('accepts optional producer omission accounting and rejects a negative count', () => {
    const tests: ReportTests = { total: 1, passed: 0, failed: 1, skipped: 0, failureDetailsTruncated: 3 };
    const document = JSON.parse(reportFile(1, 0)) as Record<string, unknown>;
    const commands = document['commands'] as Record<string, unknown>[];
    commands[0]['tests'] = tests;
    expect(validateDocument(DOCUMENT_KIND.reportFile, JSON.stringify(document))).toEqual([]);

    tests.failureDetailsTruncated = -1;
    expect(validateDocument(DOCUMENT_KIND.reportFile, JSON.stringify(document))).toEqual([
      { code: VIOLATION_CODE.invalidValue, path: 'commands[0].tests.failureDetailsTruncated' },
    ]);
  });

  it.each([
    ['a full list carries the cap', REPORT_MAX_JOBS, 0, []],
    ['a full list may elide the rest', REPORT_MAX_JOBS, 681, []],
    ['a short list elides nothing', 3, 0, []],
    ['past the cap', REPORT_MAX_JOBS + 1, 0, [{ code: VIOLATION_CODE.invalidValue, path: 'jobs' }]],
    [
      'eliding before the budget is full',
      REPORT_MAX_JOBS - 1,
      12,
      [{ code: VIOLATION_CODE.countMismatch, path: 'jobs' }],
    ],
  ] as [string, number, number, Violation[]][])('%s', (_name, jobCount, elided, expected) => {
    expect(validateDocument(DOCUMENT_KIND.reportFile, reportFile(jobCount, elided))).toEqual(expected);
  });

  it('requires the elided count to account for the whole run', () => {
    // One task accounted for by neither the list nor the count.
    const document = reportFile(REPORT_MAX_JOBS, 681).replace('"elidedJobs":681', '"elidedJobs":680');
    expect(validateDocument(DOCUMENT_KIND.reportFile, document)).toEqual([
      { code: VIOLATION_CODE.countMismatch, path: 'elidedJobs' },
    ]);
  });

  // The budget is spent in BYTES, so the cap is stated in bytes: "é" is two
  // UTF-8 bytes, so half the cap in code points is exactly the cap in bytes and
  // one code point more is over it. The schema's maxLength counts code points
  // and is deliberately the weaker check.
  it.each([
    [REPORT_MAX_MESSAGE_BYTES / 2, []],
    [REPORT_MAX_MESSAGE_BYTES / 2 + 1, [{ code: VIOLATION_CODE.invalidValue, path: 'jobs[0].diagnostics[0].message' }]],
  ] as [number, Violation[]][])('bounds a %p-code-point message by its bytes', (runes, expected) => {
    const jobs = [{ ...reportJob(0), diagnostics: [{ severity: 'warning', message: 'é'.repeat(runes) }] }];
    expect(validateDocument(DOCUMENT_KIND.reportFile, reportFile(1, 0, { jobs }))).toEqual(expected);
  });

  it.each([
    ['a full list may report what it dropped', REPORT_MAX_JOB_DIAGNOSTICS, 123, 0, []],
    ['a short complete list', 3, 0, 0, []],
    [
      'past the cap',
      REPORT_MAX_JOB_DIAGNOSTICS + 1,
      0,
      0,
      [{ code: VIOLATION_CODE.invalidValue, path: 'jobs[0].diagnostics' }],
    ],
    [
      'a truncation count must be positive',
      3,
      -1,
      0,
      [{ code: VIOLATION_CODE.invalidValue, path: 'jobs[0].truncatedCount' }],
    ],
    ['a short list may report producer omissions', 3, 9, 9, []],
    [
      'a short list cannot hide report omissions',
      3,
      9,
      3,
      [{ code: VIOLATION_CODE.countMismatch, path: 'jobs[0].truncatedCount' }],
    ],
    [
      'producer omissions cannot exceed the total',
      3,
      3,
      4,
      [{ code: VIOLATION_CODE.countMismatch, path: 'jobs[0].truncatedCount' }],
    ],
    [
      'producer omissions require a total',
      3,
      0,
      4,
      [{ code: VIOLATION_CODE.countMismatch, path: 'jobs[0].truncatedCount' }],
    ],
    [
      'producer omissions must be positive',
      3,
      0,
      -1,
      [{ code: VIOLATION_CODE.invalidValue, path: 'jobs[0].failureDetailsTruncated' }],
    ],
  ] as [
    string,
    number,
    number,
    number,
    Violation[],
  ][])('%s', (_name, count, truncatedCount, failureDetailsTruncated, expected) => {
    const diagnostics = Array.from({ length: count }, (_, i) => ({ severity: 'warning', message: `w${i}` }));
    const job: Record<string, unknown> = { ...reportJob(0), diagnostics };
    if (truncatedCount !== 0) {
      job['truncatedCount'] = truncatedCount;
    }
    if (failureDetailsTruncated !== 0) {
      job['failureDetailsTruncated'] = failureDetailsTruncated;
    }
    expect(validateDocument(DOCUMENT_KIND.reportFile, reportFile(1, 0, { jobs: [job] }))).toEqual(expected);
  });
});

describe('test-case bounds', () => {
  const testCaseRecord = (testCase: Record<string, unknown>): string =>
    JSON.stringify({ protocolVersion: 2, record: STREAM_RECORD.testCase, time: 't', identity, testCase });
  const failed = { name: 'TestA', suite: 'example.com/a', status: TEST_CASE_STATUS.failed, durationMs: 1 };

  // The bounds are spent in BYTES: "é" is two UTF-8 bytes, so half a bound in
  // code points is exactly the bound in bytes and one code point more is over
  // it. The schema's maxLength counts code points and is the weaker check.
  it.each([
    ['name', TEST_CASE_MAX_TEXT_BYTES],
    ['suite', TEST_CASE_MAX_TEXT_BYTES],
    ['file', TEST_CASE_MAX_TEXT_BYTES],
    ['output', TEST_CASE_MAX_OUTPUT_BYTES],
  ] as [string, number][])('bounds %s by its UTF-8 bytes', (member, bound) => {
    const withMember = (runes: number): string => testCaseRecord({ ...failed, [member]: 'é'.repeat(runes) });
    expect(validateDocument(DOCUMENT_KIND.sessionStreamRecord, withMember(bound / 2))).toEqual([]);
    expect(validateDocument(DOCUMENT_KIND.sessionStreamRecord, withMember(bound / 2 + 1))).toEqual([
      { code: VIOLATION_CODE.invalidValue, path: `testCase.${member}` },
    ]);
  });

  it('rejects an empty output rather than reading it as absent', () => {
    expect(validateDocument(DOCUMENT_KIND.sessionStreamRecord, testCaseRecord({ ...failed, output: '' }))).toEqual([
      { code: VIOLATION_CODE.invalidValue, path: 'testCase.output' },
    ]);
  });
});
