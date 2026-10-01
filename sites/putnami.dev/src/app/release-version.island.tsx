import { island } from '@putnami/web';
import { useEffect, useState } from 'react';

/**
 * The latest Putnami release, shown at the start of the footer line. It comes
 * from `/release.json` after hydration: pages are pre-rendered and cached, so
 * a version baked into their HTML would go stale the moment `latest` moves.
 * The server-rendered page shows no version; it appears once the lookup
 * answers.
 */
function ReleaseVersion() {
  const [version, setVersion] = useState<string | undefined>();

  useEffect(() => {
    let cancelled = false;
    fetch('/release.json')
      .then((response) => (response.ok ? response.json() : undefined))
      .then((doc: { version?: unknown } | undefined) => {
        if (!cancelled && typeof doc?.version === 'string') setVersion(doc.version);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);

  return version ? <span>v{version} — </span> : null;
}

// `idle`, not `visible`: the island sits inline in the footer line and has no
// box until it renders, so an IntersectionObserver would never fire.
export default island().load('idle').render(ReleaseVersion);
