import { describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { Button } from '../../src/components/button';
import { render } from './render';

describe('Button', () => {
  specTest(
    'loading state exposes aria-busy and disables interaction',
    {
      feature: 'typescript/ui-system',
      requirement: 'interactive-semantics',
      check: 'a-loading-control-exposes-busy-and-disables-interaction',
    },
    () => {
      const html = render(<Button loading>Submit</Button>);
      expect(html).toContain('aria-busy="true"');
      expect(html).toContain('aria-disabled="true"');
      expect(html).toContain('disabled=""');
    },
  );

  specTest(
    'disabled state sets accessibility attributes',
    {
      feature: 'typescript/ui-system',
      requirement: 'interactive-semantics',
      check: 'a-disabled-control-sets-its-accessibility-attributes',
    },
    () => {
      const html = render(<Button disabled>Submit</Button>);
      expect(html).toContain('aria-disabled="true"');
      expect(html).toContain('disabled=""');
    },
  );

  test('custom type is forwarded', () => {
    const html = render(<Button type='submit'>Submit</Button>);
    expect(html).toContain('type="submit"');
  });
});
