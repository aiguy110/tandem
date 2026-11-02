import { useEffect, useMemo, useRef, useState } from 'react';
import { isLocalHost, useStore } from '../store';
import { openArtifact, parseArtifactRoute, rememberArtifact, useLocationHash, type ArtifactEvent } from '../artifacts';
import { fuzzyFilter } from '../fuzzy';
import { ArtifactTile } from './ArtifactView';

interface Entry {
  key: string;
  // The UI's session ID: namespaced (fed~<host>~<session>) for a remote host,
  // so the asset fetch is routed to the daemon that owns it.
  sessionId: string;
  sessionName: string;
  hostName?: string;
  artifact: ArtifactEvent;
}

// Every artifact published by every readable host's sessions, newest first.
// Opening one shows the full artifact page over the gallery; Back returns here.
export function ArtifactsGallery() {
  const setModal = useStore((s) => s.setModal);
  const listArtifacts = useStore((s) => s.listArtifacts);
  const hosts = useStore((s) => s.hosts);
  const [entries, setEntries] = useState<Entry[] | null>(null);
  const [errors, setErrors] = useState<string[]>([]);
  const [query, setQuery] = useState('');
  const inputRef = useRef<HTMLInputElement>(null);
  const artifactOpen = parseArtifactRoute(useLocationHash()) !== null;

  const targets = useMemo(
    () => hosts.filter((host) => host.local || host.status === 'connected' || host.status === 'accepted'),
    [hosts],
  );
  const targetKey = targets.map((host) => host.id).join('\n');
  const remote = targets.some((host) => !isLocalHost(host.id) && !host.local);

  useEffect(() => {
    let live = true;
    // Before the host list arrives, the local daemon is still worth asking.
    const asked = targets.length ? targets : [{ id: 'local', local: true, name: undefined }];
    void Promise.allSettled(asked.map((host) => listArtifacts(host.local ? 'local' : host.id).then((list) => list.map((item): Entry => {
      const sessionId = host.local || isLocalHost(host.id) ? item.sessionId : `fed~${host.id}~${item.sessionId}`;
      return {
        key: `${sessionId}\u0000${item.artifact.artifactId}`,
        sessionId,
        sessionName: item.sessionName,
        hostName: host.local ? undefined : host.name ?? host.id,
        artifact: item.artifact,
      };
    })))).then((results) => {
      if (!live) return;
      const found: Entry[] = [];
      const failed: string[] = [];
      results.forEach((result, i) => {
        if (result.status === 'fulfilled') found.push(...result.value);
        else failed.push(`${asked[i].name ?? asked[i].id}: ${result.reason instanceof Error ? result.reason.message : String(result.reason)}`);
      });
      found.sort((a, b) => b.artifact.createdAt - a.artifact.createdAt);
      setEntries(found);
      setErrors(failed);
    });
    return () => { live = false; };
    // targetKey stands in for targets: refetch only when the host set changes.
  }, [listArtifacts, targetKey]);

  // Back from an artifact page lands here again, ready to filter.
  useEffect(() => {
    if (!artifactOpen) inputRef.current?.focus();
  }, [artifactOpen]);

  const shown = useMemo(
    () => fuzzyFilter(query.trim(), entries ?? [], (entry) => `${entry.artifact.title} ${entry.sessionName} ${entry.hostName ?? ''}`),
    [entries, query],
  );

  const open = (entry: Entry) => {
    rememberArtifact(entry.sessionId, entry.artifact, entry.sessionName);
    openArtifact(entry.sessionId, entry.artifact.artifactId, entry.artifact.revision);
  };

  return <div className="modal-scrim" onMouseDown={(e) => e.target === e.currentTarget && setModal('none')}>
    <div className="modal artifacts-gallery" role="dialog" aria-modal="true" aria-labelledby="artifacts-gallery-title" onKeyDown={(event) => {
      if (event.key === 'Escape') { event.preventDefault(); setModal('none'); }
    }}>
      <div className="fleet-header">
        <div>
          <div className="primary" id="artifacts-gallery-title">Artifacts Gallery</div>
          <div className="sub">{entries ? `${entries.length} artifact${entries.length === 1 ? '' : 's'}${remote ? ' across hosts' : ''}` : 'Loading…'}</div>
        </div>
        <button type="button" className="fleet-close" onClick={() => setModal('none')} aria-label="Close artifacts gallery">×</button>
      </div>
      <input
        ref={inputRef}
        autoFocus
        className="artifacts-gallery-filter"
        placeholder="Filter by title or session…"
        value={query}
        onChange={(event) => setQuery(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === 'Enter' && shown[0]) { event.preventDefault(); open(shown[0]); }
        }}
      />
      {errors.map((error) => <div key={error} className="modal-err">{error}</div>)}
      <div className="artifacts-gallery-body">
        {entries && shown.length === 0 && <div className="artifacts-gallery-empty">{entries.length ? 'No artifacts match.' : 'No artifacts yet. Agents publish them with artifact_publish.'}</div>}
        <ul className="artifacts-gallery-grid">
          {shown.map((entry) => <ArtifactTile
            key={entry.key}
            sessionId={entry.sessionId}
            artifact={entry.artifact}
            source={[entry.hostName, entry.sessionName].filter(Boolean).join(' / ')}
            onOpen={() => open(entry)}
          />)}
        </ul>
      </div>
    </div>
  </div>;
}
