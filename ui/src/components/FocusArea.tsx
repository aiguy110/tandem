import { useEffect, useState } from 'react';
import { useStore, PANES } from '../store';
import type { PaneId } from '../store';
import { ChatPane } from './panes/ChatPane';
import { ShellPane } from './panes/ShellPane';
import { DiffPane } from './panes/DiffPane';
import { BrowserPane } from './panes/BrowserPane';

const LABEL: Record<PaneId, string> = { chat: 'Chat', shell: 'Terminal', diff: 'Diff', browser: 'Browser' };

function ThreadAudioIcon({ enabled }: { enabled: boolean }) {
  return (
    <svg className="audio-toggle-icon" viewBox="0 0 24 24" aria-hidden="true">
      <path d="M4 10v4h4l5 4V6l-5 4H4Z" />
      {enabled && <><path d="M16 9.5a4 4 0 0 1 0 5" /><path d="M19 7a7.5 7.5 0 0 1 0 10" /></>}
    </svg>
  );
}

function audioStatus(agent: { audioOnTurnEnd: boolean; audioState: string; audioError: string | null }): string | null {
  if (!agent.audioOnTurnEnd || agent.audioState === 'idle') return null;
  if (agent.audioState === 'rendering') return 'Rendering speech…';
  if (agent.audioState === 'ready') return 'Speech ready';
  return agent.audioError ?? 'Speech rendering failed';
}

export function FocusArea() {
  const focusedId = useStore((s) => s.focusedId);
  const agent = useStore((s) => (s.focusedId ? s.agents[s.focusedId] : undefined));
  const pane = useStore((s) => s.pane);
  const setPane = useStore((s) => s.setPane);
  const enterTerminal = useStore((s) => s.enterTerminal);
  const leaveTerminal = useStore((s) => s.leaveTerminal);
  const toggleThreadAudio = useStore((s) => s.toggleThreadAudio);
  const [confirm, setConfirm] = useState<null | 'to-cli-busy' | 'to-acp'>(null);
  const [handoffError, setHandoffError] = useState<string | null>(null);

  // These were previously local to the mounted ChatPane. Keep the same reset
  // behavior now that the switch lives in the persistent focus header.
  useEffect(() => {
    setConfirm(null);
    setHandoffError(null);
  }, [focusedId, pane]);

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

  const goCli = () => {
    setHandoffError(null);
    if (agent.status === 'working' || agent.status === 'blocked') {
      setConfirm('to-cli-busy');
      return;
    }
    void enterTerminal(agent.id, false).then((r) => r.error && setHandoffError(r.error));
  };

  const goAcp = () => {
    setHandoffError(null);
    setConfirm('to-acp');
  };
  const speakingStatus = audioStatus(agent);

  return (
    <div className="focus">
      <div className="focus-head">
        <span className="focus-title">
          {agent.name}
          <span className="sub">
            {agent.workspace.repo}
            {agent.workspace.branch ? ` · ${agent.workspace.branch}` : ''}
            {agent.workspace.targetRef ? ` → ${agent.workspace.targetRef.replace(/^refs\/heads\//, '').replace(/^refs\/remotes\//, '')}` : ''}
          </span>
        </span>
        <div className="tabs">
          <button
            type="button"
            className={`audio-toggle${agent.audioOnTurnEnd ? ' active' : ''}`}
            aria-pressed={agent.audioOnTurnEnd}
            onClick={() => toggleThreadAudio(agent.id)}
            title={agent.audioOnTurnEnd ? 'Prepare this thread’s completed replies as audio — on' : 'Prepare this thread’s completed replies as audio — off'}
          >
            <ThreadAudioIcon enabled={agent.audioOnTurnEnd} />
            <span className="sr-only">{agent.audioOnTurnEnd ? 'Disable' : 'Enable'} completed-reply audio</span>
          </button>
          {speakingStatus && <span className={`audio-status ${agent.audioState}`} role="status" title={speakingStatus}>{speakingStatus}</span>}
          {agent.controlMode === 'switching' && pane === 'chat' && (
            <span className="chat-switch-note">switching…</span>
          )}
          {handoffError && pane === 'chat' && (
            <span className="chat-switch-note err">{handoffError}</span>
          )}
          {PANES.map((p, i) => {
            // The Browser tab is enabled only once a browser exists for the agent
            // (docs/browser.md: the pane appears/enables when browser_state active).
            const disabled = p === 'browser' && !agent.browserActive;
            if (p === 'chat' && pane === 'chat' && agent.canHandoff) {
              const mode = agent.controlMode;
              return (
                <div
                  key={p}
                  className="seg chat-tab-switch"
                  role="tablist"
                  aria-label="Chat interface"
                  title={handoffError || 'Chat interface · 1'}
                >
                  <button
                    role="tab"
                    aria-selected={mode === 'transcript'}
                    className={`seg-btn${mode === 'transcript' ? ' active' : ''}`}
                    disabled={mode === 'switching'}
                    onClick={() => mode === 'terminal' && goAcp()}
                  >
                    ACP
                  </button>
                  <button
                    role="tab"
                    aria-selected={mode === 'terminal'}
                    className={`seg-btn${mode === 'terminal' ? ' active' : ''}`}
                    disabled={mode === 'switching'}
                    onClick={() => mode === 'transcript' && goCli()}
                  >
                    CLI
                  </button>
                </div>
              );
            }
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
      {pane === 'chat' && (
        <ChatPane
          key={focusedId}
          confirm={confirm}
          onCancelConfirm={() => setConfirm(null)}
          onConfirmCli={() => {
            setConfirm(null);
            void enterTerminal(agent.id, true).then((r) => r.error && setHandoffError(r.error));
          }}
          onConfirmAcp={() => {
            setConfirm(null);
            void leaveTerminal(agent.id).then((r) => r.error && setHandoffError(r.error));
          }}
        />
      )}
      {pane === 'shell' && <ShellPane key={focusedId} />}
      {pane === 'diff' && <DiffPane />}
      {pane === 'browser' && <BrowserPane />}
    </div>
  );
}
