// Browser pane (D7/D13, docs/browser.md) — the shared, per-agent browser: a CDP
// screencast painted into a <canvas>, a control-owner wheel (grab/release), user
// input forwarding when the user holds the wheel, and the agent-initiated
// takeover banner. Frames arrive via browserHub (out of the reactive store).
//
// Focus/bandwidth rule: mounting opts this agent's browser channel in
// (setBrowserSub) so the daemon streams only the focused, viewing client;
// unmounting opts back out and the screencast stops.

import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useStore } from '../../store';
import { browserHub, type Frame } from '../../terminal/browserHub';
import type { BrowserInputWire } from '../../wire';

type BrowserIconName = 'agent' | 'user' | 'play' | 'restart' | 'keyboard' | 'wheel' | 'save' | 'close';

function BrowserIcon({ name }: { name: BrowserIconName }) {
  const paths: Record<BrowserIconName, ReactNode> = {
    agent: <><rect x="4" y="6" width="16" height="13" rx="3" /><path d="M9 2h6M12 2v4M8 12h.01M16 12h.01M8 16h8" /></>,
    user: <><path d="M7 11V6a2 2 0 0 1 4 0v4-6a2 2 0 0 1 4 0v6-4a2 2 0 0 1 4 0v7c0 5-3 8-8 8h-1c-3 0-5-1-7-4l-2-3a2 2 0 0 1 3-3l3 3" /></>,
    play: <path d="m8 5 11 7-11 7Z" />,
    restart: <><path d="M20 7v5h-5" /><path d="M19 12a7 7 0 1 0-2 5" /></>,
    keyboard: <><rect x="3" y="6" width="18" height="12" rx="2" /><path d="M7 10h.01M11 10h.01M15 10h.01M19 10h.01M7 14h10" /></>,
    wheel: <><rect x="7" y="3" width="10" height="18" rx="5" /><path d="M12 7v3" /></>,
    save: <><path d="M5 3h12l2 2v16H5Z" /><path d="M8 3v6h8V3M8 21v-7h8v7" /></>,
    close: <path d="m7 7 10 10M17 7 7 17" />,
  };
  return <svg className="browser-action-icon" viewBox="0 0 24 24" aria-hidden="true">{paths[name]}</svg>;
}

export function BrowserPane() {
  const agentId = useStore((s) => s.focusedId)!;
  const active = useStore((s) => s.agents[agentId]?.browserActive ?? false);
  const owner = useStore((s) => s.agents[agentId]?.browserOwner ?? 'agent');
  const takeovers = useStore((s) => s.agents[agentId]?.takeovers ?? []);
  const setBrowserSub = useStore((s) => s.setBrowserSub);
  const toggleWheel = useStore((s) => s.toggleWheel);
  const browserInput = useStore((s) => s.browserInput);
  const captureSnapshot = useStore((s) => s.captureSnapshot);
  const snapshots = useStore((s) => s.snapshots);
  const listSnapshots = useStore((s) => s.listSnapshots);
  const restartBrowser = useStore((s) => s.restartBrowser);
  const [seed, setSeed] = useState('');
  const [restartBusy, setRestartBusy] = useState(false);
  const [restartMsg, setRestartMsg] = useState('');

  useEffect(() => {
    void listSnapshots().catch(() => {});
  }, [listSnapshots]);

  const doRestart = async () => {
    if (restartBusy) return;
    setRestartBusy(true);
    setRestartMsg('');
    try {
      const result = await restartBrowser(agentId, seed || undefined);
      if (result.error) throw new Error(result.error);
    } catch (e) {
      setRestartMsg((e as Error).message);
    } finally {
      setRestartBusy(false);
    }
  };

  // Inline "capture snapshot" naming form (opens from the browser bar).
  const [capturing, setCapturing] = useState(false);
  const [snapName, setSnapName] = useState('');
  const [snapBusy, setSnapBusy] = useState(false);
  const [snapMsg, setSnapMsg] = useState('');
  const doCapture = async () => {
    const name = snapName.trim();
    if (!name || snapBusy) return;
    setSnapBusy(true);
    setSnapMsg('');
    try {
      await captureSnapshot(agentId, name);
      setSnapMsg(`Saved “${name}”`);
      setSnapName('');
      setCapturing(false);
    } catch (e) {
      setSnapMsg((e as Error).message);
    } finally {
      setSnapBusy(false);
    }
  };

  const canvasRef = useRef<HTMLCanvasElement>(null);
  // Last painted image rect + device size, for canvas→page coordinate mapping.
  const rectRef = useRef({ x: 0, y: 0, w: 1, h: 1, dw: 1280, dh: 800 });
  // In-flight single-finger touch: start point/time for tap vs. drag-scroll,
  // last point for incremental scroll deltas, and whether it became a scroll.
  const touchRef = useRef<{ sx: number; sy: number; lx: number; ly: number; t: number; scrolling: boolean } | null>(null);
  // Hidden field that drives the mobile soft keyboard (see onKbdInput).
  const kbdRef = useRef<HTMLTextAreaElement>(null);
  const userOwns = owner === 'user';

  // Opt this agent's browser channel in for the pane's lifetime (focus rule).
  useEffect(() => {
    setBrowserSub(agentId);
    return () => setBrowserSub(null);
  }, [agentId, setBrowserSub]);

  // Paint frames into the canvas (scale to fit, letterbox).
  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;
    let disposed = false;

    const paint = (f: Frame) => {
      const img = new Image();
      img.onload = () => {
        if (disposed) return;
        const parent = canvas.parentElement!;
        const cw = (canvas.width = parent.clientWidth);
        const ch = (canvas.height = parent.clientHeight);
        const dw = f.meta.deviceWidth || img.width;
        const dh = f.meta.deviceHeight || img.height;
        const scale = Math.min(cw / dw, ch / dh);
        const w = dw * scale;
        const h = dh * scale;
        const x = (cw - w) / 2;
        const y = (ch - h) / 2;
        rectRef.current = { x, y, w, h, dw, dh };
        ctx.fillStyle = '#0b0f14';
        ctx.fillRect(0, 0, cw, ch);
        ctx.drawImage(img, x, y, w, h);
      };
      img.src = 'data:image/jpeg;base64,' + f.dataB64;
    };

    const unsub = browserHub.subscribe(agentId, paint);
    return () => {
      disposed = true;
      unsub();
    };
    // `active` is a dep: the <canvas> only exists once the browser is active, so
    // the effect must re-run when it appears to attach the frame subscription.
  }, [agentId, active]);

  // Map a client (clientX/clientY) point on the canvas to device (page) coords.
  const clientToPage = (clientX: number, clientY: number): { x: number; y: number } | null => {
    const canvas = canvasRef.current;
    if (!canvas) return null;
    const r = canvas.getBoundingClientRect();
    const { x, y, w, h, dw, dh } = rectRef.current;
    const cx = clientX - r.left - x;
    const cy = clientY - r.top - y;
    if (cx < 0 || cy < 0 || cx > w || cy > h) return null;
    return { x: Math.round((cx / w) * dw), y: Math.round((cy / h) * dh) };
  };
  // Map a pointer event on the canvas to device (page) coordinates.
  const toPage = (e: React.MouseEvent): { x: number; y: number } | null => clientToPage(e.clientX, e.clientY);

  const emit = (event: BrowserInputWire) => browserInput(agentId, event);
  // Chromium's built-in editing/navigation commands (delete-backward, caret
  // movement, etc.) key off the CDP event's windowsVirtualKeyCode, not just
  // `key`/`code` — a synthetic Backspace with no VK code reaches the page's
  // JS `keydown` listener but doesn't trigger the native character deletion.
  const VK_CODES: Record<string, number> = {
    Backspace: 8, Tab: 9, Enter: 13, Escape: 27, Delete: 46,
    ArrowLeft: 37, ArrowUp: 38, ArrowRight: 39, ArrowDown: 40, Home: 36, End: 35, PageUp: 33, PageDown: 34,
  };
  const emitKey = (key: string, code: string) => emit({ kind: 'keydown', key, code, keyCode: VK_CODES[key] });

  const onMouse = (kind: BrowserInputWire['kind']) => (e: React.MouseEvent) => {
    if (!userOwns) return;
    const p = toPage(e);
    if (!p) return;
    emit({ kind, x: p.x, y: p.y, buttons: e.buttons });
  };
  const onWheel = (e: React.WheelEvent) => {
    if (!userOwns) return;
    const canvas = canvasRef.current!;
    const r = canvas.getBoundingClientRect();
    const { x, y, w, h, dw, dh } = rectRef.current;
    const cx = e.clientX - r.left - x;
    const cy = e.clientY - r.top - y;
    emit({ kind: 'wheel', x: Math.round((cx / w) * dw), y: Math.round((cy / h) * dh), deltaX: e.deltaX, deltaY: e.deltaY });
  };
  const onKey = (e: React.KeyboardEvent) => {
    if (!userOwns) return;
    // We own the wheel, so this keystroke is meant for the remote page, not the
    // app. stopPropagation keeps it from bubbling to the window-level global key
    // handler — otherwise bare keys bound there fire too (e.g. Backspace =
    // agent.close would tear down the session mid-type). preventDefault alone
    // doesn't stop propagation. The mobile soft-keyboard path rides a real
    // <textarea>, which useGlobalKeys already treats as text-input scope.
    e.preventDefault();
    e.stopPropagation();
    if (e.key.length === 1) emit({ kind: 'text', text: e.key });
    else emitKey(e.key, e.code);
  };

  // Touch → mouse/wheel mapping. A single finger that stays roughly put is a
  // tap (→ click); one that moves is a drag-scroll (→ wheel deltas, natural:
  // finger up scrolls the page down). Multi-touch is ignored (pinch is a
  // follow-up needing a dedicated CDP touch path).
  const TAP_SLOP = 8; // px of movement before a touch counts as a scroll
  const onTouchStart = (e: React.TouchEvent) => {
    if (!userOwns || e.touches.length !== 1) return;
    const t = e.touches[0];
    touchRef.current = { sx: t.clientX, sy: t.clientY, lx: t.clientX, ly: t.clientY, t: Date.now(), scrolling: false };
  };
  const onTouchMove = (e: React.TouchEvent) => {
    const st = touchRef.current;
    if (!userOwns || !st || e.touches.length !== 1) return;
    const t = e.touches[0];
    if (!st.scrolling && Math.hypot(t.clientX - st.sx, t.clientY - st.sy) < TAP_SLOP) return;
    st.scrolling = true;
    const p = clientToPage(t.clientX, t.clientY) ?? clientToPage(st.lx, st.ly);
    if (p) emit({ kind: 'wheel', x: p.x, y: p.y, deltaX: st.lx - t.clientX, deltaY: st.ly - t.clientY });
    st.lx = t.clientX;
    st.ly = t.clientY;
  };
  const onTouchEnd = () => {
    const st = touchRef.current;
    touchRef.current = null;
    if (!userOwns || !st || st.scrolling) return;
    const p = clientToPage(st.sx, st.sy);
    if (!p) return;
    emit({ kind: 'mousedown', x: p.x, y: p.y, buttons: 1 });
    emit({ kind: 'mouseup', x: p.x, y: p.y, buttons: 0 });
    emit({ kind: 'click', x: p.x, y: p.y, buttons: 0 });
  };

  // Soft keyboard. A <canvas> can't summon a phone's on-screen keyboard, so a
  // hidden <textarea> stands in: the ⌨ button focuses it, and its events are
  // forwarded to the remote page. It always holds a single sentinel char, so
  // it's never truly empty — iOS fires no event at all on an empty field.
  //
  // Printable characters go through onKbdInput: mobile IMEs report them via
  // keydown as 229/Unidentified, but they arrive cleanly as `input` events.
  // Backspace/Enter/navigation keys fire a real keydown on both GBoard and iOS,
  // so they go through onKbdKeyDown. Backspace specifically CANNOT ride the
  // input event: resetting the field there desyncs the keyboard's own edit
  // buffer, which then swallows the next deleteContentBackward.
  const SENTINEL = ' ';
  // Some mobile IMEs report one Backspace as both a keydown and a later
  // deleteContentBackward input event. Track those expected input echoes by
  // count instead of elapsed time: an IME is free to delay the input event and
  // it will still consume exactly the keydown that already emitted. Repeated
  // keydowns each add one expected echo, while keyboards that emit input only
  // see a zero count and forward the deletion normally.
  const pendingBkspInputsRef = useRef(0);
  const resetKbd = () => {
    const el = kbdRef.current;
    if (!el) return;
    el.value = SENTINEL;
    el.setSelectionRange(SENTINEL.length, SENTINEL.length);
  };
  const focusKeyboard = () => {
    pendingBkspInputsRef.current = 0;
    resetKbd();
    kbdRef.current?.focus();
  };
  const onKbdInput = (e: React.FormEvent<HTMLTextAreaElement>) => {
    if (userOwns) {
      const ne = e.nativeEvent as InputEvent;
      switch (ne.inputType) {
        case 'insertText':
        case 'insertReplacementText':
        case 'insertCompositionText':
          pendingBkspInputsRef.current = 0;
          if (ne.data) emit({ kind: 'text', text: ne.data });
          break;
        case 'insertLineBreak':
        case 'insertParagraph':
          pendingBkspInputsRef.current = 0;
          emitKey('Enter', 'Enter');
          break;
        case 'deleteContentBackward':
        case 'deleteWordBackward':
          if (pendingBkspInputsRef.current > 0) {
            pendingBkspInputsRef.current -= 1;
          } else {
            emitKey('Backspace', 'Backspace');
          }
          break;
        case 'deleteContentForward':
          pendingBkspInputsRef.current = 0;
          emitKey('Delete', 'Delete');
          break;
      }
    }
    resetKbd();
  };
  // Backspace, Enter and navigation keys fire a real keydown on mobile (unlike
  // printable characters, which report 229/Unidentified and go through
  // onKbdInput). Backspace is driven from here because resetting the field
  // inside the input handler desyncs the keyboard's edit buffer and swallows a
  // subsequent deleteContentBackward. We do NOT preventDefault Backspace, so
  // the field still deletes the sentinel and the input-event fallback can fire
  // on keyboards that skip keydown; pendingBkspInputsRef pairs and suppresses
  // each delayed input echo without relying on device-specific timing.
  const onKbdKeyDown = (e: React.KeyboardEvent) => {
    if (!userOwns) return;
    const k = e.key;
    if (k === 'Backspace') {
      pendingBkspInputsRef.current += 1;
      emitKey('Backspace', 'Backspace');
      return;
    }
    if (k.length === 1 || k === 'Enter' || k === 'Unidentified' || k === 'Process') return;
    e.preventDefault();
    emitKey(k, e.code);
  };

  return (
    <div className="pane browser-pane">
      <div className="browser-bar">
        <div className="browser-status-group">
          <span className={`owner-badge ${owner}`}>
            <BrowserIcon name={owner === 'user' ? 'user' : 'agent'} />
            {owner === 'user' ? 'You have control' : 'Agent has control'}
          </span>
          {(restartMsg || snapMsg) && <span className="browser-bar-message">{restartMsg || snapMsg}</span>}
        </div>

        <div className="browser-seed-group">
          <label htmlFor="browser-state-seed">New session</label>
          <select id="browser-state-seed" value={seed} onChange={(e) => setSeed(e.target.value)} title="Browser state for the new session">
            <option value="">Fresh state</option>
            {snapshots.map((snapshot) => (
              <option key={snapshot.id} value={snapshot.id}>{snapshot.name}</option>
            ))}
          </select>
          <button className="browser-action-btn" disabled={restartBusy} onClick={() => void doRestart()}>
            <BrowserIcon name={active ? 'restart' : 'play'} />
            {restartBusy ? 'Starting…' : active ? 'Restart' : 'Start'}
          </button>
        </div>

        <div className="browser-actions">
          {active && userOwns && (
            <button className="browser-action-btn icon-only mobile-keyboard-btn" onClick={focusKeyboard} title="Show keyboard" aria-label="Show keyboard">
              <BrowserIcon name="keyboard" />
            </button>
          )}
          <button className="browser-action-btn" disabled={!active} onClick={() => toggleWheel(agentId)}>
            <BrowserIcon name="wheel" />
            {owner === 'user' ? 'Release control' : 'Take control'} <kbd>w</kbd>
          </button>
          {active && !capturing && (
            <button className="browser-action-btn" onClick={() => { setCapturing(true); setSnapMsg(''); }} title="Save the current browser state as a reusable snapshot">
              <BrowserIcon name="save" />
              Save snapshot
            </button>
          )}
          {active && capturing && (
            <span className="snapshot-capture">
            <input
              autoFocus
              value={snapName}
              placeholder="Snapshot name…"
              onChange={(e) => setSnapName(e.target.value)}
              onKeyDown={(e) => {
                e.stopPropagation();
                if (e.key === 'Enter') void doCapture();
                if (e.key === 'Escape') { setCapturing(false); setSnapName(''); }
              }}
            />
            <button className="browser-action-btn primary" disabled={!snapName.trim() || snapBusy} onClick={() => void doCapture()}>
              <BrowserIcon name="save" /> {snapBusy ? 'Saving…' : 'Save'}
            </button>
            <button className="browser-action-btn icon-only" title="Cancel" aria-label="Cancel snapshot" onClick={() => { setCapturing(false); setSnapName(''); }}>
              <BrowserIcon name="close" />
            </button>
          </span>
          )}
        </div>
      </div>

      {takeovers.length > 0 && (
        <div className="takeover-banner">
          <span>
            <b>{agentId}</b> needs you — {takeovers[takeovers.length - 1].reason}
          </span>
          {owner !== 'user' && (
            <button className="wheel-btn hot" onClick={() => toggleWheel(agentId)}>
              Take the wheel
            </button>
          )}
        </div>
      )}

      {!active ? (
        <div className="pane-placeholder">
          <div className="big">◉</div>
          <div>
            <b>Browser</b> — no browser yet for this agent. It spins up lazily the first time the agent uses a browser tool, then the live view appears here.
          </div>
        </div>
      ) : (
        <div
          className={`browser-canvas-wrap${userOwns ? ' live' : ''}`}
          tabIndex={0}
          onKeyDown={onKey}
          onMouseDown={onMouse('mousedown')}
          onMouseUp={onMouse('mouseup')}
          onMouseMove={onMouse('mousemove')}
          onClick={onMouse('click')}
          onWheel={onWheel}
          onTouchStart={onTouchStart}
          onTouchMove={onTouchMove}
          onTouchEnd={onTouchEnd}
        >
          <canvas ref={canvasRef} className="browser-canvas" />
          <textarea
            ref={kbdRef}
            className="browser-kbd-catcher"
            autoCapitalize="off"
            autoComplete="off"
            autoCorrect="off"
            spellCheck={false}
            aria-hidden="true"
            tabIndex={-1}
            defaultValue={SENTINEL}
            onInput={onKbdInput}
            onKeyDown={onKbdKeyDown}
          />
        </div>
      )}
    </div>
  );
}
