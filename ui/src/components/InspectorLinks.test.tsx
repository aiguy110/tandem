import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { LOCAL_HOST_ID, useStore } from '../store';
import type { SessionView } from '../store';
import { WsClient } from '../ws/client';
import type { ClientMsg } from '../wire';
import { InspectorLinks } from './InspectorLinks';

const initialState = useStore.getState();
let sent: ClientMsg[] = [];

beforeEach(() => {
  sent = [];
  vi.spyOn(WsClient.prototype, 'send').mockImplementation((m) => { sent.push(m); });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  useStore.setState(initialState, true);
});

function view(overrides: Partial<SessionView>): SessionView {
  return {
    name: 'agent', status: 'idle', events: [], lastSeq: 0, pendingApprovals: [], turnNotifications: [],
    workspace: { kind: 'existing', repo: 'repo', repoPath: '/repo', branch: 'main', cwd: '/repo' },
    waitingOn: [], openAsks: 0,
    ...overrides,
  } as SessionView;
}

const hosts = [
  { id: LOCAL_HOST_ID, name: 'This host', status: 'connected' as const, local: true, nodeId: 'node-local' },
  { id: 'route-b', name: 'Builder', status: 'connected' as const, nodeId: 'node-b' },
];
const local = view({ id: 'sess-local', name: 'planner' });
const remote = view({ id: 'fed~route-b~sess-remote', name: 'api-worker', hostId: 'route-b', hostName: 'Builder' });

function setup(extra: Partial<ReturnType<typeof useStore.getState>> = {}) {
  useStore.setState({
    ...initialState,
    conn: 'connected', hosts,
    sessions: { [local.id]: local, [remote.id]: remote }, order: [local.id, remote.id],
    ...extra,
  }, true);
  return render(<InspectorLinks agent={local} />);
}

describe('InspectorLinks', () => {
  it('fetches the focused agent\'s links, routed to its own host', () => {
    setup();
    expect(sent).toContainEqual({ t: 'list_agent_links', sessionId: 'sess-local' });

    cleanup();
    sent = [];
    render(<InspectorLinks agent={remote} />);
    expect(sent).toContainEqual({ t: 'list_agent_links', hostId: 'route-b', sessionId: 'sess-remote' });
  });

  it('lists inbound links and edits, pauses, and removes them', () => {
    const from = { host: 'node-b', agent: 'sess-remote', name: 'api-worker' };
    setup({
      agentLinks: { [local.id]: { listed: true, card: 'plans work', links: [{ id: 'lnk_1', from, to: 'sess-local', delivery: 'steer', budgetPerHour: 60, maxHops: 20, paused: false, source: 'user', createdAt: '2026-01-01T00:00:00Z', usedLastHour: 7 }] } },
    });
    expect(screen.getByText('@api-worker')).toBeTruthy();
    expect(screen.getByText(/used 7\/60/)).toBeTruthy();
    expect(screen.getByText(/plans work/)).toBeTruthy();
    sent = [];

    fireEvent.click(screen.getByRole('button', { name: 'queue' }));
    expect(sent).toContainEqual({ t: 'set_agent_link', sessionId: 'sess-local', corrId: expect.any(String), link: { from, delivery: 'queue', budgetPerHour: 60, maxHops: 20, paused: false } });

    fireEvent.click(screen.getByRole('button', { name: 'Pause link from @api-worker' }));
    expect(sent.at(-1)).toMatchObject({ t: 'set_agent_link', link: { paused: true } });

    const budget = screen.getByLabelText('Budget per hour from @api-worker');
    fireEvent.change(budget, { target: { value: '5' } });
    fireEvent.blur(budget);
    expect(sent.at(-1)).toMatchObject({ t: 'set_agent_link', link: { budgetPerHour: 5, maxHops: 20 } });

    fireEvent.click(screen.getByLabelText('Remove link from @api-worker'));
    expect(sent.at(-1)).toMatchObject({ t: 'delete_agent_link', sessionId: 'sess-local', from });
  });

  it('adds a one-way link to this agent\'s host', () => {
    setup();
    sent = [];
    fireEvent.click(screen.getByRole('button', { name: '+ Add link' }));
    fireEvent.click(screen.getByText('@api-worker'));

    const links = sent.filter((m) => m.t === 'set_agent_link');
    expect(links).toHaveLength(1);
    expect(links[0]).toMatchObject({
      sessionId: 'sess-local',
      link: { from: { host: 'node-b', agent: 'sess-remote', name: 'api-worker' }, delivery: 'steer', budgetPerHour: 60, maxHops: 20, paused: false },
    });
    expect(links[0]).not.toHaveProperty('hostId');
  });

  it('both ways also grants the reverse link on the other agent\'s host', () => {
    setup();
    sent = [];
    fireEvent.click(screen.getByRole('button', { name: '+ Add link' }));
    fireEvent.click(screen.getByRole('button', { name: 'Both can message each other' }));
    fireEvent.click(screen.getByText('@api-worker'));

    const links = sent.filter((m) => m.t === 'set_agent_link');
    expect(links).toHaveLength(2);
    // picked -> this agent, enforced on this agent's (local) host.
    expect(links[0]).toMatchObject({ sessionId: 'sess-local', link: { from: { host: 'node-b', agent: 'sess-remote' } } });
    expect(links[0]).not.toHaveProperty('hostId');
    // this agent -> picked, enforced on the picked agent's host by its host-local ID.
    expect(links[1]).toMatchObject({ hostId: 'route-b', sessionId: 'sess-remote', link: { from: { host: 'node-local', agent: 'sess-local', name: 'planner' } } });
  });

  it('never offers the focused agent itself and toggles the directory listing', () => {
    setup();
    fireEvent.click(screen.getByRole('button', { name: '+ Add link' }));
    const rows = Array.from(screen.getByRole('dialog').querySelectorAll('.row .primary')).map((o) => o.textContent);
    expect(rows).toEqual(['@api-worker']);
    sent = [];
    fireEvent.click(screen.getByRole('button', { name: 'Hidden' }));
    expect(sent).toContainEqual({ t: 'set_agent_listed', sessionId: 'sess-local', listed: false, corrId: expect.any(String) });
  });

  it('both ways on an existing link only adds the reverse, keeping its settings', () => {
    const from = { host: 'node-b', agent: 'sess-remote', name: 'api-worker' };
    setup({
      agentLinks: { [local.id]: { listed: true, card: '', links: [{ id: 'lnk_1', from, to: 'sess-local', delivery: 'queue', budgetPerHour: 5, maxHops: 3, paused: false, source: 'user', createdAt: '2026-01-01T00:00:00Z', usedLastHour: 0 }] } },
    });
    sent = [];
    fireEvent.click(screen.getByRole('button', { name: '+ Add link' }));
    fireEvent.click(screen.getByRole('button', { name: 'Both can message each other' }));
    fireEvent.click(screen.getByRole('dialog').querySelector('.row')!);
    const links = sent.filter((m) => m.t === 'set_agent_link');
    expect(links).toHaveLength(1);
    expect(links[0]).toMatchObject({ hostId: 'route-b', sessionId: 'sess-remote' });
  });
});
