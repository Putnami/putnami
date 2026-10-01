import { error, Link, Meta } from '@putnami/web';
import { PageMeta } from '../components/page-meta';

export default error().render(() => (
  <main>
    <PageMeta title='Something Went Wrong — Putnami' description='An unexpected error occurred.' />
    <Meta name='robots' content='noindex' />
    <div style={{ textAlign: 'center', padding: 'var(--space-3xl)' }}>
      <h1 style={{ fontSize: '4rem', marginBottom: 'var(--space-lg)' }}>500</h1>
      <p style={{ fontSize: '1.25rem', color: 'var(--color-text-muted)', marginBottom: 'var(--space-xl)' }}>
        Something went wrong
      </p>
      <Link to='/' style={{ color: 'var(--color-text)', textDecoration: 'none' }}>
        ← Back to home
      </Link>
    </div>
  </main>
));
