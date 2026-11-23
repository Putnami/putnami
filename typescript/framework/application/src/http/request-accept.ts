/** The media types a request's Accept header lists, parsed once per distinct header value. Internal: not part of the package's public API. */
const ANY_ACCEPT: readonly string[] = ['*/*'];

/** Parsed Accept headers by header value: clients send a handful of distinct values. */
const acceptCache = new Map<string, readonly string[]>();
const ACCEPT_CACHE_SIZE = 64;
/** A longer header is parsed each time rather than kept. */
const ACCEPT_CACHE_MAX_HEADER = 256;

/** The media types an Accept header lists, parsed once per distinct header value. */
export const requestAcceptOf = (acceptHeader: string | null): readonly string[] => {
  if (!acceptHeader) return ANY_ACCEPT;
  let parsed = acceptCache.get(acceptHeader);
  if (parsed === undefined) {
    parsed = listRequestAccept(acceptHeader);
    if (acceptHeader.length <= ACCEPT_CACHE_MAX_HEADER) {
      if (acceptCache.size >= ACCEPT_CACHE_SIZE) acceptCache.clear();
      acceptCache.set(acceptHeader, parsed);
    }
  }
  return parsed;
};

const listRequestAccept = (acceptHeader?: string): string[] => {
  if (!acceptHeader) return ['*/*'];

  const parts = acceptHeader.split(',');
  const result: string[] = [];
  for (let i = 0; i < parts.length; i++) {
    const semi = parts[i].indexOf(';');
    const mediaType = (semi >= 0 ? parts[i].slice(0, semi) : parts[i]).trim();
    if (mediaType) {
      result.push(mediaType);
    }
  }

  return result.length === 0 ? ['*/*'] : result;
};
