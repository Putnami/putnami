/**
 * TypeScript binding of the Putnami code-host vocabulary
 * (`go.putnami.dev/protocol/codehost`): the slugs that name a code host, the
 * words each host uses for its change request and its diff, the public
 * defaults of the hosted slugs, and the marker of the Putnami note on a
 * change request.
 *
 * The table is held to `protocols/codehost/vocabulary.json` and its fixture
 * corpus by this package's tests, as the Go table is. A change lands in the
 * document, the Go table and this file together.
 */

/** Version of `vocabulary.json`. */
export const CODEHOST_PROTOCOL_VERSION = 1 as const;

/** `$id` of the vocabulary document's JSON schema. */
export const CODEHOST_VOCABULARY_SCHEMA_ID = 'https://putnami.dev/schemas/putnami-codehost-vocabulary.json' as const;

/** The code-host slugs. */
export const CODEHOST_SLUG = {
  github: 'github',
  gitlab: 'gitlab',
  generic: 'generic',
} as const;

/** A code-host slug. */
export type CodeHostSlug = (typeof CODEHOST_SLUG)[keyof typeof CODEHOST_SLUG];

/** The closed slug set, in its canonical order. */
export const CODEHOST_SLUGS: readonly CodeHostSlug[] = Object.freeze([
  CODEHOST_SLUG.github,
  CODEHOST_SLUG.gitlab,
  CODEHOST_SLUG.generic,
]);

/**
 * The first line of the Putnami note on a change request. Notes already
 * published carry this value, so it never changes.
 */
export const PUTNAMI_NOTE_MARKER = '<!-- putnami-observe:summary -->' as const;

/** What one code host calls its things, and where it lives by default. */
export interface CodeHostVocabulary {
  /** The code host's slug. */
  readonly slug: CodeHostSlug;
  /** The code host's display name. */
  readonly name: string;
  /** The noun of a change request, in lower case: `pull request` on GitHub. */
  readonly changeRequest: string;
  /** What comes before a change request's number: `#` on GitHub. */
  readonly referencePrefix: string;
  /** The label of the tab that shows a change request's diff. */
  readonly diffTab: string;
  /** The public host name; absent when the slug has none. */
  readonly defaultHost?: string;
  /** The public REST API base URL; absent when the slug has none. */
  readonly defaultApiBase?: string;
}

/** The vocabulary of every slug, in the slug order. */
export const CODEHOST_VOCABULARIES: readonly CodeHostVocabulary[] = Object.freeze([
  Object.freeze({
    slug: CODEHOST_SLUG.github,
    name: 'GitHub',
    changeRequest: 'pull request',
    referencePrefix: '#',
    diffTab: 'Files changed',
    defaultHost: 'github.com',
    defaultApiBase: 'https://api.github.com',
  }),
  Object.freeze({
    slug: CODEHOST_SLUG.gitlab,
    name: 'GitLab',
    changeRequest: 'merge request',
    referencePrefix: '!',
    diffTab: 'Changes',
    defaultHost: 'gitlab.com',
    defaultApiBase: 'https://gitlab.com/api/v4',
  }),
  Object.freeze({
    slug: CODEHOST_SLUG.generic,
    name: 'Git host',
    changeRequest: 'change request',
    referencePrefix: '#',
    diffTab: 'Changes',
  }),
]);

/** Whether `slug` is a code-host slug, compared case-sensitively. */
export function isCodeHostSlug(slug: unknown): slug is CodeHostSlug {
  return typeof slug === 'string' && (CODEHOST_SLUGS as readonly string[]).includes(slug);
}

/** The vocabulary of `slug`, or `undefined` when it is not a code-host slug. */
export function lookupCodeHost(slug: string): CodeHostVocabulary | undefined {
  return CODEHOST_VOCABULARIES.find((vocabulary) => vocabulary.slug === slug);
}

/**
 * The vocabulary to display for `slug`: its own, or the generic one when it
 * is not a code-host slug.
 */
export function codeHostVocabulary(slug: string): CodeHostVocabulary {
  return lookupCodeHost(slug) ?? (lookupCodeHost(CODEHOST_SLUG.generic) as CodeHostVocabulary);
}

/**
 * Names a change request on the code host `slug`: the noun, a space, the
 * prefix and the number, as in `merge request !12`. A number that is not a
 * positive integer gives the noun alone.
 */
export function changeRequestLabel(slug: string, number: number): string {
  const vocabulary = codeHostVocabulary(slug);
  if (!Number.isInteger(number) || number <= 0) {
    return vocabulary.changeRequest;
  }
  return `${vocabulary.changeRequest} ${vocabulary.referencePrefix}${number}`;
}

/**
 * Whether a note body is the Putnami note: after leading spaces, tabs,
 * carriage returns and line feeds, it starts with {@link PUTNAMI_NOTE_MARKER}.
 * Other white space, such as a no-break space, does not count.
 */
export function isPutnamiNote(body: string): boolean {
  return body.replace(/^[ \t\r\n]+/, '').startsWith(PUTNAMI_NOTE_MARKER);
}
