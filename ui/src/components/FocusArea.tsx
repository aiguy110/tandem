import { useStore, PANES } from '../store';
import type { PaneId } from '../store';
import { TranscriptPane } from './panes/TranscriptPane';
import { TerminalPane } from './panes/TerminalPane';
import { DiffPane } from './panes/DiffPane';
import { BrowserPane } from './panes/BrowserPane';

const LABEL: Record<PaneId, string> = { transcript: 'Transcript', terminal: 'Terminal', diff: 'Diff', browser: 'Browser' };

export function FocusArea() {
  const focusedId = useStore((s) => s.focusedId);
  const agent = useStore((s) => (s.focusedId ? s.agents[s.focusedId] : undefined));
  const pane = useStore((s) => s.pane);
  const setPane = useStore((s) => s.setPane);

  if (!agent) {
    return (
      <div className="focus">
        <div className="pane-placeholder">
          <div className="big">◐</div>
          <div>Select or spawn an agent to begin.</div>
        </div>
      </div>
    );
  }

  return (
    <div className="focus">
      <div className="focus-head">
        <span className="focus-title">
          {agent.name}
          <span className="sub">
            {agent.workspace.repo}
            {agent.workspace.branch ? ` · ${agent.workspace.branch}` : ''}
          </span>
        </span>
        <div className="tabs">
          {PANES.map((p, i) => {
            // The Browser tab is enabled only once a browser exists for the agent
            // (docs/browser.md: the pane appears/enables when browser_state active).
            const disabled = p === 'browser' && !agent.browserActive;
            return (
              <button
                key={p}
                className={`tab${pane === p ? ' active' : ''}${disabled ? ' disabled' : ''}${p === 'browser' && agent.takeovers.length ? ' attn' : ''}`}
                onClick={() => setPane(p)}
                title={disabled ? 'No browser yet — spins up on first agent browser use' : undefined}
              >
                {LABEL[p]}
                <span className="n">{i + 1}</span>
              </button>
            );
          })}
        </div>
      </div>
      {/* key on focusedId so panes remount per agent (fresh terminal, scroll) */}
      {pane === 'transcript' && <TranscriptPane key={focusedId} />}
      {pane === 'terminal' && <TerminalPane key={focusedId} />}
      {pane === 'diff' && <DiffPane />}
      {pane === 'browser' && <BrowserPane />}
    </div>
  );
}
