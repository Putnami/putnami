import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/spectest';
import {
  isResult,
  isResultError,
  RESULT_ERROR_CODE,
  RESULT_SCHEMA_ID,
  RESULT_STATUS,
  type Result,
  type ResultError,
} from '../src/index';

// The canonical contract is the JSON schema in protocols/cli, also implemented
// in Go (protocols/cli/result.go, guarded by drift_test.go). Loading it here and
// asserting the hand-authored TypeScript types against it keeps
// @putnami/cli-protocol from silently drifting away from the shared envelope.
const SCHEMA_PATH = join(__dirname, '../../../../protocols/cli/schemas/result.json');

interface JsonSchema {
  $id: string;
  required: string[];
  properties: Record<string, { enum?: string[] }>;
  $defs: {
    resultError: {
      required: string[];
      properties: Record<string, { enum?: string[] }>;
    };
  };
}

const schema = JSON.parse(readFileSync(SCHEMA_PATH, 'utf8')) as JsonSchema;

describe('CLI result-envelope protocol conformance', () => {
  it('agrees with the schema $id constant', () => {
    expect(schema.$id).toBe(RESULT_SCHEMA_ID);
  });

  it('Result fields match the schema properties', () => {
    // Required<Result> forces every field (including the optional data/error) to
    // be present, so adding a field to the interface fails to compile until it
    // is listed here and, in turn, added to the schema.
    const sample: Required<Result> = {
      command: 'build',
      status: 'success',
      data: null,
      error: { code: 'failure', message: 'x' },
      exitCode: 0,
    };
    expect(Object.keys(sample).sort()).toEqual(Object.keys(schema.properties).sort());
  });

  it('ResultError fields match the schema $defs/resultError', () => {
    const sample: Required<ResultError> = { code: 'failure', message: 'boom', next: 'putnami build --help' };
    expect(Object.keys(sample).sort()).toEqual(Object.keys(schema.$defs.resultError.properties).sort());
  });

  it('status enum matches the schema', () => {
    expect(Object.values(RESULT_STATUS).sort()).toEqual([...(schema.properties['status'].enum ?? [])].sort());
  });

  it('error code enum matches the schema', () => {
    expect(Object.values(RESULT_ERROR_CODE).sort()).toEqual(
      [...(schema.$defs.resultError.properties['code'].enum ?? [])].sort(),
    );
  });

  it('required fields cover the taxonomy minimum', () => {
    expect(schema.required.sort()).toEqual(['command', 'exitCode', 'status']);
    expect(schema.$defs.resultError.required.sort()).toEqual(['code', 'message']);
  });
});

describe('result guards', () => {
  specTest(
    'accepts a valid success result',
    {
      feature: 'typescript/cli-machine-output',
      requirement: 'version-token',
      check: 'the-version-1-envelope-stays-exported',
    },
    () => {
      // The version-1 envelope has no protocolVersion field at all: recorded
      // v1 output stays readable through the exported guard.
      expect(isResult({ command: 'build', status: 'success', data: { ok: true }, exitCode: 0 })).toBe(true);
    },
  );

  it('accepts a valid failure result with an error', () => {
    expect(
      isResult({
        command: 'cloud status',
        status: 'failure',
        error: { code: 'auth', message: 'expired' },
        exitCode: 3,
      }),
    ).toBe(true);
  });

  it('rejects malformed results', () => {
    expect(isResult(null)).toBe(false);
    expect(isResult({ status: 'success', exitCode: 0 })).toBe(false); // missing command
    expect(isResult({ command: 'x', status: 'done', exitCode: 0 })).toBe(false); // bad status
    expect(isResult({ command: 'x', status: 'success', exitCode: '0' })).toBe(false); // exitCode not a number
    expect(isResult({ command: 'x', status: 'failure', exitCode: 1, error: { code: 'nope', message: 'm' } })).toBe(
      false,
    ); // bad error code
  });

  it('validates result errors', () => {
    expect(isResultError({ code: 'usage', message: 'bad flag' })).toBe(true);
    expect(isResultError({ code: 'usage', message: 'bad flag', next: 'putnami build --help' })).toBe(true);
    expect(isResultError({ code: 'nope', message: 'm' })).toBe(false);
    expect(isResultError({ code: 'api' })).toBe(false); // missing message
    expect(isResultError({ code: 'api', message: 'm', next: 42 })).toBe(false); // bad next
  });
});
