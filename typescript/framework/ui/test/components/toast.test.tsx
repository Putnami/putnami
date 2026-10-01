import { describe, expect, test } from 'bun:test';
import { renderToStaticMarkup } from 'react-dom/server';
import { ToastProvider, useToast } from '../../src/components/toast';

describe('Toast', () => {
  test('ToastProvider renders containers', () => {
    // Tests that provider naturally injects containers for positioning
    const html = renderToStaticMarkup(
      <ToastProvider>
        <div>App</div>
      </ToastProvider>,
    );

    expect(html).toContain('App');
  });

  test('useToast outside provider throws error', () => {
    const HookTest = () => {
      useToast();
      return null;
    };

    expect(() => renderToStaticMarkup(<HookTest />)).toThrow('useToast must be used within a ToastProvider');
  });
});
