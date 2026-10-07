import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import type { McpServerStatus } from '../wire';
import { McpServersModal } from './McpServersModal';

afterEach(() => {
  cleanup();
  useStore.setState({ mcpServers: null });
});

function row(overrides: Partial<McpServerStatus> = {}): McpServerStatus {
  return { key: 'k1', name: 'developerhub', url: 'https://ai.developerhub.io/mcp', state: 'needs_auth', configured: true, ...overrides };
}

function mount(rows: McpServerStatus[]) {
  const authorize = vi.fn(async () => {});
  const signOut = vi.fn(async () => {});
  useStore.setState({
    mcpServers: rows,
    listMcpServers: async () => rows,
    authorizeMcpServer: authorize,
    signOutMcpServer: signOut,
    recheckMcpServer: async () => {},
  });
  render(<McpServersModal />);
  return { authorize, signOut };
}

it('offers Authorize for a server that needs sign-in', async () => {
  const { authorize } = mount([row()]);
  expect(await screen.findByText('needs sign-in')).toBeTruthy();
  expect(screen.getByText(/1 awaiting sign-in/)).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Authorize' }));
  expect(authorize).toHaveBeenCalledWith('k1');
});

it('shows an authorized server with re-authorize and sign-out', async () => {
  const { signOut } = mount([row({ state: 'authorized', scope: 'editor', refreshable: true, expiresAt: '2026-10-07T16:00:00Z' })]);
  expect(await screen.findByText('authorized')).toBeTruthy();
  expect(screen.getByText(/Scope: editor/)).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Re-authorize' })).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
  expect(signOut).toHaveBeenCalledWith('k1');
});

it('passes through servers with their own credentials without actions', async () => {
  mount([row({ state: 'static' })]);
  expect(await screen.findByText('header auth')).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Authorize' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'Check again' })).toBeNull();
});
