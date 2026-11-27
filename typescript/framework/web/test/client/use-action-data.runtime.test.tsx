import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { renderToString } from 'react-dom/server';
import { createMemoryRouter, Outlet, RouterProvider } from 'react-router';
import { useActionData } from '../../src/client/hooks/use-action-data.hook';

/** The page reads what its post answered. */
function Page() {
  const result = useActionData<{ saved: string }>();
  return <p>{result?.ok ? `saved ${result.saved}` : 'idle'}</p>;
}

/** The routes the generated client writes for `n/layout.tsx`, `n/page.tsx` and `n/action.ts`. */
function routes() {
  const action = async () => ({ ok: true, status: 200, saved: 'n' });
  return [
    {
      path: '/',
      id: 'root',
      Component: Outlet,
      children: [
        {
          path: 'n',
          id: 'root-n-layout',
          Component: Outlet,
          action,
          children: [{ path: '', id: 'root-n-page', index: true, Component: Page, action }],
        },
      ],
    },
  ];
}

describe('useActionData under a layout at the same address', () => {
  specTest(
    'returns the result of a form post to the address, with or without ?index',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'route-graph',
      check: 'a-post-to-a-layout-address-reaches-its-index-page-action',
    },
    async () => {
      for (const to of ['/n', '/n?index']) {
        const router = createMemoryRouter(routes(), { initialEntries: ['/n'] });
        await router.initialize();
        expect(renderToString(<RouterProvider router={router} />)).toContain('idle');

        await router.navigate(to, { formMethod: 'post', formData: new FormData() });

        expect(renderToString(<RouterProvider router={router} />)).toContain('saved n');
        router.dispose();
      }
    },
  );
});
