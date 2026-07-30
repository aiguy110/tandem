import { afterEach, describe, expect, it, vi } from 'vitest';
import { usesSoftKeyboard } from './mobile';

describe('usesSoftKeyboard', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('detects a coarse pointer without hover as a soft-keyboard device', () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: true }));

    expect(usesSoftKeyboard()).toBe(true);
    expect(window.matchMedia).toHaveBeenCalledWith('(hover: none) and (pointer: coarse)');
  });

  it('does not classify desktop pointer input as a soft keyboard', () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: false }));

    expect(usesSoftKeyboard()).toBe(false);
  });
});
