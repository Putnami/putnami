import { notFound } from '@putnami/web';
import { useEffect, useState } from 'react';

export default notFound().render(function NotFoundPage() {
  const [hydrated, setHydrated] = useState(false);
  useEffect(() => setHydrated(true), []);
  return (
    <section data-testid='page' data-hydrated={String(hydrated)}>
      Not found
    </section>
  );
});
