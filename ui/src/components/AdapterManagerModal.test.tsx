import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import type { ForkTracking, ManagedAdapter } from '../wire';
import { AdapterManagerModal } from './AdapterManagerModal';

afterEach(cleanup);

function adapter(overrides: Partial<ManagedAdapter> = {}): ManagedAdapter {
  return {
    agent: 'codex',
    package: '@agentclientprotocol/codex-acp',
    constraint: '^1.8.0',
    currentVersion: '1.12.0',
    installedVersions: ['1.12.0'],
    availableVersions: ['1.12.0', '1.9.0'],
    ...overrides,
  };
}

function fork(overrides: Partial<ForkTracking> = {}): ForkTracking {
  return {
    repo: 'https://github.com/me/pi-acp',
    ref: 'tandem',
    commit: '52f61f3850a883b9b5a3bb3245908bd5d95b7d43',
    shortCommit: '52f61f3850a8',
    clone: '/rt/forks/pi',
    upstreamPackage: 'pi-acp',
    upstreamRepo: 'https://github.com/svkozak/pi-acp',
    upstreamVersion: '0.0.33',
    reason: 'Emits usage_update',
    upstreamPrs: [114],
    installed: true,
    ...overrides,
  };
}

function mount(rows: ManagedAdapter[]) {
  const install = vi.fn(async () => {});
  useStore.setState({ listAgentDistributions: async () => rows, installAgentDistribution: install });
  render(<AdapterManagerModal />);
  return install;
}

it('offers a version picker for a normal adapter', async () => {
  mount([adapter()]);
  expect(await screen.findByLabelText('codex version')).toBeTruthy();
  expect(screen.getByText(/current 1\.12\.0/)).toBeTruthy();
});

it('shows a fork-tracked adapter as a distinct state with no version picker', async () => {
  mount([adapter({ agent: 'pi', package: 'pi-acp', constraint: '~0.0.33', fork: fork() })]);

  expect(await screen.findByText('tracking fork')).toBeTruthy();
  expect(screen.getByText('level with upstream')).toBeTruthy();
  expect(screen.getByText('Emits usage_update')).toBeTruthy();
  expect(screen.getByText('52f61f3850a8')).toBeTruthy();
  // The npm dropdown must not appear: installing a published version over a
  // tracked fork is refused by the daemon, so offering it would be a dead end.
  expect(screen.queryByLabelText('pi version')).toBeNull();
  expect(screen.queryByText('Update to latest')).toBeNull();
  expect(screen.getByText(/1 tracking a fork/)).toBeTruthy();
});

it('flags a fork that has fallen behind upstream', async () => {
  mount([adapter({ agent: 'pi', package: 'pi-acp', fork: fork({ latestUpstreamVersion: '0.0.34' }) })]);

  expect(await screen.findByText('upstream 0.0.34 available')).toBeTruthy();
  expect(screen.queryByText('level with upstream')).toBeNull();
  expect(screen.getByText(/checks whether the change was upstreamed/)).toBeTruthy();
});

it('marks a re-pinned fork whose tree is not built yet', async () => {
  mount([adapter({ agent: 'pi', package: 'pi-acp', fork: fork({ installed: false }) })]);
  expect(await screen.findByText('not built yet')).toBeTruthy();
});

it('links the pinned commit and the watched upstream PRs', async () => {
  mount([adapter({ agent: 'pi', package: 'pi-acp', fork: fork() })]);

  await waitFor(() => screen.getByText('tracking fork'));
  const commit = screen.getByText('github.com/me/pi-acp').closest('a');
  expect(commit?.getAttribute('href')).toBe(
    'https://github.com/me/pi-acp/commit/52f61f3850a883b9b5a3bb3245908bd5d95b7d43',
  );
  expect(screen.getByText('#114').closest('a')?.getAttribute('href')).toBe(
    'https://github.com/svkozak/pi-acp/pull/114',
  );
});

it('keeps normal and fork rows side by side', async () => {
  mount([adapter(), adapter({ agent: 'pi', package: 'pi-acp', fork: fork() })]);
  expect(await screen.findByLabelText('codex version')).toBeTruthy();
  expect(screen.getByText('tracking fork')).toBeTruthy();
  expect(screen.getByText(/2 managed adapters, 1 tracking a fork/)).toBeTruthy();
});
