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
| `paths` | Translated paths, per locale: `{"fr": {"/privacy": "/confidentialite"}}`. See [Translated paths](#translated-paths). |

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
| Tag | `Read the <link>terms</link>` | `(chunks) => ReactNode` |

Every `plural`, `selectordinal` and `select` needs an `other` branch. A `'` before `{`, `}` or `<` quotes it: `'{literal}'`, `'<b>'`.

A namespace is an explicit string, unique in the project. Keys are identifiers.

### Tags

A tag wraps part of a message, so a link or a `<code>` stays inside its sentence and a translator can move it:

```ts
export default messages('legal', {
  terms: 'Read the <link>terms</link> before you write to <mail>us</mail>.',
});
```

Pass one function per tag. It receives what the tag wraps and returns what to render:

```tsx
const t = useT(legal);
<p>
  {t.terms({
    link: (chunks) => <Link to='/terms'>{chunks}</Link>,
    mail: (chunks) => <a href='mailto:hello@example.com'>{chunks}</a>,
  })}
</p>
```

A message with tags returns a React node; a message without returns a string. The tag functions are typed from the default text, like the arguments. A tag name is an identifier, and a `<` that opens no tag, as in `a < b`, is text. A translation must use the same tags as its default text.

## Catalogs

Each non-default locale has one directory, with one file per namespace: `locales/<locale>/<namespace>.json`. A namespace with a `/` is a nested file: `docs/sidebar` is `locales/fr/docs/sidebar.json`.

```text
locales/
  fr/
    home.json
    footer.json
    docs/
      sidebar.json
```

Each file names its schema and maps a key to its entry:

```json
{
  "$schema": "https://putnami.dev/schemas/i18n/catalog.json",
  "title": {
    "text": "Tarifs",
    "source": "Pricing",
    "state": "validated",
    "validatedBy": "Ada Lovelace <ada@example.com>",
    "validatedAt": "2026-11-01"
  }
}
```

| Field | Meaning |
|-------|---------|
| `text` | The translation. |
| `source` | The default text the translation was written from. |
| `state` | `missing`, `draft`, `validated` or `stale`. |
| `validatedBy`, `validatedAt` | Who validated the translation, from their git identity, and the day. |

The schema is published at its `$schema` URL, so an editor or a translation tool checks a file without Putnami.

`putnami i18n sync` keeps the catalogs in step with the code:

- a new message gets an entry in state `missing`;
- a deleted message loses its entry, and a deleted namespace loses its file;
- a message whose default text changed has its translation marked `stale`. The text and its old `source` stay, so the translator sees what changed.

The build reads the catalogs and writes none of them:

| Finding | `putnami build` | `putnami serve` and `putnami test` |
|---------|-----------------|------------------------------------|
| A missing translation | Fails | Warns, and serves the default text |
| Invalid ICU syntax | Fails | Fails |
| A translation that reads other arguments or tags than its default text | Fails | Fails |
| A stale translation | Warns, and serves the stale text | Warns |

A locale is published only when its catalog is complete.

## Translate and validate

An agent drafts translations; a person locks the ones they checked.

1. `putnami i18n translate --projects /my-site` writes `.putnami/i18n/translate.json` in the project: each missing and stale entry with `text: null`, its default text, a stale entry's old default text and translation, the namespace's other translations, and the glossary terms it contains.
2. An agent fills each `text`. The `i18n-translate` skill tells it how: keep the arguments and tags, follow the glossary, reuse the words of the other translations. A workspace installs the skill by listing `"extension:@putnami/web"` in `agentArtifacts` in `putnami.workspace.json`.
   Running `translate` again keeps the answers whose default text did not change.
3. `putnami i18n translate --apply --projects /my-site` writes the answers as `draft` entries and deletes the work file. It writes nothing when one answer is refused: an answer for a validated entry or an unknown locale, namespace or key, invalid ICU, other arguments or tags than the default text, or a default text that changed since the work file was written.
4. `putnami i18n validate --projects /my-site --locale fr` shows a person each draft and stale translation next to its default text. Each one they accept becomes `validated`, with their git `user.name` and `user.email` and the day in their time zone. Validating a stale translation records the current default text as its `source`.

`validate` refuses without an interactive terminal, inside an agent session, or without a git identity. No Putnami command calls a model: the agent of your session translates. When a build reports missing or stale translations, it names `putnami i18n translate`, and the contributor workflow runs it before committing.

A glossary per locale, `locales/<locale>.glossary.json`, keeps terms consistent:

```json
{
  "$schema": "https://putnami.dev/schemas/i18n/glossary.json",
  "Putnami": { "translation": null, "note": "The product name." },
  "repository": { "translation": "dépôt" }
}
```

A term with `translation: null` stays as written.

## Fallback

A reader can ask for something that does not exist in their locale at three levels: a message, a page, or the whole site.

| What is missing | What the reader gets |
|-----------------|----------------------|
| A message's translation | The default text, in serve and test, with a warning. A production build fails instead, so a published locale never shows it. |
| A message's translation is stale | The old translation, with a warning, until someone revises it. |
| A page in the reader's locale (an excluded route) | `/fr/docs/intro` redirects with `308` to `/docs/intro`, query kept. Links from a French page to it point to `/docs/intro` directly. |
| The reader's locale on the site | At `/`, the locale chosen in the switcher (its cookie), else the closest `Accept-Language` match (`fr-CA` gets `fr`), else the default locale. |

A page never mixes locales by surprise: either every message of a published locale has a translation, or the build stops. The one page that shows another locale's text on purpose is an excluded route, and its URL says so.

## Routes and links

Every page, loader and action is served once per locale, except the routes in `exclude`. The router's basename carries the locale prefix, so `<Link to="/pricing">` on a French page points to `/fr/pricing`. Two links point to their target unchanged, and load it as a new document:

- a link to an excluded route (`/docs/intro`);
- a link that names a locale (`/ar/pricing`).

An excluded route has one URL. The same path under a locale prefix, such as `/fr/docs/intro`, redirects with `308` to it.

A locale prefix matches the locale tag exactly: `/fr/pricing` is French, `/FR/pricing` is a page that does not exist.

Exclude a whole subtree (`/blog/**`), not a layout alone (`/blog`): a page under a layout must be served in the locales its layout is served in.

Use `<Link>` for pages and a plain `<a>` for files such as `/LICENSE.md`: a link to a file gets no prefix only when it is a plain anchor.

`useLocale()` returns the current locale, its direction and a `localize(path, locale?)` helper for the cases a `Link` does not cover.

## Translated paths

`i18n.paths` gives a page its own path in a locale:

```json
"paths": { "fr": { "/privacy": "/confidentialite" } }
```

The French page is served at `/fr/confidentialite`. `/fr/privacy` redirects with `308` to it, with the query. `<Link to="/privacy">`, the alternate links, the switcher and the sitemap all use the translated path. Keep writing `/privacy` in your code: the framework translates it.

A person writes this block, not an agent: a translated path is a public URL, and changing it breaks every link to it.

Generation fails when a translated path:

- translates a route with parameters (`/blog/:slug`), or the locale's home;
- names a locale (`/fr/x`), or is the path of another page;
- leaves the localized set (`/docs/x` when `/docs/**` is excluded);
- leaves the layout of its page: `/blog/post` can become `/blog/billet`, not `/billet`, because the page renders inside the `/blog` layout.

## Locale domains

`i18n.domains` serves a locale at the root of its own origin:

```json
"domains": { "fr": "https://example.fr" }
```

A request to `example.fr` gets the French pages: `https://example.fr/confidentialite` serves what `/fr/confidentialite` serves on the site. Every URL built for French uses the domain, without the prefix: links, the canonical and alternate links, the switcher and the sitemap. A link to another origin is absolute; the switcher on `example.fr` links English to `https://example.com/privacy?locale=en`.

On the host of `siteUrl` or of a domain, `/fr/x` redirects with `308` to `https://example.fr/x`, with the query, and the root of the site sends a French reader to `https://example.fr/`. Any other host, such as your development server or a preview, serves French under `/fr` and links every locale there. Its static pages render under the prefix, once per page, and the server logs the host once. A path the domain does not serve in French, such as an excluded `/docs/x`, redirects to the site.

Each domain is an `http` or `https` origin, without a path, a query or a fragment, and serves one locale other than the default. Point the domain's DNS at the same deployment as the site. The sitemap on the site lists the domain's URLs; search engines accept them because the `robots.txt` every domain serves names the site's sitemap, `Sitemap: https://example.com/sitemap.xml`.

## Locale switcher

```tsx
import { LocaleSwitcher } from '@putnami/web';

<LocaleSwitcher label='Language' />
```

It renders a `<nav>` of plain links, one per locale, to the current page in that locale, or to the locale's home when the page exists in the default locale only. Each link carries `hreflang` and `lang`, shows the locale's own name (`Français`), and the current one carries `aria-current="page"`. It renders nothing when one locale is published.

Each link carries the choice in its query: `/pricing?locale=en`. The server records it in the `putnami-locale` cookie for a year and redirects with `302` to `/pricing`. The choice persists without JavaScript, on a static page as well. A `locale` value that names no published locale is ignored.

Pass a render function as `children` for your own markup: each link's `href` already carries the choice. To record a choice from your own browser code, call `rememberLocale(locale)`.

## Language suggestion

`LocaleSuggestion` offers the current page in the reader's language, written in that language:

```tsx
// src/app/locale-suggestion.messages.ts
export default messages('locale-suggestion', {
  message: 'Read this page in <link>English</link>.',
  dismiss: 'Dismiss',
});

// src/app/locale-suggestion.island.tsx
function Suggestion() {
  return <LocaleSuggestion messages={suggestion} />;
}
export default island().load('idle').render(Suggestion);
```

Name the namespace in `i18n.suggestion`:

```json
"suggestion": "locale-suggestion"
```

Every page's payload then carries that namespace in every locale, so a French reader on an English page reads French. Write the text as the suggested locale says it: the English text shows on a French page.

The component renders nothing on the server. In the browser, it picks the locale the reader chose (the `putnami-locale` cookie), else the first published locale their browser languages match. When that locale is not the page's, it shows one line with a link to the page in that locale, with `?locale=`, and a dismiss button. Dismissing records the page's locale as the reader's choice, so the line does not come back. A page served in the default locale only shows nothing.

Put it in an island: a pre-rendered page stays the same bytes for every reader and every cache.

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

A page outside the localized set gets its canonical link only. A page with `<Meta name='robots' content='noindex' />`, such as a not-found page, gets none of these links.

The build writes `/sitemap.xml`: one URL per page without parameters and per pre-rendered path, in each locale the page is served in, each with the same alternates as its page. A route with parameters and no pre-rendered path is left out. Point `robots.txt` to it with `Sitemap: https://example.com/sitemap.xml`.

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
- A route with parameters keeps its path in every locale, and a translated path stays under the layout of its page.
- A locale with a domain is served at the root of that domain, and its prefix moves there from the site only.
