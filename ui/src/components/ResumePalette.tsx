import { Fragment, useEffect, useMemo, useRef, useState } from 'react';
import { fuzzyFilter } from '../fuzzy';
import { useStore } from '../store';
import type { HistoryExcerpt, ResumableSession, SessionSearchHit, SessionSearchResult } from '../wire';

function displayDate(value?: string): string {
  if (!value) return '';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '' : date.toLocaleString();
}

function sessionKey(session: ResumableSession): string {
  return `${session.agent}\0${session.sessionId}`;
}

// Server offsets count Unicode code points. Array.from makes those offsets safe
// for React even when the excerpt contains surrogate pairs.
function Highlighted({ excerpt }: { excerpt: HistoryExcerpt }) {
  const chars = Array.from(excerpt.text);
  const ranges = [...(excerpt.highlights ?? [])]
    .map((range) => ({ start: Math.max(0, range.start), end: Math.min(chars.length, range.end) }))
    .filter((range) => range.end > range.start)
    .sort((a, b) => a.start - b.start);
  const nodes: React.ReactNode[] = [];
  let cursor = 0;
  for (const [index, range] of ranges.entries()) {
    if (range.start > cursor) nodes.push(<Fragment key={`t${index}`}>{chars.slice(cursor, range.start).join('')}</Fragment>);
    nodes.push(<mark key={`m${index}`}>{chars.slice(Math.max(cursor, range.start), range.end).join('')}</mark>);
    cursor = Math.max(cursor, range.end);
  }
  if (cursor < chars.length) nodes.push(<Fragment key="tail">{chars.slice(cursor).join('')}</Fragment>);
  return <>{nodes}</>;
}

function Hit({ hit }: { hit: SessionSearchHit }) {
  return (
    <div className="history-hit">
      {hit.before && <div className="history-context"><Highlighted excerpt={hit.before} /></div>}
      <div className="history-match">
        {(hit.role || hit.kind) && <span className="history-kind">{hit.role || hit.kind}</span>}
        <Highlighted excerpt={hit.match} />
      </div>
      {hit.after && <div className="history-context"><Highlighted excerpt={hit.after} /></div>}
    </div>
  );
}

interface DisplayResult {
  session: ResumableSession;
  hits: SessionSearchHit[];
}

export function mergeResumeResults(
  query: string,
  sessions: ResumableSession[],
  historyResults: SessionSearchResult[],
): DisplayResult[] {
  if (!query.trim()) return sessions.map((session) => ({ session, hits: [] }));
  const metadata = fuzzyFilter(
    query,
    sessions,
    (session) => `${session.title ?? ''} ${session.agentName ?? ''} ${session.agent} ${session.cwd} ${session.branch ?? ''} ${session.sessionId}`,
  );
  const seen = new Set<string>();
  const merged: DisplayResult[] = [];
  for (const session of metadata) {
    const key = sessionKey(session);
    const history = historyResults.find((result) => sessionKey(result.session) === key);
    seen.add(key);
    merged.push({ session, hits: history?.hits ?? [] });
  }
  for (const result of historyResults) {
    const key = sessionKey(result.session);
    if (!seen.has(key)) merged.push({ session: result.session, hits: result.hits });
  }
  return merged;
}

export function ResumePalette() {
  const catalog = useStore((s) => s.resumeCatalog);
  const loading = useStore((s) => s.resumeLoading);
  const searchSessions = useStore((s) => s.searchSessions);
  const resumeSession = useStore((s) => s.resumeSession);
  const setModal = useStore((s) => s.setModal);
  const [query, setQuery] = useState('');
  const [historyResults, setHistoryResults] = useState<SessionSearchResult[]>([]);
  const [searching, setSearching] = useState(false);
  const [sel, setSel] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const searchGeneration = useRef(0);

  const sessions = catalog?.sessions ?? [];
  const results = useMemo(
    () => mergeResumeResults(query, sessions, historyResults),
    [query, sessions, historyResults],
  );

  useEffect(() => {
    const trimmed = query.trim();
    const generation = ++searchGeneration.current;
    setSel(0);
    setError(null);
    if (!trimmed) {
      setHistoryResults([]);
      setSearching(false);
      return;
    }
    setHistoryResults([]);
    setSearching(true);
    const timer = window.setTimeout(() => {
      void searchSessions(trimmed).then(
        (next) => {
          if (searchGeneration.current !== generation) return;
          setHistoryResults(next);
          setSearching(false);
        },
        (cause: unknown) => {
          if (searchGeneration.current !== generation) return;
          setHistoryResults([]);
          setSearching(false);
          setError(cause instanceof Error ? cause.message : 'Session history search failed.');
        },
      );
    }, 125);
    return () => window.clearTimeout(timer);
  }, [query, searchSessions]);

  useEffect(() => {
    if (sel >= results.length) setSel(Math.max(0, results.length - 1));
  }, [results.length, sel]);

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
    <div className="modal-scrim" onMouseDown={(event) => event.target === event.currentTarget && setModal('none')}>
      <div
        className="modal resume-modal"
        onKeyDown={(event) => {
          if (event.key === 'Escape') return setModal('none');
          if (event.key === 'ArrowDown') {
            event.preventDefault();
            setSel((index) => results.length === 0 ? 0 : Math.min(results.length - 1, index + 1));
          } else if (event.key === 'ArrowUp') {
            event.preventDefault();
            setSel((index) => Math.max(0, index - 1));
          } else if (event.key === 'Enter') {
            event.preventDefault();
            // A result is a session, even when it contains several transcript hits.
            void resume(results[sel]?.session);
          }
        }}
      >
        <input
          className="q"
          autoFocus
          placeholder="Resume a session…"
          value={query}
          onChange={(event) => setQuery(event.target.value)}
        />
        <div className="rows">
          {loading && sessions.length === 0 && <div className="empty">Discovering sessions…</div>}
          {!loading && !searching && results.length === 0 && <div className="empty">No matching sessions found.</div>}
          {searching && results.length === 0 && <div className="empty">Searching conversation history…</div>}
          {results.map(({ session, hits }, index) => (
            <div
              key={sessionKey(session)}
              className={`row resume-row${index === sel ? ' sel' : ''}${!session.resumable ? ' disabled' : ''}`}
              style={busy ? { opacity: 0.6 } : undefined}
              aria-disabled={!session.resumable}
              title={!session.resumable ? session.resumeError : undefined}
              onMouseEnter={() => setSel(index)}
              onClick={() => void resume(session)}
            >
              <div className="resume-content">
                <div className="resume-heading">
                  <div>
                    <div className="primary">{session.title ?? session.agentName ?? session.sessionId}</div>
                    <div className="sub">{session.cwd}{session.branch ? ` · ${session.branch}` : ''}</div>
                  </div>
                  <div className="meta">
                    <span>{session.agent}</span>
                    <span>{session.live ? 'live' : session.source === 'tandem' ? 'Tandem' : session.source === 'acp' ? 'ACP' : session.historyOnly ? 'history only' : 'history'}</span>
                    {session.updatedAt && <span>{displayDate(session.updatedAt)}</span>}
                  </div>
                </div>
                {hits.map((hit) => <Hit key={hit.entryId} hit={hit} />)}
                {!session.resumable && session.resumeError && <div className="resume-disabled-reason">{session.resumeError}</div>}
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
