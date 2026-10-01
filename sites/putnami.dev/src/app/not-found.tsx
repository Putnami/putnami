import { Link, Meta, notFound } from '@putnami/web';
import { PageMeta } from '../components/page-meta';

export default notFound().render(() => (
  <main>
    <PageMeta title='Page Not Found — Putnami' description='The page you are looking for does not exist.' />
    <Meta name='robots' content='noindex' />
    <div style={{ textAlign: 'center', padding: 'var(--space-3xl)' }}>
      <h1 style={{ fontSize: '4rem', marginBottom: 'var(--space-lg)' }}>404</h1>
      <p style={{ fontSize: '1.25rem', color: 'var(--color-text-muted)', marginBottom: 'var(--space-xl)' }}>
        Page not found
      </p>
      <Link to='/' style={{ color: 'var(--color-text)', textDecoration: 'none' }}>
        ← Back to home
      </Link>
    </div>
  </main>
));
