// The global key handler: turns keystrokes into command IDs via the scope-aware
// ChordMatcher (D10). Bare keys never fire while typing (text-input scope); when
// a modal is open the modal owns its keys, so global dispatch is suppressed.

import { useEffect, useRef } from 'react';
import { useStore } from './store';
import { ChordMatcher, comboFromEvent, hasModifier, loadBindings } from './commands/keymap';
import { runCommand } from './commands/registry';

function scopeOf(target: EventTarget | null, modalOpen: boolean, focused: boolean): 'global' | 'agent-focused' | 'modal-open' | 'text-input' {
  if (modalOpen) return 'modal-open';
  const el = target as HTMLElement | null;
  if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.isContentEditable)) return 'text-input';
  return focused ? 'agent-focused' : 'global';
}

export function useGlobalKeys(): void {
  const matcher = useRef(new ChordMatcher(loadBindings()));

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const st = useStore.getState();
      const scope = scopeOf(e.target, st.modal !== 'none', !!st.focusedId);

      // Modals manage their own keys (Escape/Enter/arrows). Don't double-fire.
      if (scope === 'modal-open') return;

      // In a text field only modifier chords (e.g. ⌘K) may fire — bare keys type.
      if (scope === 'text-input' && !hasModifier(e)) return;

      const combo = comboFromEvent(e);
      // Ignore lone modifier presses.
      if (['mod', 'shift', 'alt', 'control', 'meta'].includes(e.key.toLowerCase())) return;

      const res = matcher.current.feed(combo);
      if (!res) return;
      if (res.kind === 'partial') {
        e.preventDefault();
        return;
      }
      // A resolved command. agent.interrupt only when working; otherwise Escape
      // is a no-op here (modals handle their own Escape).
      if (res.id === 'agent.interrupt') {
        const a = st.focusedId ? st.agents[st.focusedId] : undefined;
        if (!a || a.status !== 'working') return;
      }
      e.preventDefault();
      runCommand(res.id);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);
}
