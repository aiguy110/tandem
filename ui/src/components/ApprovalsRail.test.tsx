import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import { ApprovalsRail } from './ApprovalsRail';

afterEach(cleanup);

it('opens the ACP manager for the notifying host instead of sending a ui: action', () => {
  const act = vi.fn(async () => ({}));
  const openAdapters = vi.fn();
  useStore.setState({
    approvalsRailCollapsed: false,
    actOnSystemNotification: act,
    openAdapters,
    systemNotifications: [{
      id: 'remote-1/agent-update:omp', severity: 'failure', title: 'omp ACP fork rebase could not start',
      message: 'profile option is no longer available', hostId: 'remote-1', hostName: 'LLU',
      actions: [{ id: 'rebase', label: 'Retry', primary: true }, { id: 'ui:open-adapters', label: 'Choose profile' }],
    }],
  });
  render(<ApprovalsRail />);
  screen.getByText('Choose profile').click();
  expect(openAdapters).toHaveBeenCalledWith('remote-1');
  expect(act).not.toHaveBeenCalled();
});
