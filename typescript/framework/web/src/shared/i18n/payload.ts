import type { LocaleServing, LocaleSettings } from './locale';
import type { I18nState, LocaleMessages } from './messages';

/** Id of the `<script type="application/json">` element that carries the payload. */
export const I18N_PAYLOAD_ID = 'putnami-i18n';

/** What the server tells a browser tree about its locale. */
export interface I18nPayload {
  settings: LocaleSettings;
  locale: string;
  path: string;
  served: LocaleServing;
  messages: LocaleMessages;
  /** The `i18n.suggestion` namespace's translations, by non-default locale. */
  suggestions?: Readonly<Record<string, Readonly<Record<string, string>>>>;
}

/**
 * Read the locale payload the server rendered into the page. Returns
 * `undefined` when the page carries none (i18n off) or it cannot be read; the
 * tree then renders its default text, as the server did without i18n.
 */
export function readI18nPayload(doc: Document | undefined = globalThis.document): I18nState | undefined {
  const element = doc?.getElementById(I18N_PAYLOAD_ID);
  if (!element?.textContent) return undefined;
  try {
    const payload = JSON.parse(element.textContent) as I18nPayload;
    return {
      settings: payload.settings,
      locale: payload.locale,
      path: payload.path,
      served: payload.served ?? 'prefix',
      messages: payload.messages,
      ...(payload.suggestions ? { suggestions: payload.suggestions } : {}),
    };
  } catch {
    return undefined;
  }
}
