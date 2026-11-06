# @putnami/codehost-protocol

TypeScript binding of the Putnami code-host vocabulary: the slugs `github`,
`gitlab` and `generic`, each host's change-request noun and prefix, diff-tab
label and public defaults, and the marker of the Putnami note.

Use this package when you need:

- to say "pull request" or "merge request", and `#5` or `!5`, for a slug
- the default host or API of github.com or gitlab.com
- to find Putnami's note among the notes of a change request

Do not use this package for:

- defining or changing the vocabulary: that is `protocols/codehost`, and a
  change lands in its `vocabulary.json`, its Go table and this package together
- calling a code host's API: it holds words and defaults only

```ts
import { CODEHOST_SLUG, changeRequestLabel, isPutnamiNote, lookupCodeHost } from '@putnami/codehost-protocol';

changeRequestLabel(CODEHOST_SLUG.gitlab, 7); // 'merge request !7'
lookupCodeHost('bitbucket'); // undefined: not a code-host slug
isPutnamiNote('<!-- putnami-observe:summary -->\n...'); // true
```
