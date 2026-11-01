# Localization

`@putnami/web` serves a site in several languages from one route tree. The locale is part of the URL: the default locale has no prefix (`/pricing`), every other locale is the first path segment (`/fr/pricing`). Text lives next to the component that renders it, and each translation sits in a catalog file a person or an agent can edit and review.

> English stays the default locale. Text in another language belongs in that locale's catalog only.

## Getting started

Declare the locales in the project's `putnami.json`:

```json
{
  "options": {
    "@putnami/web:generate": {
      "i18n": {
        "locales": ["en", "fr"],
        "defaultLocale": "en",
        "siteUrl": "https://example.com",
        "exclude": ["/docs/**"]
      }
    }
  }
}
```

| Key | Meaning |
|-----|---------|
| `locales` | Every published locale, as BCP 47 tags (BCP 47 is the standard for language tags such as `fr` or `pt-BR`). |
| `defaultLocale` | The locale served without a prefix. It must be listed in `locales`. |
| `siteUrl` | The absolute origin the alternate links are built from. |
| `exclude` | Route patterns served in the default locale only. `/docs/**` covers `/docs` and everything below it; `*` matches one segment. |

Declare the text of a component in a `*.messages.ts` file under `src/`:

```ts
// src/app/home.messages.ts
import { messages } from '@putnami/web';

export default messages('home', {
  title: 'Pricing',
  seats: '{count, plural, one {# seat} other {# seats}}',
  updated: { text: 'Updated {at, date, medium}', context: 'Under the price table' },
});
```

Read it with `useT`:

```tsx
import { page, useT } from '@putnami/web';
import home from './home.messages';

export default page().render(() => {
  const t = useT(home);
  return (
    <main>
      <h1>{t.title()}</h1>
      <p>{t.seats({ count: 3 })}</p>
    </main>
  );
});
```

The arguments are typed from the message text: `t.seats()` without `count` does not compile, and `t.title({ count: 1 })` does not either.

Then create the catalogs:

```bash
putnami i18n sync --projects /my-site
```

## Messages

A message is an ICU MessageFormat string (ICU is the Unicode standard for messages with plurals and arguments). The formatter uses the browser's and the server's own `Intl` objects, so no locale data ships with your bundle.

| Syntax | Example | Value |
|--------|---------|-------|
| Argument | `Hello {name}` | `string` or `number` |
| Plural | `{count, plural, =0 {none} one {# item} other {# items}}` | `number` |
| Ordinal | `{n, selectordinal, one {#st} two {#nd} few {#rd} other {#th}}` | `number` |
| Select | `{role, select, admin {Admin} other {Member}}` | `string` |
| Number | `{ratio, number, percent}` | `number` |
| Date and time | `{at, date, long}`, `{at, time, short}` | `Date` or a timestamp |

Every `plural`, `selectordinal` and `select` needs an `other` branch. A `'` before `{` or `}` quotes it: `'{literal}'`.

A namespace is an explicit string, unique in the project. Keys are identifiers. Inline markup is not part of a message: split a paragraph that holds a `<strong>` into one message per part.

## Catalogs

Each non-default locale has one file, `locales/<locale>.json`:

```json
{
  "home": {
    "title": {
      "text": "Tarifs",
      "source": "6c09ec564736f285",
      "state": "validated",
      "validatedBy": "ada",
      "validatedAt": "2026-11-01"
    }
  }
}
```

| Field | Meaning |
|-------|---------|
| `text` | The translation. |
| `source` | A hash of the default text the translation was written from. |
| `state` | `missing`, `draft`, `validated` or `stale`. |
| `validatedBy`, `validatedAt` | Who validated the translation, and when. |

`putnami i18n sync` keeps the catalogs in step with the code:

- a new message gets an entry in state `missing`;
- a deleted message loses its entry;
- a message whose default text changed has its translation marked `stale`. The text stays, so the translator sees what to revise.

An agent drafts a translation by filling `text` and setting `state` to `draft`. A person validates it by setting `state` to `validated` with `validatedBy` and `validatedAt`. To accept a stale translation, revise `text`, set `state`, and delete `source`; the next sync records the current default text.

The build reads the catalogs and writes none of them:

| Finding | `putnami build` | `putnami serve` and `putnami test` |
|---------|-----------------|------------------------------------|
| A missing translation | Fails | Warns, and serves the default text |
| Invalid ICU syntax | Fails | Fails |
| A translation that reads other arguments than its default text | Fails | Fails |
| A stale translation | Warns, and serves the stale text | Warns |

A locale is published only when its catalog is complete.

## Routes and links

Every page, loader and action is served once per locale, except the routes in `exclude`. The router's basename carries the locale prefix, so `<Link to="/pricing">` on a French page points to `/fr/pricing`. A link to an excluded route (`/docs/intro`) points to it unchanged.

Use `<Link>` for pages and a plain `<a>` for files such as `/LICENSE.md`: a link to a file gets no prefix only when it is a plain anchor.

`useLocale()` returns the current locale, its direction and a `localize(path, locale?)` helper for the cases a `Link` does not cover.

## Locale switcher

```tsx
import { LocaleSwitcher } from '@putnami/web';

<LocaleSwitcher label='Language' />
```

It renders a `<nav>` of plain links, one per locale, to the current page in that locale, or to the locale's home when the page exists in the default locale only. Each link carries `hreflang` and `lang`, shows the locale's own name (`Français`), and the current one carries `aria-current`. It renders nothing when one locale is published.

A click records the choice in the `putnami-locale` cookie. Pass a render function as `children` for your own markup, and call each link's `select()` on click.

## Locale detection

Only the site root negotiates. A request for `/` redirects with `302` to the locale the cookie names, else to the best match of the browser's `Accept-Language` header, when that locale is not the default. The response carries `Vary: Accept-Language, Cookie`.

Every other URL names its locale and is served as it is: a shared link and a crawler always get the page they asked for.

## Search engines and accessibility

On every localized page, the framework writes:

- `<html lang dir>`, with `dir="rtl"` for right-to-left languages;
- a canonical link to the page itself, unless the page sets its own;
- an `alternate` link per locale and one for `x-default`, all absolute;
- `og:locale` and one `og:locale:alternate` per other locale;
- a `Content-Language` header on a page rendered per request.

A page outside the localized set gets its canonical link only.

## What the browser receives

| Page | Catalog shipped |
|------|-----------------|
| Default locale | None: the text is in the code. |
| Hydrated page | The whole catalog of its locale, because client navigation can render any namespace. |
| Static page with islands | The namespaces the page rendered. |
| Static page without islands | Nothing. |

The payload is a `<script type="application/json">` data block, which a strict Content Security Policy allows because the browser never runs it.

## Limits

- An application served below the site root (a module path such as `/app`) cannot be localized yet.
- Paths are not translated: `/fr/pricing`, not `/fr/tarifs`.
- One domain serves every locale.
