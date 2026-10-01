import { createContext, useContext, type ReactNode } from 'react';
import { ANONYMOUS_SECURITY_CONTEXT, type ClientSecurityContext } from '../../shared/security.types';

// ---------------------------------------------------------------------------
// React Context
// ---------------------------------------------------------------------------

const SecurityContext = createContext<ClientSecurityContext>(ANONYMOUS_SECURITY_CONTEXT);

/**
 * Provider that makes the security context available to all descendant components.
 * Injected automatically by `hydratePage()`.
 */
export function SecurityContextProvider({ children, value }: { children?: ReactNode; value: ClientSecurityContext }) {
  return <SecurityContext.Provider value={value}>{children}</SecurityContext.Provider>;
}

/**
 * Access the current user's security context (roles, scopes, authenticated).
 *
 * This is a UX convenience only — the server enforces all access rules.
 * The returned context is a snapshot from SSR time and may become stale
 * if the user's session or roles change.
 *
 * @example
 * ```tsx
 * function AdminNav() {
 *   const { authenticated, roles } = useSecurityContext();
 *   if (!roles.includes('admin')) return null;
 *   return <nav>Admin Menu</nav>;
 * }
 * ```
 */
export function useSecurityContext(): ClientSecurityContext {
  return useContext(SecurityContext);
}

// ---------------------------------------------------------------------------
// Module-level accessor (for use outside React, e.g. in loaders)
// ---------------------------------------------------------------------------

let _moduleContext: ClientSecurityContext = ANONYMOUS_SECURITY_CONTEXT;

/** Set the module-level security context. Called once during hydration. */
export function setSecurityContext(ctx: ClientSecurityContext): void {
  _moduleContext = ctx;
}

/**
 * Get the current security context outside of React components.
 * Useful in React Router loader functions and other non-component code.
 */
export function getSecurityContext(): ClientSecurityContext {
  return _moduleContext;
}
