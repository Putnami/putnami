import { describe, expect, test } from 'bun:test';
import { Tooltip } from '../../src/components/tooltip';
import { render } from './render';

describe('Tooltip', () => {
  test('renders trigger', () => {
    const html = render(
      <Tooltip label='Hover me'>
        <button type='button'>Trigger</button>
      </Tooltip>,
    );
    // Tooltip is closed by default in SSR
    expect(html).toContain('button');
    expect(html).toContain('Trigger');
    expect(html).not.toContain('Hover me');
  });

  // Tooltip interaction requires DOM events like onMouseEnter/onFocus which are not viable in static render.
  // Testing the structural rendering output guarantees contract basics in this case.
});
