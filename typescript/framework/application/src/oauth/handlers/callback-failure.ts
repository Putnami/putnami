import { escapeHtml } from '@putnami/utils';
import { negotiateType, parseAccept } from '../../http/content-negotiation';
import type { HttpRequestContext } from '../../http/http-context.type';
import { HttpResponse } from '../../http/http-response';

/**
 * A callback that cannot complete the sign-in.
 */
export interface CallbackFailure {
  /** OAuth error code. Sent in the JSON body and shown, escaped, on the HTML page. */
  error: string;
  /** Description sent in the JSON body only. The HTML page never shows it. */
  description?: string;
  /** HTTP status of the response, whatever its format. */
  status: number;
}

const MAX_ERROR_CODE_LENGTH = 64;

const PAGE_COPY: Record<string, { title: string; message: string }> = {
  invalid_state: {
    title: 'This sign-in link has expired',
    message: 'The sign-in request expired or was already used.',
  },
  access_denied: {
    title: 'Sign-in was cancelled',
    message: 'Access was not granted.',
  },
};

const DEFAULT_COPY = {
  title: 'Sign-in could not be completed',
  message: 'The sign-in did not finish.',
};

/**
 * True when the client ranks `text/html` above `application/json`.
 *
 * A browser navigation sends `text/html` first. A missing `Accept`, `*\/*`,
 * and `application/json` all resolve to JSON.
 */
export function prefersHtml(ctx: HttpRequestContext): boolean {
  const accept = ctx.headers.get('Accept');
  if (!accept) return false;
  return negotiateType(parseAccept(accept), ['application/json', 'text/html']) === 'text/html';
}

/**
 * Answers a failed callback: a short HTML page with a link to `loginRoute`
 * for clients that prefer HTML, and the OAuth JSON error body otherwise.
 */
export function callbackFailureResponse(
  ctx: HttpRequestContext,
  loginRoute: string,
  failure: CallbackFailure,
): HttpResponse {
  if (prefersHtml(ctx)) {
    return new HttpResponse(renderFailurePage(loginRoute, failure.error), {
      status: failure.status,
      headers: {
        'Content-Type': 'text/html; charset=utf-8',
        'Cache-Control': 'no-store',
        'Content-Security-Policy':
          "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
      },
    });
  }
  return new HttpResponse(
    JSON.stringify({ error: failure.error, error_description: failure.description ?? failure.error }),
    {
      status: failure.status,
      headers: { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' },
    },
  );
}

function renderFailurePage(loginRoute: string, error: string): string {
  const copy = PAGE_COPY[error] ?? DEFAULT_COPY;
  const code = escapeHtml(error.slice(0, MAX_ERROR_CODE_LENGTH));
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>${escapeHtml(copy.title)}</title>
<style>
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;font-family:system-ui,-apple-system,sans-serif;background:#f6f7f9;color:#1f2328}
main{max-width:28rem;padding:2rem;background:#fff;border:1px solid #d0d7de;border-radius:8px}
h1{font-size:1.25rem;margin:0 0 .75rem}
p{margin:0 0 1rem;line-height:1.5}
a{display:inline-block;padding:.5rem 1rem;background:#1f2328;color:#fff;border-radius:6px;text-decoration:none}
small{display:block;margin-top:1.5rem;color:#656d76}
@media (prefers-color-scheme:dark){body{background:#0d1117;color:#e6edf3}main{background:#161b22;border-color:#30363d}a{background:#e6edf3;color:#0d1117}small{color:#8d96a0}}
</style>
</head>
<body>
<main>
<h1>${escapeHtml(copy.title)}</h1>
<p>${escapeHtml(copy.message)} Sign in again to continue.</p>
<a href="${escapeHtml(loginRoute)}">Sign in again</a>
<small>Error code: ${code}</small>
</main>
</body>
</html>
`;
}
