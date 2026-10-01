import { useEffect, useMemo, useState } from 'react';
import { useStore } from '../store';
import type { SessionView } from '../store';
import { addressHostLabel, addressLabel, addressOf } from '../messaging';
import type { AgentLink, AgentLinkInput } from '../wire';

// Defaults for a freshly granted link (docs/agent-messaging.md).
const NEW_LINK = { delivery: 'steer', budgetPerHour: 60, maxHops: 20, paused: false } as const;

function linkInput(link: AgentLink, patch: Partial<AgentLinkInput> = {}): AgentLinkInput {
  return { from: link.from, delivery: link.delivery, budgetPerHour: link.budgetPerHour, maxHops: link.maxHops, paused: link.paused, ...patch };
}

// Inspector > Links: which agents may message the focused agent, plus its
// directory listing. Links are enforced on this agent's host; the daemon owns
// them, so edits only send a command and the list follows agent_links.
export function InspectorLinks({ agent }: { agent: SessionView }) {
  const conn = useStore((s) => s.conn);
  const info = useStore((s) => s.agentLinks[agent.id]);
  const sessions = useStore((s) => s.sessions);
  const hosts = useStore((s) => s.hosts);
  const fetchAgentLinks = useStore((s) => s.fetchAgentLinks);
  const setAgentLink = useStore((s) => s.setAgentLink);
  const deleteAgentLink = useStore((s) => s.deleteAgentLink);
  const setAgentListed = useStore((s) => s.setAgentListed);
  const [pick, setPick] = useState('');
  const [both, setBoth] = useState(false);
  const [error, setError] = useState('');

  // Fetch on focus (and again after a reconnect); agent_links broadcasts keep
  // the list current after that.
  useEffect(() => {
    if (conn === 'connected') fetchAgentLinks(agent.id);
  }, [agent.id, conn, fetchAgentLinks]);
  useEffect(() => {
    setPick('');
    setError('');
  }, [agent.id]);

  const candidates = useMemo(() => Object.values(sessions).filter((other) => other.id !== agent.id), [sessions, agent.id]);
  const links = info?.links ?? [];
  const report = (results: { error?: string }[]) => setError(results.find((r) => r.error)?.error ?? '');

  const addLink = () => {
    const picked = sessions[pick];
    if (!picked) return;
    // The link lives on the recipient's host: picked -> this agent here, and
    // for "both ways" this agent -> picked on the picked agent's host.
    const requests = [setAgentLink(agent.id, { from: addressOf(picked, hosts), ...NEW_LINK })];
    if (both) requests.push(setAgentLink(picked.id, { from: addressOf(agent, hosts), ...NEW_LINK }));
    void Promise.all(requests).then(report);
    setPick('');
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
        <label>
          <input
            type="checkbox"
            checked={info?.listed ?? true}
            onChange={(e) => void setAgentListed(agent.id, e.target.checked).then((r) => report([r]))}
          />
          Listed in directory
        </label>
        {info?.card && <span className="insp-card" title="Directory card (set by the agent)">“{info.card}”</span>}
      </div>
      {links.length === 0 && <div className="insp-card">No agent may message this agent.</div>}
      {links.map((link) => (
        <div key={link.id} className={`insp-link${link.paused ? ' paused' : ''}`}>
          <span className="insp-link-from">
            {addressLabel(link.from)} <small>· {addressHostLabel(hosts, link.from)}</small>
          </span>
          <label>
            delivery
            <select aria-label={`Delivery from ${addressLabel(link.from)}`} value={link.delivery} onChange={(e) => edit(link, { delivery: e.target.value as AgentLink['delivery'] })}>
              <option value="steer">steer</option>
              <option value="queue">queue</option>
            </select>
          </label>
          <label>
            budget/h
            <input
              key={`${link.id}-b-${link.budgetPerHour}`}
              type="number" min={0} aria-label={`Budget per hour from ${addressLabel(link.from)}`}
              defaultValue={link.budgetPerHour}
              onBlur={(e) => commitNumber(link, 'budgetPerHour', e.target.value, 0)}
              onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
            />
          </label>
          <label>
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
          <label>
            <input type="checkbox" checked={link.paused} aria-label={`Pause link from ${addressLabel(link.from)}`} onChange={(e) => edit(link, { paused: e.target.checked })} />
            paused
          </label>
          <button className="btn ghost" aria-label={`Remove link from ${addressLabel(link.from)}`} onClick={() => void deleteAgentLink(agent.id, link.from).then((r) => report([r]))}>
            Remove
          </button>
        </div>
      ))}
      <div className="insp-link-add">
        <select aria-label="Add link from agent" value={pick} onChange={(e) => setPick(e.target.value)}>
          <option value="">Add link…</option>
          {candidates.map((other) => (
            <option key={other.id} value={other.id}>
              @{other.name || other.id} · {other.hostName || other.hostId || 'This host'}
            </option>
          ))}
        </select>
        <label>
          <input type="checkbox" checked={both} onChange={(e) => setBoth(e.target.checked)} />
          both ways
        </label>
        <button className="btn" disabled={!pick} onClick={addLink}>Add</button>
      </div>
      {error && <div className="insp-error" role="alert">{error}</div>}
    </div>
  );
}
