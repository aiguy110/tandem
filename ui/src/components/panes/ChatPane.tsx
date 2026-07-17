import { useStore } from '../../store';
import { ptyHub } from '../../terminal/ptyHub';
import { TranscriptPane } from './TranscriptPane';
import { PtyTerminal } from './PtyTerminal';

// The Chat tab: the agent conversation. For ACP agents it shows the structured
// transcript (ACP) or the agent's resumable CLI (CLI), toggled by the ACP/CLI
// switch — that switch is the enter/leave-terminal handoff. Native pty agents
// have no ACP side, so they always show their raw terminal.
interface ChatPaneProps {
  confirm: null | 'to-cli-busy' | 'to-acp';
  onCancelConfirm: () => void;
  onConfirmCli: () => void;
  onConfirmAcp: () => void;
}

export function ChatPane({ confirm, onCancelConfirm, onConfirmCli, onConfirmAcp }: ChatPaneProps) {
  const agentId = useStore((s) => s.focusedId)!;
  const agent = useStore((s) => s.agents[agentId]);
  const send = useStore((s) => s.send);

  if (!agent) return null;

  const mode = agent.controlMode; // 'transcript' | 'switching' | 'terminal'
  // A native pty agent has no ACP transcript — its raw terminal is the chat.
  const cliView = agent.adapter === 'pty' || mode === 'terminal';

  return (
    <div className="pane chat-pane">
      <div className="chat-body">
        {cliView ? (
          <div className="term-host">
            <PtyTerminal
              key={`cli-${agentId}`}
              subscribe={(cb) => ptyHub.subscribe(agentId, cb)}
              onData={(d) => send({ t: 'input', agentId, bytesB64: encode(d) })}
              onResize={(cols, rows) => send({ t: 'resize', agentId, cols, rows })}
            />
            {mode === 'terminal' && (
              <div className="term-note">
                CLI control is active — exit the CLI to return to ACP, or switch back above.
              </div>
            )}
          </div>
        ) : (
          <TranscriptPane />
        )}
      </div>

      {confirm && (
        <div className="handoff-shroud">
          <div className="handoff-card">
            {confirm === 'to-cli-busy' ? (
              <>
                <div className="handoff-title">Interrupt the active turn?</div>
                <div className="handoff-copy">
                  The agent is mid-turn. Switching to CLI sends ACP session/cancel and waits briefly
                  for the turn to flush before starting the resumable CLI. Unsaved in-flight steps may
                  be lost.
                </div>
                <div className="handoff-actions">
                  <button className="btn" onClick={onConfirmCli}>
                    Interrupt &amp; take over
                  </button>
                  <button className="btn" onClick={onCancelConfirm}>
                    Cancel
                  </button>
                </div>
              </>
            ) : (
              <>
                <div className="handoff-title">Return to ACP?</div>
                <div className="handoff-copy">
                  This kills the live CLI process and reloads the ACP session. Anything the CLI has
                  not committed to the session will be lost.
                </div>
                <div className="handoff-actions">
                  <button className="btn" onClick={onConfirmAcp}>
                    Kill CLI &amp; return
                  </button>
                  <button className="btn" onClick={onCancelConfirm}>
                    Cancel
                  </button>
                </div>
              </>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

// Encode a UTF-8 string to base64 for the input wire fields.
function encode(d: string): string {
  const bytes = new TextEncoder().encode(d);
  let bin = '';
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}
