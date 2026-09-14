import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useStore, LOCAL_HOST_ID } from '../store';
import { FleetView, projectFleet } from './FleetView';

afterEach(() => cleanup());

describe('projectFleet', () => {
  it('connects an unscoped legacy slave to the local master', () => {
    const topology = projectFleet([
      { id: LOCAL_HOST_ID, name: 'Control', local: true },
      { id: 'worker', name: 'Worker' },
    ]);

    expect(topology.edges).toEqual([{ slaveId: 'worker', masterId: LOCAL_HOST_ID }]);
  });
});

describe('FleetView', () => {
  it('displays node versions and directs every rendered edge from slave to master', () => {
    useStore.setState({
      hosts: [
        { id: LOCAL_HOST_ID, name: 'Control', local: true, status: 'connected', buildVersion: 'v1.4.0', protocolVersion: 2 },
        { id: 'relay', name: 'Relay', parentId: LOCAL_HOST_ID, status: 'connected', buildVersion: 'v1.3.2', protocolVersion: 2 },
        { id: 'gpu', name: 'GPU worker', parentId: 'relay', status: 'offline', buildVersion: 'v1.2.8', protocolVersion: 1 },
      ],
      setModal: vi.fn(),
      refreshHosts: vi.fn(),
    });

    const view = render(<FleetView />);
    expect(screen.getByText('Control')).toBeTruthy();
    expect(screen.getByText('GPU worker')).toBeTruthy();
    expect(screen.getByText('build v1.4.0 · protocol v2')).toBeTruthy();
    expect(screen.getByText('build v1.2.8 · protocol v1')).toBeTruthy();
    expect(screen.getByText('Arrows point from slave to master')).toBeTruthy();

    const relayEdge = view.container.querySelector<SVGPathElement>('[data-slave-id="relay"][data-master-id="local"]');
    const gpuEdge = view.container.querySelector<SVGPathElement>('[data-slave-id="gpu"][data-master-id="relay"]');
    expect(relayEdge?.getAttribute('marker-end')).toBe('url(#fleet-master-arrow)');
    expect(gpuEdge?.getAttribute('marker-end')).toBe('url(#fleet-master-arrow)');
    expect(relayEdge?.querySelector('title')?.textContent).toBe('Relay → Control (slave → master)');
    expect(gpuEdge?.querySelector('title')?.textContent).toBe('GPU worker → Relay (slave → master)');
  });
});
