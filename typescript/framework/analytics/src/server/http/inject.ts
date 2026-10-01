import { incCounter } from '@putnami/application';
import type { AnalyticsBootstrap } from './bootstrap';

/**
 * The attribute the injected tag carries its bootstrap in, and the selector
 * the browser entry looks it up by.
 *
 * The value is the same object the React renderer publishes as
 * `window.__putnamiBootstrap.analytics`. It rides on an attribute rather than
 * in an inline script because a statically served page has no CSP nonce: an
 * inline script would be blocked by every `script-src` that does not say
 * `'unsafe-inline'`, and asking an application to relax its policy to be
 * measured is not a trade this package offers.
 */
export const BOOTSTRAP_ATTRIBUTE = 'data-putnami-analytics';

/** Counted when a page could not be given the tracker it should have carried. */
const NOT_INJECTED_COUNTER = 'analytics.page_view.tracker_not_injected';

/** The closing tag the script is inserted before. */
const BODY_CLOSE = /<\/body\s*>/i;

/**
 * Escapes a JSON payload for a double-quoted HTML attribute.
 *
 * `<` and `>` are escaped as well as the three characters an attribute value
 * strictly needs: the payload sits inside a `<script>` start tag, and a parser
 * that resynchronises early must not find a tag boundary in it.
 */
function escapeAttribute(value: string): string {
  return value
    .replace(/&/g, '&amp;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;');
}

/**
 * Returns `html` with the tracker script tag inserted before `</body>`.
 *
 * This is what gives a `.static()` page a browser tracker. The page was
 * rendered at build time, with no request to carry a page-view id, so the
 * renderer wrote no bootstrap into it — but the *request* that serves the
 * pre-rendered bytes has one, and it is the same id the server just recorded
 * its own row under. Injecting it here keeps one page view per load: the
 * browser enriches the server's row instead of minting a second one.
 *
 * The tag is inserted last so it never delays the parser: a module script is
 * deferred, and everything the tracker reads is in the document by then.
 *
 * @param html - The document the handler produced.
 * @param bootstrap - What the browser is told about this page view.
 * @param trackerUrl - The public route of the tracker bundle.
 * @returns The document with the tag, or `undefined` when there is no `</body>` to insert before.
 */
export function injectTrackerTag(html: string, bootstrap: AnalyticsBootstrap, trackerUrl: string): string | undefined {
  const match = BODY_CLOSE.exec(html);
  if (!match) {
    incCounter(NOT_INJECTED_COUNTER);
    return undefined;
  }
  const tag =
    `<script type="module" src="${escapeAttribute(trackerUrl)}"` +
    ` ${BOOTSTRAP_ATTRIBUTE}="${escapeAttribute(JSON.stringify(bootstrap))}"></script>`;
  return `${html.slice(0, match.index)}${tag}${html.slice(match.index)}`;
}
