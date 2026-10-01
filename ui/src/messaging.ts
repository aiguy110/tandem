// Agent-messaging helpers (docs/agent-messaging.md): mapping between the UI's
// rail agents (parent-namespaced IDs, route-relative host IDs) and the stable
// addresses the messaging protocol uses (host node ID + host-local session ID).

import type { AgentAddress, FederationHost, SessionSummary } from './wire';

const LOCAL_HOST = 'local';
const REMOTE_ID_PREFIX = 'fed~';
const LEGACY_REMOTE_ID_PREFIX = 'federation~';

// The minimal session shape these helpers need (satisfied by SessionView).
type AgentRef = Pick<SessionSummary, 'id' | 'name' | 'hostId'> & { hostName?: string };

function decodeBase64Url(value: string): string | null {
  try {
    const padded = value.replace(/-/g, '+').replace(/_/g, '/');
    return atob(padded + '='.repeat((4 - (padded.length % 4)) % 4));
  } catch {
    return null;
  }
}

// Splits a parent-namespaced rail ID (`fed~<hostId>~<sessionId>`, or the older
// base64 form) into its host and host-local session ID. A plain ID is local.
export function splitSessionId(id: string): { hostId?: string; sessionId: string } {
  if (id.startsWith(REMOTE_ID_PREFIX)) {
    const rest = id.slice(REMOTE_ID_PREFIX.length);
    const cut = rest.indexOf('~');
    if (cut > 0 && cut < rest.length - 1) return { hostId: rest.slice(0, cut), sessionId: rest.slice(cut + 1) };
  } else if (id.startsWith(LEGACY_REMOTE_ID_PREFIX)) {
    const [host, agent] = id.slice(LEGACY_REMOTE_ID_PREFIX.length).split('~');
    const hostId = host ? decodeBase64Url(host) : null;
    const sessionId = agent ? decodeBase64Url(agent) : null;
    if (hostId && sessionId) return { hostId, sessionId };
  }
  return { sessionId: id };
}

// Where a command for this agent goes: the owning host (omitted for the local
// daemon, so an older daemon sees the commands it always has) and the
// host-local session ID.
export function sessionRoute(agent: Pick<AgentRef, 'id' | 'hostId'>): { hostId?: string; sessionId: string } {
  const split = splitSessionId(agent.id);
  const hostId = agent.hostId ?? split.hostId;
  return { ...(!hostId || hostId === LOCAL_HOST ? {} : { hostId }), sessionId: split.sessionId };
}

function findHost(hosts: FederationHost[], hostId: string | undefined): FederationHost | undefined {
  return hosts.find((host) => (!hostId || hostId === LOCAL_HOST ? host.local || host.id === LOCAL_HOST : host.id === hostId));
}

// The stable node ID of the host that owns this agent. Falls back to the
// route ID for a host that does not report one (a remote's route ID is its own
// node ID when it is directly connected).
export function hostNodeId(hosts: FederationHost[], hostId: string | undefined): string {
  const host = findHost(hosts, hostId);
  return host?.nodeId ?? (!hostId || hostId === LOCAL_HOST ? LOCAL_HOST : hostId);
}

export function addressOf(agent: AgentRef, hosts: FederationHost[]): AgentAddress {
  const route = sessionRoute(agent);
  return { host: hostNodeId(hosts, route.hostId), agent: route.sessionId, name: agent.name };
}

// `<host>~<agent>`, the string form used by the message tools.
export function addressString(address: AgentAddress): string {
  return `${address.host}~${address.agent}`;
}

export function sameAddress(a: AgentAddress, b: AgentAddress): boolean {
  return a.host === b.host && a.agent === b.agent;
}

// The rail agent an address refers to, if it is in the store.
export function findAgentByAddress<T extends AgentRef>(sessions: Record<string, T>, hosts: FederationHost[], address: AgentAddress): T | undefined {
  return Object.values(sessions).find((agent) => {
    const route = sessionRoute(agent);
    return route.sessionId === address.agent && hostNodeId(hosts, route.hostId) === address.host;
  });
}

// A readable host label for an address: the host's display name when known.
export function addressHostLabel(hosts: FederationHost[], address: AgentAddress): string {
  const host = hosts.find((entry) => (entry.nodeId ?? entry.id) === address.host || (entry.local && address.host === LOCAL_HOST));
  return host?.name ?? address.host;
}

export function addressLabel(address: AgentAddress): string {
  return `@${address.name || address.agent}`;
}

// `@agent:<host>/<agent>` mentions. The backend applies the same rule: a
// segment is the display name when it is a plain token, else the stable ID
// (host node ID, host-local session ID) so the reference stays unambiguous.
const REF_SEGMENT = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

function refSegment(name: string | undefined, fallback: string): string {
  return name && REF_SEGMENT.test(name) ? name : fallback;
}

// The host name the UI shows for a rail agent (its node ID when unnamed).
export function agentHostLabel(agent: AgentRef, hosts: FederationHost[]): string {
  const route = sessionRoute(agent);
  return findHost(hosts, route.hostId)?.name ?? agent.hostName ?? hostNodeId(hosts, route.hostId);
}

// The mention for a rail agent, e.g. `@agent:bifrost/slow-drag`.
export function agentRef(agent: AgentRef, hosts: FederationHost[]): string {
  const route = sessionRoute(agent);
  const host = refSegment(findHost(hosts, route.hostId)?.name ?? agent.hostName, hostNodeId(hosts, route.hostId));
  return `@agent:${host}/${refSegment(agent.name, route.sessionId)}`;
}

// The mention for an address whose agent may not be in the store.
export function addressRef(hosts: FederationHost[], address: AgentAddress): string {
  return `@agent:${refSegment(addressHostLabel(hosts, address), address.host)}/${refSegment(address.name, address.agent)}`;
}

// Elapsed time as a compact duration: "42s", "3m 12s", "1h 05m".
export function formatElapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h > 0) return `${h}h ${String(m).padStart(2, '0')}m`;
  if (m > 0) return `${m}m ${String(s).padStart(2, '0')}s`;
  return `${s}s`;
}

// Hosts where this Tandem may change messaging state (the kill switch).
export function controllableHosts(hosts: FederationHost[]): FederationHost[] {
  return hosts.filter((host) => {
    const reachable = host.local || host.id === LOCAL_HOST || host.status === 'connected' || host.status === 'accepted';
    // Absent access is a daemon predating access control: full access.
    return reachable && (!host.access || host.access === 'operate' || host.access === 'admin');
  });
}

// Hosts whose messaging state this Tandem can read (view or better).
export function readableHosts(hosts: FederationHost[]): FederationHost[] {
  return hosts.filter((host) => {
    const reachable = host.local || host.id === LOCAL_HOST || host.status === 'connected' || host.status === 'accepted';
    return reachable && host.access !== 'none';
  });
}
