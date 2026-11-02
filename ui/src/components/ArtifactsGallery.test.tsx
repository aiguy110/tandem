import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import { parseArtifactRoute } from '../artifacts';
import type { GalleryArtifact } from '../wire';
import { ArtifactsGallery } from './ArtifactsGallery';

const initialState = useStore.getState();
const item = (sessionId: string, title: string, createdAt: number): GalleryArtifact => ({
  sessionId,
  sessionName: `name-${sessionId}`,
  artifact: { kind: 'artifact', artifactId: `${title}-id`, revision: `${title}-rev`, revisionNumber: 2, type: 'html', title, filename: `${title}.html`, mimeType: 'text/html', size: 10, createdAt },
});

beforeEach(() => {
  history.replaceState(null, '', '/');
  vi.spyOn(globalThis, 'fetch').mockImplementation(async () => new Response('<p>x</p>', { status: 200 }));
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  useStore.setState(initialState, true);
  history.replaceState(null, '', '/');
});

describe('ArtifactsGallery', () => {
  it('merges hosts newest first, namespaces remote sessions, filters, and opens', async () => {
    const listArtifacts = vi.fn(async (hostId: string) => hostId === 'local'
      ? [item('s1', 'Local report', 10)]
      : [item('s9', 'Remote plan', 20)]);
    useStore.setState({
      listArtifacts,
      hosts: [{ id: 'local', local: true }, { id: 'box', name: 'Box', status: 'connected' }, { id: 'gone', status: 'offline' }],
    });
    render(<ArtifactsGallery />);
    await screen.findByText('Remote plan');
    expect(listArtifacts.mock.calls.map(([host]) => host)).toEqual(['local', 'box']);
    const titles = screen.getAllByRole('button', { name: /^Open / }).map((el) => el.getAttribute('aria-label'));
    expect(titles).toEqual(['Open Remote plan', 'Open Local report']);
    expect(screen.getByText(/Box \/ name-s9/)).toBeTruthy();

    fireEvent.change(screen.getByPlaceholderText(/Filter/), { target: { value: 'local' } });
    await waitFor(() => expect(screen.queryByText('Remote plan')).toBeNull());

    fireEvent.change(screen.getByPlaceholderText(/Filter/), { target: { value: 'remote' } });
    fireEvent.keyDown(screen.getByPlaceholderText(/Filter/), { key: 'Enter' });
    expect(parseArtifactRoute(window.location.hash)).toEqual({ sessionId: 'fed~box~s9', artifactId: 'Remote plan-id', revision: 'Remote plan-rev' });
  });

  it('shows a failing host without hiding the others', async () => {
    useStore.setState({
      listArtifacts: vi.fn(async (hostId: string) => {
        if (hostId === 'old') throw new Error('list_artifacts not implemented yet');
        return [item('s1', 'Local report', 10)];
      }),
      hosts: [{ id: 'local', local: true }, { id: 'old', name: 'Old', status: 'connected' }],
    });
    render(<ArtifactsGallery />);
    await screen.findByText('Local report');
    expect(screen.getByText('Old: list_artifacts not implemented yet')).toBeTruthy();
  });
});
