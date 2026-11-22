# Fixture

A module that type-checks:

```ts
import { endpoint } from '@putnami/application';

export default endpoint(() => ({ ok: true }));
```

A fragment may use names it does not declare, and imports another block of
the document declares:

```ts
export const handler = endpoint(() => repository.findAll());
```

A method of a class is checked inside a class:

```ts
async findAll() {
  return this.store.list();
}
```

An export the package does not have:

```ts
import { noSuchExport } from '@putnami/application';
```

A call with the wrong arguments, through a carried import:

```ts
endpoint(42);
```

Pseudo-code is marked:

```ts nocheck
endpoint(...)
```

```ts
endpoint(...)
```

A framework export no block imports, found by name:

```ts
export const ok = () => json();
```

A property key does not cancel the import another block declares:

```ts
const options = { endpoint: true };
endpoint('not a handler');
```
