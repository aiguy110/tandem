import { Fragment, useEffect, useMemo, useRef, useState } from 'react';
import { buildResumeGroups, flattenGroups, sessionKey, sessionName } from '../resume';
import type { ResumeEntry } from '../resume';
import { useStore } from '../store';
import type { HistoryExcerpt, SessionSearchHit, SessionSearchResult } from '../wire';

function displayDate(value?: string): string {
  if (!value) return '';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '' : date.toLocaleString();
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

function sourceLabel(entry: ResumeEntry): string {
  const { session } = entry;
  if (session.source === 'tandem') return 'Tandem';
  if (session.source === 'acp') return 'ACP';
  return session.historyOnly ? 'history only' : 'history';
}

export function ResumePalette() {
  const catalog = useStore((s) => s.resumeCatalog);
  const loading = useStore((s) => s.resumeLoading);
  const agents = useStore((s) => s.agents);
  const order = useStore((s) => s.order);
  const searchSessions = useStore((s) => s.searchSessions);
  const resumeSession = useStore((s) => s.resumeSession);
  const focus = useStore((s) => s.focus);
  const setModal = useStore((s) => s.setModal);
  const setPane = useStore((s) => s.setPane);
  const [query, setQuery] = useState('');
  const [historyResults, setHistoryResults] = useState<SessionSearchResult[]>([]);
  const [searching, setSearching] = useState(false);
  const [sel, setSel] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const searchGeneration = useRef(0);
  const rowsRef = useRef<HTMLDivElement | null>(null);

  const sessions = catalog?.sessions ?? [];
  const liveAgents = useMemo(
    () => order.map((id) => agents[id]).filter((agent): agent is NonNullable<typeof agent> => !!agent),
    [order, agents],
  );
  const groups = useMemo(
    () => buildResumeGroups(query, sessions, liveAgents, historyResults),
    [query, sessions, liveAgents, historyResults],
  );
  const flat = useMemo(() => flattenGroups(groups), [groups]);

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
    if (sel >= flat.length) setSel(Math.max(0, flat.length - 1));
  }, [flat.length, sel]);

  useEffect(() => {
    rowsRef.current?.querySelector('.resume-row.sel')?.scrollIntoView({ block: 'nearest' });
  }, [sel, flat.length]);

  // Reviving an active session must not spawn a second agent against the same
  // transcript — it is already open, so Resume just focuses it.
  const open = async (entry: ResumeEntry | undefined) => {
    if (!entry || busy) return;
    if (entry.liveAgentId) {
      focus(entry.liveAgentId);
      setPane('chat');
      setModal('none');
      return;
    }
    if (!entry.session.resumable) {
      setError(entry.session.resumeError ?? 'This transcript is searchable but cannot be resumed.');
      return;
    }
    setBusy(true);
    setError(null);
    const result = await resumeSession(entry.session);
    if (result.error) {
      setError(result.error);
      setBusy(false);
    }
  };

  const unsupported = catalog?.adapters.filter((a) => !a.supportsList).map((a) => a.agent) ?? [];
  let rowIndex = -1;

  return (
    <div className="modal-scrim" onMouseDown={(event) => event.target === event.currentTarget && setModal('none')}>
      <div
        className="modal resume-modal"
        onKeyDown={(event) => {
          if (event.key === 'Escape') return setModal('none');
          if (event.key === 'ArrowDown') {
            event.preventDefault();
            setSel((index) => (flat.length === 0 ? 0 : Math.min(flat.length - 1, index + 1)));
          } else if (event.key === 'ArrowUp') {
            event.preventDefault();
            setSel((index) => Math.max(0, index - 1));
          } else if (event.key === 'Enter') {
            event.preventDefault();
            // A result is a session, even when it contains several transcript hits.
            void open(flat[sel]);
          }
        }}
      >
        <input
          className="q"
          autoFocus
          placeholder="Resume a session — name, repo, or transcript…"
          value={query}
          onChange={(event) => setQuery(event.target.value)}
        />
        <div className="rows" ref={rowsRef}>
          {loading && sessions.length === 0 && <div className="empty">Discovering sessions…</div>}
          {!loading && !searching && flat.length === 0 && <div className="empty">No matching sessions found.</div>}
          {searching && flat.length === 0 && <div className="empty">Searching conversation history…</div>}
          {groups.map((group) => (
            <div className="resume-group" key={group.repoKey}>
              <div className="resume-group-head" title={group.repoKey}>
                <span className="resume-repo">{group.repo}</span>
                <span className="resume-count">{group.entries.length}</span>
              </div>
              {group.entries.map((entry) => {
                rowIndex += 1;
                const index = rowIndex;
                const { session } = entry;
                const disabled = !entry.liveAgentId && !session.resumable;
                return (
                  <div
                    key={sessionKey(session)}
                    className={`row resume-row${index === sel ? ' sel' : ''}${disabled ? ' disabled' : ''}`}
                    style={busy ? { opacity: 0.6 } : undefined}
                    aria-disabled={disabled}
                    title={disabled ? session.resumeError : undefined}
                    onMouseEnter={() => setSel(index)}
                    onClick={() => void open(entry)}
                  >
                    <div className="resume-content">
                      <div className="resume-heading">
                        <div className="resume-title">
                          <div className="primary">
                            {sessionName(session)}
                            {entry.liveAgentId && <span className="resume-badge">active</span>}
                          </div>
                          <div className="sub">{session.cwd}{session.branch ? ` · ${session.branch}` : ''}</div>
                        </div>
                        <div className="meta">
                          <span>{session.agent}</span>
                          <span>{entry.liveAgentId ? session.status ?? 'live' : sourceLabel(entry)}</span>
                          {session.updatedAt && <span>{displayDate(session.updatedAt)}</span>}
                        </div>
                      </div>
                      {entry.hits.map((hit) => <Hit key={hit.entryId} hit={hit} />)}
                      {disabled && session.resumeError && <div className="resume-disabled-reason">{session.resumeError}</div>}
                    </div>
                  </div>
                );
              })}
            </div>
          ))}
        </div>
        {error && <div className="modal-err">{error}</div>}
        {unsupported.length > 0 && (
          <div className="modal-err">External sessions cannot be enumerated for: {unsupported.join(', ')}. Tandem-owned sessions still appear.</div>
        )}
        <div className="foot">
          <span><span className="kbd">↵</span> {flat[sel]?.liveAgentId ? 'focus' : 'resume'}</span>
          <span><span className="kbd">↑↓</span> navigate</span>
          <span><span className="kbd">Esc</span> close</span>
        </div>
      </div>
    </div>
  );
}
