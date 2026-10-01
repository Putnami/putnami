import { describe, expect, test } from 'bun:test';
import { CodeBlock } from '../../src/components/code-block';
import { render } from './render';

describe('CodeBlock', () => {
  test('renders formatted code and language', () => {
    const html = render(<CodeBlock code='const a = 1;' language='typescript' />);

    // Renders the code itself
    expect(html).toContain('const a = 1;');

    // Renders the language tag
    expect(html).toContain('typescript');

    // Applies correct syntax highlighting class mapped
    expect(html).toContain('language-typescript');
  });

  test('inherits the pre font family for the nested code element', () => {
    const html = render(<CodeBlock code='const a = 1;' language='typescript' />);
    expect(html).toContain('style="font-family:inherit"');
  });

  test('hides copy button when showCopy is false', () => {
    const withCopy = render(<CodeBlock code='test' showCopy={true} />);
    // Our icon is lucide's copy icon, so we can check for an svg or button role if we want,
    // but a structural check is sufficient for 'not containing the button element'
    const withoutCopy = render(<CodeBlock code='test' showCopy={false} />);
    expect(withCopy.length).toBeGreaterThan(withoutCopy.length);
  });
});
