import { describe, expect, it } from 'bun:test';
import { cloudTelemetryTokenSource } from '@putnami/cloud/runtime';
import { getTelemetryTokenSource } from '@putnami/runtime';
import '../src/activate';

describe('activate', () => {
  // The workspace collector refuses a push without a Google ID token (HTTP 403).
  // The source has to land in the same @putnami/runtime the telemetry plugin reads.
  it('registers the Cloud telemetry token source in the runtime the application uses', () => {
    expect(getTelemetryTokenSource()).toBe(cloudTelemetryTokenSource);
  });
});
