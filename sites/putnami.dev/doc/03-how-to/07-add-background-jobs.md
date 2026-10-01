# Add background jobs

You will run a simple in-process background task and trigger it from a route.

## Steps

### 1) Create a job function

Create `apps/web/src/jobs/send-welcome.ts`:

```ts
import { useLogger } from '@putnami/runtime';

const logger = useLogger('jobs');

export async function sendWelcomeEmail(email: string) {
  await new Promise((resolve) => setTimeout(resolve, 250));
  logger.info('Sent welcome email', { email });
}
```

### 2) Trigger it from a route

Create `apps/web/src/app/jobs/post.ts`:

```ts
import { endpoint } from '@putnami/application';
import { sendWelcomeEmail } from '../../jobs/send-welcome';

export default endpoint(async () => {
  void sendWelcomeEmail('hello@example.com');
  return { queued: true };
});
```

### 3) Observe output

```bash
putnami serve web
```

Call the route, then watch the logs.

## Result

You now have a background task running asynchronously in-process.
