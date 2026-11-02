// Rendered artifacts (docs/artifacts.md). An agent's artifact_publish commits
// a revision of the artifact and logs an `artifact` transcript event naming
// that revision; the transcript previews it and
// `#/artifact/<sessionId>/<artifactId>[/<revision>]` shows it full-page. The
// session only routes requests to the host that owns the artifact. A hash route (not a path) keeps the relative-base UI bundle
// loadable when that URL is reloaded, and puts the page on browser history so
// Back returns to the chat.

import { useEffect, useState, useSyncExternalStore } from 'react';
import { storedToken } from './ws/client';
import type { WireEvent } from './wire';

export type ArtifactEvent = Extract<WireEvent, { kind: 'artifact' }>;

export interface ArtifactRoute {
  sessionId: string;
  artifactId: string;
  // Absent for artifacts logged before revisions existed (and for links to
  // them), whose document is the session asset named by artifactId.
  revision?: string;
}

const ROUTE_PREFIX = '#/artifact/';
const ROUTE_EVENT = 'tandem:artifact-route';

export function artifactHash(sessionId: string, artifactId: string, revision?: string): string {
  const base = `${ROUTE_PREFIX}${encodeURIComponent(sessionId)}/${encodeURIComponent(artifactId)}`;
  return revision ? `${base}/${encodeURIComponent(revision)}` : base;
}

export function parseArtifactRoute(hash: string): ArtifactRoute | null {
  if (!hash.startsWith(ROUTE_PREFIX)) return null;
  const [sid, aid, rev, ...rest] = hash.slice(ROUTE_PREFIX.length).split('/');
  if (!sid || !aid || rev === '' || rest.length) return null;
  try {
    const route: ArtifactRoute = { sessionId: decodeURIComponent(sid), artifactId: decodeURIComponent(aid) };
    if (rev) route.revision = decodeURIComponent(rev);
    return route;
  } catch {
    return null;
  }
}

// Metadata for artifacts opened from the gallery, whose session transcript may
// not be loaded: the full page falls back to it for the title and filename.
const known = new Map<string, { artifact: ArtifactEvent; sessionName?: string }>();
const MAX_KNOWN = 500;

export function rememberArtifact(sessionId: string, artifact: ArtifactEvent, sessionName?: string): void {
  known.set(`${sessionId}\u0000${artifact.artifactId}`, { artifact, sessionName });
  while (known.size > MAX_KNOWN) known.delete(known.keys().next().value as string);
}

export function knownArtifact(sessionId: string, artifactId: string): { artifact: ArtifactEvent; sessionName?: string } | undefined {
  return known.get(`${sessionId}\u0000${artifactId}`);
}

export function openArtifact(sessionId: string, artifactId: string, revision?: string): void {
  history.pushState({ tandemArtifact: true }, '', artifactHash(sessionId, artifactId, revision));
  window.dispatchEvent(new Event(ROUTE_EVENT));
}

// Switch the open artifact page to another revision without adding a history
// entry, so Back still returns to where the page was opened from.
export function showArtifactRevision(route: ArtifactRoute, revision: string): void {
  history.replaceState(history.state, '', artifactHash(route.sessionId, route.artifactId, revision));
  window.dispatchEvent(new Event(ROUTE_EVENT));
}

// Back to the chat: pop the entry openArtifact pushed, or, for a page opened
// directly from a link or reload, replace it so there is nothing to go back to.
export function closeArtifact(): void {
  if ((history.state as { tandemArtifact?: boolean } | null)?.tandemArtifact) {
    history.back();
    return;
  }
  history.replaceState(null, '', window.location.pathname + window.location.search);
  window.dispatchEvent(new Event(ROUTE_EVENT));
}

function subscribe(onChange: () => void): () => void {
  window.addEventListener('popstate', onChange);
  window.addEventListener('hashchange', onChange);
  window.addEventListener(ROUTE_EVENT, onChange);
  return () => {
    window.removeEventListener('popstate', onChange);
    window.removeEventListener('hashchange', onChange);
    window.removeEventListener(ROUTE_EVENT, onChange);
  };
}

/** The current hash, re-rendering on navigation. Parse it with parseArtifactRoute. */
export function useLocationHash(): string {
  return useSyncExternalStore(subscribe, () => window.location.hash);
}

export function artifactRouteOpen(): boolean {
  return parseArtifactRoute(window.location.hash) !== null;
}

function artifactUrl(sessionId: string, artifactId: string, revision?: string): string {
  const session = `/api/agents/${encodeURIComponent(sessionId)}`;
  return revision
    ? `${session}/artifacts/${encodeURIComponent(artifactId)}/${encodeURIComponent(revision)}`
    : `${session}/assets/${encodeURIComponent(artifactId)}`;
}

// Artifacts are content-addressed and immutable, so a fetched document is
// reused by every preview and the full page. Bounded so a long session full
// of large artifacts does not pin them all in memory.
const MAX_CACHED = 24;
const cache = new Map<string, Promise<string>>();

export function fetchArtifact(sessionId: string, artifactId: string, revision?: string): Promise<string> {
  const key = `${sessionId}\u0000${artifactId}\u0000${revision ?? ''}`;
  const hit = cache.get(key);
  if (hit) {
    cache.delete(key);
    cache.set(key, hit);
    return hit;
  }
  const token = storedToken();
  const pending = fetch(artifactUrl(sessionId, artifactId, revision), { headers: token ? { Authorization: `Bearer ${token}` } : {} })
    .then((response) => {
      if (!response.ok) throw new Error(response.status === 404 ? 'Artifact not found' : `Artifact fetch failed (${response.status})`);
      return response.text();
    });
  pending.catch(() => cache.delete(key));
  cache.set(key, pending);
  while (cache.size > MAX_CACHED) cache.delete(cache.keys().next().value as string);
  return pending;
}

export type ArtifactContent = { state: 'loading' } | { state: 'ready'; html: string } | { state: 'error'; message: string };

export function useArtifactContent(sessionId: string | undefined, artifactId: string, revision?: string, enabled = true): ArtifactContent {
  const [content, setContent] = useState<ArtifactContent>({ state: 'loading' });
  useEffect(() => {
    if (!sessionId || !enabled) return;
    let live = true;
    setContent({ state: 'loading' });
    fetchArtifact(sessionId, artifactId, revision)
      .then((html) => { if (live) setContent({ state: 'ready', html }); })
      .catch((error: unknown) => { if (live) setContent({ state: 'error', message: error instanceof Error ? error.message : String(error) }); });
    return () => { live = false; };
  }, [sessionId, artifactId, revision, enabled]);
  return content;
}

export function downloadArtifact(html: string, filename: string): void {
  const url = URL.createObjectURL(new Blob([html], { type: 'text/html' }));
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

export function formatBytes(size: number): string {
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`;
  return `${(size / 1024 / 1024).toFixed(1)} MB`;
}

// The sandbox for agent-authored HTML: scripts run, but without
// allow-same-origin the document has an opaque origin and cannot reach
// Tandem's storage (the bearer token), cookies, or DOM.
export const ARTIFACT_SANDBOX = 'allow-scripts allow-forms allow-modals allow-popups allow-popups-to-escape-sandbox allow-downloads';
// The inline preview is a thumbnail: no dialogs, popups, or downloads.
export const ARTIFACT_PREVIEW_SANDBOX = 'allow-scripts';
