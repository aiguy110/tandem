import { useMemo, useState, useEffect } from 'react';
import { useStore, rankedOrder } from '../store';
import { buildCommands } from '../commands/registry';
import { loadBindings, prettyBinding } from '../commands/keymap';
import { fuzzyFilter } from '../fuzzy';

interface Entry {
  key: string;
  title: string;
  subtitle?: string;
  binding?: string;
  disabled?: boolean;
  run: () => void;
}

// Command palette (D10): every command + jump-to-agent-by-name, each row showing
// its keybinding inline so the palette teaches the hotkeys.
export function CommandPalette() {
  const setModal = useStore((s) => s.setModal);
  const focus = useStore((s) => s.focus);
  const order = useStore(rankedOrder);
  const agents = useStore((s) => s.agents);

  const [query, setQuery] = useState('');
  const [sel, setSel] = useState(0);

  const entries = useMemo<Entry[]>(() => {
    const bindings = loadBindings();
    const cmds: Entry[] = buildCommands().map((c) => ({
      key: c.id,
      title: c.title,
      subtitle: c.subtitle,
      binding: bindings[c.id] ? prettyBinding(bindings[c.id]) : undefined,
      disabled: c.enabled ? !c.enabled() : false,
      run: () => {
        setModal('none');
        c.run();
      },
    }));
    const jumps: Entry[] = order.map((id) => ({
      key: `jump:${id}`,
      title: `Go to ${agents[id].name}`,
      subtitle: `${agents[id].workspace.repo}${agents[id].workspace.branch ? ' · ' + agents[id].workspace.branch : ''} · ${agents[id].status}`,
      run: () => {
        setModal('none');
        focus(id);
      },
    }));
    return [...jumps, ...cmds];
  }, [order, agents, setModal, focus]);

  const filtered = useMemo(() => fuzzyFilter(query, entries, (e) => e.title + ' ' + (e.subtitle ?? '')), [query, entries]);
  useEffect(() => setSel(0), [query]);

  const run = (e: Entry | undefined) => {
    if (e && !e.disabled) e.run();
  };

  return (
    <div className="modal-scrim" onMouseDown={(e) => e.target === e.currentTarget && setModal('none')}>
      <div
        className="modal"
        onKeyDown={(e) => {
          if (e.key === 'Escape') return setModal('none');
          if (e.key === 'ArrowDown') {
            e.preventDefault();
            setSel((i) => Math.min(filtered.length - 1, i + 1));
          } else if (e.key === 'ArrowUp') {
            e.preventDefault();
            setSel((i) => Math.max(0, i - 1));
          } else if (e.key === 'Enter') {
            e.preventDefault();
            run(filtered[sel]);
          }
        }}
      >
        <input className="q" autoFocus placeholder="Run a command or jump to an agent…" value={query} onChange={(e) => setQuery(e.target.value)} />
        <div className="rows">
          {filtered.length === 0 && <div className="empty">No matches.</div>}
          {filtered.map((e, i) => (
            <div
              key={e.key}
              className={`row${i === sel ? ' sel' : ''}`}
              style={e.disabled ? { opacity: 0.4 } : undefined}
              onMouseEnter={() => setSel(i)}
              onClick={() => run(e)}
            >
              <div>
                <div className="primary">{e.title}</div>
                {e.subtitle && <div className="sub">{e.subtitle}</div>}
              </div>
              <div className="meta">{e.binding && <span className="kbd">{e.binding}</span>}</div>
            </div>
          ))}
        </div>
        <div className="foot">
          <span>
            <span className="kbd">↵</span> run
          </span>
          <span>
            <span className="kbd">↑↓</span> navigate
          </span>
          <span>
            <span className="kbd">Esc</span> close
          </span>
        </div>
      </div>
    </div>
  );
}
