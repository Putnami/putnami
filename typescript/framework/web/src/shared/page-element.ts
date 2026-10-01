import { type ComponentType, createElement, type ReactElement, Suspense } from 'react';

/**
 * Build the element a page route renders: the page component inside one
 * `Suspense` boundary.
 *
 * The server and the generated client routes both call this function. React
 * hydrates the server HTML in place only when both trees carry the same
 * boundaries, so neither side builds a page element another way.
 */
export function createPageElement(component: ComponentType): ReactElement {
  return createElement(Suspense, { fallback: null }, createElement(component));
}
