import type { Browser, DeviceType, OS } from '../sanitize/vocabulary';

export type { Browser, DeviceType, OS } from '../sanitize/vocabulary';

/** The closed classification of one User-Agent header. */
export interface UAInfo {
  /** The browser family, or `other` when no known token matched. */
  browser: Browser;
  /** The browser's major version, or null when it is unknown. */
  browserMajor: number | null;
  /** The operating-system family, or `other` when no known token matched. */
  os: OS;
  /** The device class, or `other` when the browser itself is unknown. */
  deviceType: DeviceType;
}

/** The longest User-Agent this classifier reads; anything longer is `other`. */
export const MAX_USER_AGENT_LEN = 1024;

const UNKNOWN: UAInfo = { browser: 'other', browserMajor: null, os: 'other', deviceType: 'other' };

/**
 * Classifies a User-Agent into the four closed vocabularies of the protocol.
 *
 * Hand-rolled on purpose: a UA-parsing dependency would ship a thousand-entry
 * regex table into every request path to produce values this contract bounds to
 * seven browsers and seven operating systems. The raw header is never stored —
 * only `browser/os` reaches the visitor hash, and only the enums reach a row.
 *
 * @param ua - The `User-Agent` header, or null when the client sent none.
 * @returns The browser, its major version, the OS, and the device class.
 */
export function classifyUserAgent(ua: string | null): UAInfo {
  if (!ua || ua.length > MAX_USER_AGENT_LEN) {
    return { ...UNKNOWN };
  }
  const browser = detectBrowser(ua);
  return {
    browser,
    browserMajor: detectMajor(ua, browser),
    os: detectOs(ua),
    deviceType: browser === 'other' ? 'other' : detectDevice(ua),
  };
}

/** First match wins: Edge and Opera both carry `Chrome/`, Chrome carries `Safari/`. */
function detectBrowser(ua: string): Browser {
  if (ua.includes('Edg/')) {
    return 'edge';
  }
  if (ua.includes('OPR/') || ua.includes('Opera')) {
    return 'opera';
  }
  if (ua.includes('SamsungBrowser/')) {
    return 'samsung';
  }
  if (ua.includes('Firefox/')) {
    return 'firefox';
  }
  if (ua.includes('Chrome/') || ua.includes('CriOS/')) {
    return 'chrome';
  }
  if (ua.includes('Safari/') && ua.includes('Version/')) {
    return 'safari';
  }
  return 'other';
}

const MAJOR_PATTERNS: Record<Browser, readonly RegExp[]> = {
  edge: [/Edg\/(\d+)/],
  opera: [/OPR\/(\d+)/, /Opera\/(\d+)/],
  samsung: [/SamsungBrowser\/(\d+)/],
  firefox: [/Firefox\/(\d+)/],
  chrome: [/CriOS\/(\d+)/, /Chrome\/(\d+)/],
  // Safari's own version lives in `Version/`; `Safari/` carries the WebKit build.
  safari: [/Version\/(\d+)/],
  other: [],
};

/** Reads the integer that follows the token the browser was detected by. */
function detectMajor(ua: string, browser: Browser): number | null {
  for (const pattern of MAJOR_PATTERNS[browser]) {
    const match = pattern.exec(ua);
    if (match?.[1] !== undefined) {
      return Number.parseInt(match[1], 10);
    }
  }
  return null;
}

function detectOs(ua: string): OS {
  if (ua.includes('Windows')) {
    return 'windows';
  }
  if (ua.includes('iPhone') || ua.includes('iPad') || ua.includes('iPod')) {
    return 'ios';
  }
  if (ua.includes('Android')) {
    return 'android';
  }
  if (ua.includes('CrOS')) {
    return 'chromeos';
  }
  if (ua.includes('Mac OS X') || ua.includes('Macintosh')) {
    return 'macos';
  }
  if (ua.includes('Linux')) {
    return 'linux';
  }
  return 'other';
}

/**
 * Tablet wins over mobile.
 *
 * An iPad sends `Mobile/15E148`, so testing `Mobi` first would make the tablet
 * branch unreachable and file every tablet as a phone. An Android tablet is
 * recognised by the absence of `Mobi`, which is how Android itself signals the
 * difference (`protocols/analytics/README.md`).
 */
function detectDevice(ua: string): DeviceType {
  const android = ua.includes('Android');
  const mobi = ua.includes('Mobi');
  if (ua.includes('iPad') || ua.includes('Tablet') || (android && !mobi)) {
    return 'tablet';
  }
  if (mobi || android) {
    return 'mobile';
  }
  return 'desktop';
}
