# Forms & Actions

Forms in `@putnami/web` use progressive enhancement: they work without JavaScript and become richer with it. Actions handle form submissions on the server.

## Basic form

### Action handler

```ts
// src/app/contact/action.ts
import { action } from '@putnami/web';

export default action(async (ctx) => {
  const data = await ctx.body<{
    name: string;
    email: string;
    message: string;
  }>();

  await sendContactEmail(data);

  return { ok: true, message: 'Message sent!' };
});
```

### Form component

```tsx
// src/app/contact/page.tsx
import { Form, useActionData, useNavigation } from '@putnami/web';

export default function ContactPage() {
  const actionData = useActionData<{ ok: boolean; message?: string; error?: string }>();
  const navigation = useNavigation();

  const isSubmitting = navigation.state === 'submitting';

  return (
    <Form method="post">
      <input name="name" placeholder="Name" required />
      <input name="email" type="email" placeholder="Email" required />
      <textarea name="message" placeholder="Message" required />

      <button type="submit" disabled={isSubmitting}>
        {isSubmitting ? 'Sending...' : 'Send'}
      </button>

      {actionData?.ok && <p className="success">{actionData.message}</p>}
      {actionData?.error && <p className="error">{actionData.error}</p>}
    </Form>
  );
}
```

## Form validation

### Server-side validation

```ts
// src/app/register/action.ts
import { action } from '@putnami/web';
import { validateSync, IsEmail, MinLength, IsString } from '@putnami/runtime';

class RegisterDto {
  @IsString()
  @MinLength(2)
  name: string;

  @IsEmail()
  email: string;

  @MinLength(8)
  password: string;
}

export default action(async (ctx) => {
  const body = await ctx.body<RegisterDto>();
  const dto = Object.assign(new RegisterDto(), body);

  const errors = validateSync(dto);
  if (errors.length > 0) {
    return {
      ok: false,
      errors: errors.reduce((acc, err) => {
        acc[err.property] = Object.values(err.constraints || {});
        return acc;
      }, {} as Record<string, string[]>),
    };
  }

  // Check if email exists
  const existing = await findUserByEmail(body.email);
  if (existing) {
    return {
      ok: false,
      errors: { email: ['Email already registered'] },
    };
  }

  const user = await createUser(body);
  return { ok: true, userId: user.id };
});
```

### Displaying validation errors

```tsx
// src/app/register/page.tsx
import { Form, useActionData, useNavigation } from '@putnami/web';

interface ActionResult {
  ok: boolean;
  errors?: Record<string, string[]>;
  userId?: string;
}

export default function RegisterPage() {
  const actionData = useActionData<ActionResult>();
  const navigation = useNavigation();

  const errors = actionData?.errors || {};

  return (
    <Form method="post">
      <div>
        <input name="name" placeholder="Name" />
        {errors.name?.map((err, i) => (
          <span key={i} className="error">{err}</span>
        ))}
      </div>

      <div>
        <input name="email" type="email" placeholder="Email" />
        {errors.email?.map((err, i) => (
          <span key={i} className="error">{err}</span>
        ))}
      </div>

      <div>
        <input name="password" type="password" placeholder="Password" />
        {errors.password?.map((err, i) => (
          <span key={i} className="error">{err}</span>
        ))}
      </div>

      <button type="submit" disabled={navigation.state === 'submitting'}>
        Register
      </button>
    </Form>
  );
}
```

## Submission states

### Navigation state

```tsx
import { useNavigation } from '@putnami/web';

function SubmitButton() {
  const navigation = useNavigation();

  // navigation.state can be:
  // - 'idle': No pending navigation
  // - 'submitting': Form is being submitted
  // - 'loading': Navigation is loading

  const isSubmitting = navigation.state === 'submitting';

  return (
    <button type="submit" disabled={isSubmitting}>
      {isSubmitting ? (
        <>
          <Spinner /> Submitting...
        </>
      ) : (
        'Submit'
      )}
    </button>
  );
}
```

### Optimistic UI

```tsx
import { Form, useNavigation, useActionData } from '@putnami/web';

function TodoItem({ todo }: { todo: Todo }) {
  const navigation = useNavigation();

  // Check if this specific form is submitting
  const isDeleting =
    navigation.state === 'submitting' &&
    navigation.formData?.get('todoId') === todo.id;

  return (
    <div style={{ opacity: isDeleting ? 0.5 : 1 }}>
      <span>{todo.title}</span>
      <Form method="post" action="/todos/delete">
        <input type="hidden" name="todoId" value={todo.id} />
        <button type="submit" disabled={isDeleting}>
          {isDeleting ? 'Deleting...' : 'Delete'}
        </button>
      </Form>
    </div>
  );
}
```

## Fetchers

Use `useFetcher` for form submissions that don't navigate.

### Basic fetcher

```tsx
import { useFetcher } from '@putnami/web';

function NewsletterSignup() {
  const fetcher = useFetcher<{ ok: boolean; error?: string }>();

  return (
    <fetcher.Form method="post" action="/api/newsletter">
      <input name="email" type="email" placeholder="Email" />
      <button type="submit" disabled={fetcher.state === 'submitting'}>
        {fetcher.state === 'submitting' ? 'Subscribing...' : 'Subscribe'}
      </button>

      {fetcher.data?.ok && <p>Thanks for subscribing!</p>}
      {fetcher.data?.error && <p className="error">{fetcher.data.error}</p>}
    </fetcher.Form>
  );
}
```

### Fetcher for data loading

```tsx
import { useFetcher } from '@putnami/web';
import { useEffect } from 'react';

function UserSearch() {
  const fetcher = useFetcher<{ users: User[] }>();
  const [query, setQuery] = useState('');

  useEffect(() => {
    if (query.length > 2) {
      fetcher.load(`/api/users/search?q=${encodeURIComponent(query)}`);
    }
  }, [query]);

  return (
    <div>
      <input
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        placeholder="Search users..."
      />

      {fetcher.state === 'loading' && <p>Searching...</p>}

      {fetcher.data?.users.map((user) => (
        <div key={user.id}>{user.name}</div>
      ))}
    </div>
  );
}
```

### Multiple fetchers

```tsx
import { useFetcher } from '@putnami/web';

function TodoList({ todos }: { todos: Todo[] }) {
  return (
    <ul>
      {todos.map((todo) => (
        <TodoItem key={todo.id} todo={todo} />
      ))}
    </ul>
  );
}

function TodoItem({ todo }: { todo: Todo }) {
  const fetcher = useFetcher();

  const isToggling = fetcher.state === 'submitting';
  const optimisticComplete = isToggling
    ? fetcher.formData?.get('completed') === 'true'
    : todo.completed;

  return (
    <li style={{ opacity: isToggling ? 0.7 : 1 }}>
      <fetcher.Form method="post" action="/api/todos/toggle">
        <input type="hidden" name="id" value={todo.id} />
        <input type="hidden" name="completed" value={String(!todo.completed)} />
        <button type="submit">
          {optimisticComplete ? '✓' : '○'}
        </button>
      </fetcher.Form>
      <span>{todo.title}</span>
    </li>
  );
}
```

## File uploads

### Form with file input

```tsx
import { Form, useActionData, useNavigation } from '@putnami/web';

export default function UploadPage() {
  const actionData = useActionData<{ ok: boolean; url?: string }>();
  const navigation = useNavigation();

  return (
    <Form method="post" encType="multipart/form-data">
      <input type="file" name="file" accept="image/*" />
      <button type="submit" disabled={navigation.state === 'submitting'}>
        Upload
      </button>

      {actionData?.ok && (
        <img src={actionData.url} alt="Uploaded" />
      )}
    </Form>
  );
}
```

### File upload action

```ts
// src/app/upload/action.ts
import { action } from '@putnami/web';

export default action(async (ctx) => {
  const formData = await ctx.req.formData();
  const file = formData.get('file') as File;

  if (!file || file.size === 0) {
    return { ok: false, error: 'No file provided' };
  }

  // Validate file type
  if (!file.type.startsWith('image/')) {
    return { ok: false, error: 'Only images allowed' };
  }

  // Save file
  const buffer = await file.arrayBuffer();
  const path = `/uploads/${crypto.randomUUID()}-${file.name}`;
  await Bun.write(`public${path}`, buffer);

  return { ok: true, url: path };
});
```

## Programmatic submission

### useSubmit

```tsx
import { useSubmit } from '@putnami/web';

function AutoSaveForm() {
  const submit = useSubmit();
  const formRef = useRef<HTMLFormElement>(null);

  // Auto-save on change
  const handleChange = () => {
    if (formRef.current) {
      submit(formRef.current, { replace: true });
    }
  };

  return (
    <form ref={formRef} method="post" onChange={handleChange}>
      <input name="title" />
      <textarea name="content" />
    </form>
  );
}
```

### Submit with custom data

```tsx
import { useSubmit } from '@putnami/web';

function DeleteButton({ id }: { id: string }) {
  const submit = useSubmit();

  const handleDelete = () => {
    if (confirm('Are you sure?')) {
      submit(
        { id, action: 'delete' },
        { method: 'post', action: '/api/items' }
      );
    }
  };

  return <button onClick={handleDelete}>Delete</button>;
}
```

## Action patterns

### Redirect after action

```ts
// action.ts
import { action } from '@putnami/web';
import { HttpResponse } from '@putnami/application';

export default action(async (ctx) => {
  const body = await ctx.body<CreatePostInput>();
  const post = await createPost(body);

  // Redirect to the new post
  return HttpResponse.redirect(`/posts/${post.id}`);
});
```

### Return errors vs throw

```ts
import { action } from '@putnami/web';

// Returning errors (for validation)
export default action(async (ctx) => {
  const body = await ctx.body<Input>();

  if (!body.email) {
    return { ok: false, error: 'Email required' };
  }

  return { ok: true };
});

// Throwing errors (for unexpected failures)
export default action(async (ctx) => {
  const body = await ctx.body<Input>();
  const user = await findUser(body.userId);

  if (!user) {
    throw new NotFoundException('User not found');
  }

  return { ok: true };
});
```

### Multiple actions in one route

```ts
// action.ts
import { action } from '@putnami/web';

export default action(async (ctx) => {
  const body = await ctx.body<{ intent: string; [key: string]: unknown }>();

  switch (body.intent) {
    case 'create':
      return handleCreate(body);
    case 'update':
      return handleUpdate(body);
    case 'delete':
      return handleDelete(body);
    default:
      return { ok: false, error: 'Unknown intent' };
  }
});
```

```tsx
// page.tsx
<Form method="post">
  <input type="hidden" name="intent" value="create" />
  {/* ... */}
</Form>

<Form method="post">
  <input type="hidden" name="intent" value="delete" />
  <input type="hidden" name="id" value={item.id} />
  <button type="submit">Delete</button>
</Form>
```

## CSRF protection

When the server enables CSRF protection via `http({ csrf: true })`, form actions and the `useFetch` hook **automatically include the CSRF token** in the `X-CSRF-Token` header. No client-side code changes are required.

- `<Form>` POST submissions — token is read from the `_csrf` cookie and attached automatically via JavaScript.
- `useFetch` with `POST`, `PUT`, `DELETE`, `PATCH` — token is injected automatically. `GET`, `HEAD`, and `OPTIONS` are skipped.

### Plain HTML forms (no-JS / progressive enhancement)

The `<Form>` component requires JavaScript to inject the CSRF header. For plain `<form>` elements or progressive enhancement scenarios where JavaScript may be disabled, use the `<CsrfInput />` component to include the CSRF token as a hidden form field:

```tsx
import { CsrfInput } from '@putnami/web';

export default function ContactPage() {
  return (
    <form method="post" action="/contact">
      <CsrfInput />
      <input name="name" required />
      <input name="email" type="email" required />
      <button type="submit">Send</button>
    </form>
  );
}
```

`<CsrfInput />` renders `<input type="hidden" name="_csrf" value="..." />`. During SSR, the token is read from the request cookie; on the client, it reads from `document.cookie`. The CSRF middleware accepts the token from either the `X-CSRF-Token` header or the `_csrf` form body field.

You can customize the field name to match your CSRF configuration:

```tsx
<CsrfInput name="csrf_token" />
```

### Manual fetch calls

For `fetch()` calls outside of `useFetch` or `<Form>`, read the token from the cookie manually:

```ts
const csrfToken = document.cookie
  .split('; ')
  .find(c => c.startsWith('_csrf='))
  ?.split('=')[1];

fetch('/api/submit', {
  method: 'POST',
  headers: { 'X-CSRF-Token': csrfToken },
  body: JSON.stringify(data),
});
```

See [HTTP & Middleware — CSRF protection](/docs/frameworks/typescript/http-and-middleware#csrf-protection) for server configuration options.

## Related guides

- [Web](/docs/frameworks/typescript/web)
- [React Routing](/docs/frameworks/typescript/react-routing)
- [HTTP & Middleware](/docs/frameworks/typescript/http-and-middleware)
- [Errors & Responses](/docs/frameworks/typescript/errors-and-responses)
