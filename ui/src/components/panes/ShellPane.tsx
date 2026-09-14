import { useRef, useState } from 'react';
import { useStore } from '../../store';
import { shellHub } from '../../terminal/shellHub';
import { PtyTerminal } from './PtyTerminal';

// The Terminal tab: the user's own default shell in the agent's worktree, for
// exploring the agent's work (docs/terminal.md). It is independent of the agent
// adapter, so it runs concurrently with the agent and survives ACP↔CLI handoffs.
// The shell is spawned lazily on first visit and its scrollback replays from the
// daemon on reconnect. On exit the pane offers a restart.
export function ShellPane() {
  const sessionId = useStore((s) => s.focusedId)!;
  const agent = useStore((s) => s.sessions[sessionId]);
  const send = useStore((s) => s.send);
  const openShell = useStore((s) => s.openShell);
  const restartShell = useStore((s) => s.restartShell);
  // Bumped to force a clean emulator remount after a restart.
  const [generation, setGeneration] = useState(0);
  // Whether we have already asked the daemon to open the shell this mount.
  const openedRef = useRef(false);
  // Last fitted size, so a restart can spawn at the current dimensions.
  const sizeRef = useRef({ cols: 100, rows: 30 });

  if (!agent) return null;
  const exited = agent.shellExited;

  const onResize = (cols: number, rows: number) => {
    sizeRef.current = { cols, rows };
    if (!openedRef.current) {
      // Re-visiting an exited shell must preserve the explicit restart state;
      // only a shell that has never exited is opened lazily on first visit.
      if (exited) return;
      openedRef.current = true;
      void openShell(sessionId, cols, rows);
    } else {
      send({ t: 'shell_resize', sessionId, cols, rows });
    }
  };

  const restart = () => {
    const { cols, rows } = sizeRef.current;
    openedRef.current = true;
    void restartShell(sessionId, cols, rows);
    setGeneration((g) => g + 1);
  };

  return (
    <div className="pane shell-pane">
      <div className="term-host">
        <PtyTerminal
          key={`shell-${sessionId}-${generation}`}
          subscribe={(cb) => shellHub.subscribe(sessionId, cb)}
          onData={(d) => send({ t: 'shell_input', sessionId, bytesB64: encode(d) })}
          onResize={onResize}
        />
        {exited && (
          <div className="handoff-shroud">
            <div className="handoff-card">
              <div className="handoff-title">Shell exited</div>
              <div className="handoff-copy">
                {agent.shellExitMessage ? `The shell ${agent.shellExitMessage}.` : 'The shell process ended.'}
              </div>
              <div className="handoff-actions">
                <button className="btn" onClick={restart}>
                  Restart shell
                </button>
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

// Encode a UTF-8 string to base64 for the shell_input wire field.
function encode(d: string): string {
  const bytes = new TextEncoder().encode(d);
  let bin = '';
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}
