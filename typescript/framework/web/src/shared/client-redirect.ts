/**
 * Request header the client action sends with an enhanced `<Form>` submission.
 *
 * The client posts with `fetch`, which follows a redirect on its own and hands
 * back the target page's HTML. With this header, the action route answers a
 * redirect with {@link CLIENT_REDIRECT_HEADER} instead, so the client can let
 * the router navigate. A plain HTML form POST does not send it and still
 * receives the real redirect.
 */
export const ACTION_REQUEST_HEADER = 'X-Putnami-Action';

/**
 * Request header the client loader sends when it fetches a route's JSON
 * endpoint during client navigation.
 *
 * Same reason as {@link ACTION_REQUEST_HEADER}: `fetch` would follow a loader's
 * redirect and hand the router the target page's HTML as loader data. A request
 * without it still receives the real redirect.
 */
export const LOADER_REQUEST_HEADER = 'X-Putnami-Loader';

/** Response header carrying the location a client action or loader redirects to. */
export const CLIENT_REDIRECT_HEADER = 'X-Putnami-Redirect';

/** Response header carrying the status of that redirect (302 when absent). */
export const CLIENT_REDIRECT_STATUS_HEADER = 'X-Putnami-Redirect-Status';
