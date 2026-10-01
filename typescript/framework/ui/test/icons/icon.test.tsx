import { describe, expect, test } from 'bun:test';
import {
  ClockIcon,
  createIcon,
  DropletsIcon,
  type Icon,
  LogOutIcon,
  ShieldIcon,
  TargetIcon,
  TrendingUpIcon,
  WalletIcon,
  ZapIcon,
} from '../../src/icons';
import { render } from '../components/render';

// A tiny icon built from one of each node kind so we can assert the rendered SVG.
const TestIcon = createIcon('test-icon', [
  ['path', { d: 'M0 0h24' }],
  ['circle', { cx: 12, cy: 12, r: 10 }],
]);

describe('createIcon', () => {
  test('renders an <svg> carrying the shared default attributes', () => {
    const html = render(<TestIcon />);
    expect(html).toContain('<svg');
    expect(html).toContain('xmlns="http://www.w3.org/2000/svg"');
    expect(html).toContain('fill="none"');
    expect(html).toContain('viewBox="0 0 24 24"');
    expect(html).toContain('stroke-linecap="round"');
    expect(html).toContain('stroke-linejoin="round"');
  });

  test('applies the default size, color, and stroke width', () => {
    const html = render(<TestIcon />);
    expect(html).toContain('width="24"');
    expect(html).toContain('height="24"');
    expect(html).toContain('stroke="currentColor"');
    expect(html).toContain('stroke-width="2"');
  });

  test('is hidden from assistive tech by default', () => {
    const html = render(<TestIcon />);
    expect(html).toContain('aria-hidden="true"');
  });

  test('maps size to both width and height', () => {
    const html = render(<TestIcon size={32} />);
    expect(html).toContain('width="32"');
    expect(html).toContain('height="32"');
  });

  test('maps color to the stroke', () => {
    const html = render(<TestIcon color='red' />);
    expect(html).toContain('stroke="red"');
  });

  test('passes a literal strokeWidth straight through', () => {
    const html = render(<TestIcon strokeWidth={3} />);
    expect(html).toContain('stroke-width="3"');
  });

  test('absoluteStrokeWidth normalises the stroke to (strokeWidth * 24) / size', () => {
    // size=48 → 24/48 scale, so a strokeWidth of 2 renders as 1 to keep the visual weight.
    const html = render(<TestIcon size={48} strokeWidth={2} absoluteStrokeWidth />);
    expect(html).toContain('stroke-width="1"');
    expect(html).not.toContain('stroke-width="2"');
  });

  test('absoluteStrokeWidth at the default size is the identity', () => {
    const html = render(<TestIcon size={24} strokeWidth={2} absoluteStrokeWidth />);
    expect(html).toContain('stroke-width="2"');
  });

  test('renders each node as a child element with its attributes', () => {
    const html = render(<TestIcon />);
    expect(html).toContain('<path d="M0 0h24"');
    expect(html).toContain('<circle cx="12" cy="12" r="10"');
  });

  test('forwards arbitrary props (className, data-*) to the svg', () => {
    const html = render(<TestIcon className='star' data-testid='icon' />);
    expect(html).toContain('class="star"');
    expect(html).toContain('data-testid="icon"');
  });

  test('...rest overrides the defaults it follows (aria-hidden, stroke)', () => {
    // `stroke` and `aria-hidden` are set before `...rest`, so a caller can override them.
    const html = render(<TestIcon stroke='blue' aria-hidden={false} />);
    expect(html).toContain('stroke="blue"');
    expect(html).toContain('aria-hidden="false"');
    expect(html).not.toContain('aria-hidden="true"');
  });

  test('exposes the icon name as the component displayName', () => {
    expect(TestIcon.displayName).toBe('test-icon');
  });
});

describe('exported icons', () => {
  const icons: [string, Icon][] = [
    ['ClockIcon', ClockIcon],
    ['DropletsIcon', DropletsIcon],
    ['LogOutIcon', LogOutIcon],
    ['ShieldIcon', ShieldIcon],
    ['TargetIcon', TargetIcon],
    ['TrendingUpIcon', TrendingUpIcon],
    ['WalletIcon', WalletIcon],
    ['ZapIcon', ZapIcon],
  ];

  test('all 8 icons are exported', () => {
    expect(icons).toHaveLength(8);
  });

  for (const [name, IconComponent] of icons) {
    test(`${name} renders an <svg> and carries its displayName`, () => {
      const html = render(<IconComponent />);
      expect(html).toContain('<svg');
      expect(html).toContain('viewBox="0 0 24 24"');
      expect(typeof IconComponent.displayName).toBe('string');
      expect(IconComponent.displayName?.length).toBeGreaterThan(0);
    });
  }

  test('ClockIcon renders its specific path and circle', () => {
    const html = render(<ClockIcon />);
    expect(html).toContain('<path d="M12 6v6l4 2"');
    expect(html).toContain('<circle cx="12" cy="12" r="10"');
  });
});
