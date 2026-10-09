import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import type { KeyboardEvent, PointerEvent } from 'react';
import { useStore, LOCAL_HOST_ID } from '../store';
import type { FederationHost } from '../wire';

const NODE_WIDTH = 260;
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
  childId: string;
  parentId: string;
  // Which end opened the link's TCP connection, when the daemon reports it.
  dialer?: 'child' | 'parent';
}

export interface FleetTopology {
  nodes: FleetNode[];
  edges: FleetEdge[];
  width: number;
  height: number;
}

// Hosts from a pre-topology daemon describe direct children only. Treating those
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
      // A Tandem that can see its parent draws that parent above itself.
      parentByID.set(host.id, host.parentId && byID.has(host.parentId) ? host.parentId : undefined);
      continue;
    }
    // The topmost host this Tandem sees through its parent has no parent;
    // only a parentless descendant is a legacy direct child of this host.
    const candidate = host.parentId ?? (host.upstream ? undefined : LOCAL_HOST_ID);
    if (!candidate) {
      parentByID.set(host.id, undefined);
      continue;
    }
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
      x: PADDING_X + NODE_WIDTH / 2 + position.x * (NODE_WIDTH + COLUMN_GAP),
      y: PADDING_Y + NODE_HEIGHT / 2 + position.depth * (NODE_HEIGHT + ROW_GAP),
    };
  });
  const edges = nodes.flatMap((node): FleetEdge[] => {
    if (!node.parentId) return [];
    return [node.host.dialer ? { childId: node.host.id, parentId: node.parentId, dialer: node.host.dialer } : { childId: node.host.id, parentId: node.parentId }];
  });
  const maxDepth = Math.max(0, ...nodes.map((node) => node.depth));
  return {
    nodes,
    edges,
    width: PADDING_X * 2 + Math.max(1, nextLeaf) * NODE_WIDTH + Math.max(0, nextLeaf - 1) * COLUMN_GAP,
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

// Restricted access is worth saying; full (admin) access is the norm.
function accessLabel(host: FederationHost): string {
  return host.access && host.access !== 'admin' ? ` · ${host.access} access` : '';
}

// Parent and TCP arrows share a link, so each is drawn a little to one side.
const EDGE_OFFSET = 5;

function edgePath(fromX: number, fromY: number, toX: number, toY: number): string {
  const midY = (fromY + toY) / 2;
  return `M ${fromX} ${fromY} C ${fromX} ${midY}, ${toX} ${midY}, ${toX} ${toY}`;
}

function hostName(host: FederationHost): string {
  return host.name ?? host.id;
}

export function FleetView() {
  const hosts = useStore((state) => state.hosts);
  const renameHost = useStore((state) => state.renameHost);
  const setModal = useStore((state) => state.setModal);
  const refreshHosts = useStore((state) => state.refreshHosts);
  const focusHostId = useStore((state) => state.fleetFocusHostId);
  const clearFocusHost = useStore((state) => state.clearFleetFocusHost);
  const openHostMetrics = useStore((state) => state.openHostMetrics);
  const fleet = useMemo(() => projectFleet(hosts), [hosts]);
  const nodeByID = useMemo(() => new Map(fleet.nodes.map((node) => [node.host.id, node])), [fleet.nodes]);
  const panStart = useRef({ x: 0, y: 0, offsetX: 0, offsetY: 0 });
  const [panOffset, setPanOffset] = useState({ x: 0, y: 0 });
  const [panning, setPanning] = useState(false);
  const [pulseHostId, setPulseHostId] = useState<string | null>(focusHostId);
  const canvasRef = useRef<HTMLDivElement>(null);
  const [editing, setEditingState] = useState<{ hostId: string; name: string } | null>(null);
  // Enter unmounts the input, whose blur must not commit the same edit again.
  const editingRef = useRef(editing);
  const setEditing = (next: { hostId: string; name: string } | null) => {
    editingRef.current = next;
    setEditingState(next);
  };
  const [renameError, setRenameError] = useState<string | null>(null);

  const commitRename = async () => {
    const pending = editingRef.current;
    if (!pending) return;
    const { hostId, name } = pending;
    setEditing(null);
    const current = hosts.find((host) => host.id === hostId);
    if (current && name.trim() === (current.name ?? '')) return;
    const result = await renameHost(hostId, name.trim());
    setRenameError(result.error ? `Rename failed: ${result.error}` : null);
  };

  // A host-level Details action opens this modal at its normal centered
  // position, then moves only far enough to reveal the requested node with a
  // small breathing room around it.
  useLayoutEffect(() => {
    if (!focusHostId || !canvasRef.current) return;
    const canvas = canvasRef.current;
    const node = Array.from(canvas.querySelectorAll<SVGGElement>('[data-host-id]'))
      .find((candidate) => candidate.dataset.hostId === focusHostId);
    if (!node) return;
    const margin = 28;
    const bounds = canvas.getBoundingClientRect();
    const target = node.getBoundingClientRect();
    const left = bounds.left + margin;
    const right = bounds.right - margin;
    const top = bounds.top + margin;
    const bottom = bounds.bottom - margin;
    const x = target.left < left ? left - target.left : target.right > right ? right - target.right : 0;
    const y = target.top < top ? top - target.top : target.bottom > bottom ? bottom - target.bottom : 0;
    setPanOffset((current) => ({ x: current.x + x, y: current.y + y }));
    setPulseHostId(focusHostId);
    clearFocusHost();
  }, [focusHostId, fleet.nodes, clearFocusHost]);

  useEffect(() => {
    if (!pulseHostId) return;
    const timeout = window.setTimeout(() => setPulseHostId(null), 2300);
    return () => window.clearTimeout(timeout);
  }, [pulseHostId]);

  const beginPan = (event: PointerEvent<HTMLDivElement>) => {
    if (event.button !== 0) return;
    const canvas = event.currentTarget;
    panStart.current = { x: event.clientX, y: event.clientY, offsetX: panOffset.x, offsetY: panOffset.y };
    canvas.setPointerCapture(event.pointerId);
    setPanning(true);
  };

  const movePan = (event: PointerEvent<HTMLDivElement>) => {
    if (!panning) return;
    const start = panStart.current;
    setPanOffset({
      x: start.offsetX + event.clientX - start.x,
      y: start.offsetY + event.clientY - start.y,
    });
  };

  const endPan = (event: PointerEvent<HTMLDivElement>) => {
    if (event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId);
    setPanning(false);
  };

  const panWithKeyboard = (event: KeyboardEvent<HTMLDivElement>) => {
    const distance = event.shiftKey ? 120 : 40;
    const offsets: Record<string, [number, number]> = {
      ArrowLeft: [-distance, 0],
      ArrowRight: [distance, 0],
      ArrowUp: [0, -distance],
      ArrowDown: [0, distance],
    };
    const offset = offsets[event.key];
    if (!offset) return;
    event.preventDefault();
    setPanOffset((current) => ({ x: current.x - offset[0], y: current.y - offset[1] }));
  };

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
        <div className="fleet-legend">
          <span className="fleet-legend-item">
            <svg width="26" height="10" aria-hidden="true"><line x1="1" y1="5" x2="18" y2="5" className="fleet-edge fleet-edge-parent" markerEnd="url(#fleet-parent-arrow)" /></svg>
            Child → parent
          </span>
          <span className="fleet-legend-item">
            <svg width="26" height="10" aria-hidden="true"><line x1="1" y1="5" x2="18" y2="5" className="fleet-edge fleet-edge-tcp" markerEnd="url(#fleet-tcp-arrow)" /></svg>
            TCP source → destination
          </span>
          <span className="fleet-legend-hint">✎ or double-click a name to rename a host</span>
        </div>
        {renameError && <div className="fleet-error" role="alert">{renameError}</div>}
        <div
          className={`fleet-canvas${panning ? ' panning' : ''}`}
          ref={canvasRef}
          aria-label="Fleet topology graph. Drag or use arrow keys to pan."
          tabIndex={0}
          onKeyDown={panWithKeyboard}
          onPointerDown={beginPan}
          onPointerMove={movePan}
          onPointerUp={endPan}
          onPointerCancel={endPan}
        >
          <div className="fleet-stage" style={{ transform: `translate(${panOffset.x}px, ${panOffset.y}px)` }}>
            <svg className="fleet-graph" width={fleet.width} height={fleet.height} viewBox={`0 0 ${fleet.width} ${fleet.height}`} role="img" aria-label="Directed fleet topology; arrows point from children to parents">
            <defs>
              <marker id="fleet-parent-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                <path d="M 0 0 L 8 4 L 0 8 z" className="fleet-arrowhead fleet-arrowhead-parent" />
              </marker>
              <marker id="fleet-tcp-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                <path d="M 0 0 L 8 4 L 0 8 z" className="fleet-arrowhead fleet-arrowhead-tcp" />
              </marker>
              {fleet.nodes.map((node, index) => (
                <clipPath id={`fleet-node-content-${index}`} key={node.host.id}><rect x="8" y="4" width={NODE_WIDTH - 32} height={NODE_HEIGHT - 8} /></clipPath>
              ))}
            </defs>
            <g className="fleet-edges">
              {fleet.edges.map((edge) => {
                const child = nodeByID.get(edge.childId)!;
                const parent = nodeByID.get(edge.parentId)!;
                const childY = child.y - NODE_HEIGHT / 2;
                const parentY = parent.y + NODE_HEIGHT / 2;
                const shift = edge.dialer ? EDGE_OFFSET : 0;
                const [src, dst] = edge.dialer === 'parent' ? [parent, child] : [child, parent];
                return (
                  <g key={`${edge.childId}->${edge.parentId}`}>
                    <path
                      className="fleet-edge fleet-edge-parent"
                      data-child-id={edge.childId}
                      data-parent-id={edge.parentId}
                      d={edgePath(child.x - shift, childY, parent.x - shift, parentY)}
                      markerEnd="url(#fleet-parent-arrow)"
                    >
                      <title>{`${hostName(child.host)} → ${hostName(parent.host)} (child → parent)`}</title>
                    </path>
                    {edge.dialer && (
                      <path
                        className="fleet-edge fleet-edge-tcp"
                        data-tcp-src={src.host.id}
                        data-tcp-dst={dst.host.id}
                        d={edge.dialer === 'parent'
                          ? edgePath(parent.x + shift, parentY, child.x + shift, childY)
                          : edgePath(child.x + shift, childY, parent.x + shift, parentY)}
                        markerEnd="url(#fleet-tcp-arrow)"
                      >
                        <title>{`${hostName(src.host)} → ${hostName(dst.host)} (TCP source → destination)`}</title>
                      </path>
                    )}
                  </g>
                );
              })}
            </g>
            <g className="fleet-nodes">
              {fleet.nodes.map((node, index) => (
                <g className={`fleet-node fleet-${statusLabel(node.host)}${pulseHostId === node.host.id ? ' fleet-node-pulse' : ''}`} data-host-id={node.host.id} transform={`translate(${node.x - NODE_WIDTH / 2} ${node.y - NODE_HEIGHT / 2})`} key={node.host.id}>
                  <title>{`${node.host.name ?? node.host.id}, ${version(node.host)}`}</title>
                  <rect width={NODE_WIDTH} height={NODE_HEIGHT} rx="8" />
                  <g clipPath={`url(#fleet-node-content-${index})`}>
                    <circle cx="15" cy="17" r="4" />
                    {editing?.hostId === node.host.id ? (
                      <foreignObject x="22" y="7" width={NODE_WIDTH - 34} height="20">
                        <input
                          className="fleet-name-input"
                          aria-label={`Display name for ${hostName(node.host)}`}
                          autoFocus
                          value={editing.name}
                          maxLength={120}
                          placeholder="Reported name"
                          onPointerDown={(event) => event.stopPropagation()}
                          onChange={(event) => setEditing({ hostId: node.host.id, name: event.target.value })}
                          onBlur={() => void commitRename()}
                          onKeyDown={(event) => {
                            event.stopPropagation();
                            if (event.key === 'Enter') void commitRename();
                            if (event.key === 'Escape') setEditing(null);
                          }}
                        />
                      </foreignObject>
                    ) : (
                      <text
                        className="fleet-name"
                        x="26"
                        y="21"
                        onPointerDown={(event) => event.stopPropagation()}
                        onDoubleClick={() => setEditing({ hostId: node.host.id, name: node.host.name ?? '' })}
                      >{hostName(node.host)}</text>
                    )}
                    <text className="fleet-version" x="12" y="42">{version(node.host)}</text>
                    <text className="fleet-status" x="12" y="57">{statusLabel(node.host)}{accessLabel(node.host)}</text>
                  </g>
                  {editing?.hostId !== node.host.id && (
                    <text
                      className="fleet-edit"
                      x={NODE_WIDTH - 20}
                      y="21"
                      role="button"
                      tabIndex={0}
                      aria-label={`Rename ${hostName(node.host)}`}
                      onPointerDown={(event) => event.stopPropagation()}
                      onClick={() => setEditing({ hostId: node.host.id, name: node.host.name ?? '' })}
                      onKeyDown={(event) => {
                        if (event.key !== 'Enter' && event.key !== ' ') return;
                        event.preventDefault();
                        event.stopPropagation();
                        setEditing({ hostId: node.host.id, name: node.host.name ?? '' });
                      }}
                    >✎</text>
                  )}
                  <text className="fleet-metrics" x={NODE_WIDTH - 38} y="58" role="button" tabIndex={0}
                    aria-label={`Resource utilization for ${hostName(node.host)}`}
                    onPointerDown={(event) => event.stopPropagation()}
                    onClick={() => openHostMetrics(node.host.id)}>▥</text>
                </g>
              ))}
            </g>
            </svg>
          </div>
        </div>
        <div className="foot fleet-foot">
          <span>{fleet.nodes.length} {fleet.nodes.length === 1 ? 'node' : 'nodes'} · {fleet.edges.length} {fleet.edges.length === 1 ? 'link' : 'links'}</span>
          <div className="fleet-actions">
            <button type="button" disabled={panOffset.x === 0 && panOffset.y === 0} onClick={() => setPanOffset({ x: 0, y: 0 })}>Recenter</button>
            <button type="button" onClick={refreshHosts}>Refresh</button>
          </div>
        </div>
      </div>
    </div>
  );
}
