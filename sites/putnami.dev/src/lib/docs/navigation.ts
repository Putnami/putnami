/**
 * Client-safe navigation types and utilities.
 * Server-only functions are in navigation.server.ts
 */

export interface NavItem {
  name: string;
  contentPath?: string;
  order: number;
  children?: NavItem[];
}

/**
 * Convert contentPath to URL path.
 * e.g., "01-getting-started/02-introduction.md" -> "/docs/getting-started/introduction"
 */
export function toUrlPath(contentPath: string | undefined): string {
  if (!contentPath) return '#';
  return (
    '/docs/' +
    contentPath
      .replace(/\.md$/, '')
      .replace(/\/?\d+-/g, '/')
      .replace(/^\//, '')
  );
}
