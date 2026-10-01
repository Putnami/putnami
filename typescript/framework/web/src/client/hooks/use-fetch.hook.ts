import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { getCsrfToken } from '../form/csrf';
import { clientFetchTimeoutMs } from '../form/fetch-timeout';

type UseFetchState<T> = {
  data?: T;
  error?: Error;
  loading: boolean;
};

type FetchRequest = {
  token: number;
  init?: RequestInit;
};

const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS']);

// `AbortSignal.timeout()` rejects with a DOMException named 'TimeoutError'.
// Check the name rather than `instanceof DOMException` so the detection also
// works in runtimes where the rejection reason is a plain Error subclass.
const isTimeoutAbort = (err: unknown): boolean =>
  typeof err === 'object' && err !== null && (err as { name?: unknown }).name === 'TimeoutError';

const serializeInit = (value?: RequestInit) => {
  if (!value) return '';
  try {
    return JSON.stringify(value, Object.keys(value).sort());
  } catch {
    // Fallback to force effect rerun if init is not serializable
    return `${Date.now()}`;
  }
};

export const useFetch = <T>(apiPath: RequestInfo | URL, init?: RequestInit) => {
  const [state, setState] = useState<UseFetchState<T>>({ loading: true });
  const [reloadToken, setReloadToken] = useState(0);
  const initKey = useMemo(() => serializeInit(init), [init]);

  // Give the fetch effect a dependency whose identity is stable across renders
  // unless the request actually changes. A new `request` object is produced only
  // when the serialized init (`initKey`) differs or `refetch()` bumps the token —
  // so an inline `init` literal (new identity every render) no longer re-runs the
  // effect, while a genuine change or explicit refetch does.
  const initKeyRef = useRef(initKey);
  const requestRef = useRef<FetchRequest>({ token: reloadToken, init });
  if (initKeyRef.current !== initKey || requestRef.current.token !== reloadToken) {
    initKeyRef.current = initKey;
    requestRef.current = { token: reloadToken, init };
  }
  const request = requestRef.current;

  const refetch = useCallback(() => setReloadToken((count) => count + 1), []);

  useEffect(() => {
    const controller = new AbortController();
    const requestInit = request.init;

    const run = async () => {
      setState((prev) => ({ ...prev, loading: true, error: undefined }));
      const timeoutMs = clientFetchTimeoutMs();
      try {
        const method = (requestInit?.method ?? 'GET').toUpperCase();

        // Normalize the caller's headers through the Headers API so every valid
        // `RequestInit.headers` form works: a `Headers` instance, a
        // `[string, string][]` array, or a record. Object-spreading a `Headers`
        // yields `{}` and spreading an array yields index keys, so the previous
        // record-cast silently dropped two of the three valid forms.
        const merged = new Headers(requestInit?.headers);

        // Auto-inject the CSRF token for state-changing requests. Set it LAST via
        // `merged.set` so a caller-supplied `X-CSRF-Token` can't clobber the
        // framework's CSRF protection.
        if (!SAFE_METHODS.has(method)) {
          const csrfToken = getCsrfToken();
          if (csrfToken) {
            merged.set('X-CSRF-Token', csrfToken);
          }
        }

        const response = await fetch(apiPath, {
          ...requestInit,
          headers: merged,
          // Bound the request by the same configurable client timeout the
          // framework's own loader/action fetches use (see action.handler.ts /
          // loader.handler.ts), while keeping the unmount/refetch abort: either
          // signal may cancel the request, so a hung server cannot leave the
          // hook in `loading: true` forever.
          signal: AbortSignal.any([controller.signal, AbortSignal.timeout(timeoutMs)]),
        });
        if (!response.ok) {
          throw new Error(`HTTP request failed: received status ${response.status}`);
        }
        const body = (await response.json()) as T;
        if (!controller.signal.aborted) {
          setState({ data: body, loading: false });
        }
      } catch (err) {
        if (controller.signal.aborted) {
          // Unmount/refetch abort: the component is gone or a newer request
          // supersedes this one — never surface it as an error state.
          return;
        }
        // A timeout abort is a real failure the caller must see; translate the
        // opaque 'TimeoutError' abort into an actionable error message.
        const error = isTimeoutAbort(err)
          ? new Error(`HTTP request timed out after ${timeoutMs}ms`, { cause: err })
          : err instanceof Error
            ? err
            : new Error(String(err));
        setState({ loading: false, error });
      }
    };

    run();

    return () => controller.abort();
  }, [apiPath, request]);

  return { ...state, refetch };
};
