import { describe, expect, it } from 'bun:test';
import { createTheme, darkTheme, defaultTheme, lightColors, darkColors } from '../../src/theme/default';

describe('createTheme', () => {
  it('creates a light theme when colorMode is "light"', () => {
    const theme = createTheme('light');
    expect(theme.colorMode).toBe('light');
    expect(theme.colors).toBe(lightColors);
  });

  it('creates a dark theme when colorMode is "dark"', () => {
    const theme = createTheme('dark');
    expect(theme.colorMode).toBe('dark');
    expect(theme.colors).toBe(darkColors);
  });

  it('includes spacing function', () => {
    const theme = createTheme('light');
    expect(typeof theme.spacing).toBe('function');
    expect(theme.spacing(4)).toBe('1rem');
  });

  it('spacing function produces correct values', () => {
    const theme = createTheme('light');
    expect(theme.spacing(0)).toBe('0rem');
    expect(theme.spacing(1)).toBe('0.25rem');
    expect(theme.spacing(2)).toBe('0.5rem');
    expect(theme.spacing(8)).toBe('2rem');
  });

  it('includes space tokens', () => {
    const theme = createTheme('light');
    expect(theme.space.xs).toBe('0.25rem');
    expect(theme.space.sm).toBe('0.5rem');
    expect(theme.space.md).toBe('1rem');
    expect(theme.space.lg).toBe('1.5rem');
    expect(theme.space.xl).toBe('2rem');
    expect(theme.space.xxl).toBe('3rem');
  });

  it('includes breakpoints', () => {
    const theme = createTheme('light');
    expect(theme.breakpoints.sm).toBe(640);
    expect(theme.breakpoints.md).toBe(768);
    expect(theme.breakpoints.lg).toBe(1024);
    expect(theme.breakpoints.xl).toBe(1280);
    expect(theme.breakpoints.xxl).toBe(1536);
  });

  it('includes shadows', () => {
    const theme = createTheme('light');
    expect(theme.shadows.sm).toBeString();
    expect(theme.shadows.md).toBeString();
    expect(theme.shadows.lg).toBeString();
    expect(theme.shadows.xl).toBeString();
  });

  it('includes radii', () => {
    const theme = createTheme('light');
    expect(theme.radii.sm).toBe('0.125rem');
    expect(theme.radii.md).toBe('0.375rem');
    expect(theme.radii.lg).toBe('0.5rem');
    expect(theme.radii.full).toBe('9999px');
  });

  it('includes typography', () => {
    const theme = createTheme('light');
    expect(theme.typography.fontFamily.sans).toBeString();
    expect(theme.typography.fontFamily.mono).toBeString();
    expect(theme.typography.fontSizes.xs).toBe('0.75rem');
    expect(theme.typography.fontSizes.sm).toBe('0.875rem');
    expect(theme.typography.fontSizes.md).toBe('1rem');
  });

  it('includes heading styles', () => {
    const theme = createTheme('light');
    expect(theme.typography.headings.fontWeight).toBe(700);
    expect(theme.typography.headings.h1.fontSize).toBe('2.25rem');
    expect(theme.typography.headings.h2.fontSize).toBe('1.875rem');
  });

  it('includes font weights', () => {
    const theme = createTheme('light');
    expect(theme.typography.fontWeights.regular).toBe(400);
    expect(theme.typography.fontWeights.medium).toBe(500);
    expect(theme.typography.fontWeights.semibold).toBe(600);
    expect(theme.typography.fontWeights.bold).toBe(700);
  });

  it('includes the terminal palette (mode-independent)', () => {
    const light = createTheme('light');
    const dark = createTheme('dark');
    expect(light.terminal.bg).toBe('#0d1117');
    expect(light.terminal.prompt).toBe('#3fb950');
    expect(light.terminal.branch).toBe('#f85149');
    // Terminal is always dark — identical across color modes.
    expect(dark.terminal).toEqual(light.terminal);
  });
});

describe('defaultTheme', () => {
  it('is a light theme', () => {
    expect(defaultTheme.colorMode).toBe('light');
  });

  it('uses light colors', () => {
    expect(defaultTheme.colors).toBe(lightColors);
  });
});

describe('darkTheme', () => {
  it('is a dark theme', () => {
    expect(darkTheme.colorMode).toBe('dark');
  });

  it('uses dark colors', () => {
    expect(darkTheme.colors).toBe(darkColors);
  });
});

describe('lightColors', () => {
  it('has white and black', () => {
    expect(lightColors.white).toBe('#ffffff');
    expect(lightColors.black).toBe('#000000');
  });

  it('has text colors', () => {
    expect(lightColors.text.primary).toBeString();
    expect(lightColors.text.secondary).toBeString();
    expect(lightColors.text.disabled).toBeString();
  });

  it('has primary color with all semantic variants', () => {
    expect(lightColors.primary.main).toBeString();
    expect(lightColors.primary.light).toBeString();
    expect(lightColors.primary.dark).toBeString();
    expect(lightColors.primary.contrastText).toBe('#ffffff');
  });

  it('has all palette scales (50–900) for primary', () => {
    for (const scale of [50, 100, 200, 300, 400, 500, 600, 700, 800, 900] as const) {
      expect(lightColors.primary[scale]).toBeString();
    }
  });

  it('has error, success, warning, info colors', () => {
    expect(lightColors.error.main).toBeString();
    expect(lightColors.success.main).toBeString();
    expect(lightColors.warning.main).toBeString();
    expect(lightColors.info.main).toBeString();
  });

  it('has background and surface colors', () => {
    expect(lightColors.background).toBe('#ffffff');
    expect(lightColors.surface).toBeString();
    expect(lightColors.surfaceHover).toBeString();
    expect(lightColors.border).toBeString();
  });
});

describe('darkColors', () => {
  it('differs from lightColors in background', () => {
    expect(darkColors.background).not.toBe(lightColors.background);
  });

  it('has a different primary main from light', () => {
    expect(darkColors.primary.main).not.toBe(lightColors.primary.main);
  });

  it('has white and black', () => {
    expect(darkColors.white).toBe('#ffffff');
    expect(darkColors.black).toBe('#000000');
  });

  it('has text colors with light text for dark mode', () => {
    // dark mode text should be light
    expect(darkColors.text.primary).toBe('#fafafa');
  });

  it('has all palette scales (50–900) for primary', () => {
    for (const scale of [50, 100, 200, 300, 400, 500, 600, 700, 800, 900] as const) {
      expect(darkColors.primary[scale]).toBeString();
    }
  });
});
