import { error, Link, Meta, useT } from '@putnami/web';
import { PageMeta } from '../components/page-meta';
import status from './status.messages';

export default error().render(() => {
  const t = useT(status);
  return (
    <main>
      <PageMeta title={t.errorTitle()} description={t.errorDescription()} />
      <Meta name='robots' content='noindex' />
      <div style={{ textAlign: 'center', padding: 'var(--space-3xl)' }}>
        <h1 style={{ fontSize: '4rem', marginBottom: 'var(--space-lg)' }}>500</h1>
        <p style={{ fontSize: '1.25rem', color: 'var(--color-text-muted)', marginBottom: 'var(--space-xl)' }}>
          {t.error()}
        </p>
        <Link to='/' style={{ color: 'var(--color-text)', textDecoration: 'none' }}>
          {t.backHome()}
        </Link>
      </div>
    </main>
  );
});
