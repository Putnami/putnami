# Frameworks

Putnami ships multiple frameworks that share the same architectural principles but use language-native implementations.

| Surface | Start here | Use it for |
|---------|------------|------------|
| [TypeScript](/docs/frameworks/typescript) | [Getting Started](/docs/frameworks/typescript/getting-started) | React SSR apps, API services, TypeScript libraries |
| [Go](/docs/frameworks/go) | [Getting Started](/docs/frameworks/go/getting-started) | Backend services, workers, Go libraries |
| [Python (experimental)](/docs/frameworks/python) | [Getting Started](/docs/frameworks/python/getting-started) | Explicit opt-in FastAPI services, Python packages, and data experiments; no Go/TypeScript parity |

Go build, test, and lint cache keys also include the exact bytes selected by
`//go:embed`. See [Go extension caching and dependencies](/docs/frameworks/go/extension/caching-and-dependencies)
for the source and asset inputs that make an asset-only change rerun a job.

The Python surface is the extension and its templates. No Putnami Python
framework package family ships today, it is never enabled by default, and it
carries no parity promise with Go or TypeScript. The reviewed status of every
package on this page is on [Support status](/docs/support).

Within the TypeScript framework, data is split by shape:

- [Persistence](/docs/frameworks/typescript/persistence) for relational PostgreSQL data
- [Document storage](/docs/frameworks/typescript/document) for document collections and NoSQL-style repositories
- [Storage](/docs/frameworks/typescript/storage) for files, blobs, and signed URLs
