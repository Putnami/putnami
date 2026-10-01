import { useLoaderData as _useLoaderData } from 'react-router';

/**
 * Returns the loader data for the current route.
 *
 * This hook is a typed wrapper around React Router's `useLoaderData`.
 *
 * @template R - The expected return type of the loader data
 * @returns The loader data cast to type R
 *
 * **Option 1 — Inline type parameter:**
 * ```tsx
 * const data = useLoaderData<{ name: string; email: string }>();
 * ```
 *
 * **Option 2 — Infer from the loader handler (recommended):**
 * ```tsx
 * // loader.ts
 * const handler = async (ctx: EndpointRequestContext) => {
 *   return { user: await getUser(ctx.params.id) };
 * };
 * export default loader(handler);
 * export type LoaderData = InferLoaderData<typeof handler>;
 *
 * // page.tsx
 * import type { LoaderData } from './loader';
 * const data = useLoaderData<LoaderData>();
 * ```
 */
export const useLoaderData = <R = unknown>(): R => _useLoaderData() as R;
