import { useEffect, useMemo, useRef, useState } from 'react';
import { useStore } from '../../store';
import type { AgentView } from '../../store';
import type { Approval, ToolStatus, WireEvent } from '../../wire';
import { renderMarkdown } from '../../markdown';

// The Transcript pane renders the normalized AgentEvent stream (docs/ui.md):
// merged prose, dimmed thoughts, collapsed tool cards with status chips, plans,
// per-terminal mini-terminals, inline permission cards, and error banners. A
// prompt input sends {t:'prompt'}; Esc/Interrupt sends {t:'interrupt'}.

type Item =
  | { kind: 'user'; key: string; text: string }
  | { kind: 'message'; key: string; text: string }
  | { kind: 'thought'; key: string; text: string }
  | { kind: 'tool'; key: string; title: string; status: ToolStatus; content?: unknown }
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
        } else {
          const item: Extract<Item, { kind: 'tool' }> = { kind: 'tool', key: `tc${ev.id}`, title: ev.title, status: ev.status, content: ev.content };
          tools.set(ev.id, item);
          items.push(item);
        }
        break;
      }
      case 'tool_call_update': {
        const t = tools.get(ev.id);
        if (t && ev.status) t.status = ev.status;
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
      <SessionConfigBar agentId={agent.id} sessionConfig={agent.sessionConfig} />
      <PromptBar agentId={agent.id} working={agent.status === 'working'} />
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

function ToolCard({ item }: { item: Extract<Item, { kind: 'tool' }> }) {
  const [open, setOpen] = useState(false);
  const hasBody = item.content != null;
  return (
    <div className="card">
      <div className="card-head" onClick={() => hasBody && setOpen((o) => !o)}>
        <span>{hasBody ? (open ? '▾' : '▸') : '⚙'}</span>
        <span className="title">{item.title}</span>
        <span className={`chip ${item.status}`}>{item.status}</span>
      </div>
      {open && hasBody && <div className="card-body">{typeof item.content === 'string' ? item.content : JSON.stringify(item.content, null, 2)}</div>}
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
    <div className="session-config-bar" style={{ display: 'flex', gap: 8, padding: '4px 12px', alignItems: 'center' }}>
      {modelOpt && (
        <label style={{ display: 'flex', gap: 4, alignItems: 'center', fontSize: 12 }}>
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
        <label style={{ display: 'flex', gap: 4, alignItems: 'center', fontSize: 12 }}>
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

function PromptBar({ agentId, working }: { agentId: string; working: boolean }) {
  const prompt = useStore((s) => s.prompt);
  const interrupt = useStore((s) => s.interrupt);
  // Draft lives in the store (keyed by agent) so it survives the remounts that a
  // tab switch or agent switch cause.
  const text = useStore((s) => s.drafts[agentId] ?? '');
  const setDraft = useStore((s) => s.setDraft);

  const send = () => {
    const t = text.trim();
    if (!t) return;
    prompt(agentId, t);
  };

  return (
    <div className="prompt-bar">
      <textarea
        placeholder={`Prompt ${agentId}…  (Enter to send, Shift+Enter for newline)`}
        value={text}
        onChange={(e) => setDraft(agentId, e.target.value)}
        onKeyDown={(e) => {
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
