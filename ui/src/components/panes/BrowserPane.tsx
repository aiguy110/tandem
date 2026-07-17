// Browser pane (D7/D13, docs/browser.md) — the shared, per-agent browser: a CDP
// screencast painted into a <canvas>, a control-owner wheel (grab/release), user
// input forwarding when the user holds the wheel, and the agent-initiated
// takeover banner. Frames arrive via browserHub (out of the reactive store).
//
// Focus/bandwidth rule: mounting opts this agent's browser channel in
// (setBrowserSub) so the daemon streams only the focused, viewing client;
// unmounting opts back out and the screencast stops.

import { useEffect, useRef } from 'react';
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
    e.preventDefault();
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
  const lastBkspRef = useRef(0);
  const resetKbd = () => {
    const el = kbdRef.current;
    if (!el) return;
    el.value = SENTINEL;
    el.setSelectionRange(SENTINEL.length, SENTINEL.length);
  };
  const focusKeyboard = () => {
    resetKbd();
    kbdRef.current?.focus();
  };
  // Emit one Backspace, collapsing the near-simultaneous keydown + input pair
  // (they land <10ms apart; real key-repeat is far slower) so it deletes once.
  const emitBackspace = () => {
    const now = Date.now();
    if (now - lastBkspRef.current < 30) return;
    lastBkspRef.current = now;
    emitKey('Backspace', 'Backspace');
  };
  const onKbdInput = (e: React.FormEvent<HTMLTextAreaElement>) => {
    if (userOwns) {
      const ne = e.nativeEvent as InputEvent;
      switch (ne.inputType) {
        case 'insertText':
        case 'insertReplacementText':
        case 'insertCompositionText':
          if (ne.data) emit({ kind: 'text', text: ne.data });
          break;
        case 'insertLineBreak':
        case 'insertParagraph':
          emitKey('Enter', 'Enter');
          break;
        case 'deleteContentBackward':
        case 'deleteWordBackward':
          emitBackspace();
          break;
        case 'deleteContentForward':
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
  // on keyboards that skip keydown; emitBackspace dedups the pair.
  const onKbdKeyDown = (e: React.KeyboardEvent) => {
    if (!userOwns) return;
    const k = e.key;
    if (k === 'Backspace') {
      emitBackspace();
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
        {active && userOwns && (
          <button className="wheel-btn kbd-btn" onClick={focusKeyboard} title="Show keyboard to type into the page">
            ⌨ Keyboard
          </button>
        )}
        <button className="wheel-btn" disabled={!active} onClick={() => toggleWheel(agentId)}>
          {owner === 'user' ? 'Release the wheel' : 'Take the wheel'} <kbd>w</kbd>
        </button>
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
