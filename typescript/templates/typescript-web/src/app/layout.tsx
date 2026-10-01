import { layout, Outlet } from '@putnami/web';
import { Container, Flex, Link, ThemeProvider, ThemeToggle } from '@putnami/ui';

export default layout().render(function RootLayout() {
  return (
    <ThemeProvider>
      <Flex
        as='nav'
        align='center'
        px='lg'
        py='sm'
        style={{ borderBottom: '1px solid var(--color-border)', background: 'var(--color-surface)' }}
      >
        <Link to='/' style={{ fontWeight: 700, fontSize: '1.1rem' }}>
          Putnami
        </Link>
        <Flex align='center' ml='lg' style={{ gap: 'var(--space-lg)' }}>
          <Link to='/about'>About</Link>
          <Link to='/guestbook'>Guestbook</Link>
        </Flex>
        <ThemeToggle style={{ marginLeft: 'auto' }} />
      </Flex>
      <Container as='main' py='xl'>
        <Outlet />
      </Container>
    </ThemeProvider>
  );
});
