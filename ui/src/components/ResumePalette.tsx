import { useEffect, useMemo, useState } from 'react';
import { fuzzyFilter } from '../fuzzy';
import { useStore } from '../store';
import type { ResumableSession } from '../wire';

function displayDate(value?: string): string {
  if (!value) return '';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '' : date.toLocaleString();
}

export function ResumePalette() {
  const catalog = useStore((s) => s.resumeCatalog);
  const loading = useStore((s) => s.resumeLoading);
  const resumeSession = useStore((s) => s.resumeSession);
  const setModal = useStore((s) => s.setModal);
  const [query, setQuery] = useState('');
  const [sel, setSel] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const sessions = catalog?.sessions ?? [];
  const filtered = useMemo(
    () =>
      fuzzyFilter(
        query,
        sessions,
        (s) => `${s.title ?? ''} ${s.agentName ?? ''} ${s.agent} ${s.cwd} ${s.branch ?? ''} ${s.sessionId}`,
      ),
    [query, sessions],
  );
  useEffect(() => setSel(0), [query]);

  const resume = async (session: ResumableSession | undefined) => {
    if (!session || busy) return;
    if (!session.resumable) {
      setError(session.resumeError ?? 'This transcript is searchable but cannot be resumed.');
      return;
    }
    setBusy(true);
    setError(null);
    const result = await resumeSession(session);
    if (result.error) {
      setError(result.error);
      setBusy(false);
    }
  };

  const unsupported = catalog?.adapters.filter((a) => !a.supportsList).map((a) => a.agent) ?? [];

  return (
    <div className="modal-scrim" onMouseDown={(e) => e.target === e.currentTarget && setModal('none')}>
      <div
        className="modal"
        onKeyDown={(e) => {
          if (e.key === 'Escape') return setModal('none');
          if (e.key === 'ArrowDown') {
            e.preventDefault();
            setSel((i) => Math.min(filtered.length - 1, i + 1));
          } else if (e.key === 'ArrowUp') {
            e.preventDefault();
            setSel((i) => Math.max(0, i - 1));
          } else if (e.key === 'Enter') {
            e.preventDefault();
            void resume(filtered[sel]);
          }
        }}
      >
        <input
          className="q"
          autoFocus
          placeholder="Resume a session…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <div className="rows">
          {loading && sessions.length === 0 && <div className="empty">Discovering sessions…</div>}
          {!loading && filtered.length === 0 && <div className="empty">No resumable sessions found.</div>}
          {filtered.map((session, i) => (
            <div
              key={`${session.agent}:${session.sessionId}`}
              className={`row${i === sel ? ' sel' : ''}`}
              style={busy ? { opacity: 0.6 } : undefined}
              onMouseEnter={() => setSel(i)}
              onClick={() => void resume(session)}
            >
              <div>
                <div className="primary">{session.agentName ?? session.title ?? session.sessionId}</div>
                <div className="sub">{session.cwd}{session.branch ? ` · ${session.branch}` : ''}</div>
              </div>
              <div className="meta">
                <span>{session.agent}</span>
                <span>{session.live ? 'live' : session.source === 'tandem' ? 'Tandem' : session.source === 'acp' ? 'ACP' : session.historyOnly ? 'history only' : 'history'}</span>
                {session.updatedAt && <span>{displayDate(session.updatedAt)}</span>}
              </div>
            </div>
          ))}
        </div>
        {error && <div className="modal-err">{error}</div>}
        {unsupported.length > 0 && (
          <div className="modal-err">External sessions cannot be enumerated for: {unsupported.join(', ')}. Tandem-owned sessions still appear.</div>
        )}
        <div className="foot">
          <span><span className="kbd">↵</span> resume</span>
          <span><span className="kbd">↑↓</span> navigate</span>
          <span><span className="kbd">Esc</span> close</span>
        </div>
      </div>
    </div>
  );
}
