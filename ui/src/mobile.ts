const SOFT_KEYBOARD_MEDIA = '(hover: none) and (pointer: coarse)';

export function usesSoftKeyboard(): boolean {
  return typeof window !== 'undefined'
    && !!window.matchMedia
    && window.matchMedia(SOFT_KEYBOARD_MEDIA).matches;
}
