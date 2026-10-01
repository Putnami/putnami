import type { CSSProperties } from 'react';
import type { SupportStatus } from '../lib/support/catalog';
import { type Tool, statusMeta, TOOLS } from '../lib/tools';

/**
 * Tool identity badge — a monospace glyph tile tinted with the tool accent.
 * Used in the docs hub cards, the home tool strip, the scoped sidebar switcher,
 * and the command palette. Inline styles keep it SSR-safe inside both static
 * pages and isolated island roots.
 */
export function ToolGlyph({ tool, size = 34, style }: { tool: string | Tool; size?: number; style?: CSSProperties }) {
  const t = typeof tool === 'string' ? TOOLS[tool] : tool;
  if (!t) return null;
  const fontSize = size <= 26 ? 10 : size >= 44 ? 15 : 12;
  return (
    <span
      aria-hidden='true'
      style={{
        width: size,
        height: size,
        flex: `0 0 ${size}px`,
        display: 'inline-flex',
        alignItems: 'center',
        justifyContent: 'center',
        fontFamily: 'var(--font-mono)',
        fontWeight: 700,
        fontSize,
        letterSpacing: '-0.01em',
        color: t.accent,
        background: `color-mix(in srgb, ${t.accent} 14%, transparent)`,
        border: `1px solid color-mix(in srgb, ${t.accent} 35%, transparent)`,
        borderRadius: 'var(--radius-md)',
        ...style,
      }}
    >
      {t.glyph}
    </span>
  );
}

/**
 * Small status indicator (colored dot + label) for a surface's REVIEWED public
 * support status. The caller resolves the status from `putnami.support.json`;
 * an unclassified surface renders nothing rather than inventing a badge.
 */
export function StatusDot({ status }: { status: SupportStatus | undefined }) {
  if (!status) return null;
  const { color, label } = statusMeta(status);
  return (
    <span
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: 6,
        fontSize: 12,
        color: 'var(--color-text-muted)',
      }}
    >
      <span style={{ width: 7, height: 7, borderRadius: '50%', background: color }} />
      {label}
    </span>
  );
}
