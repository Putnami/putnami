import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { BOT_TOKENS, isBot } from '../src/server/enrich/bots';

// Each binding proves a requirement of the analytics protocol's feature
// under a check of this package's own.
const BOT_LIST = {
  feature: 'analytics/collection-wire-contract',
  requirement: 'bots-are-one-shared-list',
  check: 'the-typescript-bot-list-equals-the-fixture',
};
const ABSENT_AGENT = {
  feature: 'analytics/collection-wire-contract',
  requirement: 'bots-are-one-shared-list',
  check: 'the-typescript-filter-counts-an-absent-user-agent-as-a-bot',
};

const FIXTURES_DIR = join(__dirname, '../../../../protocols/analytics/fixtures');
const FEATURE = 'typescript/web-analytics-collection';

interface UserAgentFixture {
  ua: string;
  bot: boolean;
}

const BOTS = JSON.parse(readFileSync(join(FIXTURES_DIR, 'bots.json'), 'utf8')) as { tokens: string[] };
const USER_AGENTS = JSON.parse(readFileSync(join(FIXTURES_DIR, 'user-agents.json'), 'utf8')) as UserAgentFixture[];

describe('BOT_TOKENS', () => {
  // The array is inlined because a published package cannot reach protocols/ at
  // runtime; this is the test that keeps the copy equal to the contract.
  specTest('equals the protocol fixture, in order', BOT_LIST, () => {
    expect(BOT_TOKENS).toEqual(BOTS.tokens);
  });
});

describe('isBot', () => {
  specTest(
    'detects every bot in the shared corpus',
    { feature: FEATURE, requirement: 'bots-leave-no-trace', check: 'every-bot-fixture-is-detected' },
    () => {
      for (const fixture of USER_AGENTS) {
        expect(isBot(fixture.ua)).toBe(fixture.bot);
      }
    },
  );

  specTest('treats an absent or blank User-Agent as a bot', ABSENT_AGENT, () => {
    // A real browser always sends one, so the cheapest forgery must not be the
    // one that inflates the audience.
    expect(isBot(null)).toBe(true);
    expect(isBot('')).toBe(true);
    expect(isBot('   ')).toBe(true);
  });

  it('matches tokens case-insensitively anywhere in the string', () => {
    expect(isBot('SomeCrawlerBOT/1.0')).toBe(true);
    expect(isBot('Mozilla/5.0 (compatible; PetalBot;+https://aspiegel.com/petalbot)')).toBe(true);
    expect(isBot('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Version/17.5 Safari/605.1.15')).toBe(false);
  });
});
