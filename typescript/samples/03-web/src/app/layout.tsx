import { Outlet } from '@putnami/web';

export default function RootLayout() {
  return (
    <div style={{ maxWidth: 800, margin: '0 auto', padding: '2rem', fontFamily: 'system-ui' }}>
      <header style={{ marginBottom: '2rem', borderBottom: '1px solid #eee', paddingBottom: '1rem' }}>
        <h1>Putnami React App</h1>
        <nav>
          <a href='/' style={{ marginRight: '1rem' }}>
            Home
          </a>
          <a href='/tasks' style={{ marginRight: '1rem' }}>
            Tasks
          </a>
          <a href='/tasks/new'>New Task</a>
        </nav>
      </header>
      <main>
        <Outlet />
      </main>
    </div>
  );
}
