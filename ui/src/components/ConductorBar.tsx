import { useStore } from '../store';
import { loadBindings, prettyBinding } from '../commands/keymap';

function PaletteIcon() {
  return (
    <svg className="palette-button-icon" viewBox="0 0 24 24" aria-hidden="true">
      <path
        fillRule="evenodd"
        d="M12 2.25C6.28 2.25 2.1 5.85 2.1 10.78c0 5.32 4.48 10.44 9.72 10.98 1.15.12 1.92-1.1 1.28-2.06-.56-.84-.08-1.91.93-1.91h1.22c4.28 0 6.65-2.65 6.65-6.3 0-5.12-4.4-9.24-9.9-9.24ZM9 5.55a1.52 1.52 0 1 1 0 3.04 1.52 1.52 0 0 1 0-3.04Zm5.4 0a1.52 1.52 0 1 1 0 3.04 1.52 1.52 0 0 1 0-3.04Zm3.18 4.15a1.52 1.52 0 1 1 0 3.04 1.52 1.52 0 0 1 0-3.04Zm-10.12 0a1.52 1.52 0 1 1 0 3.04 1.52 1.52 0 0 1 0-3.04Z"
      />
    </svg>
  );
}

export function ConductorBar({ version }: { version: string | null }) {
  const setModal = useStore((s) => s.setModal);
  const b = loadBindings();

  return (
    <div className="bar">
      <div className="brand">
        <b>Tandem</b><span className="version" aria-label={version ? `Version ${version}` : 'Loading version'}>{version ?? '…'}</span>
      </div>
      <button className="btn primary" onClick={() => setModal('spawn')} title={`Spawn session (${prettyBinding(b['agent.spawn'])})`}>
        + Session <span className="kbd">{prettyBinding(b['agent.spawn'])}</span>
      </button>
      <button className="btn ghost" onClick={() => setModal('command')} title="Command palette">
        <PaletteIcon />
        <span className="kbd">{prettyBinding(b['palette.open'])}</span>
      </button>
      <div className="spacer" />
      <button className="btn ghost" onClick={() => setModal('appearance')} title="Appearance: theme and font sizes">
        Aa
      </button>
    </div>
  );
}
