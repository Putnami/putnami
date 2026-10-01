/**
 * The bot-token list of `protocols/analytics/fixtures/bots.json`, inlined.
 *
 * The fixture is not imported: a published `@putnami/analytics` cannot reach
 * `protocols/` at runtime, so the list must ship inside the package.
 * `test/bots.test.ts` reads the fixture from disk and asserts this array equals
 * it, which keeps the copy honest without making the request path depend on a
 * file outside the package.
 */
export const BOT_TOKENS: readonly string[] = [
  'bot',
  'crawl',
  'spider',
  'slurp',
  'headless',
  'phantom',
  'puppeteer',
  'playwright',
  'selenium',
  'curl/',
  'wget/',
  'python-requests',
  'go-http-client',
  'java/',
  'okhttp',
  'lighthouse',
  'pagespeed',
  'gtmetrix',
  'pingdom',
  'uptimerobot',
  'facebookexternalhit',
  'whatsapp',
  'telegrambot',
  'discordbot',
  'slackbot',
  'linkedinbot',
  'twitterbot',
  'bingpreview',
  'yandex',
  'baiduspider',
  'duckduckbot',
  'applebot',
  'petalbot',
  'semrush',
  'ahrefs',
  'mj12bot',
  'dotbot',
];

/**
 * Reports whether a User-Agent is automated traffic.
 *
 * An empty or absent User-Agent counts as a bot: a real browser always sends
 * one, so treating its absence as human would make the cheapest possible
 * forgery the one that inflates the audience (Go `IsBotUserAgent`).
 *
 * @param ua - The `User-Agent` header, or null when the client sent none.
 * @returns True when the request must leave no trace.
 */
export function isBot(ua: string | null): boolean {
  if (!ua?.trim()) {
    return true;
  }
  const lowered = ua.toLowerCase();
  return BOT_TOKENS.some((token) => lowered.includes(token));
}
