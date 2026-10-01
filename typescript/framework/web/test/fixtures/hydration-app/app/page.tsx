import { useEffect, useState } from 'react';

/** A page exported as a plain component. */
export default function HomePage() {
  const [hydrated, setHydrated] = useState(false);
  useEffect(() => setHydrated(true), []);
  return (
    <section data-testid='page' data-hydrated={String(hydrated)}>
      Home
    </section>
  );
}
