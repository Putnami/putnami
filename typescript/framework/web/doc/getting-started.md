# Getting Started

This guide will help you set up your first React application with `@putnami/web` and understand the core concepts.

## Prerequisites

- [Bun](https://bun.sh/) v1.4.0 or higher
- Basic knowledge of React and TypeScript
- Familiarity with React Router concepts (helpful but not required)

## Installation

```bash
bun add @putnami/web @putnami/application
```

## Project Setup

### 1. Create Your Application Entry Point

Create `src/main.ts`:

```typescript
import { application } from '@putnami/application';
import { react } from '@putnami/web';

export const app = () => application().use(react());
```

### 2. Create Your First Page

Create `src/app/page.tsx`:

```tsx
export default function HomePage() {
  return (
    <div>
      <h1>Welcome to Putnami React</h1>
      <p>Your first SSR React app!</p>
    </div>
  );
}
```

### 3. Create a Root Layout

Create `src/app/layout.tsx`:

```tsx
import { Outlet } from '@putnami/web';

export default function RootLayout() {
  return (
    <html lang="en">
      <head>
        <meta charSet="utf-8" />
        <title>My App</title>
      </head>
      <body>
        <Outlet />
      </body>
    </html>
  );
}
```

### 4. Run Your Application

```bash
bun run src/main.ts
```

Visit `http://localhost:3000` to see your app!

## Understanding the File Structure

The React plugin automatically scans your `src/app/` directory (configurable) and generates routes based on file conventions:

```
src/
├── main.ts              # Application entry point
└── app/
    ├── layout.tsx       # Root layout (wraps all pages)
    ├── page.tsx         # Home page (/)
    ├── loader.ts        # Optional: data loader for home page
    └── about/
        ├── page.tsx     # About page (/about)
        └── loader.ts    # Optional: data loader for about page
```

## Adding Data Loading

Loaders fetch data on the server before rendering. Create `src/app/loader.ts`:

```typescript
import { loader } from '@putnami/web';

export default loader(async (ctx) => {
  // Fetch data from API, database, etc.
  const data = await fetch('https://api.example.com/data').then(r => r.json());

  return {
    message: 'Hello from the server!',
    data,
  };
});
```

Use the data in your page:

```tsx
import { useLoaderData } from '@putnami/web';

interface PageData {
  message: string;
  data: any;
}

export default function HomePage() {
  const { message, data } = useLoaderData<PageData>();

  return (
    <div>
      <h1>{message}</h1>
      <pre>{JSON.stringify(data, null, 2)}</pre>
    </div>
  );
}
```

## Adding Forms and Actions

Create a form page with an action:

```tsx
// src/app/contact/page.tsx
import { Form, useActionData } from '@putnami/web';

interface ActionResult {
  message: string;
  ok: boolean;
  status: number;
}

export default function ContactPage() {
  const result = useActionData<ActionResult>();

  return (
    <div>
      <h1>Contact Us</h1>
      {result?.ok && <p style={{ color: 'green' }}>{result.message}</p>}
      <Form method="post">
        <input name="email" type="email" placeholder="Your email" required />
        <textarea name="message" placeholder="Your message" required />
        <button type="submit">Send</button>
      </Form>
    </div>
  );
}
```

Create the action handler:

```typescript
// src/app/contact/action.ts
import { action } from '@putnami/web';
import { json } from '@putnami/application';

export default action(async (ctx) => {
  const formData = await ctx.req.formData();
  const email = formData.get('email') as string;
  const message = formData.get('message') as string;

  // Validate and process the form
  if (!email || !message) {
    return json(
      { message: 'Email and message are required' },
      { status: 400 }
    );
  }

  // Send email, save to database, etc.
  await sendContactEmail(email, message);

  return {
    message: 'Thank you for your message!',
    ok: true,
    status: 200,
  };
});
```

## Development vs Production

### Development Mode

In development, the plugin automatically:
- Scans for route changes and regenerates routes
- Provides detailed error messages
- Enables hot reloading

### Production Mode

For production, you should:

1. **Build your application:**
   ```bash
   bun run build
   ```

2. **Set environment variables:**
   ```bash
   NODE_ENV=production bun run src/main.ts
   ```

3. **Configure production settings:**
   ```typescript
   // src/main.ts
   import { react } from '@putnami/web';

   export const app = () => application().use(
     react({
       isDevelopment: false,
       minify: true,
       sourcemap: 'none',
     })
   );
   ```

## Project Structure Recommendations

Here's a recommended structure for a production app:

```
src/
├── main.ts                 # Application entry
├── app/                    # Routes (scanned by plugin)
│   ├── layout.tsx
│   ├── page.tsx
│   ├── loader.ts
│   ├── error.tsx           # Error boundary
│   ├── not-found.tsx       # 404 page
│   ├── dashboard/
│   │   ├── layout.tsx
│   │   ├── page.tsx
│   │   └── settings/
│   │       ├── page.tsx
│   │       ├── loader.ts
│   │       └── action.ts
│   └── api/                # API routes (if using @putnami/application API plugin)
│       └── users/
│           └── get.ts
├── components/             # Reusable components
│   ├── Button.tsx
│   └── Card.tsx
├── lib/                    # Utilities and helpers
│   ├── db.ts
│   └── api.ts
└── types/                  # TypeScript types
    └── index.ts
```

## Next Steps

- Learn about [File-based Routing](file-based-routing.md) for advanced routing patterns
- Explore [Data Loading](data-loading.md) for complex data fetching scenarios
- Read about [Forms & Actions](forms-and-actions.md) for form handling best practices
- Check out [Configuration](configuration.md) for customization options

## Common Questions

**Q: Do I need to configure routing manually?**
A: No! The plugin automatically generates routes from your file structure. You can also use manual configuration if needed.

**Q: How do I add middleware?**
A: Use `@putnami/application` middleware system. See the application package documentation.

**Q: Can I use client-side only components?**
A: Yes! Components work on both server and client. Use conditional exports if you need browser-only code.

**Q: How do I handle authentication?**
A: Use loaders to check authentication and redirect if needed. See [Advanced Patterns](advanced-patterns.md#authentication--route-guards).
