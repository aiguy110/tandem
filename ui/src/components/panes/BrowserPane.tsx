// Browser pane (D7/D13, docs/browser.md) — the shared, per-agent browser: a CDP
// screencast painted into a <canvas>, a control-owner wheel (grab/release), user
// input forwarding when the user holds the wheel, and the agent-initiated
// takeover banner. Frames arrive via browserHub (out of the reactive store).
//
// Focus/bandwidth rule: mounting opts this agent's browser channel in
// (setBrowserSub) so the daemon streams only the focused, viewing client;
// unmounting opts back out and the screencast stops.

import { useEffect, useRef, useState } from 'react';
import { useStore } from '../../store';
import { browserHub, type Frame } from '../../terminal/browserHub';
import type { BrowserInputWire } from '../../wire';

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
  // Local viewer zoom is intentionally independent of the remote page. It
  // magnifies and pans the screencast without sending a pinch gesture to CDP.
  const viewRef = useRef({ scale: 1, tx: 0, ty: 0 });
  const redrawRef = useRef<(() => void) | null>(null);
  type TouchState =
    | { kind: 'single'; sx: number; sy: number; lx: number; ly: number; scrolling: boolean }
    | { kind: 'pinch'; distance: number; scale: number; contentX: number; contentY: number };
  const touchRef = useRef<TouchState | null>(null);
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
    let image: HTMLImageElement | null = null;
    let frame: Frame | null = null;

    const redraw = () => {
      if (disposed || !image || !frame) return;
      const parent = canvas.parentElement!;
      const cw = parent.clientWidth;
      const ch = parent.clientHeight;
      const dpr = window.devicePixelRatio || 1;
      canvas.width = Math.round(cw * dpr);
      canvas.height = Math.round(ch * dpr);
      const dw = frame.meta.deviceWidth || image.width;
      const dh = frame.meta.deviceHeight || image.height;
      const fit = Math.min(cw / dw, ch / dh);
      const baseW = dw * fit;
      const baseH = dh * fit;
      const baseX = (cw - baseW) / 2;
      const baseY = (ch - baseH) / 2;
      const view = viewRef.current;
      const x = baseX + view.tx;
      const y = baseY + view.ty;
      const w = baseW * view.scale;
      const h = baseH * view.scale;
      rectRef.current = { x, y, w, h, dw, dh };
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.fillStyle = '#0b0f14';
      ctx.fillRect(0, 0, cw, ch);
      ctx.drawImage(image, x, y, w, h);
    };
    redrawRef.current = redraw;

    const paint = (f: Frame) => {
      const img = new Image();
      img.onload = () => {
        if (disposed) return;
        image = img;
        frame = f;
        redraw();
      };
      img.src = 'data:image/jpeg;base64,' + f.dataB64;
    };

    const observer = new ResizeObserver(redraw);
    observer.observe(canvas.parentElement!);
    const unsub = browserHub.subscribe(agentId, paint);
    return () => {
      disposed = true;
      redrawRef.current = null;
      observer.disconnect();
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

  // One finger taps or scrolls the remote page. Two fingers zoom and pan only
  // this local viewer; input coordinates are mapped back through that view.
  const TAP_SLOP = 8; // px of movement before a touch counts as a scroll
  const onTouchStart = (e: React.TouchEvent) => {
    if (e.touches.length === 2) {
      const [a, b] = [e.touches[0], e.touches[1]];
      const mx = (a.clientX + b.clientX) / 2;
      const my = (a.clientY + b.clientY) / 2;
      const canvasRect = canvasRef.current!.getBoundingClientRect();
      const { x, y } = rectRef.current;
      const view = viewRef.current;
      touchRef.current = {
        kind: 'pinch', distance: Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY), scale: view.scale,
        contentX: (mx - canvasRect.left - x) / view.scale,
        contentY: (my - canvasRect.top - y) / view.scale,
      };
    } else if (userOwns && e.touches.length === 1) {
      const t = e.touches[0];
      touchRef.current = { kind: 'single', sx: t.clientX, sy: t.clientY, lx: t.clientX, ly: t.clientY, scrolling: false };
    }
  };
  const onTouchMove = (e: React.TouchEvent) => {
    const st = touchRef.current;
    if (!st) return;
    if (st.kind === 'pinch' && e.touches.length === 2) {
      const [a, b] = [e.touches[0], e.touches[1]];
      const distance = Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY);
      const scale = Math.max(1, Math.min(4, st.scale * distance / Math.max(st.distance, 1)));
      const mx = (a.clientX + b.clientX) / 2;
      const my = (a.clientY + b.clientY) / 2;
      const canvasRect = canvasRef.current!.getBoundingClientRect();
      const current = rectRef.current;
      const baseX = current.x - viewRef.current.tx;
      const baseY = current.y - viewRef.current.ty;
      viewRef.current = scale === 1 ? { scale: 1, tx: 0, ty: 0 } : {
        scale,
        tx: mx - canvasRect.left - baseX - st.contentX * scale,
        ty: my - canvasRect.top - baseY - st.contentY * scale,
      };
      redrawRef.current?.();
      return;
    }
    if (!userOwns || st.kind !== 'single' || e.touches.length !== 1) return;
    const t = e.touches[0];
    if (!st.scrolling && Math.hypot(t.clientX - st.sx, t.clientY - st.sy) < TAP_SLOP) return;
    st.scrolling = true;
    const p = clientToPage(t.clientX, t.clientY) ?? clientToPage(st.lx, st.ly);
    if (p) emit({ kind: 'wheel', x: p.x, y: p.y, deltaX: st.lx - t.clientX, deltaY: st.ly - t.clientY });
    st.lx = t.clientX;
    st.ly = t.clientY;
  };
  const onTouchEnd = (e: React.TouchEvent) => {
    const st = touchRef.current;
    touchRef.current = null;
    if (!userOwns || !st || st.kind === 'pinch') {
      if (userOwns && e.touches.length === 1) {
        const t = e.touches[0];
        touchRef.current = { kind: 'single', sx: t.clientX, sy: t.clientY, lx: t.clientX, ly: t.clientY, scrolling: true };
      }
      return;
    }
    if (st.scrolling) return;
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
        <span className={`owner-badge ${owner}`}>{owner === 'user' ? '🖐 you hold the wheel' : '🤖 agent driving'}</span>
        <select value={seed} onChange={(e) => setSeed(e.target.value)} title="Browser state for the new session">
          <option value="">Fresh browser state</option>
          {snapshots.map((snapshot) => (
            <option key={snapshot.id} value={snapshot.id}>{snapshot.name}</option>
          ))}
        </select>
        <button className="wheel-btn" disabled={restartBusy} onClick={() => void doRestart()}>
          {restartBusy ? 'Starting…' : active ? '↻ Restart browser' : '▶ Start browser'}
        </button>
        {restartMsg && <span className="sub">{restartMsg}</span>}
        {active && userOwns && (
          <button className="wheel-btn kbd-btn" onClick={focusKeyboard} title="Show keyboard to type into the page">
            ⌨ Keyboard
          </button>
        )}
        <button className="wheel-btn" disabled={!active} onClick={() => toggleWheel(agentId)}>
          {owner === 'user' ? 'Release the wheel' : 'Take the wheel'} <kbd>w</kbd>
        </button>
        {active && !capturing && (
          <button className="wheel-btn" onClick={() => { setCapturing(true); setSnapMsg(''); }} title="Save the current browser state as a reusable snapshot">
            📸 Capture snapshot
          </button>
        )}
        {active && capturing && (
          <span className="snapshot-capture" style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
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
            <button className="wheel-btn" disabled={!snapName.trim() || snapBusy} onClick={() => void doCapture()}>
              {snapBusy ? 'Saving…' : 'Save'}
            </button>
            <button className="wheel-btn" onClick={() => { setCapturing(false); setSnapName(''); }}>Cancel</button>
          </span>
        )}
        {snapMsg && <span className="sub" style={{ marginLeft: 6 }}>{snapMsg}</span>}
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
          onTouchCancel={() => { touchRef.current = null; }}
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
