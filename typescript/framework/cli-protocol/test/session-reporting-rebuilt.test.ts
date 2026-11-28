import { expect } from 'bun:test';
import { specTest } from '@putnami/spectest';
import {
  parseSessionReportingAck,
  SESSION_REPORTING_PACKAGES_VERSION,
  SESSION_REPORTING_REBUILT_PACKAGES_VERSION,
} from '../src/index';

const requirement = {
  feature: 'cli/machine-contract',
  requirement: 'the-reporting-wire',
  check: 'the-typescript-reporter-corpus-and-identity',
};

const ack = (members: string): string =>
  `{"protocolVersion":1,"sessionId":"session","artifact":"plan.json","offset":0,"sequence":0,"sha256":"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881","final":false,"ok":true${members}}`;

specTest('an acknowledgement offers rebuilt packages beside packages only, as in Go', requirement, () => {
  const offered = parseSessionReportingAck(ack(',"packages":true,"packagesRebuilt":true'));
  expect(offered.packages).toBe(true);
  expect(offered.packagesRebuilt).toBe(true);
  expect(parseSessionReportingAck(ack(',"packages":true')).packagesRebuilt).toBeUndefined();
  expect(() => parseSessionReportingAck(ack(',"packages":true,"packagesRebuilt":false'))).toThrow();
  expect(() => parseSessionReportingAck(ack(',"packagesRebuilt":true'))).toThrow();
});

specTest(
  'an engine that reads rebuilt packages names framing version 5, past the packages version, as in Go',
  requirement,
  () => {
    expect(SESSION_REPORTING_PACKAGES_VERSION).toBe(4);
    expect(SESSION_REPORTING_REBUILT_PACKAGES_VERSION).toBe(5);
  },
);
