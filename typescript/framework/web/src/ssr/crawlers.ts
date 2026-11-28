/**
 * The user agents that wait for every Suspense boundary before the head:
 * search crawlers and link-preview agents, which read the head without running
 * JavaScript. `react({ crawlerUserAgents })` replaces it; an empty pattern
 * streams every request.
 */
export const DEFAULT_CRAWLER_USER_AGENTS =
  // `bot` names most crawlers (Googlebot, bingbot, Twitterbot, Slackbot, Discordbot...); `cubot` is a phone brand.
  '(?<!cu)bot|crawl|spider|slurp|facebookexternalhit|facebookcatalog|embedly|iframely|cardyb|quora link preview|' +
  'outbrain|pinterest/0|vkshare|w3c_validator|whatsapp/|slack-imgproxy|skypeuripreview|bingpreview|google-inspectiontool';

/** Compile a crawler user-agent pattern; an empty one matches nothing. */
export function crawlerPattern(source: string): RegExp | undefined {
  return source ? new RegExp(source, 'i') : undefined;
}

/** Whether a request comes from a user agent the crawler pattern names. */
export function isCrawler(req: Request, crawlers: RegExp | undefined): boolean {
  if (!crawlers) return false;
  const agent = req.headers.get('user-agent');
  return agent !== null && crawlers.test(agent);
}
