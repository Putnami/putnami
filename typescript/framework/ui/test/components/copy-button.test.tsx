import { describe, expect, test } from 'bun:test';
import { CopyButton } from '../../src/components/copy-button';
import { render } from './render';

describe('CopyButton', () => {
  test('renders initial copy state with clipboard icon', () => {
    // Tests static rendering of the default idle state
    const html = render(<CopyButton text='copy me' />);

    expect(html).toContain('aria-label="Copy to clipboard"');
    expect(html).toContain('title="Copy to clipboard"');
    // Default clipboard icon rect/path SVG
    expect(html).toContain('<rect x="9" y="9" width="13" height="13" rx="2" ry="2"');
  });
});
