import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { useStore } from '../store';
import {
  ARTIFACT_PREVIEW_SANDBOX,
  ARTIFACT_SANDBOX,
  closeArtifact,
  downloadArtifact,
  formatBytes,
  openArtifact,
  useArtifactContent,
  type ArtifactEvent,
  type ArtifactRoute,
} from '../artifacts';

// The preview renders the artifact at a desktop viewport and scales it down,
// so it looks like a thumbnail of the full page rather than a squeezed layout.
const PREVIEW_WIDTH = 1280;
const PREVIEW_HEIGHT = 800;

// Inline transcript card: title, size, and a live scaled-down preview. The
// whole preview opens the artifact page; the preview itself is inert.
export function ArtifactCard({ sessionId, artifact }: { sessionId: string; artifact: ArtifactEvent }) {
  const frameRef = useRef<HTMLDivElement>(null);
  const [scale, setScale] = useState(0.4);
  const [visible, setVisible] = useState(typeof IntersectionObserver === 'undefined');
  const content = useArtifactContent(sessionId, artifact.artifactId, visible);

  useLayoutEffect(() => {
    const el = frameRef.current;
    if (!el) return;
    const measure = () => { if (el.clientWidth > 0) setScale(el.clientWidth / PREVIEW_WIDTH); };
    measure();
    if (typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  // Long transcripts can hold many artifacts; only load (and run) the ones
  // scrolled near view. Once loaded, a preview stays mounted.
  useEffect(() => {
    const el = frameRef.current;
    if (visible || !el) return;
    const observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) setVisible(true);
    }, { rootMargin: '400px' });
    observer.observe(el);
    return () => observer.disconnect();
  }, [visible]);

  const open = () => openArtifact(sessionId, artifact.artifactId);
  return (
    <section className="artifact-card" aria-label={`Artifact: ${artifact.title}`}>
      <header className="artifact-card-head">
        <span className="artifact-card-badge">HTML</span>
        <button type="button" className="artifact-card-title" onClick={open} title="Open artifact">{artifact.title}</button>
        <span className="artifact-card-meta">{formatBytes(artifact.size)}</span>
        <button
          type="button"
          className="artifact-card-download"
          disabled={content.state !== 'ready'}
          onClick={() => content.state === 'ready' && downloadArtifact(content.html, artifact.filename)}
          title={`Download ${artifact.filename}`}
        >
          Download
        </button>
      </header>
      {/* Not a <button>: an iframe is not allowed inside one. */}
      <div
        ref={frameRef}
        role="button"
        tabIndex={0}
        className="artifact-preview"
        style={{ height: PREVIEW_HEIGHT * scale }}
        onClick={open}
        onKeyDown={(event) => {
          if (event.key === 'Enter' || event.key === ' ') {
            event.preventDefault();
            open();
          }
        }}
        aria-label={`Open ${artifact.title}`}
      >
        {content.state === 'ready' && (
          <iframe
            title={`Preview of ${artifact.title}`}
            sandbox={ARTIFACT_PREVIEW_SANDBOX}
            srcDoc={content.html}
            tabIndex={-1}
            aria-hidden="true"
            style={{ width: PREVIEW_WIDTH, height: PREVIEW_HEIGHT, transform: `scale(${scale})` }}
          />
        )}
        {content.state === 'loading' && <span className="artifact-preview-status">Loading preview…</span>}
        {content.state === 'error' && <span className="artifact-preview-status error">{content.message}</span>}
        <span className="artifact-preview-open" aria-hidden="true">Open ↗</span>
      </div>
    </section>
  );
}

// Full-page artifact view, shown over the app for #/artifact/<session>/<id>.
// The app stays mounted underneath so Back returns to the chat exactly as it
// was left.
export function ArtifactPage({ route }: { route: ArtifactRoute }) {
  const events = useStore((s) => s.sessions[route.sessionId]?.events);
  const sessionName = useStore((s) => s.sessions[route.sessionId]?.name);
  const artifact = useMemo(() => {
    if (!events) return undefined;
    for (let i = events.length - 1; i >= 0; i--) {
      const ev = events[i].event;
      if (ev.kind === 'artifact' && ev.artifactId === route.artifactId) return ev;
    }
    return undefined;
  }, [events, route.artifactId]);
  const content = useArtifactContent(route.sessionId, route.artifactId);
  const title = artifact?.title ?? 'Artifact';
  const filename = artifact?.filename ?? `artifact-${route.artifactId.slice(0, 8)}.html`;
  const backRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    backRef.current?.focus();
    const previous = document.title;
    document.title = `${title} · Tandem`;
    return () => { document.title = previous; };
  }, [title]);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        closeArtifact();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  return (
    <div className="artifact-page" role="dialog" aria-modal="true" aria-label={title}>
      <header className="artifact-page-bar">
        <button type="button" ref={backRef} className="artifact-page-back" onClick={closeArtifact} title="Back to chat (Esc)">
          ← Back
        </button>
        <div className="artifact-page-heading">
          <span className="artifact-page-title">{title}</span>
          <span className="artifact-page-meta">
            {[sessionName, artifact && formatBytes(artifact.size), artifact && new Date(artifact.createdAt).toLocaleString()].filter(Boolean).join(' · ')}
          </span>
        </div>
        <button
          type="button"
          className="artifact-page-download"
          disabled={content.state !== 'ready'}
          onClick={() => content.state === 'ready' && downloadArtifact(content.html, filename)}
          title={`Download ${filename}`}
        >
          Download
        </button>
      </header>
      <div className="artifact-page-body">
        {content.state === 'ready' && <iframe title={title} sandbox={ARTIFACT_SANDBOX} srcDoc={content.html} />}
        {content.state === 'loading' && <div className="artifact-page-status">Loading…</div>}
        {content.state === 'error' && <div className="artifact-page-status error">{content.message}</div>}
      </div>
    </div>
  );
}
