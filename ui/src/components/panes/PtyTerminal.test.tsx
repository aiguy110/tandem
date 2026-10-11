import { cleanup, render, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

const { renderer } = vi.hoisted(() => ({
  renderer: {
    write: vi.fn(),
    onData: vi.fn(),
    resize: vi.fn(),
    cols: 80,
    rows: 24,
    focus: vi.fn(),
    fit: vi.fn(),
    dispose: vi.fn(),
  },
}));

vi.mock('../../terminal/TerminalRenderer', () => ({
  createRenderer: vi.fn().mockResolvedValue({ renderer, engine: 'ghostty' }),
  selectedEngine: vi.fn().mockReturnValue('ghostty'),
}));

import { latestTerminalURL, PtyTerminal } from './PtyTerminal';

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

describe('PtyTerminal', () => {
  it('finds the latest URL in terminal output without ANSI styling or prose punctuation', () => {
    expect(latestTerminalURL('\u001b[4mhttps://first.test/a\u001b[0m then https://last.test/oauth?code=a+b.')).toBe(
      'https://last.test/oauth?code=a+b',
    );
  });

  it('prefers a complete OSC 8 target over its hard-wrapped visible label', () => {
    const target = 'https://claude.com/cai/oauth/authorize?code=true&client_id=complete';
    const output = `\u001b]8;;${target}\u001b\\https://claude.com/cai/oauth/authorize?code=tr\r\nue&client_id=complete\u001b]8;;\u001b\\`;
    expect(latestTerminalURL(output)).toBe(target);
  });

  it('forwards plain-text paste to the terminal input once', async () => {
    vi.stubGlobal('ResizeObserver', class {
      observe() {}
      disconnect() {}
    });
    const onData = vi.fn();
    const view = render(
      <PtyTerminal subscribe={() => () => {}} onData={onData} onResize={vi.fn()} />,
    );
    await waitFor(() => expect(renderer.focus).toHaveBeenCalled());

    const event = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
    Object.defineProperty(event, 'clipboardData', {
      value: { getData: vi.fn().mockReturnValue('first line\nsecond line') },
    });
    view.container.querySelector('.term-renderer')!.dispatchEvent(event);

    expect(event.defaultPrevented).toBe(true);
    expect(onData).toHaveBeenCalledTimes(1);
    expect(onData).toHaveBeenCalledWith('first line\nsecond line');
  });
});
