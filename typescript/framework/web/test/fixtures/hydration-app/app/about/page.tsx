import { useEffect, useState } from 'react';
import { page } from '@putnami/web';

/** A page exported as a page definition. */
export default page().render(function AboutPage() {
  const [hydrated, setHydrated] = useState(false);
  useEffect(() => setHydrated(true), []);
  return (
    <section data-testid='page' data-hydrated={String(hydrated)}>
      About
    </section>
  );
});
