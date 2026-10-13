import type { ActionFunction } from 'react-router';
import { ACTION_REQUEST_HEADER } from '../../shared/client-redirect';
import { getCsrfToken } from './csrf';
import { clientFetchTimeoutMs } from './fetch-timeout';
import { routerRedirect } from './follow-redirect';

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

  // A followed redirect that ends on JSON, such as a 307/308 that re-sent the
  // POST to an action, stays data; anything else is the target page.
  const redirected = routerRedirect(res, request.url, (followed) =>
    Boolean(followed.headers.get('Content-Type')?.includes('application/json')),
  );
  if (redirected) {
    return redirected;
  }

  const data = await res.json();

  if (!res.ok) {
    return { status: res.status, ok: false, ...data };
  }

  return { status: res.status, ok: true, ...data };
};
