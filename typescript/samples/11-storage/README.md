# Storage

Object storage with file uploads, downloads, and listing — powered by `@putnami/storage`.

## Features

- Bucket declaration with MIME type validation and size limits
- File backend for local development (persisted to `.putnami/.data/11-storage/`)
- Memory backend for tests
- Upload, download, list, and delete files
- Prefix filtering on list

## Routes

| Method | Path | Description |
|--------|------|-------------|
| GET | `/files` | List files (optional `?prefix=` filter) |
| POST | `/files` | Upload a file |
| GET | `/files/[id]` | Download a file |
| DELETE | `/files/[id]` | Delete a file |
| GET | `/healthz` | Aggregate health check |

## Run

```bash
putnami serve .
```

## Try It

Once the server is running, open a new terminal:

**1. Upload a text file:**

```bash
curl -X POST http://localhost:3911/files \
  -H "Content-Type: text/plain" \
  -H "X-File-Name: hello.txt" \
  -d "Hello, World!"
```

Expected response (HTTP 201):

```json
{
  "key": "hello.txt",
  "size": 13,
  "contentType": "text/plain"
}
```

**2. List all files:**

```bash
curl http://localhost:3911/files
```

Expected response:

```json
{
  "files": [
    { "key": "hello.txt", "size": 13, "contentType": "text/plain", "lastModified": "..." }
  ],
  "total": 1
}
```

**3. Filter files by prefix:**

```bash
curl "http://localhost:3911/files?prefix=hello"
```

Expected response — only files whose key starts with "hello".

**4. Download a file:**

```bash
curl http://localhost:3911/files/hello.txt
```

Expected result — the raw file content with correct `Content-Type` and `Content-Disposition` headers.

**5. Delete a file:**

```bash
curl -X DELETE http://localhost:3911/files/hello.txt
```

Expected response:

```json
{ "deleted": true, "key": "hello.txt" }
```

## Configuration

Storage backend is configured via `conf/.env.{env}.yaml`:

```yaml
# conf/.env.local.yaml — Local development (persisted to disk)
storage:
  backend: file
  dataDir: .putnami/.data/11-storage

# conf/.env.test.yaml — Tests (in-memory, no I/O)
storage:
  backend: memory
```

## Test

```bash
putnami test .
```

## What this sample proves

The bucket declaration in `src/store.ts` is the whole configuration: its size
limit and MIME allowlist are enforced on upload, and the test asserts the
rejection rather than only the happy path. Objects are served through
`GET /files/[id]` instead of public object URLs, which is what `public: false`
buys — the sample deliberately shows the private posture.

The same code runs on three backends without changing: memory in tests, the
filesystem locally, the remote service in production. That substitution is the
point, and it holds because every backend answers one shared contract suite.

Contract: [`@putnami/storage`](../../framework/storage/README.md) —
[object-storage specification](../../framework/storage/specs/object-storage.json).
