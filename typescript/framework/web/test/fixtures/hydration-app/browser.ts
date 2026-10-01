import { readFileSync } from 'node:fs';
import { Window } from 'happy-dom';

// Stands in for a browser tab: loads a server-rendered document in a DOM, runs
// the generated client entry, and prints what hydration did as one JSON line.
//
// Usage: bun --conditions=browser browser.ts <url> <html file> <client entry> <nonce>
//
// It runs in its own process, as a browser does. React keeps the context values
// of a server render on the shared context objects, so a client render in the
// process that rendered the HTML would start from that state.

export interface HydrationReport {
  /** Every `console.error` call: `hydratePage` reports each React error there. */
  errors: string[][];
  /** Whether the page reached its first client effect. */
  hydrated: boolean;
  /** Whether the page element the server sent is still the one in the DOM. */
  pageElementKept: boolean;
  /** Suspense boundary comments in `#root`, before and after hydration. */
  suspenseMarkers: { server: number; hydrated: number };
  /** `#root` before hydration. */
  serverHtml: string;
}

const PAGE = '[data-testid="page"]';
const HYDRATION_TIMEOUT_MS = 5000;
/** A boundary React still waits for: its content is not in place yet. */
const PENDING_BOUNDARY = '<!--$?-->';

const [url, htmlFile, clientEntry, nonce] = process.argv.slice(2);
if (!url || !htmlFile || !clientEntry || !nonce) {
  throw new Error('usage: browser.ts <url> <html file> <client entry> <nonce>');
}

/** Wait until `done` holds, for at most the hydration timeout. */
async function waitFor(done: () => boolean): Promise<void> {
  const deadline = Date.now() + HYDRATION_TIMEOUT_MS;
  while (!done() && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

const errors: string[][] = [];
console.error = (...args: unknown[]) => {
  errors.push(
    args.map((arg) => (arg instanceof Error ? arg.message : typeof arg === 'string' ? arg : JSON.stringify(arg))),
  );
};

const browser = new Window({ url });
const document = browser.document;
document.write(readFileSync(htmlFile, 'utf8'));
Object.assign(globalThis, {
  window: browser,
  document,
  requestAnimationFrame: browser.requestAnimationFrame.bind(browser),
});

// The DOM does not run scripts. Run the inline scripts the server wrote, under
// the policy the server sends: a script runs only when it carries the request
// nonce. They carry the router's hydration data and move streamed boundaries
// into place.
for (const script of document.querySelectorAll('script:not([src])')) {
  if (script.getAttribute('nonce') === nonce) {
    new Function(script.textContent ?? '')();
  }
}

const root = document.getElementById('root');
if (!root) {
  throw new Error('the server document has no #root');
}
// The document is complete, so every boundary a script can move is in place
// once the scripts settle.
await waitFor(() => !root.innerHTML.includes(PENDING_BOUNDARY));
const countMarkers = () => root.innerHTML.match(/<!--\$[?!~]?-->/g)?.length ?? 0;
const serverHtml = root.innerHTML;
const serverPage = root.querySelector(PAGE);
const serverMarkers = countMarkers();

// The entry calls `hydratePage` when it loads.
await import(clientEntry);

const isHydrated = () => root.querySelector(PAGE)?.getAttribute('data-hydrated') === 'true';
await waitFor(isHydrated);

const report: HydrationReport = {
  errors,
  hydrated: isHydrated(),
  pageElementKept: serverPage !== null && root.querySelector(PAGE) === serverPage,
  suspenseMarkers: { server: serverMarkers, hydrated: countMarkers() },
  serverHtml,
};
process.stdout.write(`${JSON.stringify(report)}\n`);
// A router keeps listeners on the window, so the process does not end alone.
process.exit(0);
