import { useStore } from '../store';
import { loadBindings, prettyBinding } from '../commands/keymap';

export function ConductorBar({ version }: { version: string | null }) {
  const setModal = useStore((s) => s.setModal);
  const toggleTheme = useStore((s) => s.toggleTheme);
  const theme = useStore((s) => s.theme);
  const b = loadBindings();

  return (
    <div className="bar">
      <div className="brand">
        <b>Tandem</b><span className="version" aria-label={version ? `Version ${version}` : 'Loading version'}>{version ?? '…'}</span>
      </div>
      <button className="btn primary" onClick={() => setModal('spawn')} title={`Spawn agent (${prettyBinding(b['agent.spawn'])})`}>
        + Agent <span className="kbd">{prettyBinding(b['agent.spawn'])}</span>
      </button>
      <button className="btn ghost" onClick={() => setModal('command')} title="Command palette">
        <span className="kbd">{prettyBinding(b['palette.open'])}</span> palette
      </button>
      <div className="spacer" />
      <button className="btn ghost" onClick={toggleTheme} title="Toggle light / dark">
        {theme === 'dark' ? '◐ dark' : '◑ light'}
      </button>
    </div>
  );
}
