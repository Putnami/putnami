import { afterEach, describe, expect, it } from 'bun:test';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  buildEventsInfraManifest,
  INFRA_PROTOCOL_VERSION,
  INFRA_SCHEMA_URL,
  InvalidTopicNameError,
  infraSidecarPath,
  removeInfraSidecar,
  writeInfraSidecar,
} from '../src/infra/requirements';
import { clearPublishedTopics, getPublishedTopics, getPublisher } from '../src/publisher/publisher';
import { topic } from '../src/topic/topic';

describe('buildEventsInfraManifest', () => {
  it('returns undefined when there are no topics', () => {
    expect(buildEventsInfraManifest([], [])).toBeUndefined();
  });

  it('emits a publish-only manifest', () => {
    const manifest = buildEventsInfraManifest(['order.created'], []);
    expect(manifest).toEqual({
      $schema: INFRA_SCHEMA_URL,
      protocolVersion: INFRA_PROTOCOL_VERSION,
      events: { publishes: ['order.created'], subscribes: [] },
    });
  });

  it('emits a subscribe-only manifest', () => {
    const manifest = buildEventsInfraManifest([], ['inventory.reserved']);
    expect(manifest?.events).toEqual({ publishes: [], subscribes: ['inventory.reserved'] });
  });

  it('emits a mixed publish + subscribe manifest', () => {
    const manifest = buildEventsInfraManifest(['notification.sent'], ['inventory.reserved']);
    expect(manifest?.events).toEqual({
      publishes: ['notification.sent'],
      subscribes: ['inventory.reserved'],
    });
  });

  it('deduplicates and sorts topic names deterministically', () => {
    const manifest = buildEventsInfraManifest(['b.topic', 'a.topic', 'b.topic'], ['z.topic', 'z.topic', 'a.topic']);
    expect(manifest?.events).toEqual({
      publishes: ['a.topic', 'b.topic'],
      subscribes: ['a.topic', 'z.topic'],
    });
  });

  it('emits bare-string subscribes for the default pull delivery', () => {
    const manifest = buildEventsInfraManifest([], ['inventory.reserved'], 'pull');
    expect(manifest?.events.subscribes).toEqual(['inventory.reserved']);
  });

  it('emits { topic, delivery } objects for push delivery', () => {
    const manifest = buildEventsInfraManifest([], ['inventory.reserved', 'order.created'], 'push');
    expect(manifest?.events.subscribes).toEqual([
      { topic: 'inventory.reserved', delivery: 'push' },
      { topic: 'order.created', delivery: 'push' },
    ]);
  });

  it('throws InvalidTopicNameError on a name that violates the resource pattern', () => {
    expect(() => buildEventsInfraManifest(['OrderCreated'], [])).toThrow(InvalidTopicNameError);
    expect(() => buildEventsInfraManifest([], ['has space'])).toThrow(InvalidTopicNameError);
  });

  it('reports every offending name in the diagnostic', () => {
    try {
      buildEventsInfraManifest(['Bad.One'], ['also Bad']);
      throw new Error('expected InvalidTopicNameError');
    } catch (error) {
      expect(error).toBeInstanceOf(InvalidTopicNameError);
      expect((error as InvalidTopicNameError).names.sort()).toEqual(['Bad.One', 'also Bad']);
    }
  });
});

describe('published-topic registry', () => {
  afterEach(() => clearPublishedTopics());

  it('records a topic when getPublisher is invoked', () => {
    clearPublishedTopics();
    getPublisher(topic('user.created', { id: String }));
    expect(getPublishedTopics()).toEqual(['user.created']);
  });

  it('dedups repeated publishers for the same topic', () => {
    clearPublishedTopics();
    const t = topic('order.placed', { id: String });
    getPublisher(t);
    getPublisher(t);
    expect(getPublishedTopics()).toEqual(['order.placed']);
  });

  it('clears recorded topics', () => {
    getPublisher(topic('cleared.topic', { id: String }));
    clearPublishedTopics();
    expect(getPublishedTopics()).toEqual([]);
  });
});

describe('writeInfraSidecar', () => {
  const dirs: string[] = [];
  afterEach(() => {
    for (const dir of dirs) {
      rmSync(dir, { recursive: true, force: true });
    }
    dirs.length = 0;
  });

  it('writes the manifest to .gen/infra/events.json', async () => {
    const root = mkdtempSync(join(tmpdir(), 'events-infra-'));
    dirs.push(root);

    const manifest = buildEventsInfraManifest(['order.created'], ['inventory.reserved']);
    expect(manifest).toBeDefined();

    const path = await writeInfraSidecar(root, manifest!);
    expect(path).toBe(infraSidecarPath(root));

    const written = await Bun.file(path).json();
    expect(written).toEqual(manifest!);
  });
});

describe('removeInfraSidecar', () => {
  const dirs: string[] = [];
  afterEach(() => {
    for (const dir of dirs) {
      rmSync(dir, { recursive: true, force: true });
    }
    dirs.length = 0;
  });

  it('removes an existing sidecar', async () => {
    const root = mkdtempSync(join(tmpdir(), 'events-infra-rm-'));
    dirs.push(root);

    const path = await writeInfraSidecar(root, buildEventsInfraManifest(['order.created'], [])!);
    expect(await Bun.file(path).exists()).toBe(true);

    const removed = await removeInfraSidecar(root);
    expect(removed).toBe(infraSidecarPath(root));
    expect(await Bun.file(path).exists()).toBe(false);
  });

  it('is a no-op when no sidecar exists', async () => {
    const root = mkdtempSync(join(tmpdir(), 'events-infra-rm-missing-'));
    dirs.push(root);

    await expect(removeInfraSidecar(root)).resolves.toBe(infraSidecarPath(root));
  });
});
