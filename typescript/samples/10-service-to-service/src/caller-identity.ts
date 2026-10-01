/** Header the `tenant` named credential carries. */
export const TENANT_HEADER = 'X-Tenant-Id';

/** The one tenant the sample provider serves. */
export const SAMPLE_TENANT = 'tenant-a';

/** Scope a forwarded user carries, and the one `/whoami` requires. */
export const CALLER_SCOPE = 'catalog.caller';

/**
 * Scope a caller carries only when the api key and the tenant arrived
 * together, and the one `/tenant-check` requires.
 */
export const TENANT_SCOPE = 'catalog.tenant';

/**
 * A user bearer a consumer forwards from its own inbound request, and the
 * subject this provider names it with.
 */
export const USER_TOKEN = 'user-token-alice';
export const USER_SUBJECT = 'alice';

/**
 * The user tokens the sample provider knows, and the subject each one names. A
 * real provider would verify a signed token; the sample keeps the shape and
 * never echoes the token itself.
 */
export const USER_SUBJECTS: Readonly<Record<string, string>> = { [USER_TOKEN]: USER_SUBJECT };
