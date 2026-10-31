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
});
