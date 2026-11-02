import { createContext, createElement, type ReactNode, useContext, useMemo } from 'react';
import { formatIcu, type IcuNode, type IcuValues, parseIcu } from './icu';
import type { LocaleSettings } from './locale';
import type { MessageFormatter } from './message-types';

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
 * });
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

/** Build the formatters of a namespace for a locale, outside React. */
export function translator<D extends Record<string, MessageDefinition>>(
  bundle: Messages<D>,
  locale: string,
  translations: LocaleMessages = {},
): Translator<D> {
  const own = translations[bundle.namespace] ?? {};
  const out: Record<string, (values?: IcuValues) => string> = {};
  for (const [key, definition] of Object.entries(bundle.definitions)) {
    const text = own[key] || definitionText(definition);
    out[key] = (values?: IcuValues) => formatIcu(nodesOf(text), locale, values);
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
