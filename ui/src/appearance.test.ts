import { afterEach, describe, expect, it } from 'vitest';
import {
  DEFAULT_APPEARANCE,
  TERM_FONT_RANGE,
  UI_FONT_RANGE,
  loadAppearance,
  saveAppearance,
  setLinked,
  setTermFontSize,
  setUiFontSize,
  uiScale,
} from './appearance';

afterEach(() => localStorage.clear());

describe('appearance', () => {
  it('moves both sizes together while linked, preserving the ratio', () => {
    const a = setUiFontSize(DEFAULT_APPEARANCE, 18);
    expect(a.uiFontSize).toBe(18);
    expect(a.termFontSize).toBe(16);
    const b = setTermFontSize(a, 12);
    expect(b.termFontSize).toBe(12);
    expect(b.uiFontSize).toBe(13.5);
  });

  it('adjusts sizes independently while unlinked', () => {
    const a = setUiFontSize(setLinked(DEFAULT_APPEARANCE, false), 16);
    expect(a.uiFontSize).toBe(16);
    expect(a.termFontSize).toBe(DEFAULT_APPEARANCE.termFontSize);
  });

  it('captures the current ratio when relinking so nothing jumps', () => {
    const unlinked = setTermFontSize(setLinked(DEFAULT_APPEARANCE, false), 15);
    const relinked = setLinked(unlinked, true);
    expect(relinked.termFontSize).toBe(15);
    expect(relinked.uiFontSize).toBe(13.5);
    expect(setUiFontSize(relinked, 18).termFontSize).toBe(20);
  });

  it('snaps to the step and clamps to the range', () => {
    expect(setUiFontSize(DEFAULT_APPEARANCE, 99).uiFontSize).toBe(UI_FONT_RANGE.max);
    expect(setTermFontSize(setLinked(DEFAULT_APPEARANCE, false), 1).termFontSize).toBe(TERM_FONT_RANGE.min);
    expect(setUiFontSize(DEFAULT_APPEARANCE, 14.2).uiFontSize).toBe(14);
  });

  it('scales the UI relative to the default base size', () => {
    expect(uiScale(DEFAULT_APPEARANCE)).toBe(1);
    expect(uiScale(setUiFontSize(DEFAULT_APPEARANCE, 16.5))).toBeCloseTo(16.5 / 13.5);
  });

  it('round-trips through storage and ignores garbage', () => {
    const a = setUiFontSize(setLinked(DEFAULT_APPEARANCE, false), 15);
    saveAppearance(a);
    expect(loadAppearance()).toEqual(a);
    localStorage.setItem('tandem.appearance', '{"uiFontSize":"big","linked":3}');
    expect(loadAppearance()).toEqual(DEFAULT_APPEARANCE);
    localStorage.setItem('tandem.appearance', 'not json');
    expect(loadAppearance()).toEqual(DEFAULT_APPEARANCE);
  });
});
