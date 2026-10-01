import { Outlet } from '@putnami/web';

export default function RootLayout() {
  return (
    <div style={{ maxWidth: 900, margin: '0 auto', padding: '2rem', fontFamily: 'system-ui' }}>
      <header style={{ marginBottom: '2rem', borderBottom: '2px solid #333', paddingBottom: '1rem' }}>
        <h1>Project Tracker</h1>
        <nav style={{ display: 'flex', gap: '1rem' }}>
          <a href='/'>Home</a>
          <a href='/projects'>Projects</a>
          <a href='/tasks'>Tasks</a>
          <a href='/projects/new'>New Project</a>
        </nav>
      </header>
      <main>
        <Outlet />
      </main>
      <footer
        style={{
          marginTop: '3rem',
          borderTop: '1px solid #eee',
          paddingTop: '1rem',
          color: '#888',
          fontSize: '0.85rem',
        }}
      >
        Built with Putnami
      </footer>
    </div>
  );
}
