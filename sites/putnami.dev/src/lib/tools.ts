/**
 * Product-surface information architecture — the backbone of the "un-blobbed"
 * docs navigation.
 *
 * Organizing axis: workflow surface first. Tooling, TypeScript, Go, Python,
 * and the managed Platform are first-level entry points because those are the
 * questions readers arrive with. Platform graduated from secondary once its
 * docs shipped as a real section (deploys, build cache, registries, runtime
 * services) via the site-content bundle.
 *
 * Client-safe: no server-only imports, so islands (command palette, mobile
 * drawer) and statically rendered pages can both consume it.
 */

import { type SupportStatus, type SupportSubject, statusLabel } from './support/catalog';

export interface Tool {
  /** Stable id used in URLs/keys. */
  id: string;
  /** Full display name, e.g. "TypeScript". */
  name: string;
  /** Short label used in chips, switchers, nav links, and the home strip. */
  short: string;
  /** Monospace glyph rendered in the tool badge. */
  glyph: string;
  /** Brand accent used to tint the glyph badge. */
  accent: string;
  /** One-line description shown on the docs hub cards. */
  blurb: string;
  /**
   * The subject in `putnami.support.json` whose reviewed status represents this
   * surface, or `null` when the catalog deliberately classifies nothing here.
   *
   * A status is a public commitment with exactly one reviewed home, so this
   * file names the subject and never restates its value. Surfaces resolve the
   * status server-side through `src/app/docs/loader.ts`; keeping only the
   * subject here also keeps this module client-safe.
   */
  supportSubject: SupportSubject | null;
  /** Approximate page count, shown as a quiet quantity cue. */
  pages: number;
  /** Entry-point URL for this surface's documentation. */
  href: string;
  /** First practical guide for readers who already chose this surface. */
  startHref?: string;
  /** One-line description for the surface-specific start guide. */
  startBlurb?: string;
}

export const TOOLS: Record<string, Tool> = {
  ts: {
    id: 'ts',
    name: 'TypeScript',
    short: 'TypeScript',
    glyph: 'TS',
    accent: '#3178c6',
    blurb: 'HTTP, React SSR, SQL and DI as minimal, opinionated primitives.',
    supportSubject: { kind: 'package', id: '@putnami/typescript' },
    pages: 35,
    href: '/docs/frameworks/typescript',
    startHref: '/docs/frameworks/typescript/getting-started',
    startBlurb: 'Create a web app or API service and run it locally.',
  },
  go: {
    id: 'go',
    name: 'Go',
    short: 'Go',
    glyph: 'GO',
    accent: '#00add8',
    blurb: 'The same workspace model and conventions, compiled and concurrent.',
    supportSubject: { kind: 'package', id: '@putnami/go' },
    pages: 25,
    href: '/docs/frameworks/go',
    startHref: '/docs/frameworks/go/getting-started',
    startBlurb: 'Create a Go service, run tests, and serve it from the workspace.',
  },
  py: {
    id: 'py',
    name: 'Python',
    short: 'Python',
    glyph: 'PY',
    accent: '#ffd343',
    blurb: 'Experimental, explicit opt-in workspace jobs and templates for Python experiments.',
    supportSubject: { kind: 'package', id: '@putnami/python' },
    pages: 5,
    href: '/docs/frameworks/python',
    startHref: '/docs/frameworks/python/getting-started',
    startBlurb: 'Explicitly create a FastAPI service or package experiment in the shared graph.',
  },
  tooling: {
    id: 'tooling',
    name: 'Tooling & workspace',
    short: 'Tooling',
    glyph: 'CLI',
    accent: '#3fb950',
    blurb: 'The CLI, build system and test runner that drive every workspace.',
    supportSubject: { kind: 'package', id: '@putnami/cli' },
    pages: 6,
    href: '/docs/tooling-%26-workspace',
  },
  cloud: {
    id: 'cloud',
    name: 'Platform',
    short: 'Platform',
    glyph: 'PLT',
    accent: '#2dd4bf',
    blurb: 'Deploys, build cache, registries, and managed runtime services.',
    // The managed platform is a hosted service, not a package or protocol you
    // depend on, so the support catalog deliberately classifies nothing for it.
    supportSubject: null,
    pages: 14,
    href: '/docs/platform',
  },
};

/**
 * Display order for tool grids, the home strip, and the command palette.
 *
 * Python keeps its documented position between Go and the Platform rather than
 * being appended last, so the reading order stays language-then-platform.
 */
export const TOOL_ORDER = ['tooling', 'ts', 'go', 'py', 'cloud'] as const;

const STATUS_COLORS: Record<SupportStatus, string> = {
  stable: 'var(--color-success)',
  preview: 'var(--color-primary)',
  experimental: 'var(--color-info)',
};

/**
 * Maps a reviewed support status to a semantic color token + human label.
 *
 * The vocabulary is the support protocol's closed three-value set. There is
 * deliberately no `evolving` or `beta` presentation: those are not wire values,
 * so a page that rendered one would be showing a status no catalog can hold.
 */
export function statusMeta(status: SupportStatus): { color: string; label: string } {
  return { color: STATUS_COLORS[status], label: statusLabel(status) };
}

/** Ordered list of tools for iteration. */
export function toolList(): Tool[] {
  return TOOL_ORDER.map((id) => TOOLS[id]);
}

/**
 * Ordered list of language surfaces with a dedicated start guide.
 *
 * Also feeds the navbar's Frameworks menu, where the languages share one
 * trigger instead of holding first-level anchors. Python is listed with the
 * other two because a listing may present it — its blurb states experimental —
 * but it never gets an anchor of its own: that would imply a parity the
 * workspace does not offer (`python-is-browseable-not-anchored` in the spec).
 */
export function frameworkStartList(): Tool[] {
  return [TOOLS['ts'], TOOLS['go'], TOOLS['py']];
}

/** Human-readable page count label. */
export function pageCountLabel(count: number): string {
  return `${count} ${count === 1 ? 'page' : 'pages'}`;
}
