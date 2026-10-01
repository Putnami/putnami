# Getting Started with @putnami/storage

`@putnami/storage` provides object storage integration for Putnami applications with pluggable backends, bucket declarations, presigned URLs, and a local development server.

## Installation

```bash
bunx putnami deps add @putnami/storage
```

## Quick Start

### 1. Declare Buckets

Define your buckets in any source file. Each project can declare 0 to N buckets with different constraints:

```typescript
import { Bucket } from '@putnami/storage';

export const AvatarsBucket = Bucket('avatars', {
  maxFileSize: '10mb',
  allowedMimeTypes: ['image/png', 'image/jpeg', 'image/webp'],
  public: true,
});

export const InvoicesBucket = Bucket('invoices', {
  maxFileSize: '50mb',
  allowedMimeTypes: ['application/pdf'],
  public: false,
});
```

### 2. Register the Plugin

Add the storage plugin to your application:

```typescript
import { application } from '@putnami/application';
import { storagePlugin, storageServer } from '@putnami/storage';

const app = application();
app.use(storagePlugin());

// For local development — serves files over HTTP for signed URLs
app.use(storageServer());
```

### 3. Use the Client

```typescript
import { storage } from '@putnami/storage';

// Get a client for a specific bucket
const avatars = await storage('avatars');

// Upload
await avatars.put('user-123/photo.png', file, { contentType: 'image/png' });

// Download
const result = await avatars.get('user-123/photo.png');

// Public URL (for public buckets)
const url = avatars.url('user-123/photo.png');

// Signed URLs (for browser-direct upload/download)
const uploadUrl = await avatars.signedUploadUrl('user-123/photo.png', { expiresIn: '15m' });
const downloadUrl = await avatars.signedDownloadUrl('user-123/photo.png', { expiresIn: '1h' });

// List objects
const list = await avatars.list({ prefix: 'user-123/' });

// Delete
await avatars.delete('user-123/photo.png');
```

## Configuration

Configuration follows the standard Putnami config pattern:

```yaml
# conf/.env.local.yaml — Local development
storage:
  backend: file
  dataDir: .data/storage

# conf/.env.production.yaml — Production
storage:
  backend: remote
  endpoint: https://storage.putnami.cloud
  # Sent as `Authorization: Bearer <accessKey>`. The storage service signs URLs
  # server-side, so this bearer token is the only credential the client needs.
  accessKey: ${STORAGE_ACCESS_KEY}

# conf/.env.test.yaml — Tests
storage:
  backend: memory
```

## Backends

| Backend | Description | Use Case |
|---------|-------------|----------|
| `remote` | HTTP client for `storage.putnami.cloud` | Production |
| `file` | Local filesystem storage | Local development |
| `memory` | In-memory storage (no I/O) | Tests |
