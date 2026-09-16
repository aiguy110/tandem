import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { LOCAL_HOST_ID, useStore } from '../store';
import type { SessionView } from '../store';
import { SessionsRail } from './AgentsRail';

afterEach(() => {
  cleanup();
  useStore.setState({ sessions: {}, order: [], focusedId: null });
});

function session(): SessionView {
  return {
    id: 'fed~worker~agent-1',
    name: 'Remote agent',
    hostId: 'worker',
    hostName: 'Worker',
    workspace: {
      kind: 'worktree', repo: 'repo', repoPath: '/missing/repo', branch: 'agent-1', cwd: '/managed/agent-1',
    },
    status: 'idle',
    events: [],
    lastSeq: 0,
    pendingApprovals: [],
    turnNotifications: [],
    hasPty: false,
    shellExited: false,
    shellExitMessage: null,
    browserActive: false,
    browserOwner: 'agent',
    browserTakeoverHeld: false,
    takeovers: [],
    sessionConfig: null,
    controlMode: 'transcript',
    adapter: 'acp',
    canHandoff: true,
  } as unknown as SessionView;
}

describe('SessionsRail close recovery', () => {
  it('offers a forced retry when a clean preview belongs to an orphaned worktree', async () => {
    const closeAgent = vi.fn()
      .mockResolvedValueOnce({ error: 'orphaned_worktree: repository no longer exists; close with force:true' })
      .mockResolvedValueOnce({});
    const agent = session();
    useStore.setState({
      sessions: { [agent.id]: agent },
      order: [agent.id],
      focusedId: agent.id,
      hosts: [
        { id: LOCAL_HOST_ID, name: 'This host', status: 'connected', local: true },
        { id: 'worker', name: 'Worker', status: 'connected' },
      ],
      getClosePreview: vi.fn().mockResolvedValue({ kind: 'worktree' }),
      closeAgent,
    });

    const view = render(<SessionsRail />);
    fireEvent.contextMenu(view.container.querySelector('.session-row')!);
    fireEvent.click(await screen.findByRole('menuitem', { name: 'Delete' }));

    expect(await screen.findByText('Orphaned worktree')).toBeTruthy();
    expect(closeAgent).toHaveBeenNthCalledWith(1, agent.id, false, true);

    fireEvent.click(screen.getByRole('button', { name: 'Delete agent' }));
    await waitFor(() => expect(closeAgent).toHaveBeenNthCalledWith(2, agent.id, true, true, false));
  });
});
