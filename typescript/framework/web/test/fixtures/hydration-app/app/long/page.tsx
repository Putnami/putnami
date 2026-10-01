import { useEffect, useState } from 'react';

// This page renders about 20,000 bytes. Inside a layout element, React streams
// a page above 12,800 bytes after the shell, and an inline script moves it into
// place.
const LINES = Array.from({ length: 400 }, (_, index) => `Line ${index} of a page longer than one stream chunk.`);

/** A page whose HTML is larger than the boundary size React inlines in the shell. */
export default function LongPage() {
  const [hydrated, setHydrated] = useState(false);
  useEffect(() => setHydrated(true), []);
  return (
    <section data-testid='page' data-hydrated={String(hydrated)}>
      {LINES.map((line) => (
        <p key={line}>{line}</p>
      ))}
      Long
    </section>
  );
}
