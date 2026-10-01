import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { createEngagement, MAX_ENGAGEMENT_MS } from '../src/client/engagement';
import { type FakeBrowser, installFakeBrowser } from './utils/fake-browser';

describe('createEngagement', () => {
  let fake: FakeBrowser;

  beforeEach(() => {
    fake = installFakeBrowser();
  });

  afterEach(() => {
    fake.uninstall();
  });

  it('accumulates visible time and ignores hidden time', () => {
    const engagement = createEngagement();

    fake.clock = 3000;
    engagement.pause();
    // Twenty minutes in a background tab is not twenty minutes of reading.
    fake.clock = 1_200_000;
    engagement.resume();
    fake.clock = 1_202_500;

    expect(engagement.total()).toBe(5500);
  });

  it('reports the live slice without needing a pause', () => {
    const engagement = createEngagement();
    fake.clock = 1750;

    expect(engagement.total()).toBe(1750);
  });

  it('resumes only once, so a repeated visibilitychange cannot double-count', () => {
    const engagement = createEngagement();
    fake.clock = 1000;
    engagement.pause();
    fake.clock = 5000;
    engagement.resume();
    engagement.resume();
    fake.clock = 6000;

    expect(engagement.total()).toBe(2000);
  });

  it('pauses only once, so a repeated pagehide cannot double-count', () => {
    const engagement = createEngagement();
    fake.clock = 1000;
    engagement.pause();
    fake.clock = 9000;
    engagement.pause();

    expect(engagement.total()).toBe(1000);
  });

  it('starts a new view from zero on reset', () => {
    const engagement = createEngagement();
    fake.clock = 4000;
    engagement.reset();
    fake.clock = 4250;

    expect(engagement.total()).toBe(250);
  });

  it('caps at twenty-four hours', () => {
    const engagement = createEngagement();
    fake.clock = MAX_ENGAGEMENT_MS * 3;

    expect(engagement.total()).toBe(MAX_ENGAGEMENT_MS);
  });

  it('uses the wall clock when performance is unavailable', () => {
    (globalThis as Record<string, unknown>)['performance'] = undefined;
    const engagement = createEngagement();

    // Real elapsed milliseconds, so only the shape is assertable here.
    expect(engagement.total()).toBeGreaterThanOrEqual(0);
    expect(engagement.total()).toBeLessThan(MAX_ENGAGEMENT_MS);
  });
});
