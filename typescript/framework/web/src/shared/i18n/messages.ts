import { createContext, createElement, Fragment, type ReactNode, useContext, useMemo } from 'react';
import {
  formatIcu,
  formatIcuParts,
  type IcuNode,
  type IcuTag,
  type IcuValue,
  type IcuValues,
  icuArguments,
  parseIcu,
} from './icu';
import type { LocaleServing, LocaleSettings } from './locale';
import type { MessageFormatter, TagRenderer } from './message-types';

/** A default message: its ICU text, and optional context for translators. */
export type MessageDefinition = string | { readonly text: string; readonly context?: string };

type TextOf<D> = D extends string ? D : D extends { readonly text: infer T extends string } ? T : string;

/** The declared messages of one namespace. */
export interface Messages<D extends Record<string, MessageDefinition> = Record<string, MessageDefinition>> {
  readonly namespace: string;
  readonly definitions: D;
}

const NAMESPACE = /^[a-z0-9]+(?:[-./][a-z0-9]+)*$/;
const KEY = /^[A-Za-z_][A-Za-z0-9_]*$/;

/**
 * Declare the default (source-locale) messages of a namespace. Put the call in
 * a `*.messages.ts` file next to the component: the build reads those files to
 * keep each locale's catalog aligned with the code.
 *
 * @example
 * export default messages('home', {
 *   title: 'Pricing',
 *   seats: { text: '{count, plural, one {# seat} other {# seats}}', context: 'Price card' },
 *   terms: 'Read the <link>terms</link>.',
 * });
 *
 * // A message with tags renders a React node; each tag takes a function.
 * t.terms({ link: (chunks) => <Link to='/terms'>{chunks}</Link> });
 */
export function messages<const D extends Record<string, MessageDefinition>>(
  namespace: string,
  definitions: D,
): Messages<D> {
  if (!NAMESPACE.test(namespace)) {
    throw new Error(
      `messages(): namespace '${namespace}' must be lowercase words separated by '-', '.' or '/', such as 'home' or 'docs/sidebar'`,
    );
  }
  for (const key of Object.keys(definitions)) {
    if (!KEY.test(key)) throw new Error(`messages('${namespace}'): key '${key}' must be an identifier`);
  }
  return { namespace, definitions };
}

/** The default text of a definition. */
export function definitionText(definition: MessageDefinition): string {
  return typeof definition === 'string' ? definition : definition.text;
}

/** Translations for one locale, by namespace then key. */
export type LocaleMessages = Readonly<Record<string, Readonly<Record<string, string>>>>;

/** What a rendered tree knows about its locale. */
export interface I18nState {
  readonly settings: LocaleSettings;
  readonly locale: string;
  /** The current page's path without its locale prefix. */
  readonly path: string;
  /** Where the page is served: at its locale's domain, on the site, or under its prefix elsewhere. */
  readonly served: LocaleServing;
  /** Translations of the current locale; empty for the default locale. */
  readonly messages: LocaleMessages;
  /** Namespaces read while rendering, collected on the server for the payload. */
  readonly rendered?: Set<string>;
}

export const I18nContext = createContext<I18nState | undefined>(undefined);

export function I18nProvider({ value, children }: { value: I18nState; children?: ReactNode }) {
  return createElement(I18nContext.Provider, { value }, children);
}

const parsed = new Map<string, IcuNode[]>();

function nodesOf(text: string): IcuNode[] {
  let nodes = parsed.get(text);
  if (!nodes) {
    nodes = parseIcu(text);
    parsed.set(text, nodes);
  }
  return nodes;
}

export type Translator<D extends Record<string, MessageDefinition>> = {
  readonly [K in keyof D]: MessageFormatter<TextOf<D[K]>>;
};

function hasTags(nodes: IcuNode[]): boolean {
  return [...icuArguments(nodes).values()].includes('tag');
}

/** Format a message with tags: function values render tags, the others fill arguments. */
function formatRich(nodes: IcuNode[], locale: string, values: Record<string, unknown> = {}): ReactNode {
  const plain: IcuValues = {};
  const tags: Record<string, IcuTag<ReactNode>> = {};
  for (const [name, value] of Object.entries(values)) {
    if (typeof value === 'function') {
      const render = value as TagRenderer;
      tags[name] = (parts) => render(createElement(Fragment, null, ...parts));
    } else {
      plain[name] = value as IcuValue;
    }
  }
  // Parts passed as separate children need no keys.
  return createElement(Fragment, null, ...formatIcuParts(nodes, locale, plain, tags));
}

/**
 * Build the formatters of a namespace for a locale, outside React. A message
 * whose default text has tags formats to a React node, any other to a string.
 * Reading the tags parses every default text here, so an invalid one throws
 * for the whole namespace on first use. The build of a project with an
 * `i18n` block refuses an invalid default text before that.
 */
export function translator<D extends Record<string, MessageDefinition>>(
  bundle: Messages<D>,
  locale: string,
  translations: LocaleMessages = {},
): Translator<D> {
  const own = translations[bundle.namespace] ?? {};
  const out: Record<string, (values?: Record<string, unknown>) => ReactNode> = {};
  for (const [key, definition] of Object.entries(bundle.definitions)) {
    const fallback = definitionText(definition);
    const text = own[key] || fallback;
    out[key] = hasTags(nodesOf(fallback))
      ? (values) => formatRich(nodesOf(text), locale, values)
      : (values) => formatIcu(nodesOf(text), locale, values as IcuValues);
  }
  return out as Translator<D>;
}

/**
 * Read a namespace's messages in the current locale. A translation missing
 * from the catalog falls back to the default text; a production build refuses
 * that for a published locale.
 */
export function useT<D extends Record<string, MessageDefinition>>(bundle: Messages<D>): Translator<D> {
  const state = useContext(I18nContext);
  state?.rendered?.add(bundle.namespace);
  const locale = state?.locale ?? state?.settings.defaultLocale ?? 'en';
  const translations = state?.messages;
  return useMemo(() => translator(bundle, locale, translations), [bundle, locale, translations]);
}
