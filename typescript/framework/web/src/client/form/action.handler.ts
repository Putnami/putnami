import type { ActionFunction } from 'react-router';
import { getCsrfToken } from './csrf';
import { clientFetchTimeoutMs } from './fetch-timeout';

export const actionHandler: ActionFunction = async ({ request }) => {
  const headers: Record<string, string> = {};
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
  const data = await res.json();

  if (!res.ok) {
    return { status: res.status, ok: false, ...data };
  }

  return { status: res.status, ok: true, ...data };
};
