import { useEffect, useMemo, useState } from 'react';
import { useStore } from '../store';
import type { SessionView } from '../store';
import { fuzzyFilter } from '../fuzzy';
import { addressHostLabel, addressLabel, addressOf } from '../messaging';
import type { AgentLink, AgentLinkInput } from '../wire';

// Defaults for a freshly granted link (docs/agent-messaging.md).
const NEW_LINK = { delivery: 'steer', budgetPerHour: 60, maxHops: 20, paused: false } as const;

function linkInput(link: AgentLink, patch: Partial<AgentLinkInput> = {}): AgentLinkInput {
  return { from: link.from, delivery: link.delivery, budgetPerHour: link.budgetPerHour, maxHops: link.maxHops, paused: link.paused, ...patch };
}

type Direction = 'in' | 'both';

// Inspector > Links: which agents may message the focused agent, plus its
// directory listing. Links are enforced on this agent's host; the daemon owns
// them, so edits only send a command and the list follows agent_links.
export function InspectorLinks({ agent }: { agent: SessionView }) {
  const conn = useStore((s) => s.conn);
  const info = useStore((s) => s.agentLinks[agent.id]);
  const hosts = useStore((s) => s.hosts);
  const fetchAgentLinks = useStore((s) => s.fetchAgentLinks);
  const setAgentLink = useStore((s) => s.setAgentLink);
  const deleteAgentLink = useStore((s) => s.deleteAgentLink);
  const setAgentListed = useStore((s) => s.setAgentListed);
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState('');

  // Fetch on focus (and again after a reconnect); agent_links broadcasts keep
  // the list current after that.
  useEffect(() => {
    if (conn === 'connected') fetchAgentLinks(agent.id);
  }, [agent.id, conn, fetchAgentLinks]);
  useEffect(() => {
    setAdding(false);
    setError('');
  }, [agent.id]);

  const links = info?.links ?? [];
  const listed = info?.listed ?? true;
  const report = (results: { error?: string }[]) => setError(results.find((r) => r.error)?.error ?? '');

  const addLink = (picked: SessionView, direction: Direction) => {
    // The link lives on the recipient's host: picked -> this agent here, and
    // for "both ways" this agent -> picked on the picked agent's host.
    // An existing inbound link keeps its settings; only the reverse is added.
    const from = addressOf(picked, hosts);
    const exists = links.some((l) => l.from.host === from.host && l.from.agent === from.agent);
    const requests = exists ? [] : [setAgentLink(agent.id, { from, ...NEW_LINK })];
    if (direction === 'both') requests.push(setAgentLink(picked.id, { from: addressOf(agent, hosts), ...NEW_LINK }));
    void Promise.all(requests).then(report);
    setAdding(false);
  };
  const edit = (link: AgentLink, patch: Partial<AgentLinkInput>) => {
    void setAgentLink(agent.id, linkInput(link, patch)).then((r) => report([r]));
  };
  const commitNumber = (link: AgentLink, field: 'budgetPerHour' | 'maxHops', raw: string, min: number) => {
    const value = Number.parseInt(raw, 10);
    if (Number.isNaN(value) || value < min || value === link[field]) return;
    edit(link, { [field]: value });
  };

  return (
    <div className="insp-links">
      <div className="insp-links-head">
        <span className="insp-links-title">Links</span>
        <span className="insp-hint">
          Agents below can start conversations with @{agent.name || agent.id}. Replies to its own questions never need a link.
        </span>
        <button className="btn ghost insp-link-add-btn" onClick={() => setAdding(true)}>+ Add link</button>
      </div>
      {links.length === 0 && <div className="insp-card">No agent may message this agent.</div>}
      {links.map((link) => (
        <div key={link.id} className={`insp-link${link.paused ? ' paused' : ''}`}>
          <span className="insp-link-from">
            {addressLabel(link.from)} <small>· {addressHostLabel(hosts, link.from)}</small>
          </span>
          <div className="seg" role="group" aria-label={`Delivery from ${addressLabel(link.from)}`}>
            <button className={`seg-btn${link.delivery === 'steer' ? ' active' : ''}`} title="Inject into the current turn when the agent supports steering; otherwise queue" onClick={() => link.delivery !== 'steer' && edit(link, { delivery: 'steer' })}>steer</button>
            <button className={`seg-btn${link.delivery === 'queue' ? ' active' : ''}`} title="Always wait for the current turn to finish" onClick={() => link.delivery !== 'queue' && edit(link, { delivery: 'queue' })}>queue</button>
          </div>
          <label title="Messages accepted per rolling hour over this link">
            budget/h
            <input
              key={`${link.id}-b-${link.budgetPerHour}`}
              type="number" min={0} aria-label={`Budget per hour from ${addressLabel(link.from)}`}
              defaultValue={link.budgetPerHour}
              onBlur={(e) => commitNumber(link, 'budgetPerHour', e.target.value, 0)}
              onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
            />
          </label>
          <label title="Longest back-and-forth allowed in one thread; stops agents looping">
            max hops
            <input
              key={`${link.id}-h-${link.maxHops}`}
              type="number" min={1} aria-label={`Max hops from ${addressLabel(link.from)}`}
              defaultValue={link.maxHops}
              onBlur={(e) => commitNumber(link, 'maxHops', e.target.value, 1)}
              onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
            />
          </label>
          <span className="insp-link-used" title="Messages accepted over this link in the last hour">used {link.usedLastHour}/{link.budgetPerHour} last hour</span>
          <button className="btn ghost" aria-label={`${link.paused ? 'Resume' : 'Pause'} link from ${addressLabel(link.from)}`} onClick={() => edit(link, { paused: !link.paused })}>
            {link.paused ? 'Resume' : 'Pause'}
          </button>
          <button className="btn ghost" aria-label={`Remove link from ${addressLabel(link.from)}`} onClick={() => void deleteAgentLink(agent.id, link.from).then((r) => report([r]))}>
            Remove
          </button>
        </div>
      ))}
      <div className="insp-links-foot">
        <span className="insp-links-title">Discovery</span>
        <div className="seg" role="group" aria-label="Directory listing">
          <button className={`seg-btn${listed ? ' active' : ''}`} onClick={() => !listed && void setAgentListed(agent.id, true).then((r) => report([r]))}>Listed</button>
          <button className={`seg-btn${!listed ? ' active' : ''}`} onClick={() => listed && void setAgentListed(agent.id, false).then((r) => report([r]))}>Hidden</button>
        </div>
        <span className="insp-hint">
          {listed
            ? 'Other agents can find this agent with messages_directory and request a link, which you approve. Being listed grants no access.'
            : 'Other agents cannot find this agent; links you add here still work.'}
        </span>
        {info?.card && <span className="insp-card" title="Directory card (set by the agent)">“{info.card}”</span>}
      </div>
      {error && <div className="insp-error" role="alert">{error}</div>}
      {adding && <AddLinkPalette agent={agent} links={links} onAdd={addLink} onClose={() => setAdding(false)} />}
    </div>
  );
}

// Palette-style picker (same look as the command palette) for the agent that
// should be allowed to message the focused one.
function AddLinkPalette({ agent, links, onAdd, onClose }: {
  agent: SessionView;
  links: AgentLink[];
  onAdd: (picked: SessionView, direction: Direction) => void;
  onClose: () => void;
}) {
  const sessions = useStore((s) => s.sessions);
  const hosts = useStore((s) => s.hosts);
  const [query, setQuery] = useState('');
  const [sel, setSel] = useState(0);
  const [direction, setDirection] = useState<Direction>('in');

  const linked = useMemo(() => new Set(links.map((l) => `${l.from.host}~${l.from.agent}`)), [links]);
  const candidates = useMemo(() => Object.values(sessions)
    .filter((other) => other.id !== agent.id)
    .map((other) => {
      const address = addressOf(other, hosts);
      return { other, already: linked.has(`${address.host}~${address.agent}`) };
    }), [sessions, agent.id, hosts, linked]);
  const filtered = useMemo(() => fuzzyFilter(query, candidates, ({ other }) => `${other.name} ${other.hostName ?? ''} ${other.workspace.repo}`), [query, candidates]);
  useEffect(() => setSel(0), [query]);

  const name = `@${agent.name || agent.id}`;
  const pick = (i: number) => {
    const entry = filtered[i];
    if (entry && (!entry.already || direction === 'both')) onAdd(entry.other, direction);
  };

  return (
    <div className="modal-scrim" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div
        className="modal"
        role="dialog"
        aria-label="Add link"
        onKeyDown={(e) => {
          if (e.key === 'Escape') return onClose();
          if (e.key === 'ArrowDown') {
            e.preventDefault();
            setSel((i) => Math.min(filtered.length - 1, i + 1));
          } else if (e.key === 'ArrowUp') {
            e.preventDefault();
            setSel((i) => Math.max(0, i - 1));
          } else if (e.key === 'Enter') {
            e.preventDefault();
            pick(sel);
          }
        }}
      >
        <input className="q" autoFocus placeholder={`Which agent may message ${name}?`} value={query} onChange={(e) => setQuery(e.target.value)} />
        <div className="link-direction">
          <div className="seg" role="group" aria-label="Link direction">
            <button className={`seg-btn${direction === 'in' ? ' active' : ''}`} onClick={() => setDirection('in')}>It can message {name}</button>
            <button className={`seg-btn${direction === 'both' ? ' active' : ''}`} onClick={() => setDirection('both')}>Both can message each other</button>
          </div>
          <span className="sub">Either way, an agent that asks a question can always receive the answer.</span>
        </div>
        <div className="rows">
          {filtered.length === 0 && <div className="empty">No other agents.</div>}
          {filtered.map(({ other, already }, i) => (
            <div
              key={other.id}
              className={`row${i === sel ? ' sel' : ''}`}
              style={already && direction === 'in' ? { opacity: 0.4 } : undefined}
              onMouseEnter={() => setSel(i)}
              onClick={() => pick(i)}
            >
              <div>
                <div className="primary">@{other.name || other.id}</div>
                <div className="sub">{[other.workspace.repo, other.workspace.branch, other.status].filter(Boolean).join(' · ')}</div>
              </div>
              <div className="meta">
                {already && <span>linked</span>}
                <span>{other.hostName || 'This host'}</span>
              </div>
            </div>
          ))}
        </div>
        <div className="foot">
          <span><span className="kbd">↵</span> add</span>
          <span><span className="kbd">↑↓</span> navigate</span>
          <span><span className="kbd">Esc</span> close</span>
        </div>
      </div>
    </div>
  );
}
