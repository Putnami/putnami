import { describe, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/spectest';
import {
  CODEHOST_PROTOCOL_VERSION,
  CODEHOST_SLUG,
  CODEHOST_SLUGS,
  CODEHOST_VOCABULARIES,
  CODEHOST_VOCABULARY_SCHEMA_ID,
  type CodeHostVocabulary,
  changeRequestLabel,
  codeHostVocabulary,
  isCodeHostSlug,
  isPutnamiNote,
  lookupCodeHost,
  PUTNAMI_NOTE_MARKER,
} from '../src/index';

// The vocabulary document and the fixture corpus are ONE set of files read by
// both runtimes: this suite and the Go tests in protocols/codehost. A change
// lands in vocabulary.json, protocols/codehost/codehost.go and src/index.ts
// together; never loosen a fixture to make one runtime pass.

const PROTOCOL_DIR = join(__dirname, '../../../../protocols/codehost');
const FEATURE = 'codehost/code-host-vocabulary';

interface VocabularyDocument {
  $schema?: string;
  protocolVersion: number;
  noteMarker: string;
  hosts: CodeHostVocabulary[];
}

interface Expectations {
  lookups: { slug: string; valid: boolean; displays: string }[];
  labels: { slug: string; number: number; label: string }[];
  notes: { body: string; putnami: boolean }[];
}

function readJSON<T>(...path: string[]): T {
  return JSON.parse(readFileSync(join(PROTOCOL_DIR, ...path), 'utf8')) as T;
}

const vocabulary = readJSON<VocabularyDocument>('vocabulary.json');
const expectations = readJSON<Expectations>('fixtures', 'expectations.json');

describe('code-host vocabulary', () => {
  specTest(
    'the slugs are github, gitlab and generic, compared case-sensitively',
    { feature: FEATURE, requirement: 'a-closed-slug-set', check: 'the-typescript-slugs-are-github-gitlab-generic' },
    () => {
      expect([...CODEHOST_SLUGS]).toEqual(['github', 'gitlab', 'generic']);
      expect(Object.values(CODEHOST_SLUG)).toEqual([...CODEHOST_SLUGS]);
      expect(Object.isFrozen(CODEHOST_SLUGS)).toBe(true);
      expect(expectations.lookups.length).toBeGreaterThan(0);
      for (const lookup of expectations.lookups) {
        expect({ slug: lookup.slug, valid: isCodeHostSlug(lookup.slug) }).toEqual({
          slug: lookup.slug,
          valid: lookup.valid,
        });
        expect(lookupCodeHost(lookup.slug) !== undefined).toBe(lookup.valid);
        expect(codeHostVocabulary(lookup.slug).slug).toBe(lookup.displays as typeof CODEHOST_SLUG.generic);
      }
      expect(isCodeHostSlug(42)).toBe(false);
      expect(isCodeHostSlug(undefined)).toBe(false);
    },
  );

  specTest(
    'the table is the vocabulary document',
    { feature: FEATURE, requirement: 'one-vocabulary-document', check: 'the-typescript-table-is-the-document' },
    () => {
      expect(vocabulary.$schema).toBe(CODEHOST_VOCABULARY_SCHEMA_ID);
      expect(vocabulary.protocolVersion).toBe(CODEHOST_PROTOCOL_VERSION);
      expect(vocabulary.noteMarker).toBe(PUTNAMI_NOTE_MARKER);
      expect(CODEHOST_VOCABULARIES.map((host) => ({ ...host }))).toEqual(vocabulary.hosts);
      expect(vocabulary.hosts.map((host) => host.slug)).toEqual([...CODEHOST_SLUGS]);
      const schema = readJSON<{ $id: string }>('schemas', 'vocabulary.json');
      expect(schema.$id).toBe(CODEHOST_VOCABULARY_SCHEMA_ID);
      expect(Object.isFrozen(CODEHOST_VOCABULARIES[0])).toBe(true);
    },
  );

  specTest(
    'every label fixture gets its label',
    {
      feature: FEATURE,
      requirement: 'each-host-names-its-change-request',
      check: 'the-typescript-labels-match-the-fixtures',
    },
    () => {
      expect(expectations.labels.length).toBeGreaterThan(0);
      for (const label of expectations.labels) {
        expect(changeRequestLabel(label.slug, label.number)).toBe(label.label);
      }
      // A number Go cannot hold reads as no number.
      expect(changeRequestLabel('github', 1.5)).toBe('pull request');
      expect(changeRequestLabel('gitlab', Number.NaN)).toBe('merge request');
      expect(codeHostVocabulary('github').diffTab).toBe('Files changed');
      expect(codeHostVocabulary('gitlab').diffTab).toBe('Changes');
      expect(codeHostVocabulary('generic').diffTab).toBe('Changes');
    },
  );

  specTest(
    'the hosted slugs default to their public host',
    {
      feature: FEATURE,
      requirement: 'hosted-slugs-name-their-public-host',
      check: 'the-typescript-defaults-are-the-public-hosts',
    },
    () => {
      const defaults = CODEHOST_SLUGS.map((slug) => {
        const host = codeHostVocabulary(slug);
        return [slug, host.defaultHost, host.defaultApiBase];
      });
      expect(defaults).toEqual([
        ['github', 'github.com', 'https://api.github.com'],
        ['gitlab', 'gitlab.com', 'https://gitlab.com/api/v4'],
        ['generic', undefined, undefined],
      ]);
    },
  );

  specTest(
    'every note fixture is recognized as listed',
    { feature: FEATURE, requirement: 'the-putnami-note-is-marked', check: 'the-typescript-notes-match-the-fixtures' },
    () => {
      expect(expectations.notes.length).toBeGreaterThan(0);
      for (const note of expectations.notes) {
        expect({ body: note.body, putnami: isPutnamiNote(note.body) }).toEqual({
          body: note.body,
          putnami: note.putnami,
        });
      }
    },
  );
});
