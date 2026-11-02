import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import type { SessionView } from '../store';
import { artifactHash, parseArtifactRoute, type ArtifactEvent } from '../artifacts';
import { ArtifactCard, ArtifactPage } from './ArtifactView';

const initialState = useStore.getState();
const artifact: ArtifactEvent = { kind: 'artifact', artifactId: 'a'.repeat(64), type: 'html', title: 'Latency report', filename: 'latency.html', mimeType: 'text/html', size: 2048, createdAt: 1 };

beforeEach(() => {
  history.replaceState(null, '', '/');
  useStore.setState({ listArtifactRevisions: vi.fn(async () => []) });
  vi.spyOn(globalThis, 'fetch').mockImplementation(async () => new Response('<h1>Report</h1>', { status: 200 }));
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  useStore.setState(initialState, true);
  history.replaceState(null, '', '/');
});

describe('artifact routes', () => {
  it('round-trips federated session IDs', () => {
    const hash = artifactHash('fed~host~api/7', 'abc');
    expect(parseArtifactRoute(hash)).toEqual({ sessionId: 'fed~host~api/7', artifactId: 'abc' });
    expect(parseArtifactRoute('#t=token')).toBeNull();
    expect(parseArtifactRoute('#/artifact/only')).toBeNull();
    expect(parseArtifactRoute(artifactHash('s', 'art-1', 'f00d'))).toEqual({ sessionId: 's', artifactId: 'art-1', revision: 'f00d' });
    expect(parseArtifactRoute('#/artifact/s/a/')).toBeNull();
  });
});

describe('ArtifactCard', () => {
  it('previews in a script-only sandbox and opens the artifact page', async () => {
    render(<ArtifactCard sessionId="sess-1" artifact={artifact} />);
    const frame = await screen.findByTitle('Preview of Latency report');
    expect(frame.getAttribute('sandbox')).toBe('allow-scripts');
    expect(frame.getAttribute('srcdoc')).toBe('<h1>Report</h1>');
    const [url, init] = vi.mocked(fetch).mock.calls[0];
    expect(url).toBe(`/api/agents/sess-1/assets/${artifact.artifactId}`);
    expect(init).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Open Latency report' }));
    expect(parseArtifactRoute(window.location.hash)).toEqual({ sessionId: 'sess-1', artifactId: artifact.artifactId });
  });

  it('previews and opens the exact revision it announced', async () => {
    render(<ArtifactCard sessionId="sess-1" artifact={{ ...artifact, artifactId: 'art-1', revision: 'beef', revisionNumber: 3 }} />);
    await screen.findByTitle('Preview of Latency report');
    expect(vi.mocked(fetch).mock.calls[0][0]).toBe('/api/agents/sess-1/artifacts/art-1/beef');
    expect(screen.getByText('rev 3')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Open Latency report' }));
    expect(parseArtifactRoute(window.location.hash)).toEqual({ sessionId: 'sess-1', artifactId: 'art-1', revision: 'beef' });
  });
});

describe('ArtifactPage', () => {
  it('shows the artifact full-page without same-origin access', async () => {
    useStore.setState({ sessions: { 'sess-2': { id: 'sess-2', name: 'planner', events: [{ seq: 4, event: artifact }] } as unknown as SessionView } });
    render(<ArtifactPage route={{ sessionId: 'sess-2', artifactId: artifact.artifactId }} />);
    expect(screen.getByText('Latency report')).toBeTruthy();
    const frame = await screen.findByTitle('Latency report');
    expect(frame.getAttribute('sandbox')).not.toContain('allow-same-origin');
    await waitFor(() => expect((screen.getByRole('button', { name: 'Download' }) as HTMLButtonElement).disabled).toBe(false));
  });

  it('Back leaves a directly-opened artifact route', () => {
    history.replaceState(null, '', artifactHash('sess-2', 'abc'));
    render(<ArtifactPage route={{ sessionId: 'sess-2', artifactId: 'abc' }} />);
    act(() => { fireEvent.click(screen.getByRole('button', { name: /Back/ })); });
    expect(window.location.hash).toBe('');
  });

  it('picks another revision from the header without adding history', async () => {
    const rev = (n: number, revision: string) => ({ artifactId: 'art-1', revision, revisionNumber: n, title: `Plan v${n}`, filename: 'plan.html', size: 10, contentSha256: `sha${n}`, sessionId: 'sess-3', sessionName: `s${n}`, createdAt: n * 1000 });
    const listArtifactRevisions = vi.fn(async () => [rev(2, 'r2'), rev(1, 'r1')]);
    useStore.setState({ listArtifactRevisions });
    history.replaceState({ tandemArtifact: true }, '', artifactHash('sess-3', 'art-1', 'r1'));
    const { rerender } = render(<ArtifactPage route={{ sessionId: 'sess-3', artifactId: 'art-1', revision: 'r1' }} />);
    const picker = await screen.findByRole('combobox', { name: 'Revision' }) as HTMLSelectElement;
    expect(picker.value).toBe('r1');
    expect(screen.getByText('Plan v1')).toBeTruthy();
    expect(vi.mocked(fetch).mock.calls[0][0]).toBe('/api/agents/sess-3/artifacts/art-1/r1');
    fireEvent.change(picker, { target: { value: 'r2' } });
    const route = parseArtifactRoute(window.location.hash)!;
    expect(route).toEqual({ sessionId: 'sess-3', artifactId: 'art-1', revision: 'r2' });
    expect((history.state as { tandemArtifact?: boolean }).tandemArtifact).toBe(true);
    rerender(<ArtifactPage route={route} />);
    await screen.findByText('Plan v2');
    await waitFor(() => expect(vi.mocked(fetch).mock.calls.some(([url]) => url === '/api/agents/sess-3/artifacts/art-1/r2')).toBe(true));
  });

  it('matches a pre-revision link to its revision by content hash', async () => {
    const sha = 'c'.repeat(64);
    useStore.setState({ listArtifactRevisions: vi.fn(async () => [
      { artifactId: sha, revision: 'r2', revisionNumber: 2, title: 'New', filename: 'a.html', size: 1, contentSha256: 'x', sessionId: 's', createdAt: 2 },
      { artifactId: sha, revision: 'r1', revisionNumber: 1, title: 'Old', filename: 'a.html', size: 1, contentSha256: sha, sessionId: 's', createdAt: 1 },
    ]) });
    render(<ArtifactPage route={{ sessionId: 'sess-4', artifactId: sha }} />);
    const picker = await screen.findByRole('combobox', { name: 'Revision' }) as HTMLSelectElement;
    expect(picker.value).toBe('r1');
    expect(vi.mocked(fetch).mock.calls[0][0]).toBe(`/api/agents/sess-4/assets/${sha}`);
  });
});
