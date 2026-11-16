# Capture the screens of a change

`putnami change screenshots`, which the `@putnami/contributor` executable
serves, captures the pages a change touches in a web project: before and
after, desktop and mobile, light and dark. The captures are the proof of a
web change: the agent reads them before it finalizes, the reviewer receives
them with the diff, and `putnami change finalize` asks for them
([ADR 0014](adr/0014-screenshots-are-the-proof-of-a-web-change.md)).

```bash
putnami change screenshots --project apps/web --base main \
  --base-url http://127.0.0.1:3001 --head-url http://127.0.0.1:3000
putnami change screenshots --project apps/web --base main --head-url http://127.0.0.1:3000 \
  --storage-state .putnami/auth.json --fixtures .putnami/fixtures.json --out .putnami/42/screenshots
```

## Serve the base and the head

The command starts no server: serve both sides yourself, each on its own
port, and pass their URLs.

```bash
putnami serve apps/web --port 3000
git worktree add ../web-base "$(git merge-base origin/main HEAD)"
(cd ../web-base && putnami serve apps/web --port 3001)
```

The head server must serve the committed head: the command refuses a project
directory with uncommitted changes. `--base-url` is needed only when a touched
page exists at the base; a change that only adds pages captures the head
alone.

## Which pages

A web project is a project whose committed `schema/http-routes.json` declares
a page: a `file-route` route that serves `GET`. Its page file is the route's
`evidencePath`, relative to the project. The changed paths are those between
the merge base of `--base` (`origin/<base>` when that remote branch exists) and
`HEAD`.

| Changed path | Touches |
|---|---|
| A page file | That page |
| Another file under the project's `src/`, such as a component, a layout or a style | Every page of the project |
| A file that is the evidence of a route, such as an action or an API handler | No page |
| A file outside `src/`, such as a test under `test/`, a doc or the route inventory | No page |

A test kept under `src/`, such as `src/page.test.tsx`, counts as a source file
and touches every page.

Each touched page is captured at its first route in inventory order. A page
file that a finite catch-all expands into many routes is captured once, at its
first route; `--route <path>` adds a route, as the inventory writes it, and may
repeat. A template route takes its parameter values from `--fixtures`, an
object keyed by the template:

```json
{ "/tasks/{id}": { "id": "1" }, "/docs/{slug...}": { "slug": "guide/intro" } }
```

A touched template with no value is refused, and the error names each one.

## The captures

| | |
|---|---|
| Viewports | desktop 1440x900, mobile 390x844 at device scale 2 with touch |
| Color schemes | `prefers-color-scheme: light`, then `dark` |
| Sides | before, at `--base-url`, where the merge base holds the page; after, at `--head-url`, where the head does |

A page both sides hold gets eight PNG files; a new or a removed page gets
four. Each capture is a full page, up to 10000 CSS pixels high, taken after
the document's load event, its fonts, and at most three seconds of network
activity. A failed navigation, or a document status of 400 or more, fails the
run and writes no manifest. A page that redirects is captured where it ends:
each capture records its status and final URL, and the run prints a `note:`
line for a page that ended elsewhere, such as a sign-in page, which shows no
change.

The browser is a headless Chromium driven over the DevTools pipe: `--browser
<path>`, else `PUTNAMI_BROWSER`, else the Chromium of a Playwright installation
at `/opt/pw-browsers/chromium`, else a `chromium` or `google-chrome` on `PATH`.
No Node runtime or Playwright package is needed.

`--storage-state` reads a Playwright `storageState` file. Its cookies are set
in the browser before the first capture. The `localStorage` of each of its
origins is set on the pages of the same host, whatever the port, before their
scripts run, so one recorded sign-in serves both servers.

## The manifest

The files and `screenshots.json` go to `--out`, by default
`.putnami/screenshots/<slug>`, outside Git, where the slug is the project
directory in lowercase with each other character run turned into `-`, such as
`apps-web` for `apps/web`. A new run there first removes
the files the previous manifest lists, and nothing else.

```json
{
  "protocolVersion": 1,
  "project": "apps/web",
  "baseSHA": "<merge base>",
  "headSHA": "<HEAD>",
  "pages": [
    { "file": "src/app/tasks/page.tsx", "route": "/tasks", "path": "/tasks", "before": true, "after": true }
  ],
  "captures": [
    { "file": "01-tasks-after-desktop-light.png", "route": "/tasks", "side": "after",
      "viewport": "desktop", "scheme": "light", "sha256": "<hex>",
      "status": 200, "url": "http://127.0.0.1:3000/tasks" }
  ]
}
```

`--output=json` prints the result: the manifest path, the project, the pages
and the number of captures. A change that touches no page writes nothing and
exits 0. A usage error exits 2; a failure exits 1.

## Finalize

A ready `putnami change finalize`, local or hosted, of a change that touches a
page of a web project takes `--screenshots <manifest>` for each such project,
or `--waive-screenshots <reason>` ([Publish a proposal](06-change-finalize.md#screenshots)).
It then sends each accepted manifest and its files to the bound proposals
provider, when that provider stores screenshots
([Upload](06-change-finalize.md#upload)).

## Limits

The captures leave the machine that took them only through
`change finalize`, and only when the bound proposals provider offers
`proposals.screenshots`. `@putnami/cloud` does, under a signed-in person's
session; a hosted run on a machine token uploads nothing. A route an application registers only at runtime is absent from the
inventory and never selected.
