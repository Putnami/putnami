import { describe, expect, test } from 'bun:test';
import { Breadcrumb, BreadcrumbItem, BreadcrumbLink } from '../../src/components/breadcrumb';
import { render } from './render';

describe('Breadcrumb', () => {
  test('renders navigation structure with separator', () => {
    const html = render(
      <Breadcrumb separator='/'>
        <BreadcrumbItem>
          <BreadcrumbLink href='/home'>Home</BreadcrumbLink>
        </BreadcrumbItem>
        <BreadcrumbItem isCurrentPage>
          <BreadcrumbLink isCurrentPage>Current</BreadcrumbLink>
        </BreadcrumbItem>
      </Breadcrumb>,
    );

    expect(html).toContain('aria-label="Breadcrumb"');
    expect(html).toContain('href="/home"');

    // The current page link should have aria-current
    expect(html).toContain('aria-current="page"');

    // Custom string separator
    expect(html).toContain('/');
  });
});
