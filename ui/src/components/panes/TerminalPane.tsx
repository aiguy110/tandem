import { useEffect, useRef, useState } from 'react';
import { useStore } from '../../store';
import { ptyHub } from '../../terminal/ptyHub';
import { createRenderer, selectedEngine, type EngineName, type TerminalRenderer } from '../../terminal/TerminalRenderer';

// Terminal pane (docs/terminal.md): the TerminalRenderer (ghostty-web default,
// @xterm/xterm fallback) mounted for the FOCUSED agent only. Rehydrates from the
// pty buffer on mount (reconnect/replay), forwards keystrokes as {t:'input'} and
// fit-to-container size as {t:'resize'}. ACP agents with no pty show a note.
export function TerminalPane() {
  const agentId = useStore((s) => s.focusedId)!;
  const send = useStore((s) => s.send);
  const agent = useStore((s) => s.agents[agentId]);
  const enterTerminal = useStore((s) => s.enterTerminal);
  const leaveTerminal = useStore((s) => s.leaveTerminal);
  const setPane = useStore((s) => s.setPane);
  const mountRef = useRef<HTMLDivElement>(null);
  const [engine, setEngine] = useState<EngineName | null>(null);
  const [hasPty, setHasPty] = useState(() => ptyHub.hasData(agentId));
  const [handoffError, setHandoffError] = useState<string | null>(null);

  useEffect(() => {
    let renderer: TerminalRenderer | null = null;
    let unsub: (() => void) | null = null;
    let disposed = false;
    let ro: ResizeObserver | null = null;

    const el = mountRef.current;
    if (!el) return;

    (async () => {
      const created = await createRenderer(el, selectedEngine());
      if (disposed) {
        created.renderer.dispose();
        return;
      }
      renderer = created.renderer;
      setEngine(created.engine);

      // Feed buffered scrollback + live bytes into the emulator.
      unsub = ptyHub.subscribe(agentId, (bytes) => {
        renderer?.write(bytes);
        if (bytes.length) setHasPty(true);
      });

      // Keystrokes → daemon → pty.
      renderer.onData((d) => {
        const bytes = new TextEncoder().encode(d);
        let bin = '';
        for (const b of bytes) bin += String.fromCharCode(b);
        send({ t: 'input', agentId, bytesB64: btoa(bin) });
      });

      // Fit to container and report the size; observe future resizes.
      const doFit = () => {
        renderer?.fit();
        if (renderer) send({ t: 'resize', agentId, cols: renderer.cols, rows: renderer.rows });
      };
      doFit();
      ro = new ResizeObserver(doFit);
      ro.observe(el);
      renderer.focus();
    })();

    return () => {
      disposed = true;
      ro?.disconnect();
      unsub?.();
      renderer?.dispose();
    };
  }, [agentId, send]);

  return (
    <div className="pane">
      <div className="term-host">
        <div className="term-mount" ref={mountRef} />
      </div>
      {!hasPty && (
        <div className="term-note">
          no raw terminal for this agent yet — this pane is live for PtyAdapter agents and the user escape-hatch shell.
        </div>
      )}
      {engine && <div className="term-note">engine: {engine}{engine === 'xterm' ? ' (ghostty-web fallback)' : ''}</div>}
      {agent?.controlMode === 'terminal' && (
        <div className="term-note">
          CLI control is active. Exit the CLI normally to return automatically, or{' '}
          <button
            className="link-btn"
            onClick={() => void leaveTerminal(agentId).then((r) => {
              if (r.error) setHandoffError(r.error);
              else setPane('transcript');
            })}
          >return to Transcript now</button>.
        </div>
      )}
      {agent && agent.controlMode !== 'terminal' && (
        <div className="handoff-shroud">
          <div className="handoff-card">
            <div className="handoff-title">{agent.controlMode === 'switching' ? 'Switching agent interface…' : agent.status === 'working' || agent.status === 'blocked' ? 'Agent is mid-turn in Transcript' : 'Terminal control is not active'}</div>
            <div className="handoff-copy">
              {agent.status === 'working' || agent.status === 'blocked'
                ? 'Taking control sends ACP session/cancel and waits briefly for the turn to flush before resuming the CLI. Unsaved in-flight steps may be lost.'
                : 'This will pause Transcript control and start the agent’s resumable CLI in this workspace.'}
            </div>
            {handoffError && <div className="modal-err">{handoffError}</div>}
            {agent.controlMode === 'transcript' && (
              <div className="handoff-actions">
                <button
                  className="btn"
                  onClick={() => void enterTerminal(agentId, agent.status === 'working' || agent.status === 'blocked').then((r) => r.error && setHandoffError(r.error))}
                >
                  {agent.status === 'working' || agent.status === 'blocked' ? 'Interrupt & take over' : 'Take control'}
                </button>
                <button className="btn" onClick={() => setPane('transcript')}>Back to Transcript</button>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
