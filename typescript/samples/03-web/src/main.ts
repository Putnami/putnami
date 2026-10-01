import { analytics, declareEvents } from '@putnami/analytics';
import { application, http, logger, platform } from '@putnami/application';
import { react } from '@putnami/web';

// Declared up front: the browser can only ever send a name that already exists
// here, so a page cannot mint an unbounded series of counter keys.
// `value` is typed String because `data-track-*` attributes are always strings;
// a Number-typed property would be dropped on arrival.
export const events = declareEvents({ counter_click: { value: String } });

// No sql() in this sample. The analytics plugin logs one warning at warmup,
// counts events in metrics, and stores nothing — the application still serves.
export const app = () =>
  application()
    .use(http())
    .use(logger())
    // `/readyz` and `/healthz`: `putnami qualify` waits on the readiness route
    // before it smokes the pages, so a sample without it never qualifies.
    .use(platform())
    .use(react())
    .use(analytics({ events, serverPageViews: false }));
