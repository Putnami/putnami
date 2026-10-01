import { describe, expect, it } from 'bun:test';
import {
  attr,
  attrsFromStrings,
  digest,
  gauge,
  histogram,
  histogramDataPoint,
  intVal,
  logRecord,
  logsRequest,
  marshalCanonical,
  metric,
  metricsRequest,
  numberDataPoint,
  resourceFromStrings,
  resourceLogs,
  resourceMetrics,
  resourceSpans,
  scope,
  scopeLogs,
  scopeMetrics,
  scopeSpans,
  sortAttrs,
  span,
  spanStatus,
  stringVal,
  sum,
  TEMPORALITY_DELTA,
  tracesRequest,
} from '../../src/telemetry/otlp';

// These digests are pinned by the Go reference renderer in
// protocols/telemetry/equivalence_test.go. Building the same input here
// and matching the digest proves the TypeScript renderer emits byte-identical
// OTLP/JSON. If either side changes the encoding, regenerate the goldens
// (UPDATE_GOLDEN=1 go test ./...) and update both in lockstep.
const GOLDEN_METRICS_DIGEST = '554e2e93797529779aad0d8bceca426c2f91096712544e097568a88436236c03';
const GOLDEN_TRACES_DIGEST = '7517ef04da08f3fefce56eb9f547ff2fec7c9bde46dbeec285939175221b9845';
const GOLDEN_LOGS_DIGEST = 'acb22c6a4901668d1f010ac8b02eed2a8f3c0338766b7f9ed975a3309bc88d47';

const START = '1699999999000000000';
const TIME = '1700000000000000000';

describe('cross-language OTLP equivalence', () => {
  it('metrics envelope matches the Go golden digest', () => {
    const res = resourceFromStrings({
      'putnami.framework': 'go',
      'service.name': 'checkout',
      'service.version': '1.4.2',
    });
    const sc = scope('putnami', '1.0.0');

    const httpDuration = metric({
      name: 'http.duration',
      unit: 'ms',
      histogram: histogram(
        [
          histogramDataPoint({
            startTimeUnixNano: START,
            timeUnixNano: TIME,
            count: '2',
            sum: 30,
            bucketCounts: ['2'],
            min: 10,
            max: 20,
          }),
        ],
        TEMPORALITY_DELTA,
      ),
    });
    const httpRequests = metric({
      name: 'http.requests',
      unit: '1',
      sum: sum(
        [
          numberDataPoint({
            attributes: attrsFromStrings({ method: 'GET' }),
            startTimeUnixNano: START,
            timeUnixNano: TIME,
            asInt: '5',
          }),
        ],
        TEMPORALITY_DELTA,
        true,
      ),
    });
    const queueSize = metric({
      name: 'queue.size',
      gauge: gauge([numberDataPoint({ timeUnixNano: TIME, asDouble: 3 })]),
    });

    const req = metricsRequest([resourceMetrics(res, [scopeMetrics(sc, [httpDuration, httpRequests, queueSize])])]);
    expect(digest(marshalCanonical(req))).toBe(GOLDEN_METRICS_DIGEST);
  });

  it('traces envelope matches the Go golden digest', () => {
    const res = resourceFromStrings({ 'putnami.framework': 'go', 'service.name': 'checkout' });
    const sc = scope('putnami', '1.0.0');

    const serverSpan = span({
      traceId: '5b8efff798038103d269b633813fc60c',
      spanId: 'eee19b7ec3c1b174',
      name: 'GET /checkout',
      kind: 2,
      startTimeUnixNano: '1700000000000000000',
      endTimeUnixNano: '1700000000125000000',
      attributes: sortAttrs([attr('http.method', stringVal('GET')), attr('http.status_code', intVal(200))]),
      status: spanStatus(1),
    });
    const clientSpan = span({
      traceId: '5b8efff798038103d269b633813fc60c',
      spanId: 'f0e1d2c3b4a59687',
      parentSpanId: 'eee19b7ec3c1b174',
      name: 'SELECT carts',
      kind: 3,
      startTimeUnixNano: '1700000000010000000',
      endTimeUnixNano: '1700000000040000000',
      status: spanStatus(2, 'deadline exceeded'),
    });

    const req = tracesRequest([resourceSpans(res, [scopeSpans(sc, [serverSpan, clientSpan])])]);
    expect(digest(marshalCanonical(req))).toBe(GOLDEN_TRACES_DIGEST);
  });

  it('logs envelope matches the Go golden digest', () => {
    const res = resourceFromStrings({ 'putnami.framework': 'go', 'service.name': 'checkout' });
    const sc = scope('putnami', '1.0.0');

    const infoRecord = logRecord({
      timeUnixNano: '1700000000000000000',
      observedTimeUnixNano: '1700000000000000000',
      severityNumber: 9,
      severityText: 'info',
      body: stringVal('request handled'),
      attributes: attrsFromStrings({ route: '/checkout' }),
    });
    const errorRecord = logRecord({
      timeUnixNano: '1700000000500000000',
      severityNumber: 17,
      severityText: 'error',
      body: stringVal('payment failed'),
      attributes: attrsFromStrings({ 'error.code': 'card_declined' }),
      traceId: '5b8efff798038103d269b633813fc60c',
      spanId: 'eee19b7ec3c1b174',
    });

    const req = logsRequest([resourceLogs(res, [scopeLogs(sc, [infoRecord, errorRecord])])]);
    expect(digest(marshalCanonical(req))).toBe(GOLDEN_LOGS_DIGEST);
  });
});
