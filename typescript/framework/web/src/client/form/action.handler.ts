import { type ActionFunction, redirect } from 'react-router';
import {
  ACTION_REDIRECT_HEADER,
  ACTION_REDIRECT_STATUS_HEADER,
  ACTION_REQUEST_HEADER,
} from '../../shared/action-redirect';
import { getCsrfToken } from './csrf';
import { clientFetchTimeoutMs } from './fetch-timeout';

/** The statuses React Router treats as a redirect. */
const REDIRECT_STATUSES = new Set([301, 302, 303, 307, 308]);

export const actionHandler: ActionFunction = async ({ request }) => {
  const headers: Record<string, string> = { [ACTION_REQUEST_HEADER]: '1' };
  const csrfToken = getCsrfToken();
  if (csrfToken) {
    headers['X-CSRF-Token'] = csrfToken;
  }

  const res = await fetch(request.url, {
    method: 'POST',
    headers,
    body: await request.formData(),
    signal: AbortSignal.timeout(clientFetchTimeoutMs()),
  });

  const location = res.headers.get(ACTION_REDIRECT_HEADER);
  if (location) {
    // Resolved against the action URL, as the browser resolves a Location
    // header: the router navigates in the app, or reloads the document for
    // another origin or a path outside the basename.
    const status = Number(res.headers.get(ACTION_REDIRECT_STATUS_HEADER));
    return redirect(new URL(location, request.url).href, REDIRECT_STATUSES.has(status) ? status : 302);
  }
  if (res.redirected) {
    // A redirect answered before the action route, by an app-wide middleware:
    // fetch already followed it, so the body is the target page, not data.
    return redirect(res.url);
  }

  const data = await res.json();

  if (!res.ok) {
    return { status: res.status, ok: false, ...data };
  }

  return { status: res.status, ok: true, ...data };
};
