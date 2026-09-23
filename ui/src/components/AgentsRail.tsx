import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { usePresence, useValuePresence } from '../transitions';
import { usesSoftKeyboard } from '../mobile';
import { LOCAL_HOST_ID, isLocalHost, useStore, rankedOrder, agentBadge } from '../store';
import type { NotifSeverity } from '../store';
import type { PendingSpawn, SessionView } from '../store';
import type { ClosePreview, FederationHost } from '../wire';

// Badge color per severity, matching the Notifications panel (red > yellow > green).
const SEVERITY_CLASS: Record<NotifSeverity, string> = {
  success: 'sev-success',
  attention: 'sev-attention',
  failure: 'sev-failure',
};

// Left rail — the orchestra. One row per agent in the order arranged by the
// user. Click = focus; drag = reorder.
export function SessionsRail({ onResizeStart }: { onResizeStart?: (clientX: number) => void }) {
  const order = useStore(rankedOrder);
  const agents = useStore((s) => s.sessions);
  const hosts = useStore((s) => s.hosts);
  const focusedId = useStore((s) => s.focusedId);
  const focus = useStore((s) => s.focus);
  const pendingSpawns = useStore((s) => s.pendingSpawns);
  const focusedSpawnId = useStore((s) => s.focusedSpawnId);
  const focusSpawn = useStore((s) => s.focusSpawn);
  const dismissPendingSpawn = useStore((s) => s.dismissPendingSpawn);
  const reorderAgent = useStore((s) => s.reorderAgent);
  const markAgentUnread = useStore((s) => s.markAgentUnread);
  const setPane = useStore((s) => s.setPane);
  const setDraft = useStore((s) => s.setDraft);
  const getClosePreview = useStore((s) => s.getClosePreview);
  const closeAgent = useStore((s) => s.closeAgent);
  const renameAgent = useStore((s) => s.renameAgent);
  const handOffAgent = useStore((s) => s.handOffAgent);
  const restartHarness = useStore((s) => s.restartHarness);
	const openSpawnAtHost = useStore((s) => s.openSpawnAtHost);
	const openFleetAtHost = useStore((s) => s.openFleetAtHost);
	const collapsed = useStore((s) => s.sessionsRailCollapsed);
  const toggleCollapsed = useStore((s) => s.toggleSessionsRail);
  // Keep the familiar uninterrupted rail until federation has at least one
  // accepted/known slave. Once it does, every group (including local) has a
  // labeled divider so the placement of remote controls is unambiguous.
  // A host update can briefly replace the host list before the master has
  // refreshed it. Keep grouping whenever a retained session belongs to a
  // remote host, rather than flattening those sessions into the local rail
  // during that gap. `groupedAgentRows` deliberately renders an unknown host
  // as offline, using its last reported name from the session.
  const federationGrouping = hosts.some((host) => !host.local && ['connected', 'accepted', 'offline'].includes(host.status ?? ''))
    || Object.values(agents).some((agent) => !isLocalHost(agent.hostId));
  const displayedGroups = federationGrouping
    ? groupedAgentRows(order, agents, hosts)
    : [{ hostId: 'all', label: '', link: null, ids: order }];
  // Keep the full dock alive until the grid has finished contracting, so its
  // contents stay clipped by the shrinking dock rather than disappearing at
  // the start of the transition.
  const { mounted: expandedMounted, closing: railClosing } = usePresence(!collapsed, 225);
  const [pendingConfirmation, setConfirmation] = useState<{ id: string; preview: ClosePreview; forceReason?: string } | null>(null);
  // Hold the dialog on screen while it animates away.
  const { rendered: confirmation, closing: confirmClosing } = useValuePresence(pendingConfirmation);
  const [deleteWorktree, setDeleteWorktree] = useState(true);
  const [deinitSubmodules, setDeinitSubmodules] = useState(false);
  const [closeError, setCloseError] = useState('');
  const [draggedId, setDraggedId] = useState<string | null>(null);
  const [dropTarget, setDropTarget] = useState<{ id: string; after: boolean } | null>(null);
  // A drop rewrites the order and React repaints every affected row in its new
  // place in one frame, which reads as the cards teleporting. FLIP the rail
  // instead: measure where the rows sit before the reorder, then after the
  // commit put each one back where it was and let it slide to where it landed.
  // The dragged card is excluded: the pointer already carried it to the crack
  // it was released at, so sliding it would mean snapping it back first.
  const rowNodes = useRef(new Map<string, HTMLDivElement>());
  const rowTops = useRef<Map<string, number> | null>(null);
  const registerRow = (id: string) => (node: HTMLDivElement | null) => {
    if (node) rowNodes.current.set(id, node);
    else rowNodes.current.delete(id);
  };
  const captureRowTops = (droppedId: string) => {
    const tops = new Map<string, number>();
    for (const [id, node] of rowNodes.current) {
      if (id !== droppedId) tops.set(id, node.getBoundingClientRect().top);
    }
    rowTops.current = tops;
  };
  // Layout, not paint: the rows have to be displaced before the browser has a
  // chance to show them in their new places. Runs after every render but only
  // does anything when a drop asked for it.
  useLayoutEffect(() => {
    const tops = rowTops.current;
    rowTops.current = null;
    if (!tops) return;
    if (window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) return;
    for (const [id, node] of rowNodes.current) {
      const was = tops.get(id);
      if (was === undefined || typeof node.animate !== 'function') continue;
      const delta = was - node.getBoundingClientRect().top;
      if (!delta) continue;
      node.animate(
        [{ transform: `translateY(${delta}px)` }, { transform: 'translateY(0)' }],
        { duration: 195, easing: 'cubic-bezier(.2, .8, .2, 1)' },
      );
    }
  });
  // Host dividers stick just below the (already sticky) rail head, so the rail
  // has to publish the head's measured height for the CSS `top` to key off.
  const scrollerRef = useRef<HTMLDivElement | null>(null);
  const headRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    const scroller = scrollerRef.current;
    const head = headRef.current;
    if (!scroller || !head) return;
    const sync = () => scroller.style.setProperty('--rail-head-h', `${head.offsetHeight}px`);
    sync();
    if (typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(sync);
    observer.observe(head);
    return () => observer.disconnect();
  }, [expandedMounted]);

  const requestDelete = async (id: string) => {
    setCloseError('');
    try {
      const preview = await getClosePreview(id);
      // A shared worktree always warrants the dialog: the user needs to be told
      // the checkout is staying behind, and who is still in it.
      if (!preview.notGitRepo && !preview.uncommitted && !preview.unmerged && !preview.cohabitants?.length) {
        const result = await closeAgent(id, false, true);
        if (result.error?.startsWith('submodules_block_worktree_removal')) {
          setDeleteWorktree(true);
          setDeinitSubmodules(true);
          setConfirmation({ id, preview });
        } else if (result.error?.startsWith('orphaned_worktree')) {
          // The preview can still look clean when the checkout's recorded parent
          // repository has disappeared. Teardown is the first operation that
          // detects that condition, so recover by offering the same explicit
          // force-confirmation used for other destructive closes.
          setDeleteWorktree(true);
          setDeinitSubmodules(false);
          setConfirmation({ id, preview, forceReason: result.error.replace(/^orphaned_worktree:\s*/, '') });
        } else if (result.error) setCloseError(result.error);
        return;
      }
      setDeleteWorktree(preview.kind === 'worktree');
      setDeinitSubmodules((preview.submodules?.length ?? 0) > 0);
      setConfirmation({ id, preview });
    } catch (error) {
      setCloseError((error as Error).message);
    }
  };

  // Every crack between two rows has two equivalent descriptions — "after the
  // row above" and "before the row below" — which the pointer flips between as
  // it crosses a row boundary, painting the indicator a few pixels apart for
  // what is one and the same destination. Collapse each crack to a single
  // canonical description so the line holds still, and return null for the two
  // cracks the dragged row already borders: those drops are no-ops, so
  // promising them with a line is what makes a release look like it was
  // ignored. Reordering is meaningful only within a host group, since the rail
  // re-groups by host on every render and a cross-group move would leave the
  // card exactly where it was.
  const resolveDrop = (targetId: string, after: boolean): { id: string; after: boolean } | null => {
    if (!draggedId || draggedId === targetId) return null;
    const ids = displayedGroups.find((group) => group.ids.includes(targetId))?.ids;
    if (!ids) return null;
    const from = ids.indexOf(draggedId);
    if (from < 0) return null;
    const insertion = ids.indexOf(targetId) + (after ? 1 : 0);
    if (insertion === from || insertion === from + 1) return null;
    const below = ids[insertion];
    return below ? { id: below, after: false } : { id: ids[insertion - 1], after: true };
  };

  // The strip below the last group drops onto the end of the dragged session's
  // own group, for the same reason cross-group drops are refused above.
  const dropAtEnd = (): { id: string; after: boolean } | null => {
    if (!draggedId) return null;
    const lastId = displayedGroups.find((group) => group.ids.includes(draggedId))?.ids.at(-1);
    return lastId && lastId !== draggedId ? { id: lastId, after: true } : null;
  };

  const promptForCommitMerge = (id: string) => {
    setConfirmation(null);
    focus(id);
    setPane('chat');
    const target = agents[id]?.workspace.targetRef;
    setDraft(id, target
      ? `commit your changes and merge back into ${target.replace(/^refs\/heads\//, '').replace(/^refs\/remotes\//, '')}`
      : 'commit your changes and merge back into the integration target branch');
    requestAnimationFrame(() => requestAnimationFrame(() => {
      const textarea = [...document.querySelectorAll<HTMLTextAreaElement>('[data-prompt-agent]')].find((el) => el.dataset.promptAgent === id);
      textarea?.focus();
      textarea?.setSelectionRange(textarea.value.length, textarea.value.length);
    }));
  };

  return (
	<div className={`rail sessions${collapsed ? ' collapsed' : ''}`}>
      {collapsed && !expandedMounted && (
        <button className="rail-toggle" title="Show sessions" onClick={toggleCollapsed}>
          <span className="chevron">›</span>
          <span className="label">Sessions</span>
          {order.length > 0 && <span className="count">{order.length}</span>}
        </button>
      )}
      {expandedMounted && (
      <div ref={scrollerRef} className={`rail-expanded${railClosing ? ' closing' : ' entering'}`} aria-hidden={railClosing}>
      <div
        className="dock-resize-handle dock-resize-handle-right"
        role="separator"
        aria-label="Resize sessions panel"
        aria-orientation="vertical"
        onPointerDown={(event) => {
          event.preventDefault();
          onResizeStart?.(event.clientX);
        }}
      />
      <div ref={headRef} className="rail-head">
        <button className="rail-toggle-btn" title="Collapse sessions" onClick={toggleCollapsed}>
          ‹
        </button>
        <span className="rail-head-label" onClick={toggleCollapsed}>
          Sessions <span className="count">{order.length}</span>
        </span>
      </div>
      {pendingSpawns.length > 0 && (
        <section className="session-host-group pending-spawns" aria-label="Starting sessions">
          {pendingSpawns.map((p) => (
            <PendingSpawnRow
              key={p.corrId}
              spawn={p}
              active={p.corrId === focusedSpawnId}
              onClick={() => focusSpawn(p.corrId)}
              onDismiss={() => dismissPendingSpawn(p.corrId)}
            />
          ))}
        </section>
      )}
      {order.length === 0 ? (pendingSpawns.length > 0 ? null : (
        <div className="empty">
          No sessions yet.
          <br />
          Press <span className="kbd">C</span> or <b>+ Session</b> to spawn one.
        </div>
      )) : (
        <>
          {displayedGroups.map((group) => (
            // Each group is its own box so its divider sticks only for as long
            // as the group is on screen: the next one pushes it out at the top.
            <section className="session-host-group" key={group.hostId}>
              {federationGrouping && (
                <HostHeader
                  group={group}
                  onSpawn={() => openSpawnAtHost(group.hostId)}
                  onDetails={() => openFleetAtHost(group.hostId)}
                />
              )}
              {group.ids.map((id) => (
                <Row
                  key={id}
                  rowRef={registerRow(id)}
                  agent={agents[id]}
                  active={id === focusedId}
                  onClick={() => focus(id)}
                  onMarkUnread={() => markAgentUnread(id)}
                  onRename={(name) => renameAgent(id, name)}
                  onDelete={() => void requestDelete(id)}
                  onHandOff={() => handOffAgent(id)}
                  onRestartHarness={() => restartHarness(id)}
                  dragging={id === draggedId}
                  dropPosition={dropTarget?.id === id ? (dropTarget.after ? 'after' : 'before') : null}
                  onDragStart={() => setDraggedId(id)}
                  onDragOver={(after) => setDropTarget(resolveDrop(id, after))}
                  onDrop={(after) => {
                    const drop = resolveDrop(id, after);
                    if (draggedId && drop) {
                      captureRowTops(draggedId);
                      reorderAgent(draggedId, drop.id, drop.after);
                    }
                    setDraggedId(null);
                    setDropTarget(null);
                  }}
                  onDragEnd={() => {
                    setDraggedId(null);
                    setDropTarget(null);
                  }}
                />
              ))}
            </section>
          ))}
          <div
            className={`session-drop-end${draggedId ? ' active' : ''}`}
            aria-hidden="true"
            onDragOver={(event) => {
              event.preventDefault();
              event.dataTransfer.dropEffect = 'move';
              setDropTarget(dropAtEnd());
            }}
            onDrop={(event) => {
              event.preventDefault();
              const drop = dropAtEnd();
              if (draggedId && drop) {
                captureRowTops(draggedId);
                reorderAgent(draggedId, drop.id, drop.after);
              }
              setDraggedId(null);
              setDropTarget(null);
            }}
          />
        </>
      )}
      {closeError && <div className="rail-close-error">{closeError}</div>}
      </div>
      )}
      {confirmation && agents[confirmation.id] && (
        <div style={{ display: 'contents' }} className={confirmClosing ? 'popup-closing' : undefined}>
        <div className="modal-scrim" onClick={() => setConfirmation(null)}>
          <div className="modal confirm-modal" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-body">
              <div>Delete session <b>{agents[confirmation.id].name}</b>?</div>
              {confirmation.preview.notGitRepo ? (
                <>
                  <section>
                    <strong>Not a git repository</strong>
                    <p>This existing directory will not be deleted. Deleting the session only stops it and removes it from Tandem.</p>
                    <p>Tandem cannot tell whether the directory has any changes.</p>
                  </section>
                  <section>
                    <strong>Directory</strong>
                    <pre className="copyable-path" onClick={(e) => e.stopPropagation()}>{agents[confirmation.id].workspace.cwd}</pre>
                  </section>
                </>
              ) : (
                <>
                  {confirmation.forceReason && (
                    <section>
                      <strong>Orphaned worktree</strong>
                      <p>{confirmation.forceReason}</p>
                      <p>Deleting will force-remove this checkout. Its original repository and branch are already unavailable.</p>
                    </section>
                  )}
                  {confirmation.preview.uncommitted && (
                    <section><strong>Uncommitted changes</strong><pre>{confirmation.preview.uncommitted}</pre></section>
                  )}
                  {confirmation.preview.unmerged && (
                    <section><strong>Unmerged commits</strong><pre>{confirmation.preview.unmerged}</pre></section>
                  )}
                  {confirmation.preview.submodules?.map((submodule) => (
                    <section key={submodule.path}>
                      <strong>Submodule: {submodule.path}</strong>
                      <p>This initialized checkout must be deinitialized before its containing worktree can be removed.</p>
                      {submodule.uncommitted && <><p>Uncommitted changes will be discarded if you deinitialize this submodule.</p><pre>{submodule.uncommitted}</pre></>}
                      {submodule.localCommits && <><p>Local-only commits should be pushed or saved on a named branch before deletion.</p><pre>{submodule.localCommits}</pre></>}
                    </section>
                  ))}
                  {deinitSubmodules && (
                    <section>
                      <strong>Submodule cleanup required</strong>
                      <p>Git cannot remove this worktree while its submodules are initialized. Deleting will run <code>git submodule deinit -f --all</code>, which removes their checkouts and can discard the changes shown above.</p>
                    </section>
                  )}
                  {confirmation.preview.targetRef && (
                    <section><strong>Integration target</strong><pre>{confirmation.preview.targetRef.replace(/^refs\/heads\//, '').replace(/^refs\/remotes\//, '')}{typeof confirmation.preview.ahead === 'number' ? `\n${confirmation.preview.ahead} ahead · ${confirmation.preview.behind ?? 0} behind` : ''}</pre></section>
                  )}
                  {confirmation.preview.cohabitants?.length ? (
                    <section>
                      <strong>Worktree is shared</strong>
                      <p>
                        {confirmation.preview.cohabitants.length === 1 ? 'Session' : 'Sessions'}{' '}
                        <b>{confirmation.preview.cohabitants.join(', ')}</b>{' '}
                        {confirmation.preview.cohabitants.length === 1 ? 'is' : 'are'} still working in{' '}
                        <code>{agents[confirmation.id].workspace.cwd}</code>. Deleting this session leaves the worktree and
                        its branch in place; it is removed with the last session that occupies it.
                      </p>
                    </section>
                  ) : confirmation.preview.kind === 'worktree' && (
                    <label className="delete-worktree-option">
                      <input type="checkbox" checked={deleteWorktree} onChange={(e) => setDeleteWorktree(e.target.checked)} />
                      Delete worktree
                    </label>
                  )}
                </>
              )}
            </div>
            <div className="foot">
              {!confirmation.preview.notGitRepo && (
                <button className="btn" onClick={() => promptForCommitMerge(confirmation.id)}>
                  Prompt for commit+merge
                </button>
              )}
              <button className="btn success" onClick={() => setConfirmation(null)}>Cancel</button>
              <button
                className="btn danger"
                onClick={async () => {
                  const result = await closeAgent(confirmation.id, deleteWorktree, deleteWorktree, deinitSubmodules);
                  if (result.error?.startsWith('submodules_block_worktree_removal')) {
                    setDeinitSubmodules(true);
                    setCloseError('Submodule cleanup is required before this worktree can be removed.');
                  } else if (result.error) setCloseError(result.error);
                  else setConfirmation(null);
                }}
              >
                Delete agent
              </button>
            </div>
          </div>
        </div>
        </div>
      )}
    </div>
  );
}

// A remote host's rows survive its tunnel dropping (the master keeps the last
// snapshot), so each remote divider carries a link badge saying whether what
// is listed under it is live or the last thing that host reported.
function hostLinkBadge(status: FederationHost['status']): HostLink {
  switch (status) {
    case 'connected':
      return { label: 'online', tone: 'link-online', title: 'Connected — this host is reachable and its sessions are live' };
    case 'pending':
      return { label: 'pending', tone: 'link-pending', title: 'Awaiting approval — this host has requested to join' };
    case 'rejected':
      return { label: 'rejected', tone: 'link-rejected', title: 'Rejected — this host was refused' };
    default:
      return { label: 'offline', tone: 'link-offline', title: 'Disconnected — showing the last state this host reported' };
  }
}

type HostLink = { label: string; tone: string; title: string };
type HostGroup = { hostId: string; label: string; link: HostLink | null; ids: string[] };

function HostHeader({ group, onSpawn, onDetails }: { group: HostGroup; onSpawn: () => void; onDetails: () => void }) {
  const [menu, setMenu] = useState<{ x: number; y: number } | null>(null);
  useEffect(() => {
    if (!menu) return;
    const dismiss = () => setMenu(null);
    const onKeyDown = (event: KeyboardEvent) => { if (event.key === 'Escape') dismiss(); };
    window.addEventListener('pointerdown', dismiss);
    window.addEventListener('keydown', onKeyDown);
    return () => {
      window.removeEventListener('pointerdown', dismiss);
      window.removeEventListener('keydown', onKeyDown);
    };
  }, [menu]);
  return (
    <div
      className="session-host-divider"
      onContextMenu={(event) => {
        event.preventDefault();
        setMenu({ x: event.clientX, y: event.clientY });
      }}
    >
      <span>{group.label}</span>
      {group.link && (
        <span className={`session-host-link ${group.link.tone}`} title={group.link.title}>
          <span className="session-host-link-dot" />
          {group.link.label}
        </span>
      )}
      {menu && createPortal(
        <div
          className="session-context-menu"
          style={{ left: menu.x, top: menu.y }}
          role="menu"
          aria-label={`Actions for ${group.label}`}
          onPointerDown={(event) => event.stopPropagation()}
          onClick={(event) => event.stopPropagation()}
        >
          <button type="button" role="menuitem" onClick={() => { onSpawn(); setMenu(null); }}>Spawn agent here</button>
          <button type="button" role="menuitem" onClick={() => { onDetails(); setMenu(null); }}>Details</button>
        </div>,
        document.body,
      )}
    </div>
  );
}

// A spawn the daemon has not acknowledged yet: a spinner (or error) row that
// can be focused to watch progress, and dismissed once it has failed.
function PendingSpawnRow({ spawn, active, onClick, onDismiss }: { spawn: PendingSpawn; active: boolean; onClick: () => void; onDismiss: () => void }) {
  return (
    <div className={`session-row pending-spawn${active ? ' active' : ''}`} onClick={onClick} title={spawn.error ?? spawn.phase}>
      {spawn.error ? <span className="dot error" title="spawn failed" /> : <span className="spinner small" aria-label="starting" />}
      <div style={{ minWidth: 0 }}>
        <div className="name">
          {spawn.error && '⛔ '}
          {spawn.name || 'New session'}
          {spawn.hostId && <span className="session-host-badge">{spawn.hostName || spawn.hostId}</span>}
        </div>
        <div className="ws"><span className="ws-text">{spawn.error ? `Spawn failed · ${spawn.label}` : spawn.phase}</span></div>
      </div>
      {spawn.error && (
        <div className="session-actions">
          <button
            className="delete-btn"
            title="Dismiss"
            aria-label="Dismiss failed spawn"
            onClick={(e) => {
              e.stopPropagation();
              onDismiss();
            }}
          >
            ✕
          </button>
        </div>
      )}
    </div>
  );
}

function groupedAgentRows(order: string[], agents: Record<string, SessionView>, hosts: FederationHost[]): HostGroup[] {
  const groups = new Map<string, HostGroup>();
  for (const id of order) {
    const agent = agents[id];
    if (!agent) continue;
    const hostId = agent.hostId ?? LOCAL_HOST_ID;
    const configured = hosts.find((host) => host.id === hostId);
    const local = configured?.local || hostId === LOCAL_HOST_ID;
    const label = local ? 'This host' : agent.hostName || configured?.name || hostId;
    const group = groups.get(hostId) ?? { hostId, label, link: local ? null : hostLinkBadge(configured?.status), ids: [] };
    group.ids.push(id);
    groups.set(hostId, group);
  }
  const local = groups.get(LOCAL_HOST_ID);
  return [...(local ? [local] : []), ...[...groups.values()].filter((group) => group.hostId !== LOCAL_HOST_ID)];
}

function Row({
  rowRef,
  agent,
  active,
  onClick,
  onMarkUnread,
  onRename,
  onDelete,
  onHandOff,
  onRestartHarness,
  dragging,
  dropPosition,
  onDragStart,
  onDragOver,
  onDrop,
  onDragEnd,
}: {
  rowRef: (node: HTMLDivElement | null) => void;
  agent: SessionView;
  active: boolean;
  onClick: () => void;
  onMarkUnread: () => void;
  onRename: (name: string) => Promise<{ error?: string }>;
  onDelete: () => void;
  onHandOff: () => void;
  onRestartHarness: () => Promise<{ error?: string }>;
  dragging: boolean;
  dropPosition: 'before' | 'after' | null;
  onDragStart: () => void;
  onDragOver: (after: boolean) => void;
  onDrop: (after: boolean) => void;
  onDragEnd: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(agent.name);
  const [renameError, setRenameError] = useState('');
  const [restartError, setRestartError] = useState('');
  const [restartingHarness, setRestartingHarness] = useState(false);
  const [mouseHovered, setMouseHovered] = useState(false);
  const [pendingContextMenu, setContextMenu] = useState<{ x: number; y: number } | null>(null);
  const [pendingDetails, setDetails] = useState<{ x: number; y: number } | null>(null);
  const { rendered: contextMenu, closing: menuClosing } = useValuePresence(pendingContextMenu);
  const { rendered: details, closing: detailsClosing } = useValuePresence(pendingDetails);
  // A browser-generated `contextmenu` is dependable for a mouse, but a touch
  // long-press is inconsistently delivered (and is commonly claimed by scroll
  // or native drag handling). Recognize that gesture ourselves instead.
  const longPress = useRef<{ pointerId: number; x: number; y: number; timer: number } | null>(null);
  const longPressOpened = useRef(false);
  const badge = agentBadge(agent);
  const ws = agent.workspace;
  const branch = ws.branch || (ws.kind === 'existing' ? 'no-branch' : '');
  const target = ws.targetRef?.replace(/^refs\/heads\//, '').replace(/^refs\/remotes\//, '');
  const gitStateTitle = ws.gitState
    ? {
        dirty: 'uncommitted changes',
        ahead: `${ws.ahead ?? 0} commit(s) ahead of ${target ?? 'target'}`,
        behind: `${ws.behind ?? 0} commit(s) behind ${target ?? 'target'}`,
        diverged: `diverged from ${target ?? 'target'}`,
        merged: `merged into ${target ?? 'target'}`,
        synced: 'clean and synchronized',
        target_missing: 'integration target is missing',
        unknown: 'git status unavailable',
      }[ws.gitState]
    : undefined;
  const commitRename = async () => {
    const next = name.trim();
    if (!next) {
      setRenameError('Name cannot be empty');
      return;
    }
    const result = await onRename(next);
    if (result.error) {
      setRenameError(result.error);
      return;
    }
    setEditing(false);
    setRenameError('');
  };
  // Hover is the trigger on desktop; touch devices have no hover, so keep
  // showing actions on the active (selected) row there instead.
  const showActions = mouseHovered || (active && usesSoftKeyboard());
  const beginRename = () => {
    setName(agent.name);
    setRenameError('');
    setEditing(true);
  };
  useEffect(() => {
    if (!pendingContextMenu) return;
    const dismiss = () => setContextMenu(null);
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') dismiss();
    };
    window.addEventListener('pointerdown', dismiss);
    window.addEventListener('keydown', onKeyDown);
    return () => {
      window.removeEventListener('pointerdown', dismiss);
      window.removeEventListener('keydown', onKeyDown);
    };
  }, [pendingContextMenu]);
  useEffect(() => {
    if (!pendingDetails) return;
    const dismiss = () => setDetails(null);
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') dismiss();
    };
    window.addEventListener('pointerdown', dismiss);
    window.addEventListener('keydown', onKeyDown);
    return () => {
      window.removeEventListener('pointerdown', dismiss);
      window.removeEventListener('keydown', onKeyDown);
    };
  }, [pendingDetails]);
  const openDetails = (origin: { x: number; y: number }) => {
    // Keep the initial panel near its invocation point without letting it spill
    // outside the viewport. The user can subsequently reposition it by dragging.
    const width = 390;
    const height = 430;
    setDetails({
      x: Math.max(8, Math.min(origin.x, window.innerWidth - width - 8)),
      y: Math.max(8, Math.min(origin.y, window.innerHeight - height - 8)),
    });
    setContextMenu(null);
  };
  const openContextMenu = (origin: { x: number; y: number }) => {
    // Keep the whole menu reachable when the invocation point is at a screen
    // edge. The dimensions intentionally include a little room for borders and
    // the menu's opening animation.
    const width = 190;
    const height = 230;
    setContextMenu({
      x: Math.max(8, Math.min(origin.x, window.innerWidth - width - 8)),
      y: Math.max(8, Math.min(origin.y, window.innerHeight - height - 8)),
    });
  };
  const cancelLongPress = () => {
    if (!longPress.current) return;
    window.clearTimeout(longPress.current.timer);
    longPress.current = null;
  };
  useEffect(() => cancelLongPress, []);
  return (
    <div
      ref={rowRef}
      className={`session-row${active ? ' active' : ''}${dragging ? ' dragging' : ''}${dropPosition ? ` drop-${dropPosition}` : ''}`}
      draggable={!editing}
      title={editing ? undefined : 'Drag to reorder session'}
      onClick={() => {
        if (longPressOpened.current) {
          longPressOpened.current = false;
          return;
        }
        onClick();
      }}
      onPointerDown={(event) => {
        if (event.pointerType !== 'touch' || editing) return;
        cancelLongPress();
        longPressOpened.current = false;
        const { pointerId, clientX: x, clientY: y } = event;
        const timer = window.setTimeout(() => {
          longPress.current = null;
          longPressOpened.current = true;
          openContextMenu({ x, y });
        }, 550);
        longPress.current = { pointerId, x, y, timer };
      }}
      onPointerMove={(event) => {
        const gesture = longPress.current;
        if (!gesture || event.pointerId !== gesture.pointerId) return;
        // Preserve ordinary rail scrolling: movement beyond a small natural
        // finger drift turns this interaction back into a pan.
        if (Math.hypot(event.clientX - gesture.x, event.clientY - gesture.y) > 12) cancelLongPress();
      }}
      onPointerUp={(event) => {
        if (event.pointerId === longPress.current?.pointerId) cancelLongPress();
      }}
      onPointerCancel={cancelLongPress}
      onDragStart={(event) => {
        event.dataTransfer.effectAllowed = 'move';
        event.dataTransfer.setData('text/plain', agent.id);
        onDragStart();
      }}
      onDragOver={(event) => {
        event.preventDefault();
        event.dataTransfer.dropEffect = 'move';
        const bounds = event.currentTarget.getBoundingClientRect();
        onDragOver(event.clientY > bounds.top + bounds.height / 2);
      }}
      onDrop={(event) => {
        event.preventDefault();
        const bounds = event.currentTarget.getBoundingClientRect();
        onDrop(event.clientY > bounds.top + bounds.height / 2);
      }}
      onDragEnd={onDragEnd}
      onContextMenu={(event) => {
        event.preventDefault();
        cancelLongPress();
        openContextMenu({ x: event.clientX, y: event.clientY });
      }}
      onPointerEnter={(event) => {
        if (event.pointerType === 'mouse') setMouseHovered(true);
      }}
      onPointerLeave={() => setMouseHovered(false)}
    >
      <span className={`dot ${agent.status}`} title={agent.status} />
      <div style={{ minWidth: 0 }}>
        <div className="name">
          {agent.status === 'blocked' && '⚠ '}
          {agent.status === 'error' && '⛔ '}
          {editing ? (
            <input
              className="session-rename-input"
              value={name}
              maxLength={80}
              autoFocus
              aria-label="Session name"
              onClick={(e) => e.stopPropagation()}
              onChange={(e) => setName(e.target.value)}
              onBlur={() => {
                setName(agent.name);
                setRenameError('');
                setEditing(false);
              }}
              onKeyDown={(e) => {
                if (e.key === 'Enter') void commitRename();
                if (e.key === 'Escape') {
                  setName(agent.name);
                  setRenameError('');
                  setEditing(false);
                }
              }}
            />
          ) : agent.name}
          {agent.hostId && (
            <span className="session-host-badge" title={`Running on ${agent.hostName || agent.hostId}`}>
              {agent.hostName || agent.hostId}
            </span>
          )}
          {badge.count > 0 && badge.severity && (
            <span className={`count badge notification-badge ${SEVERITY_CLASS[badge.severity]}`}>{badge.count}</span>
          )}
        </div>
        {renameError && <div className="session-rename-error">{renameError}</div>}
        <div className="ws" title={ws.cwd}>
          {ws.gitState && <span className={`git-state ${ws.gitState}`} title={gitStateTitle} />}
          <span className="ws-text">
            {ws.repo || '—'}
            {branch && ` · ${branch}`}
            {target && ` → ${target}`}
            {(ws.ahead || ws.behind) ? ` · +${ws.ahead ?? 0}/-${ws.behind ?? 0}` : ''}
          </span>
        </div>
      </div>
      {showActions && <div className="session-actions">
        <button
          className="rename-btn"
          title="Rename session"
          aria-label={`Rename ${agent.name}`}
          onClick={(e) => {
            e.stopPropagation();
            beginRename();
          }}
        >
          ✎
        </button>
        <button
          className="delete-btn"
          title="Delete session"
          onClick={(e) => {
            e.stopPropagation();
            onDelete();
          }}
        >
          🗑
        </button>
      </div>}
      {contextMenu && (
        <div
          className={`session-context-menu${menuClosing ? ' closing' : ''}`}
          style={{ left: contextMenu.x, top: contextMenu.y }}
          role="menu"
          aria-label={`Actions for ${agent.name}`}
          onPointerDown={(event) => event.stopPropagation()}
          onClick={(event) => event.stopPropagation()}
        >
          <button type="button" role="menuitem" onClick={() => { onMarkUnread(); setContextMenu(null); }}>
            Mark as unread
          </button>
          <button type="button" role="menuitem" onClick={() => { beginRename(); setContextMenu(null); }}>
            Edit name
          </button>
          <button type="button" role="menuitem" onClick={() => { onHandOff(); setContextMenu(null); }}>
            Hand off…
          </button>
          <button type="button" role="menuitem" disabled={restartingHarness} onClick={() => {
            setRestartingHarness(true);
            setRestartError('');
            void onRestartHarness().then((result) => {
              if (result.error) setRestartError(result.error);
              else setContextMenu(null);
            }).finally(() => setRestartingHarness(false));
          }}>
            {restartingHarness ? 'Restarting harness…' : 'Restart harness'}
          </button>
          <button type="button" role="menuitem" onClick={() => openDetails(contextMenu)}>
            View details
          </button>
          <button type="button" className="danger" role="menuitem" onClick={() => { onDelete(); setContextMenu(null); }}>
            Delete
          </button>
        </div>
      )}
      {details && <AgentDetails agent={agent} position={details} closing={detailsClosing} onClose={() => setDetails(null)} onMove={setDetails} />}
      {restartError && <div className="session-rename-error">{restartError}</div>}
    </div>
  );
}

function AgentDetails({
  agent,
  position,
  closing,
  onClose,
  onMove,
}: {
  agent: SessionView;
  position: { x: number; y: number };
  closing: boolean;
  onClose: () => void;
  onMove: (position: { x: number; y: number }) => void;
}) {
  const drag = useRef<{ offsetX: number; offsetY: number } | null>(null);
  const host = useStore((s) => s.hosts.find((candidate) => candidate.id === agent.hostId));
  const ws = agent.workspace;
  const profile = agent.profile;
  const gitState = ws.gitState === 'unknown' ? 'Unavailable' : (ws.gitState ?? 'Unavailable').replace(/_/g, ' ');
  const beginDrag = (event: React.PointerEvent<HTMLDivElement>) => {
    event.preventDefault();
    event.stopPropagation();
    drag.current = { offsetX: event.clientX - position.x, offsetY: event.clientY - position.y };
    event.currentTarget.setPointerCapture(event.pointerId);
  };
  const moveDrag = (event: React.PointerEvent<HTMLDivElement>) => {
    if (!drag.current) return;
    onMove({
      x: Math.max(8, Math.min(event.clientX - drag.current.offsetX, window.innerWidth - 80)),
      y: Math.max(8, Math.min(event.clientY - drag.current.offsetY, window.innerHeight - 42)),
    });
  };
  const endDrag = () => { drag.current = null; };
  const profileBits = [profile?.model, profile?.effort, profile?.permission].filter(Boolean);

  return (
    <section
      className={`session-details-popover${closing ? ' closing' : ''}`}
      style={{ left: position.x, top: position.y }}
      role="dialog"
      aria-label={`Details for ${agent.name}`}
      onPointerDown={(event) => event.stopPropagation()}
      onClick={(event) => event.stopPropagation()}
    >
      <div className="session-details-head" onPointerDown={beginDrag} onPointerMove={moveDrag} onPointerUp={endDrag} onPointerCancel={endDrag}>
        <div>
          <strong>{agent.name}</strong>
          <span className={`session-details-status ${agent.status}`}>{agent.status}</span>
        </div>
        <button type="button" aria-label="Close details" title="Close details" onPointerDown={(event) => event.stopPropagation()} onClick={onClose}>×</button>
      </div>
      <div className="session-details-body">
        <Detail label="Agent" value={agent.agent || 'Default agent'} />
        {agent.hostId && (
          <Detail
            label="Host"
            value={`${agent.hostName || agent.hostId}${host?.buildVersion ? ` · ${host.buildVersion}` : ''}`}
          />
        )}
        <Detail label="Profile" value={profileBits.join(' · ') || (profile?.id ? 'Saved profile' : 'No profile settings')} />
        {profile?.id && <Detail label="Profile ID" value={profile.id} mono />}
        {profile?.snapshot && <Detail label="Browser snapshot" value={profile.snapshot} mono />}
        <Detail label="Adapter" value={`${agent.adapter.toUpperCase()}${agent.canHandoff ? ' · CLI handoff available' : ''}`} />
        <Detail label="Workspace" value={ws.kind === 'worktree' ? 'Isolated worktree' : 'Existing directory'} />
        <Detail label="Worktree path" value={ws.cwd} mono />
        <Detail label="Repository root" value={ws.repoPath || 'Unavailable'} mono />
        <Detail label="Branch" value={ws.branch || 'Detached / not a branch'} mono />
        {ws.targetRef && <Detail label="Integration target" value={ws.targetRef.replace(/^refs\/(heads|remotes)\//, '')} mono />}
        <Detail label="Git status" value={`${gitState}${typeof ws.ahead === 'number' || typeof ws.behind === 'number' ? ` · ${ws.ahead ?? 0} ahead, ${ws.behind ?? 0} behind` : ''}`} />
        {ws.startCommit && <Detail label="Starting commit" value={ws.startCommit} mono />}
        <Detail label="Browser" value={agent.browserActive ? `${agent.browserOwner} has control` : 'Not started'} />
        {agent.pendingApprovals.length > 0 && <Detail label="Pending approvals" value={String(agent.pendingApprovals.length)} />}
      </div>
    </section>
  );
}

function Detail({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return <div className="session-detail"><span>{label}</span><code className={mono ? '' : 'plain'} title={value}>{value}</code></div>;
}
