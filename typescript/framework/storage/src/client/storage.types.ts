// ---- Object Metadata ----

export interface ObjectMetadata {
  /** Content type (MIME type) of the object */
  contentType?: string;
  /** Custom metadata key-value pairs */
  custom?: Record<string, string>;
  /** Cache control header */
  cacheControl?: string;
  /** Content disposition header */
  contentDisposition?: string;
}

// ---- Operation Results ----

export interface PutResult {
  /** Object key within the bucket */
  key: string;
  /** Size in bytes */
  size: number;
  /** ETag/hash of the uploaded content */
  etag?: string;
}

export interface GetResult {
  /** Object key */
  key: string;
  /** Object content as a ReadableStream */
  body: ReadableStream<Uint8Array>;
  /** Size in bytes */
  size: number;
  /** Content type */
  contentType?: string;
  /** ETag/hash */
  etag?: string;
  /** Last modified timestamp */
  lastModified?: Date;
  /** Custom metadata */
  metadata?: Record<string, string>;
}

export interface ObjectInfo {
  /** Object key */
  key: string;
  /** Size in bytes */
  size: number;
  /** Content type */
  contentType?: string;
  /** ETag/hash */
  etag?: string;
  /** Last modified timestamp */
  lastModified?: Date;
}

export interface ListOptions {
  /** Filter objects by key prefix */
  prefix?: string;
  /** Delimiter for directory-like grouping (e.g., '/') */
  delimiter?: string;
  /** Maximum number of objects to return */
  maxKeys?: number;
  /** Continuation token for pagination */
  continuationToken?: string;
}

export interface ListResult {
  /** Matched objects */
  objects: ObjectInfo[];
  /** Common prefixes (directories) when delimiter is used */
  prefixes: string[];
  /** Whether more results are available */
  isTruncated: boolean;
  /** Token to use for the next page */
  continuationToken?: string;
}

// ---- Signed URLs ----

export interface SignOptions {
  /** How long the signed URL is valid (e.g., '15m', '1h', '7d') */
  expiresIn?: string;
  /** Maximum file size for uploads (e.g., '10mb') */
  maxFileSize?: string;
  /** Allowed MIME types for uploads */
  allowedMimeTypes?: string[];
  /** Custom metadata to attach to the uploaded object */
  metadata?: Record<string, string>;
  /** Content type for the upload */
  contentType?: string;
  /** Content disposition for downloads (e.g., 'attachment; filename="report.pdf"') */
  contentDisposition?: string;
}

export interface SignedUrl {
  /** The signed URL */
  url: string;
  /** Expiration timestamp */
  expiresAt: Date;
  /** HTTP method to use (GET for download, PUT for upload) */
  method: 'GET' | 'PUT';
  /** HTTP headers required with the request */
  headers?: Record<string, string>;
}

// ---- Storage Operations (used by observability) ----

export type StorageOperation =
  | 'put'
  | 'get'
  | 'delete'
  | 'list'
  | 'exists'
  | 'copy'
  | 'signedUploadUrl'
  | 'signedDownloadUrl'
  | 'url';
