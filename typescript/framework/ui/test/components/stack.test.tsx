import { describe, expect, test } from 'bun:test';
import { HStack, Stack, VStack } from '../../src/components/stack';
import { render } from './render';

describe('Stack', () => {
  test('renders default flex column stack', () => {
    const html = render(<Stack>Content</Stack>);
    expect(html).toContain('Content');
  });

  test('HStack and VStack shorthand variants', () => {
    const hHtml = render(<HStack>Row</HStack>);
    expect(hHtml).toContain('Row');

    const vHtml = render(<VStack>Column</VStack>);
    expect(vHtml).toContain('Column');
  });
});
