import { useSyncExternalStore } from 'react';
import { renderMessageAudio } from '../audio';

// Single shared playback engine for spoken chat replies. One `<audio>` element
// (bound once from the app root via bindElement) plays at most one clip at a
// time; transcript rows never own media elements — they render pure UI that
// derives its progress from this module's state. See docs/ui.md and
// ui/src/components/panes/TranscriptPane.tsx for how rows and the global
// player consume this.
//
// Why not MediaSource/MSE for gapless playback: it is unavailable on iOS
// Safari, which is the primary target for this feature. Playback is instead a
// sequential `src` swap on the one element, advancing on `ended`.

export type PlaybackStatus = 'idle' | 'loading' | 'playing' | 'paused' | 'error';

export interface EngineState {
  // The chat this playlist belongs to. Switching chats resets playback.
  agentId: string | null;
  // Ordered message seqs (sections) for the focused chat.
  playlist: number[];
  // Index into playlist of the current/loading section, -1 if none armed.
  index: number;
  // Position within the current section, in seconds. Updated on discrete
  // transitions (section change, seek, pause) — NOT on every timeupdate, so
  // it is safe to read from React state without causing 4Hz re-renders. Live
  // position during playback is available via subscribeFrame/getGlobalPosition.
  position: number;
  // Duration of the current section, in seconds. NaN until known.
  duration: number;
  // Every clip's duration we've learned so far (populated on durationchange),
  // keyed by seq. Used for the global timeline and, later, real tick spacing.
  durations: Record<number, number>;
  status: PlaybackStatus;
  error: string | null;
  // Seqs rendered locally this session (via the per-row "Listen" button)
  // that the daemon hasn't necessarily confirmed as cached yet. Scoped to
  // agentId. Callers fold this into cachedAudio/playlist membership.
  renderedSeqs: number[];
  // User setting (persisted, device-local): keep the shared element producing
  // silent audio between sections / at the end of a finished playlist so a
  // phone with the screen off doesn't get frozen by the OS mid-playback. See
  // "Silence keepalive" below. Defaults to ON (the user explicitly wants
  // this); still user-toggleable because it costs battery and leaves a
  // persistent media notification.
  keepaliveEnabled: boolean;
}

type Listener = (state: EngineState) => void;
type FrameListener = (position: number, globalPosition: number) => void;

const FALLBACK_SECTION_SECONDS = 30;

const KEEPALIVE_STORAGE_KEY = 'tandem.audio.keepaliveEnabled';

function readKeepaliveSetting(): boolean {
  try {
    const raw = localStorage.getItem(KEEPALIVE_STORAGE_KEY);
    if (raw === '0') return false;
    if (raw === '1') return true;
    return true; // default ON, per explicit product decision — see EngineState.keepaliveEnabled doc.
  } catch {
    return true;
  }
}

function writeKeepaliveSetting(enabled: boolean) {
  try {
    localStorage.setItem(KEEPALIVE_STORAGE_KEY, enabled ? '1' : '0');
  } catch {
    // Storage can be unavailable (private browsing, quota) — the setting
    // just won't survive a reload; playback for this session is unaffected.
  }
}

let state: EngineState = {
  agentId: null,
  playlist: [],
  index: -1,
  position: 0,
  duration: NaN,
  durations: {},
  status: 'idle',
  error: null,
  renderedSeqs: [],
  keepaliveEnabled: readKeepaliveSetting(),
};

// Set true by the exported pause() (an explicit user action) and cleared by
// play()/activateSection() (any resume or fresh section). Distinguishes "the
// user asked us to stop" from "the playlist naturally ran out" — both leave
// EngineState.status === 'paused', but only the latter should keep the
// silence keepalive running (see wantsKeepalive below).
let userExplicitlyPaused = false;

const listeners = new Set<Listener>();
const frameListeners = new Set<FrameListener>();
let rafId: number | null = null;

let element: HTMLAudioElement | null = null;
let boundEl: HTMLAudioElement | null = null;

const renderedByAgent = new Map<string, Set<number>>();
const urlCache = new Map<string, string>();
// Object URLs the element currently points at (or is transitioning to) must
// never be revoked out from under it. Track the one live URL separately from
// the cache entries we're free to revoke once superseded.
let liveUrl: string | null = null;

function clipKey(agentId: string, seq: number): string {
  return `${agentId}:${seq}`;
}

function emit() {
  const snapshot = state;
  for (const l of listeners) l(snapshot);
}

function setState(patch: Partial<EngineState>) {
  const prevStatus = state.status;
  state = { ...state, ...patch };
  // The periodic (throttled) position flush only needs to run while actually
  // playing; entering/leaving 'playing' is therefore the single place that
  // starts/stops it. Immediate flushes on the specific triggers the brief
  // calls out (pause, section change, playlist end, tab hidden/pagehide) are
  // separate explicit calls at their call sites — see flushPosition callers.
  if (state.status === 'playing' && prevStatus !== 'playing') startPositionFlushLoop();
  else if (state.status !== 'playing' && prevStatus === 'playing') stopPositionFlushLoop();
  updateKeepalive();
  emit();
}

export function getState(): EngineState {
  return state;
}

export function subscribe(fn: Listener): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

// React binding. Discrete transitions only (see EngineState.position docs) —
// components that need live position should use subscribeFrame + a ref
// instead of this hook, or they will re-render at playback frame rate.
export function useEngineState(): EngineState {
  return useSyncExternalStore(subscribe, getState, getState);
}

// A single shared rAF loop drives all live-position consumers (the global
// timeline and the current row's inline bar) so N mounted widgets cost one
// rAF callback, not N. Consumers write directly to the DOM (CSS custom
// property / transform) rather than setState, so this never triggers React
// re-renders on its own.
function tick() {
  const pos = realCurrentTime();
  const global = currentGlobalPosition();
  for (const fn of frameListeners) fn(pos, global);
  rafId = requestAnimationFrame(tick);
}

export function subscribeFrame(fn: FrameListener): () => void {
  frameListeners.add(fn);
  if (rafId == null) rafId = requestAnimationFrame(tick);
  return () => {
    frameListeners.delete(fn);
    if (frameListeners.size === 0 && rafId != null) {
      cancelAnimationFrame(rafId);
      rafId = null;
    }
  };
}

// Even-share fallback for sections whose duration isn't known yet — used for
// both the global timeline math and (until real per-clip durations land) the
// GlobalAudioPlayer's tick spacing. Once every section's duration is known
// via `durations`, callers should read the exact value straight from that map
// instead of this estimate; see GlobalAudioPlayer's tick layout.
function estimateSectionSeconds(): number {
  const known = Object.values(state.durations).filter((d) => Number.isFinite(d) && d > 0);
  if (known.length === 0) return FALLBACK_SECTION_SECONDS;
  return known.reduce((a, b) => a + b, 0) / known.length;
}

function currentGlobalPosition(): number {
  let base = 0;
  for (let i = 0; i < state.index; i++) {
    const seq = state.playlist[i];
    const d = state.durations[seq];
    base += Number.isFinite(d) ? d : estimateSectionSeconds();
  }
  const live = realCurrentTime();
  return base + (Number.isFinite(live) ? live : 0);
}

export function getGlobalPosition(): number {
  return currentGlobalPosition();
}

// What the store's syncAudioFocus() needs to decide whether to retain audio
// focus while the document is hidden: the chat actively playing, or armed
// mid-section (paused with a section active) — NOT merely "has an unplayed
// playlist" (that's the "genuinely not listening" case the brief calls out;
// see wantsKeepalive below for the related-but-distinct predicate governing
// the silence keepalive rather than daemon pre-render focus).
export function getListeningAgentId(): string | null {
  const listening = state.status === 'playing' || (state.status === 'paused' && state.index >= 0);
  return listening ? state.agentId : null;
}

export function getGlobalDuration(): number {
  return state.playlist.reduce((sum, seq) => {
    const d = state.durations[seq];
    return sum + (Number.isFinite(d) ? d : estimateSectionSeconds());
  }, 0);
}

// --- Element binding -------------------------------------------------------

// Called once by the app-root component that owns the permanently-mounted
// <audio> element. If nothing has bound an element yet (e.g. in a unit test
// that renders a pane in isolation), the engine creates a detached one on
// first use so playback still works.
export function bindElement(el: HTMLAudioElement) {
  if (boundEl === el) return;
  boundEl = el;
  attach(el);
}

function ensureElement(): HTMLAudioElement {
  if (element) return element;
  const el = document.createElement('audio');
  attach(el);
  return el;
}

function attach(el: HTMLAudioElement) {
  if (element === el) return;
  const previous = element;
  element = el;
  if (previous && previous !== el) {
    // Transfer playback so binding the "real" mounted element after a
    // detached one was already driving playback doesn't restart the clip.
    el.src = previous.src;
    el.currentTime = previous.currentTime;
    previous.pause();
  }
  // Every listener below must ignore events fired for the keepalive silence
  // clip — it shares this element (see "Silence keepalive" below) but must
  // never be visible as playback state: no status flips, no duration
  // recorded against a real seq, no advance(), no error surfaced.
  el.addEventListener('play', () => {
    if (silencePlaying) return;
    setState({ status: 'playing', error: null });
  });
  el.addEventListener('pause', () => {
    if (silencePlaying) return;
    if (state.status !== 'playing') return;
    setState({ status: 'paused', position: el.currentTime });
    flushPosition(true);
  });
  el.addEventListener('durationchange', () => {
    if (silencePlaying) return;
    const seq = currentSeq();
    if (seq == null || !Number.isFinite(el.duration) || el.duration <= 0) return;
    setState({ duration: el.duration, durations: { ...state.durations, [seq]: el.duration } });
  });
  el.addEventListener('ended', () => {
    // The silence clip always loops (see startKeepaliveSilence), so 'ended'
    // should never fire for it — this guard is belt-and-suspenders.
    if (silencePlaying) return;
    setState({ position: Number.isFinite(el.duration) ? el.duration : state.position });
    advance();
  });
  el.addEventListener('error', () => {
    if (silencePlaying) return;
    if (el.src) setState({ status: 'error', error: 'Playback failed' });
  });
}

function currentSeq(): number | null {
  return state.index >= 0 ? state.playlist[state.index] ?? null : null;
}

// The index whose clip the shared element's `src`/`currentTime` actually
// reflects. A restored position (see "Position restore" below) sets
// state.index without going through activateSection — the element has
// nothing loaded for it yet — so play()/seekWithin() need to tell "already
// loaded, just resume/seek" apart from "never loaded, must fetch first".
let loadedIndex = -1;

// --- Silence keepalive ---------------------------------------------------
//
// Mobile browsers freeze a hidden page's timers and stall its WebSocket —
// navigator.wakeLock does NOT help (it requires a visible document and is
// auto-released on hide). The one reliable exception is that an actively
// *playing* media element keeps the page alive. So between sections (while
// the next clip is still being fetched) and once a playlist is exhausted but
// the chat is still "armed", we keep the single shared <audio> element
// producing silence rather than letting it go idle — this is what lets the
// next section start on its own with the screen off.
//
// This is best-effort, not a guarantee: iOS in particular may still suspend
// a backgrounded page eventually regardless (background audio budgets, the
// app being fully backgrounded rather than just screen-off, etc.).
//
// The silence clip shares the ONE element real sections play through (never
// a second element, never a competing WebAudio graph — see file header) so
// it never fights the real clip for the media session. Every element event
// listener in attach() ignores events fired while `silencePlaying` is set,
// and realCurrentTime()/currentGlobalPosition() fall back to the frozen
// `state.position` while it's active, so it can never be mistaken for a
// playing section, corrupt a recorded duration, or pollute a position flush
// with a fake seq.
//
// Whether the shared element currently holds the silence loop is tracked
// with this explicit flag rather than by comparing `element.src` against the
// silence blob URL — object URLs aren't guaranteed distinguishable that way
// (e.g. a test or polyfill that stubs URL.createObjectURL to a fixed value),
// and an explicit flag we set/clear ourselves is exact regardless.
let silencePlaying = false;
let silenceUrl: string | null = null;

// A minimal valid WAV: 8-bit PCM, mono, silent (all samples at the unsigned
// midpoint, 128). Generated in-process rather than shipped as a repo asset.
function buildSilentWavBytes(seconds = 1, sampleRate = 8000): Uint8Array {
  const numSamples = Math.max(1, Math.floor(seconds * sampleRate));
  const dataSize = numSamples; // 1 byte/sample (8-bit PCM)
  const buffer = new ArrayBuffer(44 + dataSize);
  const view = new DataView(buffer);
  const writeStr = (offset: number, s: string) => {
    for (let i = 0; i < s.length; i++) view.setUint8(offset + i, s.charCodeAt(i));
  };
  writeStr(0, 'RIFF');
  view.setUint32(4, 36 + dataSize, true);
  writeStr(8, 'WAVE');
  writeStr(12, 'fmt ');
  view.setUint32(16, 16, true); // PCM fmt chunk size
  view.setUint16(20, 1, true); // format = PCM
  view.setUint16(22, 1, true); // channels
  view.setUint32(24, sampleRate, true);
  view.setUint32(28, sampleRate, true); // byte rate (1 byte/sample * sampleRate)
  view.setUint16(32, 1, true); // block align
  view.setUint16(34, 8, true); // bits per sample
  writeStr(36, 'data');
  view.setUint32(40, dataSize, true);
  const bytes = new Uint8Array(buffer);
  bytes.fill(128, 44); // silence
  return bytes;
}

function ensureSilenceUrl(): string {
  if (silenceUrl) return silenceUrl;
  const bytes = buildSilentWavBytes();
  const blob = new Blob([bytes.slice().buffer], { type: 'audio/wav' });
  silenceUrl = URL.createObjectURL(blob);
  return silenceUrl;
}

// True while the shared element currently holds a real clip (or nothing yet)
// rather than the silence loop — callers that decide "is a fresh load
// needed" must treat "showing silence" the same as "nothing loaded", since
// resuming/seeking against the silence src would silently do nothing to the
// real section.
function elementHoldsRealClip(): boolean {
  return !silencePlaying;
}

// Prefer this over a raw `element.currentTime` read anywhere that number
// feeds visible/persisted state (the live timeline, position flushes) — while
// silence is looping, the element's real currentTime cycles 0..~1s and must
// not leak into the frozen `state.position` those readers expect.
function realCurrentTime(): number {
  if (silencePlaying) return state.position;
  return element?.currentTime ?? state.position;
}

function wantsKeepalive(): boolean {
  if (!state.keepaliveEnabled) return false;
  if (!state.agentId || state.playlist.length === 0) return false;
  if (userExplicitlyPaused) return false;
  // 'idle' covers both "never played anything in this chat yet" and a fresh
  // setPlaylist reset — don't start burning battery until the user has
  // actually begun listening. 'error' and 'playing' need no keepalive help.
  if (state.status === 'idle' || state.status === 'error' || state.status === 'playing') return false;
  // The very first fetch (nothing has ever loaded into the element yet) is a
  // direct consequence of the user's own tap, not a "between sections" gap —
  // don't engage keepalive for it.
  if (state.status === 'loading' && loadedIndex === -1) return false;
  return true; // 'loading' (fetching the next section) or an auto-paused 'paused' (playlist end, blocked autoplay retry)
}

function startKeepaliveSilence(el: HTMLAudioElement) {
  const url = ensureSilenceUrl();
  silencePlaying = true;
  el.loop = true;
  if (el.src !== url) el.src = url;
  void el.play().catch(() => {
    // Autoplay can be blocked outside a user gesture; nothing more to do here
    // — the next real play() (itself gesture-derived, or a continuation of an
    // already-playing session) will retry naturally.
  });
}

function stopKeepaliveSilence(el: HTMLAudioElement) {
  silencePlaying = false;
  el.loop = false;
  el.pause();
}

// Undo the silence takeover in place, synchronously — used when the section
// that's "already loaded" (loadedIndex) is the one being resumed, so there's
// a cached URL to restore without a re-fetch/activateSection round trip
// (which would also turn a same-tick resume into an async one).
function resumeFromSilence(offset: number) {
  const el = ensureElement();
  silencePlaying = false;
  el.loop = false;
  if (liveUrl && el.src !== liveUrl) el.src = liveUrl;
  el.currentTime = offset;
}

// Single choke point, called from setState() after every transition — see
// its call site for why that's sufficient (state changes are the only thing
// wantsKeepalive() depends on besides the element itself).
function updateKeepalive() {
  const el = element;
  if (!el) return;
  const want = wantsKeepalive();
  if (want && !silencePlaying) startKeepaliveSilence(el);
  else if (!want && silencePlaying) stopKeepaliveSilence(el);
}

// User-facing setting (see EngineState.keepaliveEnabled) — persisted
// device-locally like localStorage position/theme/etc, not daemon state,
// since it's purely about this browser's own background-survival behavior.
export function setKeepaliveEnabled(enabled: boolean) {
  writeKeepaliveSetting(enabled);
  setState({ keepaliveEnabled: enabled });
}

// --- Clip loading ------------------------------------------------------

// Snapshot rows can surface many ready clips at once (reconnect, chat
// switch). Batch those into a single per-agent queue, newest seq first, so
// the bottom of the chat becomes playable before a burst of older ones —
// this preserves useCachedAudioLoader's old prioritization.
const queue: { agentId: string; seq: number; resolve: (url: string) => void; reject: (cause: unknown) => void }[] = [];
let draining = false;

function drain() {
  if (draining) return;
  draining = true;
  void (async () => {
    while (queue.length) {
      queue.sort((a, b) => b.seq - a.seq);
      const job = queue.shift()!;
      const key = clipKey(job.agentId, job.seq);
      try {
        const cached = urlCache.get(key);
        const url = cached ?? await renderMessageAudio(job.agentId, job.seq);
        urlCache.set(key, url);
        job.resolve(url);
      } catch (cause) {
        job.reject(cause);
      }
    }
    draining = false;
  })();
}

function queueLoad(agentId: string, seq: number): Promise<string> {
  const cached = urlCache.get(clipKey(agentId, seq));
  if (cached) return Promise.resolve(cached);
  return new Promise((resolve, reject) => {
    queue.push({ agentId, seq, resolve, reject });
    drain();
  });
}

// Public entry point for eager, non-urgent loading — e.g. a row whose clip
// the daemon already reports as cached/ready, fetched ahead of any tap so
// playback can start immediately. Shares the same newest-first queue (and
// therefore the same in-flight request) that section activation uses, so
// there is never a duplicate fetch for a clip that's both prefetched and
// then played.
export function prefetchClip(agentId: string, seq: number): Promise<string> {
  return queueLoad(agentId, seq);
}

// Explicit user action (the per-row "Listen" button) — fetch immediately,
// ahead of the batch queue, and remember the seq as locally rendered so the
// caller can fold it into playlist membership even before the daemon's
// audioReadySeqs snapshot confirms it.
export async function renderClip(agentId: string, seq: number): Promise<string> {
  const key = clipKey(agentId, seq);
  const cached = urlCache.get(key);
  const url = cached ?? await renderMessageAudio(agentId, seq);
  urlCache.set(key, url);
  const set = renderedByAgent.get(agentId) ?? new Set<number>();
  set.add(seq);
  renderedByAgent.set(agentId, set);
  if (state.agentId === agentId) setState({ renderedSeqs: [...set] });
  return url;
}

export function getRenderedSeqs(agentId: string): number[] {
  return [...(renderedByAgent.get(agentId) ?? [])];
}

// Seed known clip durations ahead of playback (e.g. once the daemon reports
// per-clip lengths in a snapshot, rather than only learning them as each clip
// is played). A no-op today — nothing calls it yet — but GlobalAudioPlayer's
// tick layout already prefers `durations` over its even-spacing fallback, so
// wiring a real source in is a one-line call to this function, not a
// GlobalAudioPlayer change.
export function seedDurations(agentId: string, durations: Record<number, number>) {
  if (state.agentId !== agentId) return;
  setState({ durations: { ...durations, ...state.durations } });
}

function revokeIfUnused(url: string | null) {
  if (!url || url === liveUrl) return;
  URL.revokeObjectURL(url);
}

// --- Position restore ---------------------------------------------------
//
// Two layers, matching CLAUDE.md design invariant #1 ("the daemon owns all
// state ... everything must be reconstructable from daemon state on
// reconnect"): the daemon's stored position is the durable source of truth;
// localStorage is purely a latency optimization so switching chats or
// reopening a tab repaints the right position before the WS snapshot
// round-trip completes. localStorage can therefore never be allowed to
// permanently override the daemon — see reconcileDaemonPosition's tie-break.

export interface AudioPosition {
  seq: number;
  positionMs: number;
  updatedAt: number;
}

const POSITION_FLUSH_INTERVAL_MS = 5000;

type PositionSender = (agentId: string, seq: number, positionMs: number) => void;
let positionSender: PositionSender | null = null;

// Handed a sender by whatever owns the WS client (store.ts, via the app-root
// component) — kept out of the engine's own imports so it stays framework-
// agnostic (see file header).
export function setPositionSender(fn: PositionSender | null) {
  positionSender = fn;
}

function localPositionKey(agentId: string): string {
  return `tandem.audio.position.${agentId}`;
}

function writeLocalPosition(agentId: string, pos: AudioPosition) {
  try {
    localStorage.setItem(localPositionKey(agentId), JSON.stringify(pos));
  } catch {
    // Storage can be unavailable (private browsing, quota) — the daemon is
    // still the durable copy, so this is only a missed latency optimization.
  }
}

function readLocalPosition(agentId: string): AudioPosition | null {
  try {
    const raw = localStorage.getItem(localPositionKey(agentId));
    if (!raw) return null;
    const parsed: unknown = JSON.parse(raw);
    if (
      parsed && typeof parsed === 'object'
      && typeof (parsed as AudioPosition).seq === 'number'
      && typeof (parsed as AudioPosition).positionMs === 'number'
      && typeof (parsed as AudioPosition).updatedAt === 'number'
    ) {
      return parsed as AudioPosition;
    }
  } catch {
    // Malformed/unavailable storage — treat as "nothing saved".
  }
  return null;
}

let lastPositionFlush = 0;
let positionFlushTimer: ReturnType<typeof setInterval> | null = null;

// `immediate` bypasses the ~5s throttle for the specific triggers the brief
// calls out (pause, section change, playlist end, tab hidden/closing);
// periodic in-playback flushing goes through the throttled path instead of
// firing on every timeupdate.
function flushPosition(immediate: boolean) {
  const { agentId, index, playlist } = state;
  if (!agentId) return;
  const now = Date.now();
  if (!immediate && now - lastPositionFlush < POSITION_FLUSH_INTERVAL_MS) return;
  lastPositionFlush = now;
  const seq = index >= 0 ? playlist[index] : 0; // seq 0 == "no active section" (clears the daemon's stored value)
  const positionMs = index >= 0 ? Math.round(realCurrentTime() * 1000) : 0;
  const pos: AudioPosition = { seq, positionMs, updatedAt: now };
  // The localStorage write must happen synchronously here — this is also
  // called from the pagehide handler below, where an in-flight WS send has
  // no guarantee of landing before the page is gone.
  writeLocalPosition(agentId, pos);
  positionSender?.(agentId, seq, positionMs);
}

function startPositionFlushLoop() {
  if (positionFlushTimer != null) return;
  positionFlushTimer = setInterval(() => flushPosition(false), POSITION_FLUSH_INTERVAL_MS);
}

function stopPositionFlushLoop() {
  if (positionFlushTimer == null) return;
  clearInterval(positionFlushTimer);
  positionFlushTimer = null;
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden') flushPosition(true);
  });
}
if (typeof window !== 'undefined') {
  window.addEventListener('pagehide', () => flushPosition(true));
}

function applyRestoredPosition(agentId: string, pos: AudioPosition | null) {
  if (state.agentId !== agentId) return; // playlist for this chat isn't active yet
  if (!pos || pos.seq === 0) return;
  const index = state.playlist.indexOf(pos.seq);
  if (index === -1) return; // seq no longer in the transcript -- fall back to no position
  const known = state.durations[pos.seq];
  const seconds = pos.positionMs / 1000;
  const clamped = Number.isFinite(known) && known > 0 ? Math.min(seconds, known) : seconds;
  loadedIndex = -1; // nothing is actually loaded into the element yet
  // Restoring never auto-starts playback — it only primes where the next
  // play() should resume, and gives InlineAudioBar's three-state progress
  // rule (before/current/after) the right values immediately.
  setState({ index, position: Math.max(0, clamped), duration: Number.isFinite(known) ? known : NaN, status: 'paused' });
}

// Call when focusing a chat, before the daemon snapshot necessarily has
// arrived, to paint instantly from the last locally-known position.
export function restoreLocalPosition(agentId: string) {
  applyRestoredPosition(agentId, readLocalPosition(agentId));
}

// Call whenever the daemon-hydrated position for this chat is available/
// changes (AgentView.audioPosition). The daemon wins unless the local copy
// has a strictly newer `updatedAt` (e.g. a flush that raced the snapshot).
export function reconcileDaemonPosition(agentId: string, daemon: AudioPosition | null) {
  // Never yank the seek position out from under someone already listening —
  // reconciliation only matters for priming a chat that hasn't started
  // playing yet.
  if (state.agentId === agentId && state.status !== 'idle' && state.status !== 'paused') return;
  const local = readLocalPosition(agentId);
  const chosen: AudioPosition | null = local && daemon
    ? (local.updatedAt > daemon.updatedAt ? local : daemon)
    : (daemon ?? local ?? null);
  applyRestoredPosition(agentId, chosen);
}

// --- Playlist + transport ---------------------------------------------

function arraysEqual(a: number[], b: number[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}

export function setPlaylist(agentId: string, seqs: number[]) {
  if (state.agentId !== agentId) {
    // Leaving one chat for another: flush the outgoing chat's position
    // (synchronously to localStorage; best-effort to the daemon) before
    // repointing the engine at the new one.
    flushPosition(true);
    ensureElement().pause();
    loadedIndex = -1;
    userExplicitlyPaused = false;
    setState({
      agentId,
      playlist: seqs,
      index: -1,
      position: 0,
      duration: NaN,
      durations: {},
      status: 'idle',
      error: null,
      renderedSeqs: getRenderedSeqs(agentId),
    });
    return;
  }
  if (arraysEqual(state.playlist, seqs)) return;
  const activeSeq = currentSeq();
  const nextIndex = activeSeq != null ? seqs.indexOf(activeSeq) : -1;
  setState({ playlist: seqs, index: nextIndex });
}

async function activateSection(index: number, opts: { autoplay: boolean; offset?: number }) {
  const { agentId, playlist } = state;
  if (!agentId || index < 0 || index >= playlist.length) return;
  if (state.index !== -1 && state.index !== index) flushPosition(true); // flush the outgoing section
  const seq = playlist[index];
  const generation = ++loadGeneration;
  userExplicitlyPaused = false; // an activation is always a resume/fresh-section, never a pause
  setState({ index, status: 'loading', error: null, position: opts.offset ?? 0, duration: state.durations[seq] ?? NaN });
  try {
    const url = await queueLoad(agentId, seq);
    if (generation !== loadGeneration) return; // superseded by a newer activation
    const el = ensureElement();
    const stale = liveUrl;
    silencePlaying = false; // a real section is taking the element back over
    el.loop = false; // undo the keepalive silence loop if it was showing
    if (el.src !== url) el.src = url;
    liveUrl = url;
    revokeIfUnused(stale);
    el.currentTime = opts.offset ?? 0;
    loadedIndex = index;
    if (opts.autoplay) {
      await el.play().then(() => {
        // Belt-and-suspenders: the element's own 'play' listener (attach())
        // also does this, but doesn't fire in every environment (notably
        // jsdom, when .play() is stubbed rather than truly implemented), and
        // there's no reason to make correctness depend on that event firing.
        if (generation === loadGeneration) setState({ status: 'playing', error: null });
      }).catch(() => {
        if (generation === loadGeneration) setState({ status: 'paused' });
      });
    } else {
      setState({ status: 'paused' });
    }
  } catch (cause) {
    if (generation === loadGeneration) {
      setState({ status: 'error', error: cause instanceof Error ? cause.message : String(cause) });
    }
  }
}

let loadGeneration = 0;

export function play(agentId: string, seq?: number) {
  if (state.agentId !== agentId) return;
  userExplicitlyPaused = false;
  const targetIndex = seq != null ? state.playlist.indexOf(seq) : state.index;
  if (targetIndex === -1) {
    if (seq == null && state.playlist.length > 0) void activateSection(0, { autoplay: true });
    return;
  }
  if (targetIndex !== loadedIndex) {
    // Fresh tap on a different section, or resuming a restored position
    // whose section was never actually loaded into the element yet — prime
    // it at the right offset (0 for a fresh section, the restored/paused
    // position when resuming the already-current one).
    const offset = targetIndex === state.index ? state.position : 0;
    void activateSection(targetIndex, { autoplay: true, offset });
    return;
  }
  // The target section is already loaded — but the keepalive silence may
  // have taken over the element's src since (see "Silence keepalive" above).
  // Restore it synchronously (the URL is still cached) rather than routing
  // through activateSection's async re-fetch.
  if (!elementHoldsRealClip()) resumeFromSilence(state.position);
  void ensureElement().play().catch(() => setState({ status: 'paused' }));
}

export function pause() {
  userExplicitlyPaused = true;
  element?.pause();
  // The 'pause' event listener updates status for a real clip; flush
  // unconditionally here too (cheap/idempotent) so an explicit pause() call
  // flushes even in environments where a stubbed .pause() doesn't fire the
  // event (tests). Also stop the keepalive silence directly: if it (rather
  // than a real clip) was what the element was actually playing, its own
  // 'pause' event is ignored by attach()'s guard, so nothing else would.
  flushPosition(true);
  updateKeepalive();
}

export function toggle(agentId: string) {
  if (state.agentId !== agentId) return;
  if (state.status === 'playing') pause();
  else play(agentId);
}

// Seek to an absolute offset within a specific section, switching sections if
// needed. Used both by "tap a row's bar" (row not current) and scrubbing the
// current row.
export function seekWithin(seq: number, seconds: number) {
  const { agentId, playlist, index } = state;
  if (!agentId) return;
  const i = playlist.indexOf(seq);
  if (i === -1) return;
  const clamped = Math.max(0, seconds);
  if (i !== index || i !== loadedIndex) {
    void activateSection(i, { autoplay: state.status === 'playing', offset: clamped });
    return;
  }
  if (!elementHoldsRealClip()) resumeFromSilence(clamped);
  const el = ensureElement();
  el.currentTime = clamped;
  setState({ position: clamped });
}

// Seek to an absolute offset across the whole playlist (the GlobalAudioPlayer
// timeline), resolving which section that lands in.
export function seekGlobal(target: number) {
  const { playlist, durations } = state;
  if (playlist.length === 0) return;
  let remaining = Math.max(0, target);
  for (let i = 0; i < playlist.length; i++) {
    const seq = playlist[i];
    const d = durations[seq] ?? estimateSectionSeconds();
    const isLast = i === playlist.length - 1;
    if (remaining <= d || isLast) {
      seekWithin(seq, isLast ? Math.min(remaining, Number.isFinite(d) ? d : remaining) : remaining);
      return;
    }
    remaining -= d;
  }
}

export function skip(deltaSeconds: number) {
  seekGlobal(currentGlobalPosition() + deltaSeconds);
}

function advance() {
  const { index, playlist } = state;
  if (index + 1 < playlist.length) {
    void activateSection(index + 1, { autoplay: true });
  } else {
    // Stay armed at the end rather than tearing playback down, so an earbud
    // press (or GlobalAudioPlayer replay) can resume/restart.
    setState({ status: 'paused' });
    flushPosition(true); // playlist end
  }
}

export function next() {
  const { index, playlist } = state;
  if (index + 1 >= playlist.length) return;
  void activateSection(index + 1, { autoplay: state.status !== 'idle' });
}

export function prev() {
  const { index } = state;
  if (index <= 0) {
    if (state.playlist.length > 0) seekWithin(state.playlist[0], 0);
    return;
  }
  void activateSection(index - 1, { autoplay: state.status === 'playing' });
}

// --- Test support ---------------------------------------------------------
// The engine is a module-level singleton (by design — there is exactly one
// playback session in the app), so tests need a way to reset it between
// cases and reach into the element it's driving.

export function __getElementForTests(): HTMLAudioElement | null {
  return element;
}

export function __resetForTests() {
  element?.pause();
  if (rafId != null) cancelAnimationFrame(rafId);
  rafId = null;
  frameListeners.clear();
  listeners.clear();
  queue.length = 0;
  draining = false;
  loadGeneration++;
  urlCache.clear();
  renderedByAgent.clear();
  liveUrl = null;
  element = null;
  boundEl = null;
  loadedIndex = -1;
  mediaSessionInstalled = false;
  lastPositionUpdate = 0;
  stopPositionFlushLoop();
  lastPositionFlush = 0;
  positionSender = null;
  userExplicitlyPaused = false;
  silencePlaying = false;
  if (silenceUrl) {
    try {
      URL.revokeObjectURL(silenceUrl);
    } catch {
      // jsdom/test environments may not implement this; harmless either way.
    }
  }
  silenceUrl = null;
  state = {
    agentId: null,
    playlist: [],
    index: -1,
    position: 0,
    duration: NaN,
    durations: {},
    status: 'idle',
    error: null,
    renderedSeqs: [],
    keepaliveEnabled: true, // deterministic for tests, matching the default-ON product decision
  };
}

// --- Media Session ------------------------------------------------------
//
// Installed once against the engine rather than per-element, so there is no
// ownership bookkeeping left to get wrong (see the "Support earbud seeking"
// and "Keep earbud play/pause after a response clip ends" history this
// replaces). Semantics preserved from that history:
//   - previoustrack/nexttrack seek the current clip ∓10s; many earbuds report
//     double/triple presses as track changes rather than seeks, and this is
//     NOT a request to change sections.
//   - A finished clip (playlist exhausted) does not tear the session down:
//     handlers stay installed and playbackState reads 'paused', so a press
//     resumes/replays instead of the OS treating Tandem as having nothing
//     playable.
//   - playbackState only goes 'none' on true teardown (no active chat/playlist).

function mediaSession(): MediaSession | null {
  return typeof navigator !== 'undefined' && 'mediaSession' in navigator ? navigator.mediaSession : null;
}

function setMediaAction(session: MediaSession, action: MediaSessionAction, handler: MediaSessionActionHandler | null) {
  try {
    session.setActionHandler(action, handler);
  } catch {
    // Browsers expose Media Session actions independently; keep the supported ones.
  }
}

let mediaSessionInstalled = false;

export function installMediaSession() {
  const session = mediaSession();
  if (!session || mediaSessionInstalled) return;
  mediaSessionInstalled = true;
  setMediaAction(session, 'play', () => { if (state.agentId) play(state.agentId); });
  setMediaAction(session, 'pause', () => pause());
  setMediaAction(session, 'seekbackward', (details) => skip(-(details.seekOffset ?? 10)));
  setMediaAction(session, 'seekforward', (details) => skip(details.seekOffset ?? 10));
  setMediaAction(session, 'seekto', (details) => {
    if (typeof details.seekTime !== 'number') return;
    seekGlobal(details.seekTime);
  });
  setMediaAction(session, 'previoustrack', () => skip(-10));
  setMediaAction(session, 'nexttrack', () => skip(10));
  subscribe(updateMediaSessionPlaybackState);
  subscribeFrame(updateMediaSessionPosition);
  updateMediaSessionPlaybackState(state);
}

function updateMediaSessionPlaybackState(s: EngineState) {
  const session = mediaSession();
  if (!session) return;
  if (s.agentId == null || s.playlist.length === 0) {
    session.playbackState = 'none';
    return;
  }
  session.playbackState = s.status === 'playing' ? 'playing' : 'paused';
}

let lastPositionUpdate = 0;

function updateMediaSessionPosition(_position: number, global: number) {
  const session = mediaSession();
  if (!session || state.agentId == null) return;
  const now = typeof performance !== 'undefined' ? performance.now() : Date.now();
  if (now - lastPositionUpdate < 200) return;
  lastPositionUpdate = now;
  const duration = getGlobalDuration();
  if (!Number.isFinite(duration) || duration <= 0) return;
  try {
    session.setPositionState({
      duration,
      playbackRate: element?.playbackRate ?? 1,
      position: Math.max(0, Math.min(duration, global)),
    });
  } catch {
    // Metadata can briefly be inconsistent while a newly loaded clip settles.
  }
}
