import { useStore } from '../store';

// Bottom inspector — contextual to the focused agent. Stub per the spec:
// workspace path / branch / status. Collapsible.
export function Inspector() {
  const open = useStore((s) => s.inspectorOpen);
  const toggle = useStore((s) => s.toggleInspector);
  const focusedId = useStore((s) => s.focusedId);
  const agent = useStore((s) => (s.focusedId ? s.sessions[s.focusedId] : undefined));

  return (
    <div className="inspector">
      <div className="insp-head">
        Inspector{agent ? ` · ${agent.name || agent.id}${agent.hostName ? ` @ ${agent.hostName}` : ''}` : focusedId ? ` · ${focusedId}` : ''}
        <button className="btn ghost insp-toggle" onClick={toggle}>
          {open ? 'hide ▾' : 'show ▴'}
        </button>
      </div>
      {open && agent && (
        <div className="insp-body">
          <div className="field">
            <span className="k">Workspace</span>
            <span className="v">{agent.workspace.cwd || '—'}</span>
          </div>
          <div className="field">
            <span className="k">Repo</span>
            <span className="v">{agent.workspace.repo || '—'}</span>
          </div>
          <div className="field">
            <span className="k">Branch</span>
            <span className="v">{agent.workspace.branch || '—'}</span>
          </div>
          <div className="field">
            <span className="k">Integration target</span>
            <span className="v">{agent.workspace.targetRef?.replace(/^refs\/heads\//, '').replace(/^refs\/remotes\//, '') || '—'}</span>
          </div>
          <div className="field">
            <span className="k">Divergence</span>
            <span className="v">+{agent.workspace.ahead ?? 0} / -{agent.workspace.behind ?? 0}</span>
          </div>
          <div className="field">
            <span className="k">Status</span>
            <span className="v">{agent.status}</span>
          </div>
          <div className="field">
            <span className="k">Events</span>
            <span className="v">{agent.events.length}</span>
          </div>
        </div>
      )}
      {open && !agent && <div className="insp-body">No session focused.</div>}
    </div>
  );
}
