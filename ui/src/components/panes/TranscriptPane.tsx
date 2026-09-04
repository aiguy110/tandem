import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { useStore } from '../../store';
import { UnifiedDiff } from '../diff/UnifiedDiff';
import { createUnifiedPatch } from '../diff/textDiff';
import type { AckResult, AgentView } from '../../store';
import type { Annotation, Approval, ImageAssetRef, PromptBlock, QueuedPrompt, SlashCommand, ToolStatus, WireEvent, WorkspaceEntry } from '../../wire';
import { storedToken } from '../../ws/client';
import { renderMarkdown } from '../../markdown';
import { fuzzyFilter } from '../../fuzzy';
import { usesSoftKeyboard } from '../../mobile';
import { PermissionRequestDetails } from '../PermissionRequest';
import { usePresence, useUpdateFlash, useValuePresence } from '../../transitions';
import { getRenderedSeqs, getState, play, prefetchClip, reconcileDaemonPosition, renderClip, restoreLocalPosition, setPlaylist, useEngineState } from '../../audio/engine';
import { GlobalAudioPlayer } from '../audio/GlobalAudioPlayer';
import { InlineAudioBar } from '../audio/InlineAudioBar';

// The Transcript pane renders the normalized AgentEvent stream (docs/ui.md):
// merged prose, dimmed thoughts, collapsed tool cards with status chips, plans,
// per-terminal mini-terminals, inline permission cards, and error banners. A
// prompt input sends {t:'prompt'}; Esc/Interrupt sends {t:'interrupt'}.
//
// Rows also carry their representative `seq` (docs/transcript-annotations.md)
// so a DOM text selection inside an annotatable row (user/message/thought;
// tool rows are skipped for v1) can be anchored to it and turned into a
// durable Annotation. `permission` and `plan` rows are never annotatable.

type Item =
  | { kind: 'user'; key: string; seq: number; blocks: PromptBlock[] }
  | { kind: 'message'; key: string; seq: number; text: string }
  | { kind: 'thought'; key: string; seq: number; text: string }
  | { kind: 'tool'; key: string; title: string; status: ToolStatus; content?: unknown; rawInput?: unknown; toolKind?: string }
  | { kind: 'plan'; key: string; entries: { label: string; status: 'pending' | 'in_progress' | 'done' }[] }
  | { kind: 'terminal'; key: string; termId: string; text: string; truncated: boolean }
  | { kind: 'permission'; key: string; reqId: string; title: string; options: { optionId: string; name: string }[] }
  | { kind: 'error'; key: string; message: string }
  | { kind: 'aside'; key: string; id: string; question: string; answer: string; thought: string; complete: boolean; error?: string };

function build(events: { seq: number; event: WireEvent }[], pending: Approval[]): Item[] {
  const items: Item[] = [];
  const tools = new Map<string, Extract<Item, { kind: 'tool' }>>();
  const terms = new Map<string, Extract<Item, { kind: 'terminal' }>>();
  let plan: Extract<Item, { kind: 'plan' }> | undefined;
  const pendingIds = new Set(pending.map((p) => p.reqId));
  const asides = new Map<string, Extract<Item, { kind: 'aside' }>>();

  for (const { seq, event: ev } of events) {
    switch (ev.kind) {
      case 'user_message':
        items.push({
          kind: 'user',
          key: `u${seq}`,
          seq,
          blocks: ev.blocks ?? (ev.text != null ? [{ type: 'text', text: ev.text }] : []),
        });
        break;
      case 'message_chunk': {
        const last = items[items.length - 1];
        if (last && last.kind === 'message') last.text += ev.text;
        else items.push({ kind: 'message', key: `m${seq}`, seq, text: ev.text });
        break;
      }
      case 'thought_chunk': {
        const last = items[items.length - 1];
        if (last && last.kind === 'thought') last.text += ev.text;
        else items.push({ kind: 'thought', key: `t${seq}`, seq, text: ev.text });
        break;
      }
      case 'tool_call': {
        // Real agents re-send tool_call for the SAME toolCallId as it progresses
        // (pending → running → done). Keep ONE card per id, updated in place —
        // pushing a fresh item each time gives every card the key `tc${id}`, and
        // duplicate React keys make the cards render as empty slivers on remount.
        const existing = tools.get(ev.id);
        if (existing) {
          existing.title = ev.title || existing.title;
          existing.status = ev.status;
          if (ev.content != null) existing.content = ev.content;
          if (ev.rawInput != null) existing.rawInput = ev.rawInput;
          if (ev.toolKind != null) existing.toolKind = ev.toolKind;
        } else {
          const item: Extract<Item, { kind: 'tool' }> = { kind: 'tool', key: `tc${ev.id}`, title: ev.title, status: ev.status, content: ev.content, rawInput: ev.rawInput, toolKind: ev.toolKind };
          tools.set(ev.id, item);
          items.push(item);
        }
        break;
      }
      case 'tool_call_update': {
        // Some agents (Claude) send an initial tool_call before a Bash
        // command's input has finished streaming, then refine title/rawInput
        // here once the real command is known — pick those up too, not just
        // status/content, or the card gets stuck on its placeholder title.
        const t = tools.get(ev.id);
        if (t) {
          if (ev.status) t.status = ev.status;
          if (ev.content != null) t.content = ev.content;
          if (ev.title) t.title = ev.title;
          if (ev.rawInput != null) t.rawInput = ev.rawInput;
          if (ev.toolKind != null) t.toolKind = ev.toolKind;
        }
        break;
      }
      case 'plan':
        // A plan is current session state, not another point-in-time chat
        // message. Keep one stable item and replace its contents on updates.
        plan ??= { kind: 'plan', key: 'plan', entries: [] };
        plan.entries = ev.entries;
        break;
      case 'terminal_output': {
        let term = terms.get(ev.termId);
        if (!term) {
          term = { kind: 'terminal', key: `term${ev.termId}`, termId: ev.termId, text: '', truncated: false };
          terms.set(ev.termId, term);
          items.push(term);
        }
        term.text += ev.chunk;
        term.truncated = term.truncated || ev.truncated;
        break;
      }
      case 'permission_request':
        items.push({ kind: 'permission', key: `perm${ev.reqId}`, reqId: ev.reqId, title: ev.title, options: ev.options });
        break;
      case 'error':
        items.push({ kind: 'error', key: `err${seq}`, message: ev.message });
        break;
      case 'aside_started': {
        const item: Extract<Item, { kind: 'aside' }> = { kind: 'aside', key: `aside-${ev.asideId}`, id: ev.asideId, question: ev.question, answer: '', thought: '', complete: false };
        asides.set(ev.asideId, item);
        items.push(item);
        break;
      }
      case 'aside_event': {
        const aside = asides.get(ev.asideId);
        if (!aside) break;
        if (ev.event.kind === 'message_chunk') aside.answer += ev.event.text;
        if (ev.event.kind === 'thought_chunk') aside.thought += ev.event.text;
        break;
      }
      case 'aside_completed': {
        const aside = asides.get(ev.asideId);
        if (aside) { aside.complete = true; aside.error = ev.error; }
        break;
      }
    }
  }
  // Keep only still-pending permission cards inline (answered ones fall away).
  const visible = items.filter((it) => it.kind !== 'permission' || pendingIds.has(it.reqId));
  // The current task list is session state rather than transcript history.
  // Append it here so callers can split it into the pane's fixed bottom slot.
  if (plan) visible.push(plan);
  return visible;
}

// A captured, in-progress selection: the anchor row it resolved to plus the
// selection's client rect (for positioning the floating Comment button /
// popover). docs/transcript-annotations.md "Selection capture".
interface SelectionAnchor {
  seq: number;
  role: string;
  quote: string;
  rect: { top: number; left: number; width: number; height: number };
}

// A quote can point to either a pending annotation in the review tray or a
// citation chip in an already-sent user message.  Keeping the target as a DOM
// id lets the source highlight work for both without giving persisted prompt
// blocks a UI-only identifier.
interface QuoteLink {
  quote: string;
  targetId: string;
}

function annotationTargetId(id: string) {
  return `annotation-${id}`;
}

function citationTargetId(seq: number, index: number) {
  return `citation-${seq}-${index}`;
}

// Markdown is rendered as HTML, so a quote may span several text nodes. Split
// those nodes at every quoted-range boundary and attach all links that cover
// each resulting fragment. This preserves the existing markdown DOM while
// making the exact source text clickable. Re-run from the pristine React DOM
// whenever the row's text or links change.
function applyQuoteHighlights(container: HTMLElement, links: QuoteLink[]) {
  for (const mark of Array.from(container.querySelectorAll('.annotation-quote-highlight'))) {
    mark.replaceWith(...Array.from(mark.childNodes));
  }
  container.normalize();
  if (links.length === 0) return;

  const walker = document.createTreeWalker(container, NodeFilter.SHOW_TEXT);
  const nodes: { node: Text; start: number; end: number }[] = [];
  let text = '';
  let node: Text | null;
  while ((node = walker.nextNode() as Text | null)) {
    const start = text.length;
    text += node.data;
    nodes.push({ node, start, end: text.length });
  }

  const ranges = links.flatMap((link) => {
    const start = text.indexOf(link.quote);
    return start < 0 ? [] : [{ start, end: start + link.quote.length, targetId: link.targetId }];
  });
  if (ranges.length === 0) return;

  for (const { node: textNode, start, end } of nodes) {
    const overlapping = ranges.filter((range) => range.start < end && range.end > start);
    if (overlapping.length === 0) continue;
    const cuts = new Set<number>([0, textNode.data.length]);
    for (const range of overlapping) {
      cuts.add(Math.max(0, range.start - start));
      cuts.add(Math.min(textNode.data.length, range.end - start));
    }
    const boundaries = [...cuts].sort((a, b) => a - b);
    const fragment = document.createDocumentFragment();
    for (let i = 0; i < boundaries.length - 1; i++) {
      const from = boundaries[i];
      const to = boundaries[i + 1];
      const part = textNode.data.slice(from, to);
      const targets = overlapping
        .filter((range) => range.start < start + to && range.end > start + from)
        .map((range) => range.targetId);
      if (targets.length === 0) {
        fragment.append(part);
      } else {
        const mark = document.createElement('span');
        mark.className = 'annotation-quote-highlight';
        mark.dataset.annotationTargets = targets.join('|');
        mark.textContent = part;
        fragment.append(mark);
      }
    }
    textNode.replaceWith(fragment);
  }
}

// Walks up from a Selection's anchorNode to the nearest annotatable row
// (stamped with data-seq by Row, above). Text nodes aren't Elements, so start
// from the parent when needed.
function closestRow(node: Node | null): HTMLElement | null {
  if (!node) return null;
  const el = node.nodeType === Node.ELEMENT_NODE ? (node as Element) : node.parentElement;
  return (el?.closest('[data-seq]') as HTMLElement | null) ?? null;
}

const ANNOTATION_QUOTE_MAX = 2048;

function visibleViewport() {
  const viewport = window.visualViewport;
  return {
    top: viewport?.offsetTop ?? 0,
    left: viewport?.offsetLeft ?? 0,
    width: viewport?.width ?? window.innerWidth,
    height: viewport?.height ?? window.innerHeight,
  };
}

function mobilePopoverPosition() {
  const viewport = visibleViewport();
  const width = Math.min(360, viewport.width - 24);
  return { top: viewport.top + 8, left: viewport.left + (viewport.width - width) / 2 };
}

export function TranscriptPane() {
  const agent = useStore((s) => (s.focusedId ? s.agents[s.focusedId] : undefined)) as AgentView | undefined;
  const respond = useStore((s) => s.respond);
  const annotations = useStore((s) => (agent ? s.annotations[agent.id] : undefined)) ?? [];
  const addAnnotation = useStore((s) => s.addAnnotation);
  const updateAnnotation = useStore((s) => s.updateAnnotation);
  const removeAnnotation = useStore((s) => s.removeAnnotation);
  const scrollRef = useRef<HTMLDivElement>(null);
  // `stick` follows the tail as new items arrive; it flips off the moment the
  // user scrolls up and back on when they return (or hit the button). Kept in a
  // ref so the scroll handler and the items effect share it without re-rendering.
  const stick = useRef(true);
  const [atBottom, setAtBottom] = useState(true);
  const [selAnchor, setSelAnchor] = useState<SelectionAnchor | null>(null);
  const [popoverOpen, setPopoverOpen] = useState(false);
  const [popoverText, setPopoverText] = useState('');
  const [popoverPosition, setPopoverPosition] = useState<{ top: number; left: number } | null>(null);
  const popoverDragOffset = useRef<{ x: number; y: number } | null>(null);
  const captureSelectionRef = useRef<() => void>(() => {});
  // Memoized so useValuePresence sees a stable identity across re-renders.
  const popoverView = useMemo(
    () => (selAnchor && popoverOpen && popoverPosition ? { anchor: selAnchor, position: popoverPosition } : null),
    [selAnchor, popoverOpen, popoverPosition],
  );
  const { rendered: shownPopover, closing: popoverClosing } = useValuePresence(popoverView);

  const items = useMemo(() => (agent ? build(agent.events, agent.pendingApprovals) : []), [agent?.events, agent?.pendingApprovals]);
  const taskList = items.find((item): item is Extract<Item, { kind: 'plan' }> => item.kind === 'plan');
  const transcriptItems = items.filter((item) => item.kind !== 'plan');
  // The actively-streaming last message row never offers annotation — its text
  // is still growing underneath any selection the user made.
  const lastMessageItem = useMemo(
    () => [...transcriptItems].reverse().find((it): it is Extract<Item, { kind: 'message' }> => it.kind === 'message'),
    [transcriptItems],
  );
  // Rows animate in only when they arrive while the transcript is already on
  // screen; the first render for an agent (initial load, replay, or switching
  // agents) seeds the set silently so history does not stampede in.
  const seenRows = useRef<{ agentId: string; keys: Set<string> }>({ agentId: '', keys: new Set() });
  const seededForAgent = seenRows.current.agentId === (agent?.id ?? '');
  const isNewRow = (key: string) => seededForAgent && !seenRows.current.keys.has(key);
  useLayoutEffect(() => {
    const seen = seenRows.current;
    const id = agent?.id ?? '';
    if (seen.agentId !== id) {
      seen.agentId = id;
      seen.keys = new Set();
    }
    for (const it of transcriptItems) seen.keys.add(it.key);
  });

  // The playlist is every message row with audio, in transcript order: rows
  // the daemon has confirmed cached/ready, plus ones rendered locally this
  // session via "Listen" (engine.renderClip) that the daemon snapshot hasn't
  // caught up to yet. Subscribing to the engine here (rather than just
  // reading it once) lets a fresh "Listen" render extend the playlist without
  // a page reload.
  const engineAudio = useEngineState();
  const audioPlaylist = useMemo(() => {
    if (!agent) return [];
    const renderedSet = new Set(engineAudio.agentId === agent.id ? engineAudio.renderedSeqs : getRenderedSeqs(agent.id));
    const seqs: number[] = [];
    for (const it of items) {
      if (it.kind !== 'message') continue;
      const hasAudio = agent.audioReadySeqs.includes(it.seq) || (agent.audioState === 'ready' && agent.audioSeq === it.seq) || renderedSet.has(it.seq);
      if (hasAudio) seqs.push(it.seq);
    }
    return seqs;
  }, [agent, items, engineAudio]);
  useEffect(() => {
    if (agent) setPlaylist(agent.id, audioPlaylist);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agent?.id, audioPlaylist]);
  // Position restore: paint instantly from localStorage on focus (declared
  // after the setPlaylist effect above so the engine already has this
  // chat's playlist active — see engine.ts's applyRestoredPosition), then
  // reconcile against the daemon-durable value once the snapshot has it.
  // Depends on the playlist's *length*, not its identity: audioPlaylist is a
  // fresh array every time the engine's own state changes (useEngineState
  // above), and restoring calls back into the engine — depending on the
  // array itself would re-run this effect on every engine tick and loop.
  const audioPlaylistLength = audioPlaylist.length;
  useEffect(() => {
    if (agent && audioPlaylistLength > 0) restoreLocalPosition(agent.id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agent?.id, audioPlaylistLength]);
  useEffect(() => {
    if (agent) reconcileDaemonPosition(agent.id, agent.audioPosition);
  }, [agent?.id, agent?.audioPosition]);
  useLiveAudioAutoplay(agent?.id ?? '', agent?.audioSeq ?? null, agent?.audioReadyRevision ?? 0);
  // Which annotation anchors still resolve to a rendered row (vs. "context
  // unavailable" — compaction, etc.).
  const knownSeqs = useMemo(() => {
    const set = new Set<number>();
    for (const it of transcriptItems) {
      if (it.kind === 'user' || it.kind === 'message' || it.kind === 'thought') set.add(it.seq);
    }
    return set;
  }, [transcriptItems]);
  const quoteLinksBySeq = useMemo(() => {
    const links = new Map<number, QuoteLink[]>();
    const add = (seq: number, link: QuoteLink) => links.set(seq, [...(links.get(seq) ?? []), link]);
    for (const annotation of annotations) add(annotation.seq, { quote: annotation.quote, targetId: annotationTargetId(annotation.id) });
    for (const item of transcriptItems) {
      if (item.kind !== 'user') continue;
      item.blocks.forEach((block, index) => {
        if (block.type === 'quote') add(block.refSeq, { quote: block.quote, targetId: citationTargetId(item.seq, index) });
      });
    }
    return links;
  }, [annotations, transcriptItems]);

  const scrollToBottom = () => {
    const el = scrollRef.current;
    if (!el) return;
    el.scrollTop = el.scrollHeight;
    stick.current = true;
    setAtBottom(true);
  };

  // On mount and whenever the focused agent changes, open to the latest message.
  const agentId = agent?.id;
  useLayoutEffect(() => {
    stick.current = true;
    scrollToBottom();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentId]);

  // As new items stream in, keep the tail pinned only while sticking.
  useEffect(() => {
    if (stick.current) scrollToBottom();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [items]);

  const onScroll = () => {
    const el = scrollRef.current;
    if (!el) return;
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 120;
    stick.current = nearBottom;
    setAtBottom(nearBottom);
  };

  const clearSelectionUi = () => {
    setSelAnchor(null);
    setPopoverOpen(false);
    setPopoverText('');
    setPopoverPosition(null);
    popoverDragOffset.current = null;
  };

  const flash = (el: HTMLElement, className: string, duration = 1300) => {
    el.classList.remove(className);
    // Restart the animation when a user revisits the same link before its
    // previous pulse has finished.
    void el.offsetWidth;
    el.classList.add(className);
    window.setTimeout(() => el.classList.remove(className), duration);
  };

  // Scroll to the precise quoted region where it is available. Older saved
  // annotations and unusual rendered markdown can lack an exact DOM match; in
  // that case retain the useful row-level fallback.
  const jumpToQuote = (seq: number, targetId: string): boolean => {
    const el = scrollRef.current?.querySelector(`[data-seq="${seq}"]`) as HTMLElement | null;
    if (!el) return false;
    const highlight = Array.from(el.querySelectorAll<HTMLElement>('.annotation-quote-highlight'))
      .find((mark) => mark.dataset.annotationTargets?.split('|').includes(targetId));
    const destination = highlight ?? el;
    destination.scrollIntoView({ behavior: 'smooth', block: 'center' });
    flash(destination, highlight ? 'annotation-quote-flash' : 'annotation-flash', 1700);
    return true;
  };

  const jumpToLinkedBlock = (targetId: string) => {
    const target = document.getElementById(targetId);
    if (!target) return;
    target.scrollIntoView({ behavior: 'smooth', block: 'center' });
    flash(target, 'annotation-link-flash', 1700);
  };

  // Native long-press selection is not tied reliably to a touchend event. iOS
  // may omit it while its edit menu owns the gesture, and both iOS and Android
  // can update the selection after touchend as the handles move. The document
  // selectionchange listener below is therefore the primary mobile path;
  // mouseup/touchend remain useful fast-paths and compatibility fallbacks.
  const captureSelection = () => {
    // Focusing the comment textarea collapses the native selection. Keep the
    // already-captured anchor while the popover is being used.
    if (popoverOpen) return;
    const sel = window.getSelection();
    if (!sel || sel.isCollapsed || sel.rangeCount === 0) {
      clearSelectionUi();
      return;
    }
    const text = sel.toString().trim();
    if (!text) {
      clearSelectionUi();
      return;
    }
    const rowEl = closestRow(sel.anchorNode);
    if (!rowEl || !rowEl.dataset.seq || !scrollRef.current?.contains(rowEl)) {
      clearSelectionUi();
      return;
    }
    const seq = Number(rowEl.dataset.seq);
    const role = rowEl.dataset.role ?? 'assistant';
    // Never offer annotation on the row still streaming in.
    if (agent?.status === 'working' && lastMessageItem && rowEl.dataset.key === lastMessageItem.key) {
      clearSelectionUi();
      return;
    }
    const rect = sel.getRangeAt(0).getBoundingClientRect();
    if (rect.width === 0 && rect.height === 0) {
      clearSelectionUi();
      return;
    }
    setSelAnchor({
      seq,
      role,
      quote: text.length > ANNOTATION_QUOTE_MAX ? text.slice(0, ANNOTATION_QUOTE_MAX) : text,
      rect: { top: rect.top, left: rect.left, width: rect.width, height: rect.height },
    });
    setPopoverOpen(false);
    setPopoverText('');
  };
  captureSelectionRef.current = captureSelection;

  useEffect(() => {
    let timer: number | undefined;
    const onSelectionChange = () => {
      window.clearTimeout(timer);
      // Let WebKit/Chromium finish laying out the native selection before
      // asking its Range for geometry.
      timer = window.setTimeout(() => captureSelectionRef.current(), 40);
    };
    document.addEventListener('selectionchange', onSelectionChange);
    return () => {
      window.clearTimeout(timer);
      document.removeEventListener('selectionchange', onSelectionChange);
    };
  }, []);

  // Dismiss the floating button/popover on any click outside them (including
  // the start of a fresh selection drag).
  useEffect(() => {
    if (!selAnchor) return;
    const onDocPointerDown = (e: PointerEvent) => {
      const target = e.target as HTMLElement;
      if (target.closest('.annotation-comment-btn') || target.closest('.annotation-popover')) return;
      clearSelectionUi();
    };
    document.addEventListener('pointerdown', onDocPointerDown);
    return () => document.removeEventListener('pointerdown', onDocPointerDown);
  }, [selAnchor]);

  if (!agent) return null;

  return (
    <div className="pane">
      <div className="transcript-wrap">
        <div className="transcript-history">
          <div
            className="transcript"
            ref={scrollRef}
            onScroll={onScroll}
            onMouseUp={captureSelection}
            onTouchEnd={() => window.setTimeout(() => captureSelectionRef.current(), 80)}
          >
            {transcriptItems.length === 0 && <div className="empty">No activity yet. Send a prompt below to start a turn.</div>}
            {transcriptItems.map((it) => (
              <Row
                key={`${agent.id}:${it.key}`}
                item={it}
                entering={isNewRow(it.key)}
                commands={agent.commands}
                quoteLinks={it.kind === 'user' || it.kind === 'message' || it.kind === 'thought' ? quoteLinksBySeq.get(it.seq) ?? [] : []}
                onRespond={(opt) => it.kind === 'permission' && respond(agent.id, it.reqId, opt)}
                onJumpToQuote={jumpToQuote}
                onJumpToLinkedBlock={jumpToLinkedBlock}
                agentId={agent.id}
                canRenderAudio={it.kind === 'message' && !(agent.status === 'working' && it.key === lastMessageItem?.key)}
                cachedAudio={it.kind === 'message' && (agent.audioReadySeqs.includes(it.seq) || (agent.audioState === 'ready' && agent.audioSeq === it.seq))}
                renderingAudio={it.kind === 'message' && agent.audioState === 'rendering' && agent.audioSeq === it.seq}
              />
            ))}
          </div>
          {!atBottom && (
            <button className="scroll-latest" onClick={scrollToBottom} title="Scroll to latest">
              ↓ Latest
            </button>
          )}
          {selAnchor && !popoverOpen && (
            <button
              type="button"
              className="annotation-comment-btn"
              style={{
                top: usesSoftKeyboard() ? selAnchor.rect.top + selAnchor.rect.height + 10 : selAnchor.rect.top - 34,
                left: selAnchor.rect.left + selAnchor.rect.width / 2,
              }}
              onClick={() => {
                setPopoverPosition(usesSoftKeyboard()
                  ? mobilePopoverPosition()
                  : { top: selAnchor.rect.top - 34, left: selAnchor.rect.left });
                setPopoverOpen(true);
              }}
            >
              💬 Comment
            </button>
          )}
          {shownPopover && (
            <div className={`annotation-popover${popoverClosing ? ' closing' : ''}`} style={shownPopover.position}>
              <div
                className="annotation-popover-quote annotation-popover-drag-handle"
                title="Drag to move comment"
                onPointerDown={(e) => {
                  popoverDragOffset.current = { x: e.clientX - shownPopover.position.left, y: e.clientY - shownPopover.position.top };
                  e.currentTarget.setPointerCapture(e.pointerId);
                  e.preventDefault();
                }}
                onPointerMove={(e) => {
                  const offset = popoverDragOffset.current;
                  if (!offset) return;
                  const popover = e.currentTarget.parentElement;
                  const viewport = visibleViewport();
                  const width = popover?.offsetWidth ?? 260;
                  const height = popover?.offsetHeight ?? 160;
                  const gutter = 8;
                  setPopoverPosition({
                    top: Math.min(
                      Math.max(e.clientY - offset.y, viewport.top + gutter),
                      Math.max(viewport.top + gutter, viewport.top + viewport.height - height - gutter),
                    ),
                    left: Math.min(
                      Math.max(e.clientX - offset.x, viewport.left + gutter),
                      Math.max(viewport.left + gutter, viewport.left + viewport.width - width - gutter),
                    ),
                  });
                }}
                onPointerUp={(e) => {
                  popoverDragOffset.current = null;
                  if (e.currentTarget.hasPointerCapture(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId);
                }}
                onPointerCancel={() => { popoverDragOffset.current = null; }}
              >
                &ldquo;{previewText(shownPopover.anchor.quote, 160)}&rdquo;
              </div>
              <textarea
                autoFocus
                rows={2}
                placeholder="Add a comment…"
                value={popoverText}
                onChange={(e) => setPopoverText(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Escape') {
                    e.preventDefault();
                    clearSelectionUi();
                  } else if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault();
                    void addAnnotation(agent.id, { seq: shownPopover.anchor.seq, role: shownPopover.anchor.role, quote: shownPopover.anchor.quote }, popoverText.trim());
                    window.getSelection()?.removeAllRanges();
                    clearSelectionUi();
                  }
                }}
              />
              <div className="annotation-popover-actions">
                <button type="button" onClick={clearSelectionUi}>Cancel</button>
                <button
                  type="button"
                  className="btn primary"
                  onClick={() => {
                    void addAnnotation(agent.id, { seq: shownPopover.anchor.seq, role: shownPopover.anchor.role, quote: shownPopover.anchor.quote }, popoverText.trim());
                    window.getSelection()?.removeAllRanges();
                    clearSelectionUi();
                  }}
                >
                  Add
                </button>
              </div>
            </div>
          )}
        </div>
        {annotations.length > 0 && (
          <AnnotationTray
            agentId={agent.id}
            annotations={annotations}
            knownSeqs={knownSeqs}
            onJump={jumpToQuote}
            onUpdate={updateAnnotation}
            onRemove={removeAnnotation}
          />
        )}
        {taskList && <TaskList item={taskList} />}
      </div>
      {audioPlaylist.length > 0 && <GlobalAudioPlayer agentId={agent.id} />}
      <PromptBar agentId={agent.id} working={agent.status === 'working' || agent.status === 'blocked'} />
      <SessionConfigBar agentId={agent.id} sessionConfig={agent.sessionConfig} usage={agent.usage} />
    </div>
  );
}

// A focused chat receives live ready events in message order. The engine's
// playlist already advances through completed clips sequentially on `ended`
// (see setPlaylist/activateSection in ui/src/audio/engine.ts), so this only
// needs to kick off playback for a freshly-arrived clip when nothing is
// already mid-playback — anything already playing will reach the new clip on
// its own once it's appended to the playlist.
//
// `revision` only advances on a live audio_state ready event (store.ts), not
// on snapshot replay/backfill — that's what keeps a reconnect or chat switch
// from re-triggering autoplay for old history. `seen` additionally guards
// against a duplicate revision bump for the same seq.
function useLiveAudioAutoplay(agentId: string, seq: number | null, revision: number) {
  const baseline = useRef({ agentId, revision });
  const seen = useRef(new Set<number>());

  useEffect(() => {
    if (baseline.current.agentId !== agentId) {
      baseline.current = { agentId, revision };
      seen.current = new Set();
      return;
    }
    if (baseline.current.revision === revision) return;
    baseline.current.revision = revision;
    if (seq === null || seen.current.has(seq)) return;
    seen.current.add(seq);
    if (getState().status === 'playing') return;
    play(agentId, seq);
  }, [agentId, revision, seq]);
}

// The pending-annotation review tray, rendered above PromptBar (visual
// precedent: the queued-prompts tray in PromptBar below). Annotations are
// daemon-owned; edits/removes just send the WS message and wait for the
// `annotations` broadcast to update this list.
function AnnotationTray({
  agentId,
  annotations,
  knownSeqs,
  onJump,
  onUpdate,
  onRemove,
}: {
  agentId: string;
  annotations: Annotation[];
  knownSeqs: Set<number>;
  onJump: (seq: number, targetId: string) => boolean;
  onUpdate: (agentId: string, id: string, comment: string) => Promise<AckResult>;
  onRemove: (agentId: string, id: string) => Promise<AckResult>;
}) {
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editText, setEditText] = useState('');

  const startEdit = (a: Annotation) => {
    setEditingId(a.id);
    setEditText(a.comment);
  };
  const saveEdit = async () => {
    if (!editingId) return;
    await onUpdate(agentId, editingId, editText.trim());
    setEditingId(null);
    setEditText('');
  };

  return (
    <div className="annotation-tray">
      <div className="annotation-tray-header">
        <span>Annotations ({annotations.length})</span>
      </div>
      {annotations.map((a) => {
        const available = knownSeqs.has(a.seq);
        const editing = editingId === a.id;
        return (
          <div className="annotation-row" id={annotationTargetId(a.id)} key={a.id}>
            <div
              className={`annotation-snippet${available ? '' : ' unavailable'}`}
              onClick={() => available && onJump(a.seq, annotationTargetId(a.id))}
              title={available ? 'Jump to source' : 'Context unavailable'}
            >
              <span className={`annotation-quote${available ? '' : ' unavailable'}`}>
                {available ? <>&ldquo;{previewText(a.quote, 90)}&rdquo;</> : 'context unavailable'}
              </span>
              {!editing && a.comment && <span className="annotation-comment">{a.comment}</span>}
            </div>
            {editing ? (
              <div className="annotation-edit">
                <textarea rows={2} autoFocus value={editText} onChange={(e) => setEditText(e.target.value)} />
                <button type="button" onClick={() => void saveEdit()}>Save</button>
                <button type="button" onClick={() => setEditingId(null)}>Cancel</button>
              </div>
            ) : (
              <div className="annotation-actions">
                <button type="button" onClick={() => startEdit(a)} title="Edit comment" aria-label="Edit annotation">Edit</button>
                <button type="button" onClick={() => void onRemove(agentId, a.id)} title="Remove annotation" aria-label="Remove annotation">×</button>
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

// renderMarkdown wraps fenced code blocks with a `[data-copy-btn]` button;
// since that HTML is injected via innerHTML we can't attach a React handler
// to the button itself, so we delegate from the containing message div.
function handleCodeCopyClick(e: React.MouseEvent<HTMLDivElement>) {
  const target = e.target as HTMLElement;
  const btn = target.closest('[data-copy-btn]') as HTMLButtonElement | null;
  if (!btn) return;
  const code = btn.parentElement?.querySelector('pre code');
  const text = code?.textContent ?? '';
  navigator.clipboard.writeText(text).then(() => {
    const prevLabel = btn.textContent;
    btn.textContent = 'Copied!';
    btn.classList.add('copied');
    window.setTimeout(() => {
      btn.textContent = prevLabel;
      btn.classList.remove('copied');
    }, 1500);
  });
}

// Truncates for compact display (citation chips, tray snippets) — the full
// text is still sent/stored; this is purely a rendering affordance.
function previewText(text: string, max: number): string {
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

// A `quote` prompt block rendered as a citation chip in a sent user message —
// click to scroll to (and flash) the row it references.
function QuoteChip({
  block,
  targetId,
  onJump,
}: {
  block: Extract<PromptBlock, { type: 'quote' }>;
  targetId: string;
  onJump: (seq: number, targetId: string) => boolean;
}) {
  return (
    <div id={targetId} className="citation-chip" onClick={() => onJump(block.refSeq, targetId)} title={`Jump to the referenced ${block.role} quote`}>
      <div className="citation-chip-quote">&ldquo;{previewText(block.quote, 160)}&rdquo;</div>
      {block.comment && <div className="citation-chip-comment">{block.comment}</div>}
    </div>
  );
}

function Row({
  item,
  entering,
  commands,
  agentId,
  canRenderAudio,
  cachedAudio,
  renderingAudio,
  quoteLinks,
  onRespond,
  onJumpToQuote,
  onJumpToLinkedBlock,
}: {
  item: Item;
  entering: boolean;
  commands: SlashCommand[];
  agentId: string;
  canRenderAudio: boolean;
  cachedAudio: boolean;
  renderingAudio: boolean;
  quoteLinks: QuoteLink[];
  onRespond: (optionId: string) => void;
  onJumpToQuote: (seq: number, targetId: string) => boolean;
  onJumpToLinkedBlock: (targetId: string) => void;
}) {
  const sourceRef = useRef<HTMLElement>(null);
  // Latched at mount so re-renders while the row streams cannot restart (or cut
  // short) the entrance animation, then dropped once it has played -- leaving it
  // on would collide with the update-flash animation on the same element.
  const [enterClass, setEnterClass] = useState(entering ? ' ev-enter' : '');
  useEffect(() => {
    if (!enterClass) return;
    const timer = window.setTimeout(() => setEnterClass(''), 250);
    return () => window.clearTimeout(timer);
  }, [enterClass]);
  // A user prompt can change after it is first rendered (an image attachment
  // finishing upload, a quote resolving); flash so the edit is noticeable.
  const userFlash = useUpdateFlash(
    item.kind === 'user' ? item.blocks.map((b) => (b.type === 'text' ? b.text : b.type)).join('\u0000') : null,
  );
  useLayoutEffect(() => {
    if (sourceRef.current) applyQuoteHighlights(sourceRef.current, quoteLinks);
  }, [quoteLinks, item]);

  const onSourceClick = (event: React.MouseEvent<HTMLElement>) => {
    const highlight = (event.target as HTMLElement).closest<HTMLElement>('.annotation-quote-highlight');
    const targetId = highlight?.dataset.annotationTargets?.split('|')[0];
    if (!targetId) return;
    event.preventDefault();
    event.stopPropagation();
    onJumpToLinkedBlock(targetId);
  };

  switch (item.kind) {
    case 'user':
      return (
        <div ref={sourceRef as React.RefObject<HTMLDivElement>} className={`ev user${enterClass}${userFlash ? ` ${userFlash}` : ''}`} data-seq={item.seq} data-role="user" data-key={item.key} onClick={onSourceClick}>
          {item.blocks.map((block, i) => {
            if (block.type === 'text') return <div key={i}><SkillText text={block.text} commands={commands} /></div>;
            if (block.type === 'image') return <TranscriptImage key={`${block.assetId}-${i}`} block={block} />;
            return <QuoteChip key={i} block={block} targetId={citationTargetId(item.seq, i)} onJump={onJumpToQuote} />;
          })}
        </div>
      );
    case 'message':
      return (
        <div
          className="ev msg"
          ref={sourceRef as React.RefObject<HTMLDivElement>}
          data-seq={item.seq}
          data-role="assistant"
          data-key={item.key}
          onClick={(event) => {
            if ((event.target as HTMLElement).closest('.annotation-quote-highlight')) onSourceClick(event);
            else handleCodeCopyClick(event);
          }}
        >
          <div className="message-content" dangerouslySetInnerHTML={{ __html: renderMarkdown(item.text) }} />
          <MessageAudio agentId={agentId} seq={item.seq} enabled={canRenderAudio} cachedAudio={cachedAudio} renderingAudio={renderingAudio} />
        </div>
      );
    case 'thought':
      return (
        <details ref={sourceRef as React.RefObject<HTMLDetailsElement>} className="ev thought" data-seq={item.seq} data-role="thought" data-key={item.key} onClick={onSourceClick}>
          <summary>thinking…</summary>
          {item.text}
        </details>
      );
    case 'tool':
      return <ToolCard item={item} enterClass={enterClass} />;
    case 'plan':
      return <TaskList item={item} />;
    case 'terminal':
      return (
        <div className="mini-term">
          <div className="th">▌ terminal {item.termId}{item.truncated ? ' (truncated)' : ''}</div>
          {item.text}
        </div>
      );
    case 'permission':
      return (
        <div className="inline-approval">
          <div className="permission-request-label">Permission request</div>
          <PermissionRequestDetails title={item.title} />
          <div className="acts permission-request-actions">
            {item.options.map((o) => (
              <button key={o.optionId} className={/reject|deny|no/i.test(o.name) ? 'btn-deny' : 'btn-approve'} onClick={() => onRespond(o.optionId)}>
                {o.name}
              </button>
            ))}
          </div>
        </div>
      );
    case 'error':
      return <div className="err-banner">⛔ {item.message}</div>;
    case 'aside':
      return (
        <section className="aside-card" aria-label="Context-isolated aside">
          <div className="aside-label">Aside · excluded from future turns</div>
          <div className="aside-question"><SkillText text={item.question} commands={commands} /></div>
          {item.thought && <details className="aside-thought"><summary>thinking…</summary>{item.thought}</details>}
          {item.answer && <div className="aside-answer" dangerouslySetInnerHTML={{ __html: renderMarkdown(item.answer) }} onClick={handleCodeCopyClick} />}
          {!item.complete && !item.answer && <div className="aside-pending">Asking in existing context…</div>}
          {item.error && <div className="aside-error">{item.error}</div>}
        </section>
      );
  }
}

// Rows are pure presentation: no media element, no local playback state.
// `cachedAudio`/`renderingAudio` come from the daemon-owned audio state
// (store.ts); this component's own state is just "have I fetched/kicked off
// a clip for this seq" bookkeeping, delegated to the shared engine
// (ui/src/audio/engine.ts) for everything about actually playing it.
function MessageAudio({ agentId, seq, enabled, cachedAudio, renderingAudio }: { agentId: string; seq: number; enabled: boolean; cachedAudio: boolean; renderingAudio: boolean }) {
  const [prefetching, setPrefetching] = useState(false);
  const [prefetched, setPrefetched] = useState(false);
  const [pending, setPending] = useState(false);
  const [manuallyRendered, setManuallyRendered] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // A cached clip is daemon-confirmed ready; prefetch its bytes eagerly
  // (batched newest-first by the engine) so the InlineAudioBar it renders can
  // start playback without a fetch delay the first time it's tapped.
  useEffect(() => {
    if (!cachedAudio || prefetched || prefetching || error) return;
    setPrefetching(true);
    void prefetchClip(agentId, seq)
      .then(() => setPrefetched(true))
      .catch((cause: unknown) => setError(cause instanceof Error ? cause.message : String(cause)))
      .finally(() => setPrefetching(false));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cachedAudio]);

  const onListen = async () => {
    setPending(true);
    setError(null);
    try {
      await renderClip(agentId, seq);
      setManuallyRendered(true);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setPending(false);
    }
  };

  const ready = prefetched || manuallyRendered;
  // A cached clip is known to become a player, so render its full in-flow
  // placeholder immediately. This keeps its row at the final player height
  // while the browser fetches the retained daemon bytes.
  const playerPending = renderingAudio || pending || prefetching || (cachedAudio && !ready && !error);

  return (
    <div className={`message-audio${ready || playerPending ? '' : ' message-listen'}`}>
      {ready ? <InlineAudioBar agentId={agentId} seq={seq} /> : playerPending ? (
        <div className="message-audio-placeholder" role="status">{renderingAudio || pending ? 'Rendering speech…' : 'Loading speech…'}</div>
      ) : (
        <button type="button" onClick={() => void onListen()} disabled={!enabled || pending} title={enabled ? 'Render this response as speech' : 'Available when the response is complete'}>
          Listen
        </button>
      )}
      {error && <span className="message-audio-error" role="alert">{error}</span>}
    </div>
  );
}

function TaskList({ item }: { item: Extract<Item, { kind: 'plan' }> }) {
  const [expanded, setExpanded] = useState(true);
  const done = item.entries.filter((entry) => entry.status === 'done').length;
  return (
    <details
      className="task-list card"
      open={expanded}
      onToggle={(event) => setExpanded(event.currentTarget.open)}
    >
      <summary className="card-head">
        <span className="task-list-chevron" aria-hidden="true">›</span>
        <span className="title">Task list</span>
        <span className="task-list-progress">{done}/{item.entries.length}</span>
      </summary>
      <ul className="plan">
        {item.entries.map((entry, index) => (
          <li key={index} className={entry.status}>
            <span className="mark">{entry.status === 'done' ? '✓' : entry.status === 'in_progress' ? '▸' : '○'}</span>
            <span className={entry.status}>{entry.label}</span>
          </li>
        ))}
      </ul>
    </details>
  );
}

function assetUrl(agentId: string, assetId: string): string {
  return `/api/agents/${encodeURIComponent(agentId)}/assets/${encodeURIComponent(assetId)}`;
}

function TranscriptImage({ block }: { block: Extract<PromptBlock, { type: 'image' }> }) {
  const agentId = useStore((s) => s.focusedId);
  const [src, setSrc] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    if (!agentId) return;
    const controller = new AbortController();
    let objectUrl: string | null = null;
    void fetch(assetUrl(agentId, block.assetId), {
      headers: storedToken() ? { Authorization: `Bearer ${storedToken()}` } : {},
      signal: controller.signal,
    })
      .then((response) => {
        if (!response.ok) throw new Error(`asset fetch failed (${response.status})`);
        return response.blob();
      })
      .then((blob) => {
        objectUrl = URL.createObjectURL(blob);
        setSrc(objectUrl);
      })
      .catch((error: unknown) => {
        if (!(error instanceof DOMException && error.name === 'AbortError')) setFailed(true);
      });
    return () => {
      controller.abort();
      if (objectUrl) URL.revokeObjectURL(objectUrl);
    };
  }, [agentId, block.assetId]);

  if (failed) return <div className="transcript-image-failed">Image unavailable: {block.name ?? 'attachment'}</div>;
  if (!src) return <div className="transcript-image-loading">Loading {block.name ?? 'image'}…</div>;
  return <img className="transcript-image" src={src} alt={block.name ?? 'Uploaded image'} />;
}

// ACP tool_call content is an array of ToolCallContent blocks — most are
// `{ type: 'content', content: { type: 'text', text } }`, some are
// `{ type: 'diff', path, oldText, newText }`. Flatten to plain text for
// display rather than dumping the raw JSON envelope.
type ToolImage =
  | ({ type: 'image' } & ImageAssetRef)
  | { type: 'image'; data: string; mimeType: string; name?: string };

type ToolDiff = { path: string; oldText: string; newText: string };

function parseToolContent(content: unknown): { text: string | null; images: ToolImage[]; diffs: ToolDiff[] } {
  if (content == null) return { text: null, images: [], diffs: [] };
  if (typeof content === 'string') return { text: content || null, images: [], diffs: [] };
  if (Array.isArray(content)) {
    const images: ToolImage[] = [];
    const diffs: ToolDiff[] = [];
    const parts = content
      .map((block: any) => {
        if (block?.type === 'content' && block.content?.type === 'text') return block.content.text ?? '';
        if (block?.type === 'content' && block.content?.type === 'image' && block.content.assetId && block.content.mimeType) {
          images.push({ type: 'image', assetId: block.content.assetId, mimeType: block.content.mimeType, name: block.content.name });
          return null;
        }
        // Inline data only exists in transcripts written before tool images
        // were asset-backed. Render it for replay compatibility; new events
        // are normalized by the daemon before persistence.
        if (block?.type === 'content' && block.content?.type === 'image' && block.content.data && block.content.mimeType) {
          images.push({ type: 'image', data: block.content.data, mimeType: block.content.mimeType, name: block.content.name });
          return null;
        }
        // Older events may still contain uncaptured local resource links. Keep
        // them legible even though browsers cannot load local paths directly.
        if (block?.type === 'content' && block.content?.type === 'resource_link') return block.content.name ?? block.content.uri ?? null;
        if (block?.type === 'diff' && typeof block.path === 'string') {
          diffs.push({ path: block.path, oldText: typeof block.oldText === 'string' ? block.oldText : '', newText: typeof block.newText === 'string' ? block.newText : '' });
        }
        return null;
      })
      .filter((s): s is string => !!s);
    return { text: parts.length ? parts.join('\n') : null, images, diffs };
  }
  return { text: JSON.stringify(content, null, 2), images: [], diffs: [] };
}

// ACP diff content holds the complete before/after text instead of a unified
// patch. Synthesize one so tool results use the same readable renderer as the
// workspace Diff tab.
function toolDiffPatch({ path, oldText, newText }: ToolDiff): string {
  return createUnifiedPatch(path, oldText, newText);
}

// rawInput is the ACP tool_call's arguments (e.g. { path, content } for a
// write) — kept separate from `content`, which is the tool's *output*. Shown
// as pretty-printed JSON since it's a structured params object, not prose.
function formatArgs(rawInput: unknown): string | null {
  if (rawInput == null) return null;
  if (typeof rawInput === 'string') return rawInput || null;
  try {
    const s = JSON.stringify(rawInput, null, 2);
    return s === '{}' ? null : s;
  } catch {
    return null;
  }
}

// A shell command run via the 'execute' tool kind carries its command text in
// rawInput.command (structured) — fall back to the title, which agents that
// skip rawInput (e.g. pi's generic tool name) or a bare literal title don't
// give us a struct for.
function terminalCommand(item: Extract<Item, { kind: 'tool' }>): string | null {
  const input = item.rawInput;
  if (input && typeof input === 'object' && 'command' in input) {
    const cmd = (input as { command?: unknown }).command;
    if (typeof cmd === 'string' && cmd) return cmd;
  }
  if (item.title && item.title !== 'Terminal') return item.title;
  return null;
}

function ToolCard({ item, enterClass }: { item: Extract<Item, { kind: 'tool' }>; enterClass: string }) {
  const [open, setOpen] = useState(false);
  // Flash on status transitions (pending -> in_progress -> completed/failed)
  // rather than on every streamed output chunk, which would strobe.
  const statusFlash = useUpdateFlash(item.status);
  const { text: body, images, diffs } = parseToolContent(item.content);
  const args = formatArgs(item.rawInput);
  const isExecute = item.toolKind === 'execute';
  const command = isExecute ? terminalCommand(item) : null;
  // For a plain execute call, the command *is* the args — showing it again as
  // a raw JSON "Arguments" blob under a terminal prompt line is noise.
  const showArgs = args != null && !(isExecute && command != null);
  const hasBody = body != null || showArgs || images.length > 0 || diffs.length > 0 || command != null;
  useEffect(() => {
    if (images.length > 0) setOpen(true);
  }, [images.length]);
  return (
    <div className={`card${enterClass}${statusFlash ? ` ${statusFlash}` : ''}`}>
      <div className={`card-head${open ? ' open' : ''}`} onClick={() => hasBody && setOpen((o) => !o)}>
        <span>{hasBody ? (open ? '▾' : '▸') : '⚙'}</span>
        {command != null ? (
          <span className="title tool-cmd-title">
            <span className="tool-prompt">$</span> {command}
          </span>
        ) : (
          <span className="title">{item.title}</span>
        )}
        <span className={`chip ${item.status}`}>{item.status}</span>
      </div>
      {open && hasBody && (
        <div className="card-body">
          {command != null ? (
            <>
              <ToolText label="Command" className="tool-terminal-command">{command}</ToolText>
              {body != null ? (
                <ToolText label="Output" className="tool-terminal-output">{body}</ToolText>
              ) : (
                <div className="tool-terminal-empty">(no output)</div>
              )}
            </>
          ) : (
            <>
              <ToolText label="Tool" className="tool-call-title">{item.title}</ToolText>
              {diffs.map((diff, index) => <UnifiedDiff key={`${diff.path}-${index}`} patch={toolDiffPatch(diff)} />)}
              {body != null && <ToolText label="Output" className="tool-output">{body}</ToolText>}
            </>
          )}
          {showArgs && (
            <div className="tool-args">
              <div className="tool-args-label">Arguments</div>
              <pre>{args}</pre>
            </div>
          )}
          {images.map((image, index) => <ToolResultImage key={`${'assetId' in image ? image.assetId : index}-${index}`} image={image} />)}
        </div>
      )}
    </div>
  );
}

// Headers are deliberately compact, but opening a tool call must expose every
// byte of the command, input, and output without relying on a hover tooltip or
// a fixed-height inner scroller.
function ToolText({ label, className, children }: { label: string; className: string; children: string }) {
  return (
    <div className="tool-text-section">
      <div className="tool-args-label">{label}</div>
      <pre className={className}>{children}</pre>
    </div>
  );
}

function ToolResultImage({ image }: { image: ToolImage }) {
  if ('assetId' in image) return <TranscriptImage block={image} />;
  return <img className="transcript-image" src={`data:${image.mimeType};base64,${image.data}`} alt={image.name ?? 'Tool result'} />;
}

// Model + Permission Mode pickers (ACP session-modes / session-config-options,
// docs/acp-notes.md). `sessionConfig` is null until the agent reports it (or
// always, for pty agents) — the bar renders nothing in that case rather than an
// empty shell.
function compactTokens(value: number): string {
  const units = [
    { value: 1_000_000_000, suffix: 'B' },
    { value: 1_000_000, suffix: 'M' },
    { value: 1_000, suffix: 'k' },
  ];
  const unit = units.find((candidate) => value >= candidate.value);
  if (!unit) return Math.round(value).toString();
  const scaled = value / unit.value;
  return `${scaled >= 100 || Number.isInteger(scaled) ? scaled.toFixed(0) : scaled.toFixed(1)}${unit.suffix}`;
}

function UsageMeter({ usage }: { usage: NonNullable<AgentView['usage']> }) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, []);
  const percentage = Math.round((usage.used / usage.size) * 100);
  const fill = Math.max(0, Math.min(100, (usage.used / usage.size) * 100));
  const ageSeconds = Math.max(0, Math.floor((now - usage.updatedAt) / 1_000));
  const relativeTime = ageSeconds < 60
    ? `${ageSeconds}s ago`
    : ageSeconds < 3_600
      ? `${Math.floor(ageSeconds / 60)}m ago`
      : ageSeconds < 86_400
        ? `${Math.floor(ageSeconds / 3_600)}h ago`
        : `${Math.floor(ageSeconds / 86_400)}d ago`;
  const label = `${compactTokens(usage.used)}/${compactTokens(usage.size)} (${percentage}%) · ${relativeTime}`;
  const title = usage.cost
    ? `${usage.used.toLocaleString()} of ${usage.size.toLocaleString()} tokens · updated ${relativeTime} · ${usage.cost.amount} ${usage.cost.currency} cumulative`
    : `${usage.used.toLocaleString()} of ${usage.size.toLocaleString()} tokens · updated ${relativeTime}`;

  return (
    <div className="usage-meter" role="meter" aria-label="Context window usage" aria-valuemin={0} aria-valuemax={usage.size} aria-valuenow={usage.used} title={title}>
      <div className="usage-meter-fill" style={{ width: `${fill}%` }} />
      <span className="usage-meter-label usage-meter-label-empty">{label}</span>
      <span className="usage-meter-label usage-meter-label-filled" style={{ clipPath: `inset(0 ${100 - fill}% 0 0)` }}>{label}</span>
    </div>
  );
}

function SessionConfigBar({ agentId, sessionConfig, usage }: { agentId: string; sessionConfig: AgentView['sessionConfig']; usage: AgentView['usage'] }) {
  const setMode = useStore((s) => s.setMode);
  const setConfigOption = useStore((s) => s.setConfigOption);
  if (!sessionConfig && !usage) return null;

  const { modes, configOptions } = sessionConfig ?? { modes: null, configOptions: [] };
  // The model selector is a config option with category 'model' (pinned id
  // "model" on the real agent, but category is the spec-sanctioned way to find
  // it). 'thought_level' is the spec's thinking-effort selector. Newer ACP
  // agents expose permission mode as a config option too; prefer that because
  // it is authoritative when the legacy parallel `modes` state is stale.
  const modelOpt = configOptions.find((o) => o.category === 'model' && o.type === 'select');
  const thoughtLevelOpt = configOptions.find((o) => o.category === 'thought_level' && o.type === 'select');
  const permissionOpt = configOptions.find((o) => o.category === 'mode' && o.type === 'select');
  const hasModes = !!modes && modes.availableModes.length > 0;
  if (!permissionOpt && !hasModes && !modelOpt && !thoughtLevelOpt && !usage) return null;

  return (
    <div className="session-config-bar">
      {modelOpt && (
        <label>
          Model
          <select className="model-select" value={String(modelOpt.currentValue)} onChange={(e) => setConfigOption(agentId, modelOpt.id, e.target.value)}>
            {(modelOpt.options ?? []).map((o) => (
              <option key={o.value} value={o.value} title={o.name}>
                {o.name}
              </option>
            ))}
          </select>
        </label>
      )}
      {thoughtLevelOpt && (
        <label>
          Thinking
          <select
            value={String(thoughtLevelOpt.currentValue)}
            onChange={(e) => setConfigOption(agentId, thoughtLevelOpt.id, e.target.value)}
          >
            {(thoughtLevelOpt.options ?? []).map((o) => (
              <option key={o.value} value={o.value}>
                {o.name}
              </option>
            ))}
          </select>
        </label>
      )}
      {permissionOpt ? (
        <label>
          Permission Mode
          <select value={String(permissionOpt.currentValue)} onChange={(e) => setConfigOption(agentId, permissionOpt.id, e.target.value)}>
            {(permissionOpt.options ?? []).map((o) => (
              <option key={o.value} value={o.value} title={o.description}>
                {o.name}
              </option>
            ))}
          </select>
        </label>
      ) : hasModes && modes ? (
        <label>
          Permission Mode
          <select value={modes.currentModeId} onChange={(e) => setMode(agentId, e.target.value)}>
            {modes.availableModes.map((m) => (
              <option key={m.id} value={m.id} title={m.description}>
                {m.name}
              </option>
            ))}
          </select>
        </label>
      ) : null}
      {usage && <UsageMeter usage={usage} />}
    </div>
  );
}

type CompletionToken = { start: number; end: number; query: string };

function isMentionBoundary(text: string, index: number): boolean {
  return index === 0 || /\s/.test(text[index - 1]);
}

// Finds a slash command only at the beginning of a message or after
// whitespace. This prevents ordinary paths such as src/foo from opening the
// command picker while still allowing slash commands on later lines.
export function findSlashToken(text: string, caret: number): CompletionToken | null {
  if (caret <= 0 || caret > text.length) return null;
  let i = caret;
  while (i > 0 && /[A-Za-z0-9_-]/.test(text[i - 1])) i--;
  if (i === 0 || text[i - 1] !== '/' || !isMentionBoundary(text, i - 1)) return null;
  const query = text.slice(i, caret);
  if (!query) return null;
  return { start: i - 1, end: caret, query };
}

// File mentions are relative to the workspace, with ../ for its immediate
// parent and ~/ for the user's home directory. A token may be empty so typing
// "@" opens the root picker, and a trailing slash lists that directory.
export function findFileToken(text: string, caret: number): CompletionToken | null {
  if (caret <= 0 || caret > text.length) return null;
  let i = caret;
  while (i > 0 && /[A-Za-z0-9_./~-]/.test(text[i - 1])) i--;
  if (i === 0 || text[i - 1] !== '@' || !isMentionBoundary(text, i - 1)) return null;
  return { start: i - 1, end: caret, query: text.slice(i, caret) };
}

function fileCompletionRequest(query: string): { dir: string; filter: string } | null {
  if (query.startsWith('/') || (query.startsWith('~') && query !== '~' && !query.startsWith('~/')) || (!query.startsWith('~/') && query !== '~' && (query.startsWith('../../') || query === '../..' || query.split('/').slice(1).some((segment) => segment === '..')))) return null;
  const slash = query.lastIndexOf('/');
  return slash < 0
    ? { dir: '', filter: query }
    : { dir: query.slice(0, slash) || '.', filter: query.slice(slash + 1) };
}

function SkillText({ text, commands, asideSupport = false }: { text: string; commands: SlashCommand[]; asideSupport?: boolean | null }) {
  const names = new Set(commands.map((command) => command.name));
  const parts: React.ReactNode[] = [];
  // Keep the highlight layer in sync with the two kinds of composer tokens:
  // known slash commands and workspace file mentions.
  const pattern = /\/[A-Za-z0-9_-]+|@[A-Za-z0-9_./~-]+/g;
  let previous = 0;
  for (let match = pattern.exec(text); match; match = pattern.exec(text)) {
    const start = match.index;
    const token = match[0];
    const isCommand = token[0] === '/';
    const isKnownCommand = isCommand && names.has(token.slice(1));
    // `/btw` is a transport-level aside, rather than an agent-provided slash
    // command. It is only special at the start of a prompt (apart from leading
    // whitespace), matching the send path below exactly.
    const isAside = asideSupport === true
      && text.slice(0, start).trim() === ''
      && /^\/btw$/i.test(token)
      && (start + token.length === text.length || /\s/.test(text[start + token.length]));
    const isFileMention = token[0] === '@';
    if (!isMentionBoundary(text, start) || (!isKnownCommand && !isAside && !isFileMention)) continue;
    if (start > previous) parts.push(text.slice(previous, start));
    parts.push(<span className={isAside ? 'aside-mention' : 'skill-mention'} key={start}>{token}</span>);
    previous = start + token.length;
  }
  if (previous < text.length) parts.push(text.slice(previous));
  return <>{parts}</>;
}

const ACCEPTED_IMAGE_TYPES = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp']);
const MAX_IMAGE_BYTES = 10 * 1024 * 1024;
const MAX_TURN_IMAGE_BYTES = 20 * 1024 * 1024;
const MAX_IMAGES = 4;

type DraftAttachment = {
  localId: string;
  file: File;
  previewUrl?: string;
  status: 'uploading' | 'ready' | 'error';
  uploadProgress?: number;
  asset?: Extract<PromptBlock, { type: 'image' }>;
  uploadPath?: string;
  error?: string;
};

function PromptBar({ agentId, working }: { agentId: string; working: boolean }) {
  const prompt = useStore((s) => s.prompt);
  const steer = useStore((s) => s.steer);
  const aside = useStore((s) => s.aside);
  const interrupt = useStore((s) => s.interrupt);
  const removeQueuedPrompt = useStore((s) => s.removeQueuedPrompt);
  const clearPromptQueue = useStore((s) => s.clearPromptQueue);
  // Pending annotations (review tray) consumed into quote blocks on send.
  const annotations = useStore((s) => s.annotations[agentId] ?? []);
  const clearAnnotations = useStore((s) => s.clearAnnotations);
  // Draft lives in the store (keyed by agent) so it survives the remounts that a
  // tab switch or agent switch cause.
  const text = useStore((s) => s.drafts[agentId] ?? '');
  const setDraft = useStore((s) => s.setDraft);
  const commands = useStore((s) => s.agents[agentId]?.commands ?? []);
  const listWorkspaceEntries = useStore((s) => s.listWorkspaceEntries);
  const imageSupport = useStore((s) => s.agents[agentId]?.imagePromptSupport ?? null);
  const asideSupport = useStore((s) => s.agents[agentId]?.asideSupport ?? null);
  const steeringSupport = useStore((s) => s.agents[agentId]?.steeringSupport ?? null);
  const queuedPrompts = useStore((s) => s.agents[agentId]?.queuedPrompts ?? []);
  const textRef = useRef<HTMLTextAreaElement>(null);
  const highlightRef = useRef<HTMLDivElement>(null);
  // The draft is shared Zustand state so it survives pane remounts. Preserve
  // the native selection across that controlled-value update; otherwise some
  // browsers collapse it to the end while the user is typing mid-prompt.
  const pendingSelectionRef = useRef<{ value: string; start: number; end: number } | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const photoRef = useRef<HTMLInputElement>(null);
  const aborts = useRef(new Map<string, AbortController>());
  const attachmentRef = useRef<DraftAttachment[]>([]);
  const [caret, setCaret] = useState(0);
  const [sel, setSel] = useState(0);
  const [dismissed, setDismissed] = useState(false);
  const [attachments, setAttachments] = useState<DraftAttachment[]>([]);
  const [attachmentError, setAttachmentError] = useState<string | null>(null);
  const [attachmentMenuOpen, setAttachmentMenuOpen] = useState(false);
  const { mounted: attachmentMenuMounted, closing: attachmentMenuClosing } = usePresence(attachmentMenuOpen);
  const [dragging, setDragging] = useState(false);
  const [sending, setSending] = useState(false);
  const [queuedFlash, setQueuedFlash] = useState(false);
  const [fileEntries, setFileEntries] = useState<WorkspaceEntry[]>([]);
  const [fileEntriesDir, setFileEntriesDir] = useState<string | null>(null);

  useEffect(() => {
    attachmentRef.current = attachments;
  }, [attachments]);

  useEffect(() => () => {
    for (const controller of aborts.current.values()) controller.abort();
    for (const attachment of attachmentRef.current) if (attachment.previewUrl) URL.revokeObjectURL(attachment.previewUrl);
  }, []);

  const slash = useMemo(() => findSlashToken(text, caret), [text, caret]);
  const file = useMemo(() => findFileToken(text, caret), [text, caret]);
  const fileRequest = useMemo(() => file ? fileCompletionRequest(file.query) : null, [file]);
  const commandsWithBtw = useMemo(() => asideSupport ? [{ name: 'btw', description: 'Ask without adding to future context', input: 'question' }, ...commands.filter((c) => c.name !== 'btw')] : commands, [asideSupport, commands]);
  const commandMatches = useMemo(() => (slash ? fuzzyFilter(slash.query, commandsWithBtw, (c) => c.name).slice(0, 8) : []), [slash, commandsWithBtw]);
  const fileMatches = useMemo(() => (fileRequest && fileEntriesDir === fileRequest.dir
    ? fuzzyFilter(fileRequest.filter, fileEntries, (entry) => entry.path).slice(0, 8)
    : []), [fileEntries, fileEntriesDir, fileRequest]);
  const completionMatches = slash
    ? commandMatches.map((command) => ({ kind: 'command' as const, command }))
    : file
      ? fileMatches.map((entry) => ({ kind: 'file' as const, entry }))
      : [];
  const showPopup = completionMatches.length > 0 && !dismissed;

  useEffect(() => {
    if (!file || !fileRequest) {
      setFileEntries([]);
      setFileEntriesDir(null);
      return;
    }
    let current = true;
    setFileEntries([]);
    setFileEntriesDir(fileRequest.dir);
    const timer = window.setTimeout(() => {
      void listWorkspaceEntries(agentId, fileRequest.dir).then(
        (entries) => { if (current) setFileEntries(entries); },
        () => { if (current) setFileEntries([]); },
      );
    }, 75);
    return () => {
      current = false;
      window.clearTimeout(timer);
    };
  }, [agentId, file?.start, fileRequest?.dir, listWorkspaceEntries]);

  useLayoutEffect(() => {
    const el = textRef.current;
    if (!el) return;

    el.style.height = 'auto';
    const maxHeight = Number.parseFloat(getComputedStyle(el).maxHeight);
    const height = Math.min(el.scrollHeight, maxHeight);
    el.style.height = `${height}px`;
    el.style.overflowY = el.scrollHeight > maxHeight ? 'auto' : 'hidden';
    if (highlightRef.current) {
      highlightRef.current.scrollTop = el.scrollTop;
      highlightRef.current.scrollLeft = el.scrollLeft;
    }
    const pendingSelection = pendingSelectionRef.current;
    if (pendingSelection?.value === text && document.activeElement === el) {
      el.setSelectionRange(pendingSelection.start, pendingSelection.end);
      pendingSelectionRef.current = null;
    }
  }, [text]);

  // Re-arm the popup (and reset the highlighted row) whenever the token itself
  // changes — a fresh "/" or continued typing should reopen it even if the
  // previous token was dismissed with Escape.
  useEffect(() => {
    setSel(0);
    setDismissed(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [slash?.start, slash?.query, file?.start, file?.query]);

  const updateCaret = (el: HTMLTextAreaElement) => setCaret(el.selectionStart ?? 0);

  const applyCommand = (cmd: SlashCommand | undefined) => {
    if (!slash || !cmd) return;
    const insertion = `/${cmd.name} `;
    const next = text.slice(0, slash.start) + insertion + text.slice(slash.end);
    setDraft(agentId, next);
    const pos = slash.start + insertion.length;
    setCaret(pos);
    requestAnimationFrame(() => {
      const el = textRef.current;
      if (el) {
        el.focus();
        el.setSelectionRange(pos, pos);
      }
    });
  };

  const applyFile = (entry: WorkspaceEntry | undefined) => {
    if (!file || !entry) return;
    const insertion = `@${entry.path}${entry.isDir ? '/' : ' '}`;
    const next = text.slice(0, file.start) + insertion + text.slice(file.end);
    setDraft(agentId, next);
    const pos = file.start + insertion.length;
    setCaret(pos);
    requestAnimationFrame(() => {
      const el = textRef.current;
      if (el) {
        el.focus();
        el.setSelectionRange(pos, pos);
      }
    });
  };

  const applyCompletion = (completion: typeof completionMatches[number] | undefined) => {
    if (!completion) return;
    if (completion.kind === 'command') applyCommand(completion.command);
    else applyFile(completion.entry);
  };

  const upload = async (attachment: DraftAttachment) => {
    const controller = new AbortController();
    aborts.current.set(attachment.localId, controller);
    try {
      const token = storedToken();
      const { status, body } = await new Promise<{ status: number; body: unknown }>((resolve, reject) => {
        const request = new XMLHttpRequest();
        request.open('POST', `/api/agents/${encodeURIComponent(agentId)}/assets`);
        request.setRequestHeader('Content-Type', attachment.file.type);
        request.setRequestHeader('X-File-Name', encodeURIComponent(attachment.file.name));
        if (attachment.file.type.startsWith('image/') && imageSupport === true) request.setRequestHeader('X-Store-Image-Asset', 'true');
        if (token) request.setRequestHeader('Authorization', `Bearer ${token}`);
        request.upload.onprogress = (event) => {
          if (!event.lengthComputable) return;
          const uploadProgress = Math.min(100, Math.round((event.loaded / event.total) * 100));
          setAttachments((current) => current.map((item) => item.localId === attachment.localId
            ? { ...item, uploadProgress }
            : item));
        };
        request.onload = () => {
          let responseBody: unknown = null;
          try { responseBody = JSON.parse(request.responseText); } catch { /* response error handled below */ }
          resolve({ status: request.status, body: responseBody });
        };
        request.onerror = () => reject(new Error('Upload failed: network error'));
        request.onabort = () => reject(new DOMException('Upload cancelled', 'AbortError'));
        controller.signal.addEventListener('abort', () => request.abort(), { once: true });
        request.send(attachment.file);
      });
      const response = { ok: status >= 200 && status < 300, status };
      const parsedBody = body as
        | { asset?: { assetId: string; mimeType: string; name?: string }; upload?: { path: string }; error?: string }
        | null;
      if (!response.ok || (!parsedBody?.upload && !parsedBody?.asset)) throw new Error(parsedBody?.error ?? `Upload failed (${response.status})`);
      const asset = parsedBody.asset && imageSupport === true ? { type: 'image' as const, ...parsedBody.asset } : undefined;
      setAttachments((current) => current.map((item) => item.localId === attachment.localId
        ? { ...item, status: 'ready', uploadProgress: 100, asset, uploadPath: parsedBody.upload?.path, error: undefined }
        : item));
    } catch (error) {
      if (error instanceof DOMException && error.name === 'AbortError') return;
      const message = error instanceof Error ? error.message : 'Upload failed';
      setAttachments((current) => current.map((item) => item.localId === attachment.localId
        ? { ...item, status: 'error', error: message }
        : item));
    } finally {
      aborts.current.delete(attachment.localId);
    }
  };

  const addFiles = (files: File[]) => {
    setAttachmentError(null);
    if (!files.length) return;
    const images = files.filter((file) => file.type.startsWith('image/') && imageSupport === true);
    const invalid = images.find((file) => !ACCEPTED_IMAGE_TYPES.has(file.type));
    if (invalid) {
      setAttachmentError(`${invalid.name}: use PNG, JPEG, GIF, or WebP.`);
      return;
    }
    const oversized = images.find((file) => file.size > MAX_IMAGE_BYTES);
    if (oversized) {
      setAttachmentError(`${oversized.name} exceeds the 10 MiB per-image limit.`);
      return;
    }
    if (attachments.filter((attachment) => attachment.asset || (attachment.file.type.startsWith('image/') && imageSupport === true)).length + images.length > MAX_IMAGES) {
      setAttachmentError(`A prompt can contain at most ${MAX_IMAGES} images.`);
      return;
    }
    const total = attachments.filter((item) => item.file.type.startsWith('image/') && imageSupport === true).reduce((sum, item) => sum + item.file.size, 0) + images.reduce((sum, file) => sum + file.size, 0);
    if (total > MAX_TURN_IMAGE_BYTES) {
      setAttachmentError('Images exceed the 20 MiB per-prompt limit.');
      return;
    }
    const added: DraftAttachment[] = files.map((file) => ({
      localId: crypto.randomUUID(),
      file,
      previewUrl: file.type.startsWith('image/') ? URL.createObjectURL(file) : undefined,
      status: 'uploading',
      uploadProgress: 0,
    }));
    setAttachments((current) => [...current, ...added]);
    for (const attachment of added) void upload(attachment);
  };

  const removeAttachment = (localId: string) => {
    const attachment = attachments.find((item) => item.localId === localId);
    aborts.current.get(localId)?.abort();
    if (attachment?.previewUrl) URL.revokeObjectURL(attachment.previewUrl);
    setAttachments((current) => current.filter((item) => item.localId !== localId));
    setAttachmentError(null);
  };

  const retryAttachment = (attachment: DraftAttachment) => {
    setAttachments((current) => current.map((item) => item.localId === attachment.localId
      ? { ...item, status: 'uploading', uploadProgress: 0, error: undefined }
      : item));
    void upload(attachment);
  };

  const send = async (steering = false) => {
    if (sending) return;
    const t = text.trim();
    const hasAnnotations = annotations.length > 0;
    if (!t && attachments.length === 0 && !hasAnnotations) return;
    if (attachments.some((attachment) => attachment.status === 'uploading')) {
      setAttachmentError('Wait for image uploads to finish.');
      return;
    }
    if (attachments.some((attachment) => attachment.status === 'error' || (!attachment.uploadPath && !attachment.asset) || (attachment.file.type.startsWith('image/') && imageSupport === true && !attachment.asset))) {
      setAttachmentError('Remove or retry failed uploads before sending.');
      return;
    }
    if (attachments.length === 0 && !hasAnnotations) {
      setSending(true);
      const btw = t.match(/^\/btw(?:\s+|$)([\s\S]*)$/i);
      const result = !steering && btw ? await aside(agentId, btw[1].trim()) : await (steering ? steer : prompt)(agentId, t);
      setSending(false);
      if (result.error) setAttachmentError(result.error);
      if (result.disposition === 'queued') {
        setQueuedFlash(true);
        window.setTimeout(() => setQueuedFlash(false), 1200);
      }
      return;
    }
    // Consumed annotations become one `quote` block each, in transcript/seq
    // order, ahead of any trailing free-text block (docs/transcript-annotations.md
    // "Sending flow"). Quote blocks alone (no text, no attachments) are valid.
    const blocks: PromptBlock[] = [...annotations]
      .sort((a, b) => a.seq - b.seq)
      .map((a) => ({ type: 'quote', refSeq: a.seq, role: a.role, quote: a.quote, comment: a.comment }));
    if (t) blocks.push({ type: 'text', text: t });
    blocks.push(...attachments.flatMap((attachment) => attachment.asset ? [attachment.asset] : []));
    const uploaded = attachments.flatMap((attachment) => attachment.uploadPath ? [attachment.uploadPath] : []);
    if (uploaded.length > 0) blocks.push({ type: 'text', text: `Tandem uploaded these files into your workspace: ${uploaded.join(', ')}. Read or use them as needed. If you configure a dedicated upload directory, add it to .gitignore unless the user asks to commit uploaded files.` });
    setSending(true);
    const result = await (steering ? steer : prompt)(agentId, blocks);
    setSending(false);
    if (result.error) {
      setAttachmentError(result.error);
      return;
    }
    for (const attachment of attachments) if (attachment.previewUrl) URL.revokeObjectURL(attachment.previewUrl);
    setAttachments([]);
    setAttachmentError(null);
    if (hasAnnotations) void clearAnnotations(agentId);
    if (result.disposition === 'queued') {
      setQueuedFlash(true);
      window.setTimeout(() => setQueuedFlash(false), 1200);
    }
  };

  const canSubmit = !!text.trim() || attachments.length > 0 || annotations.length > 0;
  const uploadsPending = attachments.some((item) => item.status === 'uploading');
  const previewQueuedPrompt = (blocks: PromptBlock[]) => {
    const message = blocks.filter((block): block is Extract<PromptBlock, { type: 'text' }> => block.type === 'text').map((block) => block.text).join(' ').trim();
    const images = blocks.filter((block) => block.type === 'image').length;
    return message || `${images} image${images === 1 ? '' : 's'}`;
  };

  const editQueuedPrompt = async (queued: QueuedPrompt) => {
    if (text.trim() || attachments.length > 0) {
      setAttachmentError('Send or clear the current draft before editing a queued prompt.');
      return;
    }
    if (queued.blocks.some((block) => block.type === 'image')) {
      setAttachmentError('Queued prompts with images cannot be edited yet.');
      return;
    }
    const result = await removeQueuedPrompt(agentId, queued.id);
    if (result.error) {
      setAttachmentError(result.error);
      return;
    }
    const draft = queued.blocks
      .filter((block): block is Extract<PromptBlock, { type: 'text' }> => block.type === 'text')
      .map((block) => block.text)
      .join('\n\n');
    setDraft(agentId, draft);
    setAttachmentError(null);
    requestAnimationFrame(() => {
      const el = textRef.current;
      if (!el) return;
      el.focus();
      el.setSelectionRange(draft.length, draft.length);
      setCaret(draft.length);
    });
  };

  return (
    <div
      className={`prompt-bar${dragging ? ' dragging' : ''}`}
      onDragEnter={(e) => { if (e.dataTransfer.types.includes('Files')) { e.preventDefault(); setDragging(true); } }}
      onDragOver={(e) => { if (e.dataTransfer.types.includes('Files')) e.preventDefault(); }}
      onDragLeave={(e) => { if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setDragging(false); }}
      onDrop={(e) => {
        e.preventDefault();
        setDragging(false);
        addFiles(Array.from(e.dataTransfer.files));
      }}
    >
      {showPopup && (
        <div className="slash-popup">
          {completionMatches.map((completion, i) => (
            <div
              key={completion.kind === 'command' ? completion.command.name : completion.entry.path}
              className={`slash-row${i === sel ? ' sel' : ''}`}
              onMouseEnter={() => setSel(i)}
              onMouseDown={(e) => {
                e.preventDefault();
                applyCompletion(completion);
              }}
            >
              {completion.kind === 'command' ? <>
                <span className="name">/{completion.command.name}</span>
                {completion.command.input && <span className="hint">{completion.command.input}</span>}
                {completion.command.description && <span className="desc">{completion.command.description}</span>}
              </> : <>
                <span className="name">@{completion.entry.path}{completion.entry.isDir ? '/' : ''}</span>
                <span className="desc">{completion.entry.isDir ? 'folder' : 'file'}</span>
              </>}
            </div>
          ))}
        </div>
      )}
      {attachments.length > 0 && (
        <div className="prompt-attachments">
          {attachments.map((attachment) => (
            <div className={`prompt-attachment ${attachment.status}`} key={attachment.localId}>
              {attachment.previewUrl ? <img src={attachment.previewUrl} alt="" /> : <span className="attachment-file-icon" aria-hidden="true">▤</span>}
              <div className="attachment-meta">
                <span title={attachment.file.name}>{attachment.file.name}</span>
                {attachment.status === 'uploading' ? <>
                  <small>Uploading… {attachment.uploadProgress ?? 0}%</small>
                  <progress value={attachment.uploadProgress ?? 0} max="100" aria-label={`Uploading ${attachment.file.name}: ${attachment.uploadProgress ?? 0}%`} />
                </> : <small>{attachment.status === 'error' ? attachment.error : 'Ready'}</small>}
              </div>
              {attachment.status === 'error' && <button type="button" onClick={() => retryAttachment(attachment)} title="Retry upload">↻</button>}
              <button type="button" onClick={() => removeAttachment(attachment.localId)} title={`Remove ${attachment.file.name}`}>×</button>
            </div>
          ))}
        </div>
      )}
      {attachmentError && <div className="prompt-attachment-error">{attachmentError}</div>}
      {queuedPrompts.length > 0 && (
        <div className="prompt-queue">
          <div className="prompt-queue-header">
            <span>Next up ({queuedPrompts.length})</span>
            <span className="prompt-queue-actions">
              <button type="button" onClick={() => void clearPromptQueue(agentId)}>Clear queue</button>
            </span>
          </div>
          {queuedPrompts.map((queued, index) => {
            const preview = previewQueuedPrompt(queued.blocks);
            return (
              <div className="prompt-queue-row" key={queued.id}>
                <span className="prompt-queue-position">{index + 1}.</span>
                <span className="prompt-queue-preview" title={preview}>{preview}</span>
                <button type="button" onClick={() => void editQueuedPrompt(queued)} title="Edit queued prompt" aria-label={`Edit queued prompt ${index + 1}`}>Edit</button>
                <button type="button" onClick={() => void removeQueuedPrompt(agentId, queued.id)} title="Remove queued prompt" aria-label={`Remove queued prompt ${index + 1}`}>×</button>
              </div>
            );
          })}
        </div>
      )}
      <div className="prompt-main">
        <input
          ref={fileRef}
          className="visually-hidden"
          type="file"
          multiple
          onChange={(e) => {
            addFiles(Array.from(e.target.files ?? []));
            e.target.value = '';
          }}
        />
        <input
          ref={photoRef}
          className="visually-hidden"
          type="file"
          accept="image/*"
          multiple
          onChange={(e) => {
            addFiles(Array.from(e.target.files ?? []));
            e.target.value = '';
          }}
        />
        <div className="prompt-text-wrap">
          <div className="prompt-text-highlight" ref={highlightRef} aria-hidden="true"><SkillText text={text} commands={commands} asideSupport={asideSupport} /></div>
          <textarea
            ref={textRef}
            data-prompt-agent={agentId}
            placeholder={usesSoftKeyboard()
              ? (working ? 'Queue a follow-up…  (use the button to queue)' : `Prompt ${agentId}…  (use the button to send)`)
              : (working ? 'Queue a follow-up…  (Enter to queue, Shift+Enter for newline)' : `Prompt ${agentId}…  (Enter to send, Shift+Enter for newline)`)}
            value={text}
            onPaste={(e) => {
              const images = Array.from(e.clipboardData.files).filter((file) => file.type.startsWith('image/'));
              if (images.length) {
                e.preventDefault();
                addFiles(images);
              }
            }}
            onChange={(e) => {
              pendingSelectionRef.current = {
                value: e.target.value,
                start: e.target.selectionStart ?? e.target.value.length,
                end: e.target.selectionEnd ?? e.target.value.length,
              };
              setDraft(agentId, e.target.value);
              updateCaret(e.target);
            }}
            onClick={(e) => updateCaret(e.currentTarget)}
            onKeyUp={(e) => updateCaret(e.currentTarget)}
            onScroll={(e) => {
              if (highlightRef.current) {
                highlightRef.current.scrollTop = e.currentTarget.scrollTop;
                highlightRef.current.scrollLeft = e.currentTarget.scrollLeft;
              }
            }}
            onKeyDown={(e) => {
              if (e.key === 'Escape') {
                e.preventDefault();
                setDismissed(true);
                e.currentTarget.blur();
                return;
              }
              if (showPopup) {
                if (e.key === 'ArrowDown') {
                  e.preventDefault();
                  setSel((i) => Math.min(completionMatches.length - 1, i + 1));
                  return;
                }
                if (e.key === 'ArrowUp') {
                  e.preventDefault();
                  setSel((i) => Math.max(0, i - 1));
                  return;
                }
                if (e.key === 'Enter' || e.key === 'Tab') {
                  e.preventDefault();
                  applyCompletion(completionMatches[sel]);
                  return;
                }
              }
              if (e.key === 'Enter' && !e.shiftKey && !usesSoftKeyboard()) {
                e.preventDefault();
                void send();
              }
            }}
            rows={1}
          />
        </div>
        <div className={`prompt-actions${working ? ' working' : ''}${working && steeringSupport === true ? ' has-steering' : ''}`}>
          <div className="attachment-menu-wrap">
            <button
              type="button"
              className="btn attach-btn"
              disabled={imageSupport !== true}
              onClick={() => setAttachmentMenuOpen((open) => !open)}
              title="Add an attachment"
              aria-label="Add an attachment"
              aria-expanded={attachmentMenuOpen}
              aria-haspopup="menu"
            >
              <svg viewBox="0 0 16 16" aria-hidden="true">
                <path d="M8 3v10M3 8h10" />
              </svg>
            </button>
            {attachmentMenuMounted && (
              <div className={`attachment-menu${attachmentMenuClosing ? ' closing' : ''}`} role="menu">
                <button type="button" role="menuitem" onClick={() => { setAttachmentMenuOpen(false); photoRef.current?.click(); }}>
                  Photo gallery
                </button>
                <button type="button" role="menuitem" onClick={() => { setAttachmentMenuOpen(false); fileRef.current?.click(); }}>
                  Files
                </button>
              </div>
            )}
          </div>
          {working && (
              <button className="btn stop-btn" onClick={() => interrupt(agentId)} title="Stop current turn; queued prompts will continue" aria-label="Stop current turn">
              <span className="stop-btn-icon" aria-hidden="true" />
            </button>
          )}
          {working && steeringSupport === true && (
            <button
              className="btn steer-btn"
              onClick={() => void send(true)}
              disabled={sending || uploadsPending || !canSubmit}
              title="Inject this message into the current turn"
              aria-label="Steer current turn"
            >
              <svg viewBox="0 0 24 24" aria-hidden="true">
                <circle cx="12" cy="12" r="8" />
                <circle cx="12" cy="12" r="2" />
                <path d="M6.3 6.3 10.6 10.6M17.7 6.3l-4.3 4.3M12 14v6" />
              </svg>
            </button>
          )}
          <button
            className="btn primary"
            onClick={() => void send()}
            disabled={sending || uploadsPending || !canSubmit}
            title={working ? 'Send after the current turn finishes' : 'Send prompt'}
          >
            {sending ? 'Sending…' : queuedFlash ? 'Queued ✓' : working ? 'Queue' : 'Send'}
          </button>
        </div>
      </div>
      {dragging && <div className="prompt-drop-hint">Drop files to attach</div>}
    </div>
  );
}
