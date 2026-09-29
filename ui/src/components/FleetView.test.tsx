import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useStore, LOCAL_HOST_ID } from '../store';
import { FleetView, projectFleet } from './FleetView';

afterEach(() => cleanup());

describe('projectFleet', () => {
  it('connects an unscoped legacy child to the local parent', () => {
    const topology = projectFleet([
      { id: LOCAL_HOST_ID, name: 'Control', local: true },
      { id: 'worker', name: 'Worker' },
    ]);

    expect(topology.edges).toEqual([{ childId: 'worker', parentId: LOCAL_HOST_ID }]);
  });

  it('draws a visible parent above the local host, with its other children', () => {
    const topology = projectFleet([
      { id: LOCAL_HOST_ID, name: 'Laptop', local: true, parentId: 'up' },
      { id: 'up', name: 'Root', upstream: true, access: 'operate' },
      { id: 'sibling', name: 'Builder', upstream: true, parentId: 'up', access: 'view' },
      { id: 'worker', name: 'Worker' },
    ]);

    expect(topology.edges).toEqual(expect.arrayContaining([
      { childId: LOCAL_HOST_ID, parentId: 'up' },
      { childId: 'sibling', parentId: 'up' },
      { childId: 'worker', parentId: LOCAL_HOST_ID },
    ]));
    expect(topology.nodes.find((node) => node.host.id === 'up')?.depth).toBe(0);
    expect(topology.nodes.find((node) => node.host.id === 'worker')?.depth).toBe(2);
  });

  it('keeps every node fully inside the graph bounds', () => {
    const topology = projectFleet([
      { id: LOCAL_HOST_ID, name: 'Control', local: true },
      { id: 'worker', name: 'Worker' },
    ]);

    expect(topology.nodes.every((node) => (
      node.x >= 130
      && node.x <= topology.width - 130
      && node.y >= 33
      && node.y <= topology.height - 33
    ))).toBe(true);
  });
});

describe('FleetView', () => {
  it('displays node versions and directs every rendered edge from child to parent', () => {
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
    expect(screen.getByText('Child → parent')).toBeTruthy();
    expect(screen.getByText('TCP source → destination')).toBeTruthy();

    expect(view.container.querySelector('.fleet-stage')).toBeTruthy();
    const canvas = screen.getByLabelText('Fleet topology graph. Drag or use arrow keys to pan.');
    const stage = view.container.querySelector<HTMLElement>('.fleet-stage')!;
    const recenter = screen.getByRole('button', { name: 'Recenter' }) as HTMLButtonElement;
    expect(canvas.getAttribute('tabindex')).toBe('0');
    expect(recenter.disabled).toBe(true);
    fireEvent.keyDown(canvas, { key: 'ArrowRight' });
    expect(stage.style.transform).toBe('translate(-40px, 0px)');
    expect(recenter.disabled).toBe(false);
    fireEvent.click(recenter);
    expect(stage.style.transform).toBe('translate(0px, 0px)');
    expect(view.container.querySelector('.fleet-graph')?.getAttribute('width')).toBeTruthy();
    expect(view.container.querySelectorAll('clipPath')).toHaveLength(3);
    expect(view.container.querySelectorAll('.fleet-node > g[clip-path]')).toHaveLength(3);
    const relayEdge = view.container.querySelector<SVGPathElement>('[data-child-id="relay"][data-parent-id="local"]');
    const gpuEdge = view.container.querySelector<SVGPathElement>('[data-child-id="gpu"][data-parent-id="relay"]');
    expect(relayEdge?.getAttribute('marker-end')).toBe('url(#fleet-parent-arrow)');
    expect(gpuEdge?.getAttribute('marker-end')).toBe('url(#fleet-parent-arrow)');
    expect(relayEdge?.querySelector('title')?.textContent).toBe('Relay → Control (child → parent)');
    expect(gpuEdge?.querySelector('title')?.textContent).toBe('GPU worker → Relay (child → parent)');
  });

  it('draws a TCP arrow from whichever end dialed the link', () => {
    useStore.setState({
      hosts: [
        { id: LOCAL_HOST_ID, name: 'Control', local: true, status: 'connected' },
        { id: 'laptop', name: 'Laptop', parentId: LOCAL_HOST_ID, status: 'connected', dialer: 'child' },
        { id: 'box', name: 'Container', parentId: LOCAL_HOST_ID, status: 'connected', dialer: 'parent' },
        { id: 'legacy', name: 'Legacy', parentId: LOCAL_HOST_ID, status: 'connected' },
      ],
      setModal: vi.fn(),
      refreshHosts: vi.fn(),
    });

    const view = render(<FleetView />);
    const dialed = view.container.querySelector('.fleet-edge-tcp[data-tcp-src="laptop"][data-tcp-dst="local"]');
    const adopted = view.container.querySelector('.fleet-edge-tcp[data-tcp-src="local"][data-tcp-dst="box"]');
    expect(dialed?.getAttribute('marker-end')).toBe('url(#fleet-tcp-arrow)');
    expect(adopted?.querySelector('title')?.textContent).toBe('Control → Container (TCP source → destination)');
    expect(view.container.querySelectorAll('.fleet-graph .fleet-edge-tcp')).toHaveLength(2);
    expect(view.container.querySelectorAll('.fleet-graph .fleet-edge-parent')).toHaveLength(3);
  });

  it('renames a host inline', async () => {
    const renameHost = vi.fn().mockResolvedValue({});
    useStore.setState({
      hosts: [
        { id: LOCAL_HOST_ID, name: 'Control', local: true, status: 'connected' },
        { id: 'worker', name: 'Worker', status: 'connected' },
      ],
      setModal: vi.fn(),
      refreshHosts: vi.fn(),
      renameHost,
    });

    render(<FleetView />);
    fireEvent.click(screen.getByRole('button', { name: 'Rename Worker' }));
    const input = screen.getByLabelText('Display name for Worker');
    fireEvent.change(input, { target: { value: '  GPU box ' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(renameHost).toHaveBeenCalledOnce();
    expect(renameHost).toHaveBeenCalledWith('worker', 'GPU box');
    expect(screen.queryByLabelText('Display name for Worker')).toBeNull();
  });

  it('pulses a host requested by the dock Details action', () => {
    const clearFleetFocusHost = vi.fn();
    useStore.setState({
      hosts: [
        { id: LOCAL_HOST_ID, name: 'Control', local: true, status: 'connected' },
        { id: 'worker', name: 'Worker', status: 'connected' },
      ],
      fleetFocusHostId: 'worker',
      clearFleetFocusHost,
    });

    const view = render(<FleetView />);
    expect(view.container.querySelector('[data-host-id="worker"]')?.classList.contains('fleet-node-pulse')).toBe(true);
    expect(clearFleetFocusHost).toHaveBeenCalledOnce();
  });
});
