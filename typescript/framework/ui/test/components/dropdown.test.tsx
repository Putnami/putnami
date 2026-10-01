import { describe, expect, test } from 'bun:test';
import { createElement, Fragment, type ReactNode } from 'react';
import { Dropdown, DropdownItem, DropdownMenu, DropdownTrigger } from '../../src/components/dropdown';
import { render } from './render';

// Build a Fragment dynamically so Biome's noUselessFragments lint can't strip it
// at lint time — this lets us exercise the real Fragment-handling code path.
const fragmentOf = (...children: ReactNode[]) => createElement(Fragment, null, ...children);

describe('Dropdown', () => {
  test('renders trigger with aria attributes mapped from Context', () => {
    // We cannot easily test interaction using purely static render,
    // but we can ensure the trigger wrapper sets base mappings.
    const html = render(
      <Dropdown>
        <DropdownTrigger>
          <button type='button'>Open</button>
        </DropdownTrigger>
        <DropdownMenu>
          <DropdownItem>Item</DropdownItem>
        </DropdownMenu>
      </Dropdown>,
    );
    // isOpen is false by default
    expect(html).toContain('aria-haspopup="menu"');
    expect(html).toContain('aria-expanded="false"');
  });

  test('finds trigger when wrapped in a Fragment', () => {
    // Fragments around trigger/menu used to hide them from the lookup,
    // dropping the trigger entirely.
    const html = render(
      <Dropdown>
        {fragmentOf(
          <DropdownTrigger key='trigger'>
            <button type='button'>Open</button>
          </DropdownTrigger>,
          <DropdownMenu key='menu'>
            <DropdownItem>Item</DropdownItem>
          </DropdownMenu>,
        )}
      </Dropdown>,
    );

    expect(html).toContain('aria-haspopup="menu"');
    expect(html).toContain('aria-expanded="false"');
    expect(html).toContain('Open');
  });

  test('skips falsy children when finding trigger and menu', () => {
    // Conditional rendering (e.g. {show && <X />}) yields null/false children, which
    // Children.toArray drops; the lookup should still find the real trigger/menu.
    const show = true;
    const html = render(
      <Dropdown>
        {!!show && (
          <DropdownTrigger>
            <button type='button'>Open</button>
          </DropdownTrigger>
        )}
        {null}
        <DropdownMenu>
          <DropdownItem>Item</DropdownItem>
        </DropdownMenu>
      </Dropdown>,
    );

    expect(html).toContain('aria-haspopup="menu"');
    expect(html).toContain('Open');
  });
});
