// Scope-aware, rebindable single-key + chord keymap (D10). Mirrors the
// keybindings.json model: keys → stable command IDs, resolved by the current
// scope so a bare `c` spawns an agent globally but types "c" in a prompt field.

// Kept as a persisted keymap value for backward compatibility. It means that
// a Tandem session is focused; command IDs and stored bindings remain stable.
export type Scope = 'global' | 'agent-focused' | 'modal-open' | 'text-input';

// A binding is a chord: space-separated steps, each step a '+'-joined combo.
// Examples: "c", "shift+c", "mod+k", "g a". `mod` = Cmd on mac, Ctrl elsewhere.
export type Binding = string;

export interface KeymapDefault {
  command: string;
  binding: Binding;
}

// Illustrative defaults from docs/spawn-and-workspaces.md — all rebindable.
export const DEFAULT_BINDINGS: Record<string, Binding> = {
  'agent.spawn': 'c',
  'agent.spawn.sibling': 'shift+c',
  'agent.resume': 'shift+r',
  'palette.open': 'mod+k',
  'nav.goToAgent': 'g a',
  'nav.next': 'j',
  'nav.prev': 'k',
  'pane.chat': '1',
  'pane.shell': '2',
  'pane.diff': '3',
  'pane.browser': '4',
  'approvals.approveFocused': 'a',
  'approvals.denyFocused': 'd',
  'browser.toggleWheel': 'w',
  'agent.close': 'backspace',
  'agent.interrupt': 'escape',
};

const LS_KEY = 'tandem.keybindings';

export function loadBindings(): Record<string, Binding> {
  try {
    const raw = localStorage.getItem(LS_KEY);
    if (raw) return { ...DEFAULT_BINDINGS, ...(JSON.parse(raw) as Record<string, Binding>) };
  } catch {
    /* ignore malformed */
  }
  return { ...DEFAULT_BINDINGS };
}

export function saveBindings(overrides: Record<string, Binding>): void {
  localStorage.setItem(LS_KEY, JSON.stringify(overrides));
}

export function resetBindings(): void {
  localStorage.removeItem(LS_KEY);
}

const isMac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform);

// Normalize a KeyboardEvent into a combo token like "mod+k", "shift+c", "1".
export function comboFromEvent(e: KeyboardEvent): string {
  const parts: string[] = [];
  const mod = isMac ? e.metaKey : e.ctrlKey;
  if (mod) parts.push('mod');
  if (e.altKey) parts.push('alt');
  if (e.shiftKey && e.key.length !== 1) parts.push('shift'); // shift folded into printable keys
  let key = e.key.toLowerCase();
  if (key === ' ') key = 'space';
  // For printable single chars, shift is represented by the resulting char case.
  if (e.key.length === 1) {
    if (e.shiftKey && /[a-z]/i.test(e.key)) parts.push('shift');
    key = e.key.toLowerCase();
  }
  parts.push(key);
  return parts.join('+');
}

export function hasModifier(e: KeyboardEvent): boolean {
  return e.metaKey || e.ctrlKey || e.altKey;
}

// Pretty-print a binding for display in palettes/tooltips.
export function prettyBinding(b: Binding): string {
  return b
    .split(' ')
    .map((step) =>
      step
        .split('+')
        .map((k) => {
          if (k === 'mod') return isMac ? '⌘' : 'Ctrl';
          if (k === 'shift') return '⇧';
          if (k === 'alt') return isMac ? '⌥' : 'Alt';
          if (k === 'escape') return 'Esc';
          if (k === 'backspace') return '⌫';
          if (k === 'space') return '␣';
          return k.length === 1 ? k.toUpperCase() : k[0].toUpperCase() + k.slice(1);
        })
        .join(isMac ? '' : '+'),
    )
    .join(' ');
}

// A chord resolver: feed it combos; it tracks a pending prefix for multi-step
// chords (e.g. "g a") and returns the matched command id, or null.
export class ChordMatcher {
  private prefix: string[] = [];
  private timer: ReturnType<typeof setTimeout> | null = null;

  constructor(private bindings: Record<string, Binding>) {}

  setBindings(b: Record<string, Binding>): void {
    this.bindings = b;
  }

  reset(): void {
    this.prefix = [];
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = null;
    }
  }

  // Returns 'partial' if the combo begins a longer chord, a command id if it
  // completes one, or null if nothing matches.
  feed(combo: string): { kind: 'command'; id: string } | { kind: 'partial' } | null {
    const seq = [...this.prefix, combo];
    const seqStr = seq.join(' ');
    let exact: string | null = null;
    let isPrefix = false;
    for (const [id, binding] of Object.entries(this.bindings)) {
      if (binding === seqStr) exact = id;
      else if (binding.startsWith(seqStr + ' ')) isPrefix = true;
    }
    if (exact) {
      this.reset();
      return { kind: 'command', id: exact };
    }
    if (isPrefix) {
      this.prefix = seq;
      if (this.timer) clearTimeout(this.timer);
      this.timer = setTimeout(() => this.reset(), 900);
      return { kind: 'partial' };
    }
    this.reset();
    return null;
  }
}
