# @putnami/codehost-protocol

TypeScript binding of the Putnami **code-host vocabulary**
([`go.putnami.dev/protocol/codehost`](../../../protocols/codehost/README.md)):
the slugs that name a code host, what each host calls a change request and its
diff, where it lives by default, and the marker of the note Putnami writes on a
change request.

```ts
import { changeRequestLabel, codeHostVocabulary, isPutnamiNote } from '@putnami/codehost-protocol';

changeRequestLabel('gitlab', 42); // 'merge request !42'
codeHostVocabulary('github').diffTab; // 'Files changed'
const ours = notes.find((note) => isPutnamiNote(note.body));
```

## Surface

| Symbol | Purpose |
|--------|---------|
| `CODEHOST_SLUG` / `CODEHOST_SLUGS` / `CodeHostSlug` | `github`, `gitlab`, `generic`, in that order |
| `isCodeHostSlug` | Case-sensitive membership test |
| `CODEHOST_VOCABULARIES` / `CodeHostVocabulary` | Per slug: name, change-request noun and prefix, diff tab, default host and API |
| `lookupCodeHost` | A slug's vocabulary, or `undefined` |
| `codeHostVocabulary` | A slug's vocabulary, or the generic one for display |
| `changeRequestLabel` | `pull request #42`, `merge request !42`, `change request #42` |
| `PUTNAMI_NOTE_MARKER` / `isPutnamiNote` | Find Putnami's note among a change request's notes |

The table is hand-authored and held to `protocols/codehost/vocabulary.json` and
`protocols/codehost/fixtures/expectations.json` by this package's tests, the
same files the Go module is tested against.
