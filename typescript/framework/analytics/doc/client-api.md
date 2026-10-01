# Client API

The browser half of `@putnami/analytics`: a dependency-free tracker of under
4 KiB gzipped that the server injects into the pages it renders.

## What the tracker is, and where it runs

The pre-build hook bundles the tracker entry (`src/client/entry.ts` in this
repository, the published `src/client/entry.js` in an installed package) into
`.gen/<publicFolder>/analytics/analytics.<hash>.js.gz`. The page-view
middleware adds the `<script type="module">` tag that loads it, and the page
renderer writes `window.__putnamiBootstrap.analytics` next to it.

The tracker installs only when that bootstrap is present:

- **Server-rendered pages** get the tracker.
- **Static and prerendered pages** do not. They are served as files, so no
  middleware runs and no bootstrap is written. They still get a server-side
  page view; `engagement_ms` and `session_id` stay null for them.

The tracker never sets a cookie and never reads one. It is not told who the
visitor is: the bootstrap carries `pv`, `route`, `endpoint`, `app`, `env`,
`version`, and `declared` — no `user_id`, no visitor id, no secret.

## The first page view is the server's

The server writes the page view when it renders the HTML and puts that row's
event id in the bootstrap as `pv`. The tracker re-sends **the same id**,
adding what only a browser knows:

| Added by the browser | Where it comes from |
|---|---|
| `sessionId`, `seq` | the client-owned session |
| `page.referrer` | `document.referrer`, query and fragment stripped |
| `page.utm` | the five `utm_*` parameters of the landing URL |
| `viewportClass` | `window.innerWidth` |
| `language` | `navigator.language` |
| `engagementMs` | visible time, sent again on navigation and unload |

The sink upserts on `event_id`, so the two halves become one row and the page
is counted once. A tracker that minted a fresh id here would double-count
every page view in the application.

## `track(name, props)`

Records one declared action.

```ts
import { track } from '@putnami/analytics';

track('signup_click', { plan: 'pro', seats: 3, trial: true });
```

- `name` must be declared server-side with `declareEvents`. An undeclared name
  logs a warning and sends nothing; the server would drop it silently, so the
  warning is the only place you learn about the typo.
- `props` values are strings, finite numbers, or booleans. Keys must match
  `^[a-z][a-z0-9_]{0,31}$`; up to 20 of them; string values are truncated to
  256 UTF-8 bytes. A property the protocol would reject is dropped — the event
  is not.
- Calling it where no tracker is installed does nothing. There is no guard to
  write.

## `useTrack()`

The same thing, shaped for a component:

```tsx
import { useTrack } from '@putnami/analytics';

export function SignupButton() {
  const track = useTrack();
  return <button onClick={() => track('signup_click', { plan: 'pro' })}>Sign up</button>;
}
```

It imports neither React nor the tracker; it reads `window.__putnamiAnalytics`
at call time. That makes it safe during server rendering and on a static page,
which is why the same function is exported from both package entries.

## `data-track`

For a click that needs no handler, declare it in the markup:

```tsx
<button data-track="signup_click" data-track-plan="pro" data-track-seat-count="3">
  Sign up
</button>
```

One capturing, passive listener on the document serves the whole page,
including markup React mounts later. `data-track` is the action name; each
`data-track-*` attribute becomes a property whose key has its dashes turned
into underscores (`data-track-seat-count` → `seat_count`).

## `onNavigation(listener)`

Client-side route changes, re-exported from `@putnami/web`:

```ts
import { onNavigation } from '@putnami/analytics';

const stop = onNavigation((detail) => console.log(detail.route, detail.pathname));
```

**It does not fire for the initial load.** The server bootstrap is the initial
view. A listener that assumed otherwise would count the landing page twice.

The tracker subscribes to the same event. On each navigation it re-sends the
outgoing view with its accumulated engagement — same event id, so the sink
takes `GREATEST` and no counter moves — and opens a new view with a fresh
UUID v7 whose referrer is the previous path.

## Queue, retries, and what gets lost

Events do not go out one by one.

1. **Queue.** Every event lands in `localStorage` under an app-owned key derived
   from `putnami.analytics.queue:<app>:<endpoint>`, capped at 200. The app and
   endpoint components are URL-encoded. Over the cap the *oldest* event is
   dropped. Persisting means a batch that lost the race with a closing tab is
   retried by the next page. The former origin-wide
   `putnami.analytics.queue` key is deliberately not migrated: its events have
   no trustworthy app owner, so claiming them could disclose one path-mounted
   app's events to another app's endpoint. Changing either bootstrap value
   starts a fresh queue and leaves the former owner's key untouched.
2. **Flush.** Every 5 seconds, as soon as 20 events are waiting, when the tab
   goes hidden, and on `pagehide`. A batch is at most 50 events — the wire
   ceiling.
3. **Transport.** On unload, `navigator.sendBeacon` with a `text/plain` blob:
   same-origin, no preflight, and it survives the document. Otherwise `fetch`
   with `keepalive: true` and `credentials: 'same-origin'`. A beacon the
   browser refuses falls back to `fetch`. A beacon the browser queues remains
   in the durable queue because that boolean is not delivery confirmation; a
   later fetch retries it and may safely duplicate it.
4. **Outcome.** A fetch `2xx` acknowledges and clears the batch. A fetch `4xx`
   other than 429 drops it — the server has decided it will never take it, and
   retrying would only overflow the queue. `429`, any `5xx`, and a network
   error keep the batch and back off `min(60s, 1s × 2^attempt)` with ±20 %
   jitter.

Event ids never change across a retry, so a batch that arrives twice folds
into the rows it already wrote. Re-sending changes enrichment columns and no
counter.

Analytics are lossy by construction: a visitor who fills the queue faster than
it drains loses its oldest events, and a browser whose `localStorage` failed
loses its in-memory fallback when the page closes. The alternative — blocking
the page on delivery — is not one.

## Sessions

`sessionStorage`, key `putnami.analytics.session`. A new session id (UUID v7,
`seq` back to 0) is minted when there is none, after 30 minutes without an
event, or across a UTC day boundary. The day boundary matters: the server's
daily visitor hash rotates at midnight, and a session that straddled it would
be the one record able to link the two days.

When `sessionStorage` throws — private mode on some browsers — the session
falls back to an in-memory object for the page lifetime. The same applies to
the queue and `localStorage`. Analytics degrade; the page does not break.

## What the tracker never does

- Set or read a cookie.
- Read the query string beyond the five `utm_*` keys.
- Send a referrer with its query or fragment attached.
- Send an IP address, a `User-Agent`, a page title, or a form value.
- Send an action name the server did not declare.

## Related documents

- `doc/configuration.md` — the server options and the fail-closed startup checks.
- `doc/data-model.md` — the tables and what each column may hold.
