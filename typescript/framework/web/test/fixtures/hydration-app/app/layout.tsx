import { layout, Outlet } from '@putnami/web';

export default layout().render(function RootLayout() {
  return (
    <main data-testid='layout'>
      <Outlet />
    </main>
  );
});
