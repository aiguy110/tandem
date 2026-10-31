// Rendered artifacts (docs/artifacts.md). An agent's artifact_publish stores
// the document as a session asset and logs an `artifact` transcript event; the
// transcript previews it and `#/artifact/<sessionId>/<artifactId>` shows it
// full-page. A hash route (not a path) keeps the relative-base UI bundle
// loadable when that URL is reloaded, and puts the page on browser history so
// Back returns to the chat.

import { useEffect, useState, useSyncExternalStore } from 'react';
import { storedToken } from './ws/client';
import type { WireEvent } from './wire';

export type ArtifactEvent = Extract<WireEvent, { kind: 'artifact' }>;

export interface ArtifactRoute {
  sessionId: string;
  artifactId: string;
}

const ROUTE_PREFIX = '#/artifact/';
const ROUTE_EVENT = 'tandem:artifact-route';

export function artifactHash(sessionId: string, artifactId: string): string {
  return `${ROUTE_PREFIX}${encodeURIComponent(sessionId)}/${encodeURIComponent(artifactId)}`;
}

export function parseArtifactRoute(hash: string): ArtifactRoute | null {
  if (!hash.startsWith(ROUTE_PREFIX)) return null;
  const [sid, aid, ...rest] = hash.slice(ROUTE_PREFIX.length).split('/');
  if (!sid || !aid || rest.length) return null;
  try {
    return { sessionId: decodeURIComponent(sid), artifactId: decodeURIComponent(aid) };
  } catch {
    return null;
  }
}

export function openArtifact(sessionId: string, artifactId: string): void {
  history.pushState({ tandemArtifact: true }, '', artifactHash(sessionId, artifactId));
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

function artifactUrl(sessionId: string, artifactId: string): string {
  return `/api/agents/${encodeURIComponent(sessionId)}/assets/${encodeURIComponent(artifactId)}`;
}

// Artifacts are content-addressed and immutable, so a fetched document is
// reused by every preview and the full page. Bounded so a long session full
// of large artifacts does not pin them all in memory.
const MAX_CACHED = 24;
const cache = new Map<string, Promise<string>>();

export function fetchArtifact(sessionId: string, artifactId: string): Promise<string> {
  const key = `${sessionId}\u0000${artifactId}`;
  const hit = cache.get(key);
  if (hit) {
    cache.delete(key);
    cache.set(key, hit);
    return hit;
  }
  const token = storedToken();
  const pending = fetch(artifactUrl(sessionId, artifactId), { headers: token ? { Authorization: `Bearer ${token}` } : {} })
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

export function useArtifactContent(sessionId: string | undefined, artifactId: string, enabled = true): ArtifactContent {
  const [content, setContent] = useState<ArtifactContent>({ state: 'loading' });
  useEffect(() => {
    if (!sessionId || !enabled) return;
    let live = true;
    setContent({ state: 'loading' });
    fetchArtifact(sessionId, artifactId)
      .then((html) => { if (live) setContent({ state: 'ready', html }); })
      .catch((error: unknown) => { if (live) setContent({ state: 'error', message: error instanceof Error ? error.message : String(error) }); });
    return () => { live = false; };
  }, [sessionId, artifactId, enabled]);
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
