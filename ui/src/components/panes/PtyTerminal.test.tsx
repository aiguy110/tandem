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

import { PtyTerminal } from './PtyTerminal';

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

describe('PtyTerminal', () => {
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
