import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { useStore } from '../../store';
import type { AgentView } from '../../store';
import type { Approval, ImageAssetRef, PromptBlock, QueuedPrompt, SlashCommand, ToolStatus, WireEvent } from '../../wire';
import { storedToken } from '../../ws/client';
import { renderMarkdown } from '../../markdown';
import { fuzzyFilter } from '../../fuzzy';
import { usesSoftKeyboard } from '../../mobile';

// The Transcript pane renders the normalized AgentEvent stream (docs/ui.md):
// merged prose, dimmed thoughts, collapsed tool cards with status chips, plans,
// per-terminal mini-terminals, inline permission cards, and error banners. A
// prompt input sends {t:'prompt'}; Esc/Interrupt sends {t:'interrupt'}.

type Item =
  | { kind: 'user'; key: string; blocks: PromptBlock[] }
  | { kind: 'message'; key: string; text: string }
  | { kind: 'thought'; key: string; text: string }
  | { kind: 'tool'; key: string; title: string; status: ToolStatus; content?: unknown; rawInput?: unknown; toolKind?: string }
  | { kind: 'plan'; key: string; entries: { label: string; status: 'pending' | 'in_progress' | 'done' }[] }
  | { kind: 'terminal'; key: string; termId: string; text: string; truncated: boolean }
  | { kind: 'permission'; key: string; reqId: string; title: string; options: { optionId: string; name: string }[] }
  | { kind: 'error'; key: string; message: string };

function build(events: { seq: number; event: WireEvent }[], pending: Approval[]): Item[] {
  const items: Item[] = [];
  const tools = new Map<string, Extract<Item, { kind: 'tool' }>>();
  const terms = new Map<string, Extract<Item, { kind: 'terminal' }>>();
  let plan: Extract<Item, { kind: 'plan' }> | undefined;
  const pendingIds = new Set(pending.map((p) => p.reqId));

  for (const { seq, event: ev } of events) {
    switch (ev.kind) {
      case 'user_message':
        items.push({
          kind: 'user',
          key: `u${seq}`,
          blocks: ev.blocks ?? (ev.text != null ? [{ type: 'text', text: ev.text }] : []),
        });
        break;
      case 'message_chunk': {
        const last = items[items.length - 1];
        if (last && last.kind === 'message') last.text += ev.text;
        else items.push({ kind: 'message', key: `m${seq}`, text: ev.text });
        break;
      }
      case 'thought_chunk': {
        const last = items[items.length - 1];
        if (last && last.kind === 'thought') last.text += ev.text;
        else items.push({ kind: 'thought', key: `t${seq}`, text: ev.text });
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
    }
  }
  // Keep only still-pending permission cards inline (answered ones fall away).
  const visible = items.filter((it) => it.kind !== 'permission' || pendingIds.has(it.reqId));
  // The current task list is session state rather than transcript history.
  // Append it here so callers can split it into the pane's fixed bottom slot.
  if (plan) visible.push(plan);
  return visible;
}

export function TranscriptPane() {
  const agent = useStore((s) => (s.focusedId ? s.agents[s.focusedId] : undefined)) as AgentView | undefined;
  const respond = useStore((s) => s.respond);
  const scrollRef = useRef<HTMLDivElement>(null);
  // `stick` follows the tail as new items arrive; it flips off the moment the
  // user scrolls up and back on when they return (or hit the button). Kept in a
  // ref so the scroll handler and the items effect share it without re-rendering.
  const stick = useRef(true);
  const [atBottom, setAtBottom] = useState(true);

  const items = useMemo(() => (agent ? build(agent.events, agent.pendingApprovals) : []), [agent?.events, agent?.pendingApprovals]);
  const taskList = items.find((item): item is Extract<Item, { kind: 'plan' }> => item.kind === 'plan');
  const transcriptItems = items.filter((item) => item.kind !== 'plan');

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

  if (!agent) return null;

  return (
    <div className="pane">
      <div className="transcript-wrap">
        <div className="transcript-history">
          <div className="transcript" ref={scrollRef} onScroll={onScroll}>
            {transcriptItems.length === 0 && <div className="empty">No activity yet. Send a prompt below to start a turn.</div>}
            {transcriptItems.map((it) => (
              <Row key={it.key} item={it} onRespond={(opt) => it.kind === 'permission' && respond(agent.id, it.reqId, opt)} />
            ))}
          </div>
          {!atBottom && (
            <button className="scroll-latest" onClick={scrollToBottom} title="Scroll to latest">
              ↓ Latest
            </button>
          )}
        </div>
        {taskList && <TaskList item={taskList} />}
      </div>
      <PromptBar agentId={agent.id} working={agent.status === 'working' || agent.status === 'blocked'} />
      <SessionConfigBar agentId={agent.id} sessionConfig={agent.sessionConfig} usage={agent.usage} />
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

function Row({ item, onRespond }: { item: Item; onRespond: (optionId: string) => void }) {
  switch (item.kind) {
    case 'user':
      return (
        <div className="ev user">
          {item.blocks.map((block, i) =>
            block.type === 'text' ? <div key={i}>{block.text}</div> : <TranscriptImage key={`${block.assetId}-${i}`} block={block} />,
          )}
        </div>
      );
    case 'message':
      return (
        <div className="ev msg" onClick={handleCodeCopyClick} dangerouslySetInnerHTML={{ __html: renderMarkdown(item.text) }} />
      );
    case 'thought':
      return (
        <details className="ev thought">
          <summary>thinking…</summary>
          {item.text}
        </details>
      );
    case 'tool':
      return <ToolCard item={item} />;
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
          <div className="t">⚠ Permission required: {item.title}</div>
          <div className="acts" style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
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
  }
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

function parseToolContent(content: unknown): { text: string | null; images: ToolImage[] } {
  if (content == null) return { text: null, images: [] };
  if (typeof content === 'string') return { text: content || null, images: [] };
  if (Array.isArray(content)) {
    const images: ToolImage[] = [];
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
        if (block?.type === 'diff') return `--- ${block.path}\n${block.newText ?? ''}`;
        return null;
      })
      .filter((s): s is string => !!s);
    return { text: parts.length ? parts.join('\n') : null, images };
  }
  return { text: JSON.stringify(content, null, 2), images: [] };
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

function ToolCard({ item }: { item: Extract<Item, { kind: 'tool' }> }) {
  const [open, setOpen] = useState(false);
  const { text: body, images } = parseToolContent(item.content);
  const args = formatArgs(item.rawInput);
  const isExecute = item.toolKind === 'execute';
  const command = isExecute ? terminalCommand(item) : null;
  // For a plain execute call, the command *is* the args — showing it again as
  // a raw JSON "Arguments" blob under a terminal prompt line is noise.
  const showArgs = args != null && !(isExecute && command != null);
  const hasBody = body != null || showArgs || images.length > 0 || command != null;
  useEffect(() => {
    if (images.length > 0) setOpen(true);
  }, [images.length]);
  return (
    <div className="card">
      <div className="card-head" onClick={() => hasBody && setOpen((o) => !o)}>
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
            body != null ? (
              <pre className="tool-terminal-output">{body}</pre>
            ) : (
              <div className="tool-terminal-empty">(no output)</div>
            )
          ) : (
            body != null && <div className="tool-output">{body}</div>
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

// Finds a slash-command token ending at `caret`: a run of alphanumeric/-/_
// chars immediately preceded by "/", anywhere in the text (not just at the
// start of a line). Returns null once nothing has been typed after the "/"
// yet, so the popup only appears once there's something to fuzzy-match.
function findSlashToken(text: string, caret: number): { start: number; end: number; query: string } | null {
  if (caret <= 0 || caret > text.length) return null;
  let i = caret;
  while (i > 0 && /[A-Za-z0-9_-]/.test(text[i - 1])) i--;
  if (i === 0 || text[i - 1] !== '/') return null;
  const query = text.slice(i, caret);
  if (!query) return null;
  return { start: i - 1, end: caret, query };
}

const ACCEPTED_IMAGE_TYPES = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp']);
const MAX_IMAGE_BYTES = 10 * 1024 * 1024;
const MAX_TURN_IMAGE_BYTES = 20 * 1024 * 1024;
const MAX_IMAGES = 4;

type DraftAttachment = {
  localId: string;
  file: File;
  previewUrl: string;
  status: 'uploading' | 'ready' | 'error';
  asset?: Extract<PromptBlock, { type: 'image' }>;
  error?: string;
};

function PromptBar({ agentId, working }: { agentId: string; working: boolean }) {
  const prompt = useStore((s) => s.prompt);
  const interrupt = useStore((s) => s.interrupt);
  const removeQueuedPrompt = useStore((s) => s.removeQueuedPrompt);
  const clearPromptQueue = useStore((s) => s.clearPromptQueue);
  // Draft lives in the store (keyed by agent) so it survives the remounts that a
  // tab switch or agent switch cause.
  const text = useStore((s) => s.drafts[agentId] ?? '');
  const setDraft = useStore((s) => s.setDraft);
  const commands = useStore((s) => s.agents[agentId]?.commands ?? []);
  const imageSupport = useStore((s) => s.agents[agentId]?.imagePromptSupport ?? null);
  const queuedPrompts = useStore((s) => s.agents[agentId]?.queuedPrompts ?? []);
  const textRef = useRef<HTMLTextAreaElement>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const aborts = useRef(new Map<string, AbortController>());
  const attachmentRef = useRef<DraftAttachment[]>([]);
  const [caret, setCaret] = useState(0);
  const [sel, setSel] = useState(0);
  const [dismissed, setDismissed] = useState(false);
  const [attachments, setAttachments] = useState<DraftAttachment[]>([]);
  const [attachmentError, setAttachmentError] = useState<string | null>(null);
  const [dragging, setDragging] = useState(false);
  const [sending, setSending] = useState(false);
  const [queuedFlash, setQueuedFlash] = useState(false);

  useEffect(() => {
    attachmentRef.current = attachments;
  }, [attachments]);

  useEffect(() => () => {
    for (const controller of aborts.current.values()) controller.abort();
    for (const attachment of attachmentRef.current) URL.revokeObjectURL(attachment.previewUrl);
  }, []);

  const slash = useMemo(() => findSlashToken(text, caret), [text, caret]);
  const matches = useMemo(() => (slash ? fuzzyFilter(slash.query, commands, (c) => c.name).slice(0, 8) : []), [slash, commands]);
  const showPopup = !!slash && matches.length > 0 && !dismissed;

  useLayoutEffect(() => {
    const el = textRef.current;
    if (!el) return;

    el.style.height = 'auto';
    const maxHeight = Number.parseFloat(getComputedStyle(el).maxHeight);
    const height = Math.min(el.scrollHeight, maxHeight);
    el.style.height = `${height}px`;
    el.style.overflowY = el.scrollHeight > maxHeight ? 'auto' : 'hidden';
  }, [text]);

  // Re-arm the popup (and reset the highlighted row) whenever the token itself
  // changes — a fresh "/" or continued typing should reopen it even if the
  // previous token was dismissed with Escape.
  useEffect(() => {
    setSel(0);
    setDismissed(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [slash?.start, slash?.query]);

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

  const upload = async (attachment: DraftAttachment) => {
    const controller = new AbortController();
    aborts.current.set(attachment.localId, controller);
    try {
      const token = storedToken();
      const response = await fetch(`/api/agents/${encodeURIComponent(agentId)}/assets`, {
        method: 'POST',
        headers: {
          'Content-Type': attachment.file.type,
          'X-File-Name': encodeURIComponent(attachment.file.name),
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
        },
        body: attachment.file,
        signal: controller.signal,
      });
      const body = (await response.json().catch(() => null)) as
        | { asset?: { assetId: string; mimeType: string; name?: string }; error?: string }
        | null;
      if (!response.ok || !body?.asset) throw new Error(body?.error ?? `Upload failed (${response.status})`);
      const asset: Extract<PromptBlock, { type: 'image' }> = { type: 'image', ...body.asset };
      setAttachments((current) => current.map((item) => item.localId === attachment.localId
        ? { ...item, status: 'ready', asset, error: undefined }
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
    if (imageSupport !== true) {
      setAttachmentError(imageSupport === false ? 'This agent does not accept image prompts.' : 'Image support is not available yet.');
      return;
    }
    const images = files.filter((file) => file.type.startsWith('image/'));
    if (!images.length) return;
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
    if (attachments.length + images.length > MAX_IMAGES) {
      setAttachmentError(`A prompt can contain at most ${MAX_IMAGES} images.`);
      return;
    }
    const total = attachments.reduce((sum, item) => sum + item.file.size, 0) + images.reduce((sum, file) => sum + file.size, 0);
    if (total > MAX_TURN_IMAGE_BYTES) {
      setAttachmentError('Images exceed the 20 MiB per-prompt limit.');
      return;
    }
    const added: DraftAttachment[] = images.map((file) => ({
      localId: crypto.randomUUID(),
      file,
      previewUrl: URL.createObjectURL(file),
      status: 'uploading',
    }));
    setAttachments((current) => [...current, ...added]);
    for (const attachment of added) void upload(attachment);
  };

  const removeAttachment = (localId: string) => {
    const attachment = attachments.find((item) => item.localId === localId);
    aborts.current.get(localId)?.abort();
    if (attachment) URL.revokeObjectURL(attachment.previewUrl);
    setAttachments((current) => current.filter((item) => item.localId !== localId));
    setAttachmentError(null);
  };

  const retryAttachment = (attachment: DraftAttachment) => {
    setAttachments((current) => current.map((item) => item.localId === attachment.localId
      ? { ...item, status: 'uploading', error: undefined }
      : item));
    void upload(attachment);
  };

  const send = async () => {
    if (sending) return;
    const t = text.trim();
    if (!t && attachments.length === 0) return;
    if (attachments.some((attachment) => attachment.status === 'uploading')) {
      setAttachmentError('Wait for image uploads to finish.');
      return;
    }
    if (attachments.some((attachment) => attachment.status === 'error' || !attachment.asset)) {
      setAttachmentError('Remove or retry failed images before sending.');
      return;
    }
    if (attachments.length === 0) {
      setSending(true);
      const result = await prompt(agentId, t);
      setSending(false);
      if (result.error) setAttachmentError(result.error);
      if (result.disposition === 'queued') {
        setQueuedFlash(true);
        window.setTimeout(() => setQueuedFlash(false), 1200);
      }
      return;
    }
    const blocks: PromptBlock[] = [];
    if (t) blocks.push({ type: 'text', text: t });
    blocks.push(...attachments.map((attachment) => attachment.asset!));
    setSending(true);
    const result = await prompt(agentId, blocks);
    setSending(false);
    if (result.error) {
      setAttachmentError(result.error);
      return;
    }
    for (const attachment of attachments) URL.revokeObjectURL(attachment.previewUrl);
    setAttachments([]);
    setAttachmentError(null);
    if (result.disposition === 'queued') {
      setQueuedFlash(true);
      window.setTimeout(() => setQueuedFlash(false), 1200);
    }
  };

  const canSubmit = !!text.trim() || attachments.length > 0;
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
          {matches.map((c, i) => (
            <div
              key={c.name}
              className={`slash-row${i === sel ? ' sel' : ''}`}
              onMouseEnter={() => setSel(i)}
              onMouseDown={(e) => {
                e.preventDefault();
                applyCommand(c);
              }}
            >
              <span className="name">/{c.name}</span>
              {c.input && <span className="hint">{c.input}</span>}
              {c.description && <span className="desc">{c.description}</span>}
            </div>
          ))}
        </div>
      )}
      {attachments.length > 0 && (
        <div className="prompt-attachments">
          {attachments.map((attachment) => (
            <div className={`prompt-attachment ${attachment.status}`} key={attachment.localId}>
              <img src={attachment.previewUrl} alt="" />
              <div className="attachment-meta">
                <span title={attachment.file.name}>{attachment.file.name}</span>
                <small>{attachment.status === 'uploading' ? 'Uploading…' : attachment.status === 'error' ? attachment.error : 'Ready'}</small>
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
          accept="image/png,image/jpeg,image/gif,image/webp"
          multiple
          disabled={imageSupport !== true}
          onChange={(e) => {
            addFiles(Array.from(e.target.files ?? []));
            e.target.value = '';
          }}
        />
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
            setDraft(agentId, e.target.value);
            updateCaret(e.target);
          }}
          onClick={(e) => updateCaret(e.currentTarget)}
          onKeyUp={(e) => updateCaret(e.currentTarget)}
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
              setSel((i) => Math.min(matches.length - 1, i + 1));
              return;
            }
            if (e.key === 'ArrowUp') {
              e.preventDefault();
              setSel((i) => Math.max(0, i - 1));
              return;
            }
            if (e.key === 'Enter' || e.key === 'Tab') {
              e.preventDefault();
              applyCommand(matches[sel]);
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
        <div className="prompt-actions">
          <button
            type="button"
            className="btn attach-btn"
            disabled={imageSupport !== true}
            onClick={() => fileRef.current?.click()}
            title={imageSupport === true ? 'Attach images (or paste/drop)' : imageSupport === false ? 'This agent does not support image prompts' : 'Waiting for agent image capabilities'}
            aria-label="Attach images"
          >
            <svg viewBox="0 0 16 16" aria-hidden="true">
              <path d="M8 3v10M3 8h10" />
            </svg>
          </button>
          {working && (
            <button className="btn stop-btn" onClick={() => interrupt(agentId)} title="Stop current turn; queued prompts will continue" aria-label="Stop current turn">
              <span className="stop-btn-icon" aria-hidden="true" />
            </button>
          )}
          <button
            className="btn primary"
            onClick={() => void send()}
            disabled={sending || uploadsPending || !canSubmit}
            title={working ? 'Send after the current turn finishes' : 'Send prompt'}
          >
            {sending ? (working ? 'Queueing…' : 'Sending…') : queuedFlash ? 'Queued ✓' : working ? 'Queue' : 'Send'}
          </button>
        </div>
      </div>
      {dragging && <div className="prompt-drop-hint">Drop images to attach</div>}
    </div>
  );
}
