import { redirect } from 'react-router';
import { CLIENT_REDIRECT_HEADER, CLIENT_REDIRECT_STATUS_HEADER } from '../../shared/client-redirect';

/** The statuses React Router treats as a redirect. */
const REDIRECT_STATUSES = new Set([301, 302, 303, 307, 308]);

/**
 * Turns the answer of a client action or loader fetch into a router redirect,
 * when the server redirected.
 *
 * A redirect answered before the route, by an app-wide middleware, reaches the
 * client already followed by `fetch`. `isData` tells whether such an answer is
 * still the data of an action or loader, or the page the redirect leads to.
 *
 * @param res - What the action or loader endpoint answered.
 * @param requestUrl - The URL the router is submitting or navigating to.
 * @param isData - Whether a followed answer is data.
 * @returns The redirect for React Router, or `undefined` when `res` is data.
 */
export function routerRedirect(
  res: Response,
  requestUrl: string,
  isData: (followed: Response) => boolean,
): Response | undefined {
  const location = res.headers.get(CLIENT_REDIRECT_HEADER);
  if (location) {
    // Resolved against the router's URL, as the browser resolves a Location
    // header: the router navigates in the app, or reloads the document for
    // another origin or a path outside the basename.
    const status = Number(res.headers.get(CLIENT_REDIRECT_STATUS_HEADER));
    return redirect(new URL(location, requestUrl).href, REDIRECT_STATUSES.has(status) ? status : 302);
  }
  if (res.redirected && !isData(res)) {
    return redirect(res.url);
  }
  return undefined;
}
