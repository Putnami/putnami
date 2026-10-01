# Signed URLs

Signed URLs allow browsers to upload to or download from storage directly, bypassing the application server. This is essential for production-scale file handling.

## How It Works

```
┌─────────┐    1. Request signed URL     ┌─────────────┐
│ Browser  │ ──────────────────────────▸  │  App Server  │
│          │                              │              │
│          │  ◂── 2. Return signed URL ── │              │
│          │                              └──────────────┘
│          │
│          │    3. PUT/GET directly        ┌──────────────────────┐
│          │ ──────────────────────────▸   │ storage.putnami.cloud│
│          │                              │ (or local server)     │
└──────────┘                              └───────────────────────┘
```

## Upload Flow

```typescript
import { storage } from '@putnami/storage';

// Server-side: generate a signed upload URL
const avatars = await storage('avatars');
const { url, headers, expiresAt } = await avatars.signedUploadUrl('user-123/photo.png', {
  expiresIn: '15m',
  maxFileSize: '10mb',
  contentType: 'image/png',
});

// Client-side: upload directly to the signed URL
await fetch(url, {
  method: 'PUT',
  headers,
  body: file,
});
```

When the bucket declares `allowedMimeTypes`, `signedUploadUrl` requires a
`contentType` from that list. It throws `StorageValidationError` for a content
type that is not listed, or for none.

## Download Flow

```typescript
// Server-side: generate a signed download URL
const invoices = await storage('invoices');
const { url, expiresAt } = await invoices.signedDownloadUrl('inv-2025-001.pdf', {
  expiresIn: '1h',
  contentDisposition: 'attachment; filename="invoice.pdf"',
});

// Client-side: redirect or fetch from the signed URL
window.location.href = url;
```

## Sign Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `expiresIn` | `string` | `'15m'` | URL validity duration (e.g., `'15m'`, `'1h'`, `'7d'`). **Capped at `7d`** — a longer value is rejected at signing time |
| `maxFileSize` | `string` | — | Maximum upload size (e.g., `'10mb'`) |
| `allowedMimeTypes` | `string[]` | — | Restrict upload content types |
| `contentType` | `string` | — | Expected content type |
| `contentDisposition` | `string` | — | Content disposition for downloads |
| `metadata` | `Record<string, string>` | — | Custom metadata to attach |

## Environment Behavior

| Environment | Signed URL target |
|-------------|-------------------|
| Production | `https://storage.putnami.cloud/{bucket}/{key}?signature=...` |
| Local dev | `http://localhost:{port}/_storage/{bucket}/{key}?token=...` |
| Tests | `memory://{bucket}/{key}` (not intended for HTTP use) |

For local development, the `storageServer()` plugin handles signed URL requests on `/_storage/` routes.

## Security

Signed URLs are protected by HMAC-SHA256 signatures keyed on `storage.tokenSecret`.

> **Set a stable, shared `tokenSecret` in production.** When `tokenSecret` is
> omitted, the file backend falls back to a secret generated fresh on every
> process start (and logs a warning). That fallback is fine for a single local
> process, but in any horizontally-scaled or restarted deployment a URL minted
> by one replica (or before a restart) will fail validation on another replica
> (or after a restart). Configure the same `tokenSecret` across all instances.

**Key constraints:**
- Object keys are validated to prevent path traversal (`../` sequences, absolute paths, null bytes)
- Tokens are bound to a specific bucket and key — reusing a token for a different path is rejected
- Tokens expire after `expiresIn` (default `15m`, capped at `7d`) and are rejected after expiry
- Upload size is validated against the actual request body, not the `Content-Length` header
