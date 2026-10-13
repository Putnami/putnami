import { redirect } from 'react-router';
import { CLIENT_REDIRECT_HEADER, CLIENT_REDIRECT_STATUS_HEADER } from '../../shared/client-redirect';

/** The statuses React Router treats as a redirect. */
const REDIRECT_STATUSES = new Set([301, 302, 303, 307, 308]);

/**
 * Turns the answer of a client action or loader fetch into a router redirect,
 * when the server redirected.
 *
 * @param res - What the action or loader endpoint answered.
 * @param requestUrl - The URL the router is submitting or navigating to.
 * @returns The redirect for React Router, or `undefined` when `res` is data.
 */
export function routerRedirect(res: Response, requestUrl: string): Response | undefined {
  const location = res.headers.get(CLIENT_REDIRECT_HEADER);
  if (location) {
    // Resolved against the router's URL, as the browser resolves a Location
    // header: the router navigates in the app, or reloads the document for
    // another origin or a path outside the basename.
    const status = Number(res.headers.get(CLIENT_REDIRECT_STATUS_HEADER));
    return redirect(new URL(location, requestUrl).href, REDIRECT_STATUSES.has(status) ? status : 302);
  }
  if (res.redirected && !res.headers.get('Content-Type')?.includes('application/json')) {
    // A redirect answered before the route, by an app-wide middleware: fetch
    // already followed it, so the body is the target page, not data. A followed
    // redirect that ends on JSON, such as a 307/308 that re-sent an action's
    // POST, stays data.
    return redirect(res.url);
  }
  return undefined;
}
