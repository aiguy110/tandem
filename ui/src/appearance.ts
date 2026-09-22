// Appearance preferences: app-wide and terminal font sizes, optionally locked
// together. Pure state transitions live here so the modal, the store, and the
// terminal renderer agree on one model; persistence is a single localStorage key.

export interface Appearance {
  // Base UI font size in px; every --fs-* token scales from it (styles.css).
  uiFontSize: number;
  // Terminal cell font size in px.
  termFontSize: number;
  // While locked, the terminal is held at `linkRatio` × the UI size, so
  // adjusting either side moves both and preserves the ratio chosen at lock
  // time without accumulating rounding drift.
  linked: boolean;
  linkRatio: number;
}

export const UI_FONT_DEFAULT = 13.5;
export const TERM_FONT_DEFAULT = 12;
export const UI_FONT_RANGE = { min: 10, max: 20, step: 0.5 } as const;
export const TERM_FONT_RANGE = { min: 8, max: 28, step: 0.5 } as const;

export const DEFAULT_APPEARANCE: Appearance = {
  uiFontSize: UI_FONT_DEFAULT,
  termFontSize: TERM_FONT_DEFAULT,
  linked: true,
  linkRatio: TERM_FONT_DEFAULT / UI_FONT_DEFAULT,
};

const STORAGE_KEY = 'tandem.appearance';

function clampStep(v: number, range: { min: number; max: number; step: number }): number {
  const stepped = Math.round(v / range.step) * range.step;
  return Math.min(range.max, Math.max(range.min, stepped));
}

// The UI scale factor applied to every font-size token.
export function uiScale(a: Appearance): number {
  return a.uiFontSize / UI_FONT_DEFAULT;
}

export function setUiFontSize(a: Appearance, px: number): Appearance {
  const uiFontSize = clampStep(px, UI_FONT_RANGE);
  if (!a.linked) return { ...a, uiFontSize };
  return { ...a, uiFontSize, termFontSize: clampStep(uiFontSize * a.linkRatio, TERM_FONT_RANGE) };
}

export function setTermFontSize(a: Appearance, px: number): Appearance {
  const termFontSize = clampStep(px, TERM_FONT_RANGE);
  if (!a.linked) return { ...a, termFontSize };
  return { ...a, termFontSize, uiFontSize: clampStep(termFontSize / a.linkRatio, UI_FONT_RANGE) };
}

// Locking captures the current ratio, so it never jumps either size.
export function setLinked(a: Appearance, linked: boolean): Appearance {
  return linked ? { ...a, linked, linkRatio: a.termFontSize / a.uiFontSize } : { ...a, linked };
}

export function loadAppearance(): Appearance {
  try {
    const raw: unknown = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? 'null');
    if (!raw || typeof raw !== 'object') return DEFAULT_APPEARANCE;
    const r = raw as Partial<Appearance>;
    const num = (v: unknown, d: number) => (typeof v === 'number' && Number.isFinite(v) && v > 0 ? v : d);
    const uiFontSize = clampStep(num(r.uiFontSize, UI_FONT_DEFAULT), UI_FONT_RANGE);
    const termFontSize = clampStep(num(r.termFontSize, TERM_FONT_DEFAULT), TERM_FONT_RANGE);
    return {
      uiFontSize,
      termFontSize,
      linked: typeof r.linked === 'boolean' ? r.linked : DEFAULT_APPEARANCE.linked,
      linkRatio: num(r.linkRatio, termFontSize / uiFontSize),
    };
  } catch {
    return DEFAULT_APPEARANCE;
  }
}

export function saveAppearance(a: Appearance): void {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(a));
}
