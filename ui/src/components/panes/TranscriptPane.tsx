import { useEffect, useMemo, useRef, useState } from 'react';
import { useStore } from '../../store';
import type { AgentView } from '../../store';
import type { Approval, SlashCommand, ToolStatus, WireEvent } from '../../wire';
import { renderMarkdown } from '../../markdown';
import { fuzzyFilter } from '../../fuzzy';

// The Transcript pane renders the normalized AgentEvent stream (docs/ui.md):
// merged prose, dimmed thoughts, collapsed tool cards with status chips, plans,
// per-terminal mini-terminals, inline permission cards, and error banners. A
// prompt input sends {t:'prompt'}; Esc/Interrupt sends {t:'interrupt'}.

type Item =
  | { kind: 'user'; key: string; text: string }
  | { kind: 'message'; key: string; text: string }
  | { kind: 'thought'; key: string; text: string }
  | { kind: 'tool'; key: string; title: string; status: ToolStatus; content?: unknown; rawInput?: unknown }
  | { kind: 'plan'; key: string; entries: { label: string; status: 'pending' | 'in_progress' | 'done' }[] }
  | { kind: 'terminal'; key: string; termId: string; text: string; truncated: boolean }
  | { kind: 'permission'; key: string; reqId: string; title: string; options: { optionId: string; name: string }[] }
  | { kind: 'error'; key: string; message: string };

function build(events: { seq: number; event: WireEvent }[], pending: Approval[]): Item[] {
  const items: Item[] = [];
  const tools = new Map<string, Extract<Item, { kind: 'tool' }>>();
  const terms = new Map<string, Extract<Item, { kind: 'terminal' }>>();
  const pendingIds = new Set(pending.map((p) => p.reqId));

  for (const { seq, event: ev } of events) {
    switch (ev.kind) {
      case 'user_message':
        items.push({ kind: 'user', key: `u${seq}`, text: ev.text });
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
        } else {
          const item: Extract<Item, { kind: 'tool' }> = { kind: 'tool', key: `tc${ev.id}`, title: ev.title, status: ev.status, content: ev.content, rawInput: ev.rawInput };
          tools.set(ev.id, item);
          items.push(item);
        }
        break;
      }
      case 'tool_call_update': {
        const t = tools.get(ev.id);
        if (t) {
          if (ev.status) t.status = ev.status;
          if (ev.content != null) t.content = ev.content;
        }
        break;
      }
      case 'plan':
        items.push({ kind: 'plan', key: `p${seq}`, entries: ev.entries });
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
  return items.filter((it) => it.kind !== 'permission' || pendingIds.has(it.reqId));
}

export function TranscriptPane() {
  const agent = useStore((s) => (s.focusedId ? s.agents[s.focusedId] : undefined)) as AgentView | undefined;
  const respond = useStore((s) => s.respond);
  const scrollRef = useRef<HTMLDivElement>(null);

  const items = useMemo(() => (agent ? build(agent.events, agent.pendingApprovals) : []), [agent?.events, agent?.pendingApprovals]);

  // Autoscroll to the tail unless the user scrolled up.
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 120;
    if (nearBottom) el.scrollTop = el.scrollHeight;
  }, [items]);

  if (!agent) return null;

  return (
    <div className="pane">
      <div className="transcript" ref={scrollRef}>
        {items.length === 0 && <div className="empty">No activity yet. Send a prompt below to start a turn.</div>}
        {items.map((it) => (
          <Row key={it.key} item={it} onRespond={(opt) => it.kind === 'permission' && respond(agent.id, it.reqId, opt)} />
        ))}
      </div>
      <PromptBar agentId={agent.id} working={agent.status === 'working'} />
      <SessionConfigBar agentId={agent.id} sessionConfig={agent.sessionConfig} />
    </div>
  );
}

function Row({ item, onRespond }: { item: Item; onRespond: (optionId: string) => void }) {
  switch (item.kind) {
    case 'user':
      return <div className="ev user">{item.text}</div>;
    case 'message':
      return <div className="ev msg" dangerouslySetInnerHTML={{ __html: renderMarkdown(item.text) }} />;
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
      return (
        <ul className="plan card" style={{ padding: '6px 12px' }}>
          {item.entries.map((e, i) => (
            <li key={i} className={e.status}>
              <span className="mark">{e.status === 'done' ? '✓' : e.status === 'in_progress' ? '▸' : '○'}</span>
              <span className={e.status}>{e.label}</span>
            </li>
          ))}
        </ul>
      );
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

// ACP tool_call content is an array of ToolCallContent blocks — most are
// `{ type: 'content', content: { type: 'text', text } }`, some are
// `{ type: 'diff', path, oldText, newText }`. Flatten to plain text for
// display rather than dumping the raw JSON envelope.
function formatToolContent(content: unknown): string | null {
  if (content == null) return null;
  if (typeof content === 'string') return content || null;
  if (Array.isArray(content)) {
    const parts = content
      .map((block: any) => {
        if (block?.type === 'content' && block.content?.type === 'text') return block.content.text ?? '';
        if (block?.type === 'diff') return `--- ${block.path}\n${block.newText ?? ''}`;
        return null;
      })
      .filter((s): s is string => !!s);
    return parts.length ? parts.join('\n') : null;
  }
  return JSON.stringify(content, null, 2);
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

function ToolCard({ item }: { item: Extract<Item, { kind: 'tool' }> }) {
  const [open, setOpen] = useState(false);
  const body = formatToolContent(item.content);
  const args = formatArgs(item.rawInput);
  const hasBody = body != null || args != null;
  return (
    <div className="card">
      <div className="card-head" onClick={() => hasBody && setOpen((o) => !o)}>
        <span>{hasBody ? (open ? '▾' : '▸') : '⚙'}</span>
        <span className="title">{item.title}</span>
        <span className={`chip ${item.status}`}>{item.status}</span>
      </div>
      {open && hasBody && (
        <div className="card-body">
          {args != null && (
            <div className="tool-args">
              <div className="tool-args-label">Arguments</div>
              <pre>{args}</pre>
            </div>
          )}
          {body != null && <div className="tool-output">{body}</div>}
        </div>
      )}
    </div>
  );
}

// Model + Permission Mode pickers (ACP session-modes / session-config-options,
// docs/acp-notes.md). `sessionConfig` is null until the agent reports it (or
// always, for pty agents) — the bar renders nothing in that case rather than an
// empty shell.
function SessionConfigBar({ agentId, sessionConfig }: { agentId: string; sessionConfig: AgentView['sessionConfig'] }) {
  const setMode = useStore((s) => s.setMode);
  const setConfigOption = useStore((s) => s.setConfigOption);
  if (!sessionConfig) return null;

  const { modes, configOptions } = sessionConfig;
  // The model selector is a config option with category 'model' (pinned id
  // "model" on the real agent, but category is the spec-sanctioned way to find
  // it). Everything else with category 'mode' is redundant with `modes` below.
  const modelOpt = configOptions.find((o) => o.category === 'model' && o.type === 'select');
  const hasModes = !!modes && modes.availableModes.length > 0;
  if (!hasModes && !modelOpt) return null;

  return (
    <div className="session-config-bar">
      {modelOpt && (
        <label>
          Model
          <select value={String(modelOpt.currentValue)} onChange={(e) => setConfigOption(agentId, modelOpt.id, e.target.value)}>
            {(modelOpt.options ?? []).map((o) => (
              <option key={o.value} value={o.value}>
                {o.name}
              </option>
            ))}
          </select>
        </label>
      )}
      {hasModes && modes && (
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
      )}
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

function PromptBar({ agentId, working }: { agentId: string; working: boolean }) {
  const prompt = useStore((s) => s.prompt);
  const interrupt = useStore((s) => s.interrupt);
  // Draft lives in the store (keyed by agent) so it survives the remounts that a
  // tab switch or agent switch cause.
  const text = useStore((s) => s.drafts[agentId] ?? '');
  const setDraft = useStore((s) => s.setDraft);
  const commands = useStore((s) => s.agents[agentId]?.commands ?? []);
  const textRef = useRef<HTMLTextAreaElement>(null);
  const [caret, setCaret] = useState(0);
  const [sel, setSel] = useState(0);
  const [dismissed, setDismissed] = useState(false);

  const slash = useMemo(() => findSlashToken(text, caret), [text, caret]);
  const matches = useMemo(() => (slash ? fuzzyFilter(slash.query, commands, (c) => c.name).slice(0, 8) : []), [slash, commands]);
  const showPopup = !!slash && matches.length > 0 && !dismissed;

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

  const send = () => {
    const t = text.trim();
    if (!t) return;
    prompt(agentId, t);
  };

  return (
    <div className="prompt-bar">
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
      <textarea
        ref={textRef}
        placeholder={`Prompt ${agentId}…  (Enter to send, Shift+Enter for newline)`}
        value={text}
        onChange={(e) => {
          setDraft(agentId, e.target.value);
          updateCaret(e.target);
        }}
        onClick={(e) => updateCaret(e.currentTarget)}
        onKeyUp={(e) => updateCaret(e.currentTarget)}
        onKeyDown={(e) => {
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
            if (e.key === 'Escape') {
              e.preventDefault();
              setDismissed(true);
              return;
            }
          }
          if (e.key === 'Enter' && !e.shiftKey) {
            e.preventDefault();
            send();
          }
          if (e.key === 'Escape' && working) {
            e.preventDefault();
            interrupt(agentId);
          }
        }}
        rows={1}
      />
      {working ? (
        <button className="btn" onClick={() => interrupt(agentId)} title="Interrupt (Esc)">
          ◼ Esc
        </button>
      ) : (
        <button className="btn primary" onClick={send}>
          Send
        </button>
      )}
    </div>
  );
}
