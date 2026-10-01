import { useRouteError } from 'react-router';
import { shouldExposeClientErrors } from './expose-errors';

export function ErrorBoundary() {
  const error = useRouteError() as Error;
  // Take the decision at render time, not at bundle time. Written inline,
  // `process.env.NODE_ENV !== 'production'` is constant-folded to `true` when
  // this package is published, which froze the guard open in every consumer
  // build and leaked `error.stack` in production. This boundary runs in the
  // browser too, where there is no environment to read at all, so the answer
  // comes from the flag SSR injected — absent means "do not expose".
  const isDev = shouldExposeClientErrors();

  return (
    <>
      <h2>Oups, we met an issue!</h2>
      <div>
        <h3>{isDev ? error.message : 'An unexpected error occurred'}</h3>
        {isDev && <pre>{error.stack}</pre>}
      </div>
    </>
  );
}
