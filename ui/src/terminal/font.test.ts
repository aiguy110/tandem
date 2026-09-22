import { afterEach, describe, expect, it, vi } from 'vitest';
import { TERM_FONT_FAMILY, ensureTermFont, selectedFontFamily } from './font';

afterEach(() => {
  localStorage.clear();
  window.history.replaceState({}, '', '/');
  vi.unstubAllGlobals();
});

describe('selectedFontFamily', () => {
  it('defaults to the bundled Nerd Font stack', () => {
    expect(selectedFontFamily()).toBe(TERM_FONT_FAMILY);
    expect(TERM_FONT_FAMILY).toContain('Cascadia Mono NF');
  });

  it('prefers the query override over localStorage', () => {
    localStorage.setItem('tandem.termFont', 'Stored Mono');
    window.history.replaceState({}, '', '/?termFont=Query%20Mono');
    expect(selectedFontFamily()).toBe('Query Mono');
  });

  it('falls back to the stored family', () => {
    localStorage.setItem('tandem.termFont', 'Stored Mono');
    expect(selectedFontFamily()).toBe('Stored Mono');
  });
});

describe('ensureTermFont', () => {
  it('loads the regular and bold faces before the grid is measured', async () => {
    const load = vi.fn().mockResolvedValue([]);
    vi.stubGlobal('document', { ...document, fonts: { load } });
    await ensureTermFont('Test Mono', 12);
    expect(load).toHaveBeenCalledWith('12px Test Mono', 'M');
    expect(load).toHaveBeenCalledWith('bold 12px Test Mono', 'M');
  });

  it('resolves when the font cannot be loaded', async () => {
    const load = vi.fn().mockRejectedValue(new Error('nope'));
    vi.stubGlobal('document', { ...document, fonts: { load } });
    await expect(ensureTermFont('Test Mono', 12)).resolves.toBeUndefined();
  });
});
