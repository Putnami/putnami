import { describe, expect, it } from 'bun:test';
import {
  isRouteErrorResponse,
  useActionData,
  useFetch,
  useLoaderData,
  useRouteError,
  useRouteLoaderData,
  usePrefetch,
  type ErrorResponse,
  type PrefetchBehavior,
} from '../../src/client/hooks';

/**
 * These tests verify the type signatures and export availability of hooks.
 * Full integration tests would require a React renderer and router context.
 */
describe('React Hooks Exports', () => {
  describe('useLoaderData', () => {
    it('is exported as a function', () => {
      expect(typeof useLoaderData).toBe('function');
    });

    it('returns typed data when called in context', () => {
      // Type assertion test - verifies generic signature works
      type LoaderData = { message: string };
      const hook: () => LoaderData = useLoaderData<LoaderData>;
      expect(typeof hook).toBe('function');
    });
  });

  describe('useActionData', () => {
    it('is exported as a function', () => {
      expect(typeof useActionData).toBe('function');
    });

    it('has correct return type signature', () => {
      // Type assertion test - verifies nullable return with status fields
      type ActionData = { success: boolean };
      const hook: () => (ActionData & { ok: boolean; status: number }) | undefined = useActionData<ActionData>;
      expect(typeof hook).toBe('function');
    });
  });

  describe('useRouteLoaderData', () => {
    it('is exported as a function', () => {
      expect(typeof useRouteLoaderData).toBe('function');
    });

    it('accepts route id parameter', () => {
      // Type assertion test - verifies generic signature with id param
      type RouteData = { data: string };
      const hook: (id: string) => RouteData = useRouteLoaderData<RouteData>;
      expect(typeof hook).toBe('function');
    });
  });

  describe('useRouteError', () => {
    it('is exported as a function', () => {
      expect(typeof useRouteError).toBe('function');
    });

    it('defaults to unknown so a boundary must narrow before reading the value', () => {
      // Type assertion test - the default return type stays honest about what a
      // route may throw; naming the type is opt-in.
      const hook: () => unknown = useRouteError;
      const typed: () => Error = useRouteError<Error>;
      expect(typeof hook).toBe('function');
      expect(typeof typed).toBe('function');
    });

    it('exports isRouteErrorResponse so a boundary never imports react-router', () => {
      expect(typeof isRouteErrorResponse).toBe('function');

      const thrown: unknown = { status: 404, statusText: 'Not Found', data: null, internal: false };
      expect(isRouteErrorResponse(thrown)).toBe(true);
      expect(isRouteErrorResponse(new Error('boom'))).toBe(false);
    });

    it('exports the ErrorResponse type', () => {
      // Type assertion test - verifies the narrowed shape is nameable.
      const response: ErrorResponse = { status: 500, statusText: 'Internal Server Error', data: 'boom' };
      expect(response.status).toBe(500);
    });
  });

  describe('useFetch', () => {
    it('is exported as a function', () => {
      expect(typeof useFetch).toBe('function');
    });

    it('returns typed state shape', () => {
      type FetchData = { message: string };
      // Type assertion: result should include refetch and loading fields
      type FetchReturn = ReturnType<typeof useFetch<FetchData>>;
      const _state: FetchReturn | undefined = undefined;
      expect(_state).toBeUndefined();
    });
  });
  describe('usePrefetch', () => {
    it('is exported as a function', () => {
      expect(typeof usePrefetch).toBe('function');
    });

    it('has correct type signature', () => {
      // Type assertion test - verifies generic signature with To and PrefetchBehavior
      type PrefetchReturn = ReturnType<typeof usePrefetch>;
      const _return: PrefetchReturn | undefined = undefined;
      expect(_return).toBeUndefined();
    });

    it('PrefetchBehavior type is exported', () => {
      // Type assertion test - verifies PrefetchBehavior type
      const _behavior: PrefetchBehavior = 'intent';
      const _behavior2: PrefetchBehavior = 'render';
      const _behavior3: PrefetchBehavior = 'none';
      expect(_behavior).toBe('intent');
      expect(_behavior2).toBe('render');
      expect(_behavior3).toBe('none');
    });
  });

  describe('Other Hooks', () => {
    it('exports standard React Router hooks', async () => {
      const hooks = await import('../../src/client/hooks');

      expect(typeof hooks.useNavigate).toBe('function');
      expect(typeof hooks.useLocation).toBe('function');
      expect(typeof hooks.useParams).toBe('function');
      expect(typeof hooks.useSearchParams).toBe('function');
      expect(typeof hooks.useSubmit).toBe('function');
      expect(typeof hooks.useNavigation).toBe('function');
      expect(typeof hooks.useMatch).toBe('function');
      expect(typeof hooks.useOutlet).toBe('function');
      expect(typeof hooks.useOutletContext).toBe('function');
      expect(typeof hooks.useResolvedPath).toBe('function');
      expect(typeof hooks.useRevalidator).toBe('function');
      expect(typeof hooks.useRoutes).toBe('function');
      expect(typeof hooks.useBlocker).toBe('function');
      expect(typeof hooks.useBeforeUnload).toBe('function');
      expect(typeof hooks.useFormAction).toBe('function');
      expect(typeof hooks.useLinkClickHandler).toBe('function');
      expect(typeof hooks.useNavigationType).toBe('function');
      expect(typeof hooks.useRouteError).toBe('function');
      expect(typeof hooks.isRouteErrorResponse).toBe('function');
    });
  });
});
