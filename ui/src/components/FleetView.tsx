import { useMemo } from 'react';
import { useStore, LOCAL_HOST_ID } from '../store';
import type { FederationHost } from '../wire';

const NODE_WIDTH = 168;
const NODE_HEIGHT = 66;
const COLUMN_GAP = 34;
const ROW_GAP = 54;
const PADDING_X = 42;
const PADDING_Y = 32;

export interface FleetNode {
  host: FederationHost;
  parentId?: string;
  depth: number;
  x: number;
  y: number;
}

export interface FleetEdge {
  slaveId: string;
  masterId: string;
}

export interface FleetTopology {
  nodes: FleetNode[];
  edges: FleetEdge[];
  width: number;
  height: number;
}

// Hosts from a pre-topology daemon describe direct slaves only. Treating those
// as local children retains a useful, correct one-hop Fleet View while newer
// daemons provide parentId for all descendants.
export function projectFleet(hosts: FederationHost[]): FleetTopology {
  const byID = new Map<string, FederationHost>();
  for (const host of hosts) byID.set(host.id, host);
  if (!byID.has(LOCAL_HOST_ID)) {
    byID.set(LOCAL_HOST_ID, { id: LOCAL_HOST_ID, name: 'This host', local: true, status: 'connected' });
  }

  const parentByID = new Map<string, string | undefined>();
  for (const host of byID.values()) {
    if (host.id === LOCAL_HOST_ID || host.local) {
      parentByID.set(host.id, undefined);
      continue;
    }
    const candidate = host.parentId ?? LOCAL_HOST_ID;
    // An unknown/cyclic parent must not render a misleading or infinite graph.
    parentByID.set(host.id, candidate !== host.id && byID.has(candidate) ? candidate : undefined);
  }

  const children = new Map<string, string[]>();
  for (const [id, parent] of parentByID) {
    if (!parent) continue;
    const siblings = children.get(parent) ?? [];
    siblings.push(id);
    children.set(parent, siblings);
  }

  const roots = [...byID.keys()].filter((id) => !parentByID.get(id));
  const positions = new Map<string, { x: number; depth: number }>();
  let nextLeaf = 0;
  const place = (id: string, depth: number, ancestry: Set<string>): number => {
    if (ancestry.has(id)) return nextLeaf++;
    const descendants = children.get(id) ?? [];
    const branch = new Set(ancestry).add(id);
    if (!descendants.length) {
      const x = nextLeaf++;
      positions.set(id, { x, depth });
      return x;
    }
    const xs = descendants.map((child) => place(child, depth + 1, branch));
    const x = (Math.min(...xs) + Math.max(...xs)) / 2;
    positions.set(id, { x, depth });
    return x;
  };
  // Local is listed first by the store, then retain server order for a stable map.
  for (const root of roots) place(root, 0, new Set());

  const nodes = [...byID.values()].map((host) => {
    const position = positions.get(host.id) ?? { x: nextLeaf++, depth: 0 };
    return {
      host,
      parentId: parentByID.get(host.id),
      depth: position.depth,
      x: PADDING_X + position.x * (NODE_WIDTH + COLUMN_GAP),
      y: PADDING_Y + position.depth * (NODE_HEIGHT + ROW_GAP),
    };
  });
  const edges = nodes.flatMap((node) => node.parentId ? [{ slaveId: node.host.id, masterId: node.parentId }] : []);
  const maxDepth = Math.max(0, ...nodes.map((node) => node.depth));
  return {
    nodes,
    edges,
    width: Math.max(430, PADDING_X * 2 + Math.max(1, nextLeaf) * NODE_WIDTH + Math.max(0, nextLeaf - 1) * COLUMN_GAP),
    height: PADDING_Y * 2 + (maxDepth + 1) * NODE_HEIGHT + maxDepth * ROW_GAP,
  };
}

function version(host: FederationHost): string {
  const build = host.buildVersion ? `build ${host.buildVersion}` : 'build unknown';
  const protocol = host.protocolVersion === undefined ? 'protocol unknown' : `protocol v${host.protocolVersion}`;
  return `${build} · ${protocol}`;
}

function statusLabel(host: FederationHost): string {
  return host.status ?? (host.local ? 'connected' : 'unknown');
}

export function FleetView() {
  const hosts = useStore((state) => state.hosts);
  const setModal = useStore((state) => state.setModal);
  const refreshHosts = useStore((state) => state.refreshHosts);
  const fleet = useMemo(() => projectFleet(hosts), [hosts]);
  const nodeByID = useMemo(() => new Map(fleet.nodes.map((node) => [node.host.id, node])), [fleet.nodes]);

  return (
    <div className="modal-scrim" onMouseDown={(event) => event.target === event.currentTarget && setModal('none')}>
      <div className="modal fleet-modal" role="dialog" aria-modal="true" aria-labelledby="fleet-title" onKeyDown={(event) => event.key === 'Escape' && setModal('none')}>
        <div className="fleet-header">
          <div>
            <div className="primary" id="fleet-title">Fleet View</div>
            <div className="sub">Tandem federation topology</div>
          </div>
          <button type="button" className="fleet-close" onClick={() => setModal('none')} aria-label="Close Fleet View">×</button>
        </div>
        <div className="fleet-legend"><span className="fleet-arrow" aria-hidden="true">↑</span> Arrows point from slave to master</div>
        <div className="fleet-canvas" aria-label="Fleet topology graph">
          <svg className="fleet-graph" viewBox={`0 0 ${fleet.width} ${fleet.height}`} role="img" aria-label="Directed fleet topology; arrows point from slaves to masters">
            <defs>
              <marker id="fleet-master-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                <path d="M 0 0 L 8 4 L 0 8 z" className="fleet-arrowhead" />
              </marker>
            </defs>
            <g className="fleet-edges">
              {fleet.edges.map((edge) => {
                const slave = nodeByID.get(edge.slaveId)!;
                const master = nodeByID.get(edge.masterId)!;
                const fromY = slave.y - NODE_HEIGHT / 2;
                const toY = master.y + NODE_HEIGHT / 2;
                return (
                  <path
                    className="fleet-edge"
                    data-slave-id={edge.slaveId}
                    data-master-id={edge.masterId}
                    key={`${edge.slaveId}->${edge.masterId}`}
                    d={`M ${slave.x} ${fromY} C ${slave.x} ${(fromY + toY) / 2}, ${master.x} ${(fromY + toY) / 2}, ${master.x} ${toY}`}
                    markerEnd="url(#fleet-master-arrow)"
                  >
                    <title>{`${slave.host.name ?? slave.host.id} → ${master.host.name ?? master.host.id} (slave → master)`}</title>
                  </path>
                );
              })}
            </g>
            <g className="fleet-nodes">
              {fleet.nodes.map((node) => (
                <g className={`fleet-node fleet-${statusLabel(node.host)}`} transform={`translate(${node.x - NODE_WIDTH / 2} ${node.y - NODE_HEIGHT / 2})`} key={node.host.id}>
                  <title>{`${node.host.name ?? node.host.id}, ${version(node.host)}`}</title>
                  <rect width={NODE_WIDTH} height={NODE_HEIGHT} rx="8" />
                  <circle cx="15" cy="17" r="4" />
                  <text className="fleet-name" x="26" y="21">{node.host.name ?? node.host.id}</text>
                  <text className="fleet-version" x="12" y="42">{version(node.host)}</text>
                  <text className="fleet-status" x="12" y="57">{statusLabel(node.host)}</text>
                </g>
              ))}
            </g>
          </svg>
        </div>
        <div className="foot fleet-foot">
          <span>{fleet.nodes.length} {fleet.nodes.length === 1 ? 'node' : 'nodes'} · {fleet.edges.length} {fleet.edges.length === 1 ? 'link' : 'links'}</span>
          <button type="button" onClick={refreshHosts}>Refresh</button>
        </div>
      </div>
    </div>
  );
}
