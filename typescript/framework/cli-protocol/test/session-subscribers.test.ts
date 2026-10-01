import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  parseSessionSubscribersFile,
  SESSION_SUBSCRIBERS_MAX,
  SESSION_SUBSCRIBERS_SCHEMA_ID,
  SESSION_SUBSCRIBERS_VERSION,
  SUBSCRIBER_EVIDENCE,
  type SessionStreamPosition,
  type SessionSubscriberEvidence,
  type SessionSubscribersFile,
} from '../src/index';

const root = join(__dirname, '../../../../protocols/cli');
const corpus = JSON.parse(readFileSync(join(root, 'conformance/session-subscribers.json'), 'utf8')) as {
  name: string;
  valid: boolean;
  document: unknown;
}[];
const schema = JSON.parse(readFileSync(join(root, 'schemas/session-subscribers.json'), 'utf8'));

test('subscriber evidence shares the Go corpus', () => {
  expect(corpus.some((item) => item.valid)).toBe(true);
  expect(corpus.some((item) => !item.valid)).toBe(true);
  for (const item of corpus) {
    let valid = true;
    try {
      parseSessionSubscribersFile(JSON.stringify(item.document));
    } catch {
      valid = false;
    }
    expect({ name: item.name, valid }).toEqual({ name: item.name, valid: item.valid });
  }
});

test('subscriber evidence member sets, bounds and vocabulary follow the schema', () => {
  expect(schema.$id).toBe(SESSION_SUBSCRIBERS_SCHEMA_ID);
  const file = parseSessionSubscribersFile(JSON.stringify(corpus.find((item) => item.name === 'delivered')?.document));
  const documentMembers: Required<SessionSubscribersFile> = file;
  const subscriber: Required<SessionSubscriberEvidence> | undefined = file.subscribers[0];
  const stream: Required<SessionStreamPosition> = file.stream;
  const defs = [
    ['document', documentMembers],
    ['evidence', subscriber],
    ['position', stream],
  ] as const;
  for (const [name, value] of defs) {
    const pinned = Object.keys(value ?? {}).sort();
    expect({ name, members: pinned }).toEqual({ name, members: Object.keys(schema.$defs[name].properties).sort() });
    expect({ name, required: [...schema.$defs[name].required].sort() }).toEqual({ name, required: pinned });
  }
  expect(schema.$defs.document.properties.subscribers.maxItems).toBe(SESSION_SUBSCRIBERS_MAX);
  expect(schema.$defs.document.properties.protocolVersion.const).toBe(SESSION_SUBSCRIBERS_VERSION);
  expect(schema.$defs.evidence.properties.evidence.enum).toEqual(Object.values(SUBSCRIBER_EVIDENCE));
});
