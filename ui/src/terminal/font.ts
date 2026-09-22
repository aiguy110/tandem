// Terminal font selection and loading. Both engines size their cell grid from
// the font's own metrics, so the face has to be loaded *before* a terminal is
// constructed — see ensureTermFont below.

// Cascadia Mono NF is self-hosted (@font-face in styles.css) so Nerd Fonts and
// powerline glyphs render without a network round-trip. The trailing families
// are what an OS-installed Nerd Font would be called, giving anyone who has one
// a path to their own glyphs before the generic monospace fallback.
export const TERM_FONT_FAMILY =
  "'Cascadia Mono NF', 'Symbols Nerd Font Mono', 'CaskaydiaCove Nerd Font Mono', ui-monospace, 'SF Mono', Menlo, monospace";

// Override with ?termFont=... or localStorage, mirroring the engine switch.
export function selectedFontFamily(): string {
  const q = new URLSearchParams(window.location.search).get('termFont');
  if (q) return q;
  return localStorage.getItem('tandem.termFont') || TERM_FONT_FAMILY;
}

// Resolve the terminal font before the grid is measured. ghostty-web measures
// `measureText('M')` once in its constructor; xterm.js does the same in open().
// Constructing against an unloaded webfont therefore locks in the *fallback's*
// cell size, and every cell stays misaligned until something forces a
// remeasure. Awaiting the face first is the fix; a failure here is never fatal,
// it just means the terminal renders in the fallback family.
export async function ensureTermFont(family: string, fontSize: number): Promise<void> {
  if (typeof document === 'undefined' || !document.fonts?.load) return;
  try {
    await Promise.all([
      document.fonts.load(`${fontSize}px ${family}`, 'M'),
      document.fonts.load(`bold ${fontSize}px ${family}`, 'M'),
    ]);
  } catch {
    /* fallback metrics are still usable */
  }
}
