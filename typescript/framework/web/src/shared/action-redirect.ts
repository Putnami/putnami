/**
 * Request header the client action sends with an enhanced `<Form>` submission.
 *
 * The client posts with `fetch`, which follows a redirect on its own and hands
 * back the target page's HTML. With this header, the action route answers a
 * redirect with {@link ACTION_REDIRECT_HEADER} instead, so the client can let
 * the router navigate. A plain HTML form POST does not send it and still
 * receives the real redirect.
 */
export const ACTION_REQUEST_HEADER = 'X-Putnami-Action';

/** Response header carrying the location an enhanced submission redirects to. */
export const ACTION_REDIRECT_HEADER = 'X-Putnami-Redirect';

/** Response header carrying the status of that redirect (302 when absent). */
export const ACTION_REDIRECT_STATUS_HEADER = 'X-Putnami-Redirect-Status';
