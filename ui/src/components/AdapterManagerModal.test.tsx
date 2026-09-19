import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
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
    compatibleVersions: ['1.12.0', '1.9.0'],
    latestVersion: '1.12.0',
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

it('never offers a prerelease as "latest"', async () => {
  // availableVersions leads with 1.12.1-preview.1 because it outranks 1.12.0 in
  // semver; "Update to latest" must still mean the newest release.
  mount([
    adapter({
      currentVersion: '1.9.0',
      installedVersions: ['1.9.0'],
      availableVersions: ['1.12.1-preview.1', '1.12.0', '1.9.0'],
      compatibleVersions: ['1.12.0', '1.9.0'],
      latestVersion: '1.12.0',
    }),
  ]);
  const install = useStore.getState().installAgentDistribution;
  (await screen.findByText('Update to latest')).click();
  await waitFor(() => expect(install).toHaveBeenCalledWith('codex', '1.12.0'));
});

it('marks versions beyond the tested range and still allows installing them', async () => {
  // The claude case: pinned ^0.70.0 while upstream is on 0.79.0. An optimistic
  // check surfaces it, so the UI has to label it rather than hide it.
  mount([
    adapter({
      agent: 'claude',
      package: '@agentclientprotocol/claude-agent-acp',
      constraint: '^0.70.0',
      currentVersion: '0.70.0',
      installedVersions: ['0.70.0'],
      availableVersions: ['0.79.0', '0.70.0'],
      compatibleVersions: ['0.70.0'],
      latestVersion: '0.79.0',
      latestCompatibleVersion: '0.70.0',
    }),
  ]);
  const select = (await screen.findByLabelText('claude version')) as HTMLSelectElement;
  const untested = [...select.options].find((o) => o.value === '0.79.0');
  expect(untested?.textContent).toContain('untested');
  expect(screen.getByText(/newest tested 0\.70\.0, newest published 0\.79\.0/)).toBeTruthy();

  fireEvent.change(select, { target: { value: '0.79.0' } });
  const install = useStore.getState().installAgentDistribution;
  screen.getByText('Use anyway').click();
  await waitFor(() => expect(install).toHaveBeenCalledWith('claude', '0.79.0'));
});

it('flags a current version that has fallen outside the tested range', async () => {
  mount([
    adapter({
      currentVersion: '2.0.0',
      installedVersions: ['2.0.0'],
      availableVersions: ['2.0.0', '1.12.0'],
      compatibleVersions: ['1.12.0'],
      latestVersion: '2.0.0',
      latestCompatibleVersion: '1.12.0',
    }),
  ]);
  expect(await screen.findByText('beyond tested range')).toBeTruthy();
});
