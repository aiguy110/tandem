import { useEffect, useState } from 'react';
import { useStore } from '../../store';
import type { WorkspaceDiff } from '../../wire';
import { UnifiedDiff } from '../diff/UnifiedDiff';

export function DiffPane() {
  const agentId = useStore((s) => s.focusedId);
  const getDiff = useStore((s) => s.getDiff);
  const [diff, setDiff] = useState<WorkspaceDiff | null>(null);
  const [error, setError] = useState('');
  const [refresh, setRefresh] = useState(0);
  const [collapseRevision, setCollapseRevision] = useState<number | null>(null);

  useEffect(() => {
    let current = true;
    setDiff(null);
    setError('');
    if (agentId) getDiff(agentId).then((value) => current && setDiff(value)).catch((err: Error) => current && setError(err.message));
    return () => { current = false; };
  }, [agentId, getDiff, refresh]);

  if (!agentId) return null;
  return (
    <div className="pane diff-pane">
      <div className="diff-toolbar">
        <span>{diff?.targetRef ? `Integration target: ${diff.targetRef.replace(/^refs\/(heads|remotes)\//, '')}` : 'Workspace changes'}</span>
        <div className="diff-toolbar-actions">
          <button onClick={() => setCollapseRevision((n) => (n ?? 0) + 1)}>Collapse All</button>
          <button onClick={() => setRefresh((n) => n + 1)}>Refresh</button>
        </div>
      </div>
      {error && <div className="diff-error">{error}</div>}
      {!diff && !error && <div className="pane-placeholder">Loading changes…</div>}
      {diff && <div className="diff-scroll">
        <DiffSection title="Not yet committed" patch={diff.uncommitted} empty="No uncommitted changes." collapseRevision={collapseRevision} />
        <DiffSection title="Committed, not yet merged" patch={diff.committed} empty="No commits waiting to be merged." collapseRevision={collapseRevision} />
      </div>}
    </div>
  );
}

function DiffSection({ title, patch, empty, collapseRevision }: { title: string; patch: string; empty: string; collapseRevision: number | null }) {
  return <section className="diff-section">
    <h2>{title}</h2>
    {patch ? <UnifiedDiff patch={patch} collapseRevision={collapseRevision ?? undefined} /> : <div className="diff-empty">{empty}</div>}
  </section>;
}
