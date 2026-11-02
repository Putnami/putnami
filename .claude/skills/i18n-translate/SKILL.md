---
name: i18n-translate
description: Translate the missing and stale messages of a localized @putnami/web project into drafts, when a build, a gate or `putnami i18n sync` reports translations to write
allowed-tools: Bash, Read, Edit, Grep, Glob
---

# Translate messages

A localized web project declares its default texts in `*.messages.ts` files
and keeps one catalog per namespace and locale in
`locales/<locale>/<namespace>.json`. A production build fails while a
published locale lacks a translation, and warns for a stale one: its default
text changed after it was translated. This skill fills both as drafts. A
person validates drafts later with `putnami i18n validate`; you never do.

## Steps

1. Run `putnami i18n translate --projects <project>`. It writes the work file
   `<project>/.putnami/i18n/translate.json`: for each locale and namespace,
   the `entries` to translate, each with `text: null`. When it writes no
   work file, every published locale is translated: stop there.
2. Fill each `text` with the translation of `source` into the locale.
3. Run `putnami i18n translate --apply --projects <project>`. It writes your
   answers as drafts and deletes the work file. When it refuses an answer, it
   writes nothing and names each refused entry: correct those in the work
   file and apply again.
4. Commit the catalogs with the change that altered the messages.

## How to translate

- `source` is ICU MessageFormat. Keep every `{argument}`, every
  `{count, plural, …}` or `{kind, select, …}` with its keywords and `#`, and
  every `<tag>…</tag>` with the same names. Translate the text inside plural
  and select branches, and add the plural categories the locale needs.
- Use `glossary`: a term with a `translation` takes it, a term whose
  `translation` is `null` stays as written, and a `note` says what to keep in
  mind.
- `context` holds the namespace's other translations: reuse their words and
  their register.
- A stale entry carries `previous`, the old default text and its
  translation: change only what the change of default text requires.
- `file` declares the messages: read it, and the component that renders
  them, when a message's meaning or length constraint is unclear.
- Leave `text: null` for a message you cannot translate with confidence.
- Never edit `source`, a catalog's `state`, or a validated entry.
