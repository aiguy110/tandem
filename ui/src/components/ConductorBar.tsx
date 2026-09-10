import { useStore } from '../store';
import { loadBindings, prettyBinding } from '../commands/keymap';

function PaletteIcon() {
  return (
    <svg className="palette-button-icon" viewBox="0 0 24 24" aria-hidden="true">
      <path d="M12 3a9 9 0 0 0 0 18h1.5a1.5 1.5 0 0 0 0-3h-1a2.5 2.5 0 0 1 0-5H15a6 6 0 0 0 0-12Z" />
      <path d="M7.5 10h.01M9.5 6.5h.01M14.5 6.5h.01M17.5 10h.01" />
    </svg>
  );
}

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
        <PaletteIcon />
        <span className="kbd">{prettyBinding(b['palette.open'])}</span>
      </button>
      <div className="spacer" />
      <button className="btn ghost" onClick={toggleTheme} title="Toggle light / dark">
        {theme === 'dark' ? '◐ dark' : '◑ light'}
      </button>
    </div>
  );
}
