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

  const redirected = routerRedirect(res, request.url);
  if (redirected) {
    return redirected;
  }

  const data = await res.json();

  if (!res.ok) {
    return { status: res.status, ok: false, ...data };
  }

  return { status: res.status, ok: true, ...data };
};
