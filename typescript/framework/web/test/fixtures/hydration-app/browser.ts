import { readFileSync } from 'node:fs';
import { Window } from 'happy-dom';

// Stands in for a browser tab: loads a server-rendered document in a DOM, runs
// the generated client entry, and prints what hydration did as one JSON line.
//
// Usage: bun --conditions=browser browser.ts <url> <html file> <client entry>
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
  /** `<!--$-->` comments in `#root`, before and after hydration. */
  suspenseMarkers: { server: number; hydrated: number };
  /** `#root` before hydration. */
  serverHtml: string;
}

const PAGE = '[data-testid="page"]';
const HYDRATION_TIMEOUT_MS = 5000;

const [url, htmlFile, clientEntry] = process.argv.slice(2);
if (!url || !htmlFile || !clientEntry) {
  throw new Error('usage: browser.ts <url> <html file> <client entry>');
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
Object.assign(globalThis, { window: browser, document });

// The DOM does not run scripts. Run the inline bootstrap the server wrote
// before `#root`: it carries the router's hydration data.
for (const script of document.querySelectorAll('script:not([src])')) {
  new Function(script.textContent ?? '')();
}

const root = document.getElementById('root');
if (!root) {
  throw new Error('the server document has no #root');
}
const countMarkers = () => root.innerHTML.split('<!--$-->').length - 1;
const serverHtml = root.innerHTML;
const serverPage = root.querySelector(PAGE);
const serverMarkers = countMarkers();

// The entry calls `hydratePage` when it loads.
await import(clientEntry);

const isHydrated = () => root.querySelector(PAGE)?.getAttribute('data-hydrated') === 'true';
const deadline = Date.now() + HYDRATION_TIMEOUT_MS;
while (!isHydrated() && Date.now() < deadline) {
  await new Promise((done) => setTimeout(done, 5));
}

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
