import { useRouteLoaderData as _useRouteLoaderData } from 'react-router';

/**
 * Returns the loader data for a specific route by its ID.
 *
 * @template R - The expected return type of the loader data
 * @param id - The route ID to fetch loader data for
 * @returns The loader data cast to type R
 *
 * @example
 * ```tsx
 * interface RootData {
 *   user: User;
 * }
 *
 * function ChildComponent() {
 *   const rootData = useRouteLoaderData<RootData>('root');
 *   return <div>Welcome, {rootData.user.name}</div>;
 * }
 * ```
 */
export const useRouteLoaderData = <R = unknown>(id: string): R => _useRouteLoaderData(id) as R;
