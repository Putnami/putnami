import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { responsiveCss, up, down, between } from '../../src/layout/responsive';
import { defaultTheme } from '../../src/theme/default';

// Breakpoint values from the default theme
const { sm, md, lg, xl } = defaultTheme.breakpoints; // 640, 768, 1024, 1280

describe('up', () => {
  it('returns a min-width media query for sm', () => {
    expect(up(defaultTheme, 'sm')).toBe(`@media (min-width: ${sm}px)`);
  });

  it('returns a min-width media query for md', () => {
    expect(up(defaultTheme, 'md')).toBe(`@media (min-width: ${md}px)`);
  });

  it('returns a min-width media query for lg', () => {
    expect(up(defaultTheme, 'lg')).toBe(`@media (min-width: ${lg}px)`);
  });

  it('returns a min-width media query for xl', () => {
    expect(up(defaultTheme, 'xl')).toBe(`@media (min-width: ${xl}px)`);
  });

  it('returns a min-width media query for xxl', () => {
    const { xxl } = defaultTheme.breakpoints;
    expect(up(defaultTheme, 'xxl')).toBe(`@media (min-width: ${xxl}px)`);
  });
});

describe('down', () => {
  it('returns a max-width media query for sm (exclusive)', () => {
    expect(down(defaultTheme, 'sm')).toBe(`@media (max-width: ${sm - 0.01}px)`);
  });

  it('returns a max-width media query for md (exclusive)', () => {
    expect(down(defaultTheme, 'md')).toBe(`@media (max-width: ${md - 0.01}px)`);
  });

  it('returns a max-width media query for lg (exclusive)', () => {
    expect(down(defaultTheme, 'lg')).toBe(`@media (max-width: ${lg - 0.01}px)`);
  });

  it('returns a max-width media query for xl (exclusive)', () => {
    expect(down(defaultTheme, 'xl')).toBe(`@media (max-width: ${xl - 0.01}px)`);
  });
});

describe('between', () => {
  it('returns a range media query from sm to md', () => {
    expect(between(defaultTheme, 'sm', 'md')).toBe(`@media (min-width: ${sm}px) and (max-width: ${md - 0.01}px)`);
  });

  it('returns a range media query from md to lg', () => {
    expect(between(defaultTheme, 'md', 'lg')).toBe(`@media (min-width: ${md}px) and (max-width: ${lg - 0.01}px)`);
  });

  it('returns a range media query from lg to xl', () => {
    expect(between(defaultTheme, 'lg', 'xl')).toBe(`@media (min-width: ${lg}px) and (max-width: ${xl - 0.01}px)`);
  });

  it('produces a valid range when min equals max (degenerate case)', () => {
    const result = between(defaultTheme, 'sm', 'sm');
    expect(result).toBe(`@media (min-width: ${sm}px) and (max-width: ${sm - 0.01}px)`);
  });
});

describe('responsiveCss', () => {
  describe('scalar value (non-array)', () => {
    specTest(
      'returns a simple css declaration',
      {
        feature: 'typescript/ui-system',
        requirement: 'responsive-layout',
        check: 'a-scalar-value-produces-a-simple-declaration',
      },
      () => {
        const result = responsiveCss(defaultTheme, 'padding', '16px');
        expect(result).toBe('padding: 16px;');
      },
    );

    it('applies transform to scalar value', () => {
      const result = responsiveCss(defaultTheme, 'font-size', 16, (v) => `${v}px`);
      expect(result).toBe('font-size: 16px;');
    });

    it('handles number scalar without transform', () => {
      const result = responsiveCss(defaultTheme, 'z-index', 10);
      expect(result).toBe('z-index: 10;');
    });
  });

  describe('array of responsive values', () => {
    specTest(
      'uses first array element as base (mobile-first)',
      {
        feature: 'typescript/ui-system',
        requirement: 'responsive-layout',
        check: 'the-first-array-value-is-the-base-declaration',
      },
      () => {
        const result = responsiveCss(defaultTheme, 'padding', ['8px', '16px']);
        expect(result).toContain('padding: 8px;');
      },
    );

    it('adds sm breakpoint for second array element', () => {
      const result = responsiveCss(defaultTheme, 'padding', ['8px', '16px']);
      expect(result).toContain(`@media (min-width: ${sm}px)`);
      expect(result).toContain('padding: 16px;');
    });

    it('adds md breakpoint for third array element', () => {
      const result = responsiveCss(defaultTheme, 'padding', ['8px', '16px', '24px']);
      expect(result).toContain(`@media (min-width: ${md}px)`);
      expect(result).toContain('padding: 24px;');
    });

    it('adds lg breakpoint for fourth array element', () => {
      const result = responsiveCss(defaultTheme, 'padding', ['8px', '16px', '24px', '32px']);
      expect(result).toContain(`@media (min-width: ${lg}px)`);
      expect(result).toContain('padding: 32px;');
    });

    specTest(
      'handles single-element array (no breakpoints added)',
      {
        feature: 'typescript/ui-system',
        requirement: 'responsive-layout',
        check: 'a-single-element-array-adds-no-breakpoint',
      },
      () => {
        const result = responsiveCss(defaultTheme, 'margin', ['4px']);
        expect(result).toBe('margin: 4px;');
        expect(result).not.toContain('@media');
      },
    );

    it('applies transform to each array element', () => {
      const result = responsiveCss(defaultTheme, 'gap', [2, 4], (v) => `${v * 0.25}rem`);
      expect(result).toContain('gap: 0.5rem;');
      expect(result).toContain('gap: 1rem;');
    });

    specTest(
      'skips null values in the array',
      { feature: 'typescript/ui-system', requirement: 'responsive-layout', check: 'a-null-entry-is-skipped' },
      () => {
        const result = responsiveCss(defaultTheme, 'padding', ['8px', null as unknown as string, '24px']);
        expect(result).not.toContain(`@media (min-width: ${sm}px)`);
        expect(result).toContain(`@media (min-width: ${md}px)`);
      },
    );

    it('adds xl breakpoint for fifth array element', () => {
      const result = responsiveCss(defaultTheme, 'padding', ['8px', '16px', '24px', '32px', '40px']);
      expect(result).toContain(`@media (min-width: ${xl}px)`);
      expect(result).toContain('padding: 40px;');
    });

    it('adds xxl breakpoint for sixth array element', () => {
      const { xxl } = defaultTheme.breakpoints;
      const result = responsiveCss(defaultTheme, 'padding', ['8px', '16px', '24px', '32px', '40px', '48px']);
      expect(result).toContain(`@media (min-width: ${xxl}px)`);
      expect(result).toContain('padding: 48px;');
    });

    specTest(
      'reaches every theme breakpoint, matching up()',
      {
        feature: 'typescript/ui-system',
        requirement: 'responsive-layout',
        check: 'later-values-map-in-order-to-every-declared-breakpoint',
      },
      () => {
        const { xxl } = defaultTheme.breakpoints;
        const result = responsiveCss(defaultTheme, 'padding', ['4px', '8px', '12px', '16px', '20px', '24px']);
        // All five breakpoints advertised by the theme and up() are reachable via array props.
        expect(result).toContain('padding: 4px;');
        expect(result).toContain(`@media (min-width: ${sm}px)`);
        expect(result).toContain(`@media (min-width: ${md}px)`);
        expect(result).toContain(`@media (min-width: ${lg}px)`);
        expect(result).toContain(`@media (min-width: ${xl}px)`);
        expect(result).toContain(`@media (min-width: ${xxl}px)`);
      },
    );

    specTest(
      'ignores extra array elements beyond the defined breakpoints',
      {
        feature: 'typescript/ui-system',
        requirement: 'responsive-layout',
        check: 'surplus-values-beyond-the-breakpoints-are-ignored',
      },
      () => {
        // breakpointOrder has five entries (sm..xxl) — a 7th element has no mapping.
        const { xxl } = defaultTheme.breakpoints;
        const result = responsiveCss(defaultTheme, 'padding', ['4px', '8px', '12px', '16px', '20px', '24px', '28px']);
        // Should not crash; the 7th value (28px) is dropped because there is no breakpoint after xxl.
        expect(result).toContain(`@media (min-width: ${xxl}px) { padding: 24px; }`);
        expect(result).not.toContain('padding: 28px;');
      },
    );
  });
});
