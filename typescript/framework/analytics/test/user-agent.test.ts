import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { classifyUserAgent, MAX_USER_AGENT_LEN } from '../src/server/enrich/user-agent';
import { BROWSERS, DEVICE_TYPES, OPERATING_SYSTEMS } from '../src/server/sanitize/vocabulary';

// The classifier lives here, but its expectations are the shared corpus of
// protocols/analytics: the Go side proves every expected value is a closed-enum
// member, and this test proves the classification itself.
const FIXTURES_DIR = join(__dirname, '../../../../protocols/analytics/fixtures');

interface UserAgentFixture {
  ua: string;
  browser: string;
  browserMajor: number | null;
  os: string;
  deviceType: string;
  bot: boolean;
}

const FIXTURES = JSON.parse(readFileSync(join(FIXTURES_DIR, 'user-agents.json'), 'utf8')) as UserAgentFixture[];

describe('classifyUserAgent', () => {
  it('reads the whole shared corpus', () => {
    expect(FIXTURES).toHaveLength(25);
  });

  for (const fixture of FIXTURES) {
    it(`classifies ${fixture.ua.slice(0, 60) || '(empty)'}`, () => {
      expect(classifyUserAgent(fixture.ua)).toEqual({
        browser: fixture.browser,
        browserMajor: fixture.browserMajor,
        os: fixture.os,
        deviceType: fixture.deviceType,
      });
    });
  }

  it('only ever emits protocol enum members', () => {
    for (const fixture of FIXTURES) {
      const info = classifyUserAgent(fixture.ua);
      expect(BROWSERS).toContain(info.browser);
      expect(OPERATING_SYSTEMS).toContain(info.os);
      expect(DEVICE_TYPES).toContain(info.deviceType);
    }
  });

  it('treats an absent or oversized User-Agent as unknown', () => {
    const unknown = { browser: 'other', browserMajor: null, os: 'other', deviceType: 'other' };

    expect(classifyUserAgent(null)).toEqual(unknown);
    expect(classifyUserAgent('')).toEqual(unknown);
    expect(classifyUserAgent(`Mozilla/5.0 ${'x'.repeat(MAX_USER_AGENT_LEN)} Chrome/126.0.0.0`)).toEqual(unknown);
  });

  // Ordering rule of protocols/analytics/README.md: an iPad sends `Mobile/15E148`,
  // so testing `Mobi` first would make the tablet branch unreachable.
  it('lets tablet win over mobile', () => {
    const ipad =
      'Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1';
    const androidTablet =
      'Mozilla/5.0 (Linux; Android 13; SM-X710) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.6478.71 Safari/537.36';
    const androidPhone =
      'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.6478.71 Mobile Safari/537.36';

    expect(classifyUserAgent(ipad).deviceType).toBe('tablet');
    expect(classifyUserAgent(androidTablet).deviceType).toBe('tablet');
    expect(classifyUserAgent(androidPhone).deviceType).toBe('mobile');
  });

  it('keeps the detection order between families that share tokens', () => {
    const edge =
      'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.2592.68';
    const criOS =
      'Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/126.0.6478.54 Mobile/15E148 Safari/604.1';

    expect(classifyUserAgent(edge)).toEqual({
      browser: 'edge',
      browserMajor: 126,
      os: 'windows',
      deviceType: 'desktop',
    });
    expect(classifyUserAgent(criOS)).toEqual({
      browser: 'chrome',
      browserMajor: 126,
      os: 'ios',
      deviceType: 'mobile',
    });
  });

  it('reports a null major when the token carries no version', () => {
    expect(classifyUserAgent('Mozilla/5.0 (Windows NT 10.0) Firefox/').browserMajor).toBeNull();
  });
});
