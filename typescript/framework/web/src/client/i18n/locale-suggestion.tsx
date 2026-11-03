import { type ReactNode, useContext, useEffect, useState } from 'react';
import { LOCALE_PARAM, suggestedLocale, textDirection } from '../../shared/i18n/locale';
import {
  I18nContext,
  type I18nState,
  type MessageDefinition,
  type Messages,
  translator,
} from '../../shared/i18n/messages';
import { type LocaleInfo, rememberLocale, useLocale } from './locale-switcher';

/** The messages of a suggestion: one line whose `<link>` tag opens the page, and the dismiss button's label. */
export type LocaleSuggestionMessages = Messages<{ message: MessageDefinition; dismiss: MessageDefinition }>;

export interface LocaleSuggestionProps {
  /** The `i18n.suggestion` namespace, such as `messages('locale-suggestion', { message: 'Read this page in <link>English</link>.', dismiss: 'Dismiss' })`. */
  messages: LocaleSuggestionMessages;
  className?: string;
}

/**
 * Offer the current page in the locale the reader prefers, in that locale:
 * the locale they chose, else the one their browser languages match first.
 * It renders nothing on the server, so a page stays the same bytes for every
 * reader, and decides once the browser runs it. Put it in an island. Dismissing
 * it records the page's locale as the reader's choice.
 */
export function LocaleSuggestion({ messages, className }: LocaleSuggestionProps) {
  const state = useContext(I18nContext);
  const info = useLocale();
  const { locale, path } = info;
  const [suggested, setSuggested] = useState<string>();
  useEffect(() => {
    if (!state || state.settings.suggestion !== messages.namespace) return;
    const languages = typeof navigator === 'undefined' ? [] : navigator.languages;
    setSuggested(suggestedLocale(state.settings, { locale, path }, document.cookie, languages));
  }, [state, locale, path, messages.namespace]);
  if (!state || !suggested) return null;
  return (
    <SuggestionLine
      state={state}
      info={info}
      suggested={suggested}
      messages={messages}
      className={className}
      onDismiss={() => setSuggested(undefined)}
    />
  );
}

interface SuggestionLineProps extends LocaleSuggestionProps {
  state: I18nState;
  info: LocaleInfo;
  suggested: string;
  onDismiss: () => void;
}

/** The suggestion, written in the suggested locale. */
export function SuggestionLine({ state, info, suggested, messages, className, onDismiss }: SuggestionLineProps) {
  const entries = state.suggestions?.[suggested];
  const t = translator(messages, suggested, entries ? { [messages.namespace]: entries } : {});
  const href = `${info.localize(info.path, suggested)}?${LOCALE_PARAM}=${encodeURIComponent(suggested)}`;
  // The site declares the texts, so their arguments are not known here.
  const message = t.message as (values: Record<string, unknown>) => ReactNode;
  const label = t.dismiss as () => ReactNode;
  const dismiss = () => {
    rememberLocale(info.locale);
    onDismiss();
  };
  return (
    <aside className={className} lang={suggested} dir={textDirection(suggested)}>
      <p>
        {message({
          link: (chunks: ReactNode) => (
            <a href={href} hrefLang={suggested}>
              {chunks}
            </a>
          ),
        })}
      </p>
      <button type='button' onClick={dismiss}>
        {label()}
      </button>
    </aside>
  );
}
