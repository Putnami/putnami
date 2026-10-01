# Build a web app

You will create a React web app with Putnami, explore its structure, and add a new page with server-side data loading.

## Steps

### 1) Create the app

Start in an empty directory after [installing Putnami](/docs/getting-started):

```bash
putnami init --project webapp --extension ts
```

This scaffolds a full React SSR application from the `typescript-web` template, with file-based routing, a layout, pages, and a client-side counter component.

### 2) Run it

```bash
putnami serve webapp
```

Open `http://localhost:3000`. You see the welcome page with navigation to About and Guestbook pages.

### 3) Explore the project structure

The scaffolded project lives in `webapp/`:

```
webapp/
├── src/
│   ├── main.ts               # Application entrypoint
│   ├── serve.ts               # Server bootstrap
│   ├── app/
│   │   ├── layout.tsx         # Root layout (nav, theme toggle)
│   │   ├── page.tsx           # Home page
│   │   ├── about/page.tsx     # About page
│   │   ├── guestbook/
│   │   │   ├── page.tsx       # Guestbook page
│   │   │   ├── loader.ts      # Server-side data loader
│   │   │   └── action.ts      # Form action handler
│   │   ├── error.tsx          # Error boundary
│   │   └── not-found.tsx      # 404 page
│   └── components/
│       └── counter.tsx        # Client-side interactive component
└── test/
    └── app.test.ts
```

### 4) Add a new page

Create `webapp/src/app/hello/page.tsx`:

```tsx
import { page } from '@putnami/web';
import { Heading, Text } from '@putnami/ui';

export default page().render(function HelloPage() {
  return (
    <>
      <Heading level={1}>Hello</Heading>
      <Text as='p' color='secondary'>This is a new page.</Text>
    </>
  );
});
```

Navigate to `http://localhost:3000/hello`. File-based routing picks it up automatically.

### 5) Load data from the server

Create `webapp/src/app/hello/loader.ts`:

```ts
import { loader } from '@putnami/web';

export default loader(async () => {
  return { message: 'Hello from the server', timestamp: new Date().toISOString() };
});
```

Update `webapp/src/app/hello/page.tsx` to use the loaded data:

```tsx
import { page, useLoaderData } from '@putnami/web';
import { Heading, Text } from '@putnami/ui';

export default page().render(function HelloPage() {
  const { message, timestamp } = useLoaderData<{ message: string; timestamp: string }>();

  return (
    <>
      <Heading level={1}>{message}</Heading>
      <Text as='p' color='secondary'>Loaded at {timestamp}</Text>
    </>
  );
});
```

The loader runs on the server before rendering. The data is typed and available via `useLoaderData`.

### 6) Choose a render mode

Routes are SSR by default. Declare other modes explicitly per route — the mode
is never inferred:

```tsx
// Static (SSG): pre-rendered at build, ships zero client JS
export default page().static().render(HelloPage);

// Incremental Static Regeneration: revalidate every 60s
export default page().static({ revalidate: 60 }).render(HelloPage);
```

Add interactivity to a static page with an **island** — a component in a
`*.island.tsx` file that hydrates on its own:

```tsx
// src/app/hello/Counter.island.tsx
import { island } from '@putnami/web';
export default island().load('visible').render(Counter);
```

Each build writes a diffable `.gen/putnami-web-manifest.json` (route · mode ·
hydration · client-JS budget). A `.static()` route that reads request data fails
the build — static means static. See the framework's
`doc/rendering-modes.md` for the full guide.

## Result

You have a React SSR app with layout, file-based routing, and server-side data loading. The template gave you a working starting point — you extended it with a new page and a loader.

## Next steps

- Check the existing `guestbook/` page for an example of form actions with `action.ts`
- See the `components/counter.tsx` for a client-side interactive component
- Add [persistence](/docs/how-to/add-persistence) for database-backed data
- Add [authentication](/docs/how-to/add-authentication) to protect routes

> **Companion sample:** [`typescript/samples/03-web`](https://github.com/putnami/putnami/tree/main/typescript/samples/03-web) — a runnable project covering pages, layouts, loaders, actions, and forms. Run `putnami serve @example/web` from the workspace root.
