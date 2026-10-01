import { Bucket } from '@putnami/storage';

// Demo bucket. Kept private (the default posture): objects are served
// through the GET /files/[id] endpoint instead of public object URLs.
// Setting `public: true` would make every stored object world-readable
// on the provider — only do that for genuinely public assets, and pair
// uploads with authentication.
Bucket('files', {
  maxFileSize: '10mb',
  allowedMimeTypes: ['image/png', 'image/jpeg', 'image/gif', 'application/pdf', 'text/plain'],
  public: false,
});
