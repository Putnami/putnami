import { describe, expect, test } from 'bun:test';
import { Pagination } from '../../src/components/pagination';
import { render } from './render';

describe('Pagination', () => {
  test('renders active page with aria-current', () => {
    const html = render(<Pagination page={2} totalPages={5} />);

    // The current page shouldn't be a link or should have aria-current="page"
    expect(html).toContain('aria-current="page"');

    // We should see next and prev navigation labeled
    expect(html).toContain('aria-label="Next page"');
    expect(html).toContain('aria-label="Previous page"');
  });

  test('does not render if 1 or 0 totalPages', () => {
    const html = render(<Pagination page={1} totalPages={1} />);
    expect(html).toBe(''); // renders null
  });

  test('renders outer bounds and ellipses', () => {
    // 1 ... 4 5 6 ... 10
    const html = render(<Pagination page={5} totalPages={10} siblingCount={1} showFirstLast />);
    expect(html).toContain('aria-label="First page"');
    expect(html).toContain('aria-label="Last page"');
    expect(html).toContain('…');
  });

  const buttonTag = (html: string, label: string): string =>
    html.match(new RegExp(`<button[^>]*aria-label="${label}"[^>]*>`))?.[0] ?? '';

  test('sets the disabled attribute on the boundary navigation buttons', () => {
    const firstPage = render(<Pagination page={1} totalPages={5} showFirstLast />);
    expect(buttonTag(firstPage, 'Previous page')).toContain('disabled');
    expect(buttonTag(firstPage, 'First page')).toContain('disabled');
    expect(buttonTag(firstPage, 'Next page')).not.toContain('disabled');

    const lastPage = render(<Pagination page={5} totalPages={5} showFirstLast />);
    expect(buttonTag(lastPage, 'Next page')).toContain('disabled');
    expect(buttonTag(lastPage, 'Last page')).toContain('disabled');
    expect(buttonTag(lastPage, 'Previous page')).not.toContain('disabled');
  });
});
